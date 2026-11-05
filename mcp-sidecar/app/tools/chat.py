"""ask 工具（契约 v1，名字/参数/描述/输出与内置 Go MCP 冻结一致）。

  9. ask  端点配置的智能体 + SSE 聚合答案/引用，240s 整体超时；
          多轮 session_id 是侧车自签的 HMAC 不透明票据，绑定 sid+端点+用户。

安全约束（需求.md §5/§9）：
  - 客户端不能点名智能体，只能用端点配置的 default_agent_id；
  - 请求体永不带 web_search_enabled（省略字段），杜绝只读端点触网；
  - 多轮票据验签即归属校验：共用上游 API Key 的唯一隔离边界；
  - 只读 hint=False：会建会话/消息且智能体可能调工具，客户端不得自动批准。

REST 与 Go 内部调用的差异：HTTP QA 处理器自行建消息，侧车不预建；
SSE 事件经 buildStreamResponse 提升后 answer/references/complete/error
字段形状与 Go 总线上的事件一一对应。
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
from typing import Annotated

from mcp.types import CallToolResult, TextContent, ToolAnnotations
from pydantic import Field

from app.auth.middleware import current_principal
from app.backend.chat import (
    AGENT_MODE_SMART_REASONING,
    BUILTIN_AGENT_IDS,
    BUILTIN_QUICK_ANSWER,
    ChatService,
    truncate_title,
)
from app.backend.client import BackendError
from app.backend.kb import KBService, ScopeError
from app.session_token import InvalidSessionTicket, issue_session_ticket, verify_session_ticket
from app.tools.retrieve import (
    _clean_selectors,
    _DENY_MESSAGES,
    ToolError,
    _tool_error_result,
)

logger = logging.getLogger("sidecar.tools.ask")

TOOL_ASK = "ask"

# 引用摘要常量（与 tools_ask.go 对齐）
ASK_EXCERPT_MAX_RUNES = 300
NO_ANSWER_TEXT = "No answer could be produced for this question."
ASK_CHANNEL_DESCRIPTION = "MCP endpoint "

# ask 注解逐字对齐 Go askTool（tools_annotations_test.go 冻结）：Go 侧只显式
# 贴 readOnly=false，其余三个保持 mcp-go SDK 默认（destructive=true、
# idempotent=false、openWorld=true）——语义上 ask 只建会话/消息，但契约即如此，
# 客户端按这套默认提示决定是否自动批准，不能自行软化。
ASK_ANNOTATIONS = ToolAnnotations(
    readOnlyHint=False,
    destructiveHint=True,
    idempotentHint=False,
    openWorldHint=True,
)

# 存储引用正则（Go types.StorageReferencePattern 的 Python 移植）：
# 截断摘录时绝不把 resource:// / xxx:// 句柄切成两半。
STORAGE_REFERENCE_PATTERN = re.compile(
    r"\b(?:resource://[0-9A-Za-z_-]+|(?:storage://[0-9A-Za-z_-]+/)?"
    r"(?:local|minio|s3|cos|tos|oss|obs|ks3)://[^\s)\]>\"]+)"
)


# ===========================================================================
# 纯函数：引用整理（移植 summarizeReferences / truncateExcerpt / images）
# ===========================================================================

def truncate_excerpt(excerpt: str) -> str:
    """300 rune 截断；切点落在存储引用中间时回退到引用起点，末尾补 …。"""
    if len(excerpt) <= ASK_EXCERPT_MAX_RUNES:
        return excerpt
    cut = ASK_EXCERPT_MAX_RUNES
    for match in STORAGE_REFERENCE_PATTERN.finditer(excerpt):
        if match.start() >= cut:
            break
        if cut < match.end():
            cut = match.start()
            break
    return excerpt[:cut] + "…"


def summarize_reference_images(raw_image_info: object, merged: list[dict] | None = None) -> list[dict]:
    """image_info（JSON 字符串）→ 图片引用；按 url 去重并合并空 caption/ocr。"""
    out: list[dict] = list(merged or [])
    try:
        infos = json.loads(raw_image_info) if isinstance(raw_image_info, str) else None
    except (ValueError, TypeError):
        return out
    if not isinstance(infos, list):
        return out
    seen: dict[str, int] = {img["url"]: i for i, img in enumerate(out)}
    for info in infos:
        if not isinstance(info, dict):
            continue
        url = str(info.get("url") or "").strip()
        if not url:
            continue
        caption = str(info.get("caption") or "")
        ocr_text = str(info.get("ocr_text") or "")
        if url in seen:
            index = seen[url]
            if not out[index].get("caption"):
                if caption:
                    out[index]["caption"] = caption
            if not out[index].get("ocr_text"):
                if ocr_text:
                    out[index]["ocr_text"] = ocr_text
            continue
        seen[url] = len(out)
        image = {"url": url}
        if caption:
            image["caption"] = caption
        if ocr_text:
            image["ocr_text"] = ocr_text
        out.append(image)
    return out


def summarize_references(refs: list[dict]) -> list[dict]:
    """SearchResult 列表 → askReference 结构（去重、摘录截断、图片合并）。"""
    out: list[dict] = []
    seen: dict[str, int] = {}
    for raw in refs:
        if not isinstance(raw, dict):
            continue
        key = str(raw.get("id") or "") or (
            f"{raw.get('knowledge_id', '')}:{raw.get('chunk_index', '')}"
        )
        if key in seen:
            index = seen[key]
            out[index]["images"] = summarize_reference_images(
                raw.get("image_info"), out[index].get("images", [])
            )
            if not out[index]["images"]:
                out[index].pop("images", None)
            continue
        seen[key] = len(out)
        ref = {
            "knowledge_id": str(raw.get("knowledge_id") or ""),
            "chunk_id": str(raw.get("id") or ""),
            "excerpt": truncate_excerpt(str(raw.get("content") or "").strip()),
        }
        title = str(raw.get("knowledge_title") or "")
        if title:
            ref["knowledge_title"] = title
        score = raw.get("score")
        try:
            score_value = float(score) if score is not None else 0.0
        except (TypeError, ValueError):
            score_value = 0.0
        if score_value != 0:
            ref["score"] = score_value
        images = summarize_reference_images(raw.get("image_info"))
        if images:
            ref["images"] = images
        out.append(ref)
    return out


def render_answer_text(answer: str, ticket: str, refs: list[dict]) -> str:
    """答案 + Sources 块 + session_id（与 Go handleAsk 文案逐字对齐）。"""
    if not refs:
        return f"{answer}\n\nsession_id: {ticket}"
    lines = [answer, "", "Sources:"]
    for i, ref in enumerate(refs):
        title = ref.get("knowledge_title") or ref["knowledge_id"]
        lines.append(f"[{i + 1}] {title} (document {ref['knowledge_id']})")
        for image in ref.get("images", []):
            # Go strconv.Quote：双引号转义、&<> 保持字面（json.dumps 行为一致）
            lines.append(f"  Image: {json.dumps(image['url'], ensure_ascii=False)}")
    lines.append("")
    lines.append(f"session_id: {ticket}")
    return "\n".join(lines)


# ===========================================================================
# SSE 聚合
# ===========================================================================

async def aggregate_qa(events) -> tuple[str, list[dict], str]:
    """消费 QA SSE 事件流，返回（答案文本, 原始 SearchResult 引用, 错误文案）。

    只认 answer / references / complete / error 四类；其余事件
    （thinking/tool_call/session_title/...）一律忽略，与 Go runQA 一致。

    注意：agent-chat 的 error 事件由 EventBus 异步派发，可能晚于 complete 到达
    （实测 complete.final_content="" 之后才吐 error）。因此 complete 只记录
    final_content 兜底，不中断循环；只有 error 才 break。
    """
    answer_parts: list[str] = []
    refs: list[dict] = []
    error_text = ""
    final_content = ""

    async for event in events:
        rtype = str(event.get("response_type") or "")
        if rtype == "answer":
            answer_parts.append(str(event.get("content") or ""))
        elif rtype == "references":
            top_refs = event.get("knowledge_references")
            if isinstance(top_refs, list):
                refs.extend(r for r in top_refs if isinstance(r, dict))
            else:
                data = event.get("data")
                inner = data.get("references") if isinstance(data, dict) else None
                if isinstance(inner, list):
                    refs.extend(r for r in inner if isinstance(r, dict))
        elif rtype == "error":
            error_text = str(event.get("content") or "")
            if not error_text and isinstance(event.get("data"), dict):
                error_text = str(event["data"].get("error") or "")
            break
        elif rtype == "complete":
            data = event.get("data")
            if isinstance(data, dict):
                final_content = str(data.get("final_content") or "")

    answer = "".join(answer_parts)
    if not answer.strip() and final_content:
        answer = final_content
    return answer, refs, error_text


# ===========================================================================
# 工具集
# ===========================================================================

class AskToolSet:
    """注册并承载 ask 工具；持有票据密钥与整体超时秒数。"""

    def __init__(self, client, guard, session_secret: str, ask_timeout: int = 240) -> None:
        self._client = client
        self._guard = guard
        self._secret = session_secret or ""
        self._timeout = ask_timeout
        self._kbs = KBService(client)
        self._chat = ChatService(client)

    async def _authorize(self, tool_name: str, arguments: dict):
        principal = current_principal()
        if principal is None:
            raise ToolError("unauthorized")
        verdict = await self._guard.authorize(principal, tool_name, arguments)
        if not verdict.allowed:
            raise ToolError(_DENY_MESSAGES.get(verdict.deny_reason, "request denied"))
        return principal, verdict.scope

    async def ask(
        self,
        question: str,
        session_id: str,
        knowledge_base_ids: list[str] | None,
    ) -> CallToolResult:
        question = (question or "").strip()
        if not question:
            raise ToolError("question is required")
        raw_ticket = (session_id or "").strip()
        selectors = _clean_selectors(knowledge_base_ids)

        arguments: dict = {}
        if selectors is not None:
            arguments["knowledge_base_ids"] = selectors
        principal, scope = await self._authorize(TOOL_ASK, arguments)
        endpoint = principal.endpoint

        # ① 解析端点配置的智能体（客户端无权点名）
        agent = await self._resolve_agent(endpoint.default_agent_id, endpoint.tenant_id)
        config = agent.get("config") if isinstance(agent.get("config"), dict) else {}
        use_agent_route = (
            str(config.get("agent_mode") or "").strip().lower() == AGENT_MODE_SMART_REASONING
        )
        agent_id = str(agent.get("id") or "")

        # ② 解析检索库范围（生效闭集已由 guard 编码在 scope 内）
        kb_ids = await self._resolve_kb_ids(scope, selectors)

        # ③ 会话：续问验票（绑定端点+用户）；首轮建会话并签票
        sid, ticket = await self._resolve_session(raw_ticket, question, endpoint, principal.user_id)

        # ④ 发起 QA 并在整体超时内聚合 SSE
        events = self._chat.qa_events(
            sid,
            query=question,
            kb_ids=kb_ids,
            agent_id=agent_id,
            use_agent_route=use_agent_route,
            timeout=float(self._timeout),
        )
        try:
            answer, raw_refs, error_text = await asyncio.wait_for(
                aggregate_qa(events), timeout=self._timeout
            )
        except asyncio.TimeoutError as exc:
            # 冻结文案：Go runQA 返回 "answer timed out: context deadline exceeded"，
            # handleAsk 再套 "failed to answer: " 前缀
            raise ToolError(
                "failed to answer: answer timed out: context deadline exceeded"
            ) from exc
        except BackendError as exc:
            raise ToolError(f"failed to answer: {exc.message}") from exc

        if not answer.strip() and error_text:
            raise ToolError(f"failed to answer: {error_text}")
        if not answer.strip():
            answer = NO_ANSWER_TEXT

        refs = summarize_references(raw_refs)
        structured = {
            "answer": answer,
            "session_id": ticket,
            "agent_id": agent_id,
            "references": refs,
        }
        return CallToolResult(
            content=[TextContent(type="text", text=render_answer_text(answer, ticket, refs))],
            structured_content=structured,
        )

    async def _resolve_agent(self, configured_id: str, tenant_id: int) -> dict:
        agent_id = (configured_id or "").strip() or BUILTIN_QUICK_ANSWER
        try:
            agent = await self._chat.get_agent(agent_id)
        except BackendError:
            # 与 Go 一致：任何取智能体失败都给同一句冻结文案，不暴露上游细节
            agent = None
        if agent is None:
            raise ToolError(
                f'the agent configured on this endpoint ("{agent_id}") is not available'
            )
        is_builtin = bool(agent.get("is_builtin")) or agent_id in BUILTIN_AGENT_IDS
        if is_builtin:
            allowed = agent_id in BUILTIN_AGENT_IDS
        else:
            try:
                agent_tenant = int(agent.get("tenant_id") or 0)
            except (TypeError, ValueError):
                agent_tenant = 0
            allowed = agent_tenant == tenant_id
        if not allowed:
            raise ToolError(
                f'the agent configured on this endpoint ("{agent_id}") is not available'
            )
        return agent

    async def _resolve_kb_ids(self, scope, selectors) -> list[str]:
        """显式选择器优先；否则钉死在生效范围（公共书架∪个人授权）内。

        scope 是闭集，不再有"由智能体配置自选库"的通道：那会让 ask 绕过
        guard 分发的库范围。范围为空直接拒绝。
        """
        try:
            if selectors:
                kbs = await self._kbs.resolve_selectors(scope, selectors)
            else:
                kbs = await self._kbs.resolve_visible(scope)
        except ScopeError as exc:
            raise ToolError(str(exc)) from exc
        except BackendError as exc:
            raise ToolError(f"failed to resolve knowledge bases: {exc.message}") from exc
        if not kbs:
            raise ToolError("no knowledge base is available on this endpoint")
        return [kb.id for kb in kbs]

    async def _resolve_session(self, raw_ticket: str, question: str, endpoint, user_id: str | None):
        """续问验票；首轮建会话。返回 (真实 sid, 对外票据)。"""
        if not self._secret:
            # fail-closed：没配票据密钥时 ask 整体不可用，防止真实 sid 裸发
            raise ToolError("ask is unavailable on this server: session ticket secret is not configured")

        if raw_ticket:
            try:
                sid = verify_session_ticket(self._secret, raw_ticket, endpoint.id, user_id)
            except InvalidSessionTicket:
                # 冻结文案里回显客户端原样传入的 session_id（与 Go %q 一致）
                raise ToolError(f'session "{raw_ticket}" was not found on this endpoint')
            return sid, raw_ticket

        title = truncate_title(question)
        try:
            sid = await self._chat.create_session(title, f"{ASK_CHANNEL_DESCRIPTION}{endpoint.name}")
        except BackendError as exc:
            raise ToolError(f"failed to create session: {exc.message}") from exc
        return sid, issue_session_ticket(self._secret, sid, endpoint.id, user_id)

    # ------------------------------------------------------------------
    # 注册
    # ------------------------------------------------------------------

    def register(self, server) -> None:
        toolset = self

        @server.tool(
            name=TOOL_ASK,
            description=(
                "Ask the workspace a question and get a synthesized answer with citations. "
                "WeKnora retrieves from the knowledge bases in scope and runs the agent configured "
                "on this endpoint. Pass the returned session_id on follow-up questions to keep the "
                "conversation going. This can take up to a few minutes for agentic runs; prefer "
                "search_knowledge when you only need raw passages."
            ),
            annotations=ASK_ANNOTATIONS,
        )
        async def ask(
            question: Annotated[str, Field(
                description="The question to answer", min_length=1,
            )],
            session_id: Annotated[str, Field(
                description="Session id returned by a previous ask call, to continue that conversation",
            )] = "",
            knowledge_base_ids: Annotated[list[str] | None, Field(
                description="Optional knowledge base ids or names to retrieve from; defaults to the "
                "endpoint scope or the agent's own configuration",
            )] = None,
        ) -> dict:
            try:
                return await toolset.ask(question, session_id, knowledge_base_ids)
            except ToolError as exc:
                return _tool_error_result(exc)
