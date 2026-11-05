"""只读工具 1-5（契约 v1，名字/参数/描述与内置 Go MCP 冻结一致）。

  1. list_knowledge_bases
  2. search_knowledge（含 knowledge_ids 文档级检索，≤100）
  3. grep_chunks（关键词候选 + Python 正则后置过滤）
  4. list_documents
  5. read_document（分页 / query 词命中扫描）

每个工具进入时先过 ToolGuard.authorize（白名单→公共书架并集范围→限流→审计），
主体身份只从 contextvars 取，绝不从参数取。所有入参在运行时再钳制一遍，
不信任 JSON Schema。业务错误一律抛 ToolError（MCP isError=true，英文文案）。
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import re
from typing import Annotated, Literal

from mcp.server.mcpserver.exceptions import ToolError
from mcp.types import CallToolResult, TextContent, ToolAnnotations
from pydantic import Field

from app.auth.middleware import current_principal
from app.backend.client import BackendError, RESTClient
from app.backend.chunks import ChunkService
from app.backend.documents import DocumentService, as_dict, parse_document_summary
from app.backend.kb import KBSummary, KBService, ScopeError
from app.guards import (
    REASON_PERMISSION_UNAVAILABLE,
    REASON_RATE_LIMITED,
    REASON_TOOL_DISABLED,
    ToolGuard,
)

logger = logging.getLogger("sidecar.tools")

# 工具名（与 internal/types/mcp_endpoint_tools.go 常量值一致）
TOOL_LIST_KNOWLEDGE_BASES = "list_knowledge_bases"
TOOL_SEARCH_KNOWLEDGE = "search_knowledge"
TOOL_GREP_CHUNKS = "grep_chunks"
TOOL_LIST_DOCUMENTS = "list_documents"
TOOL_READ_DOCUMENT = "read_document"

# 入参钳制常量（与 Go tools_retrieve.go / agent tools 对齐）
SEARCH_DEFAULT_LIMIT = 10
SEARCH_MAX_LIMIT = 30
SEARCH_CANDIDATE_LIMIT = 30  # 每个库拉取的候选池（Go topK 缺省值）
SEARCH_MAX_DOCUMENTS = 100
LIST_DEFAULT_PAGE_SIZE = 20
LIST_MAX_PAGE_SIZE = 100
READ_DEFAULT_LIMIT = 20
READ_MAX_LIMIT = 100
READ_SCAN_PAGE_SIZE = 100
READ_MAX_MATCHES = 20
# Go 的输出预算按模型上下文计算；侧车拿不到该值，用固定字符预算（4/5 法则
# 的近似：保留首块，按内容长度+每块约 120 字符标签开销裁剪）
READ_OUTPUT_BUDGET_CHARS = 60_000
READ_CHUNK_OVERHEAD = 120
# 检索类 REST 调用单次超时（embedding + 召回可能较慢）
SEARCH_TIMEOUT_SECONDS = 60.0

# 只读工具的完整 hint 注解（四个一次写全，原因见 Go readOnlyEndpointAnnotations）
READ_ONLY_ANNOTATIONS = ToolAnnotations(
    readOnlyHint=True,
    destructiveHint=False,
    idempotentHint=True,
    openWorldHint=False,
)

def _tool_error_result(exc: ToolError) -> CallToolResult:
    """业务预期错误 → isError 的 CallToolResult。

    直接返回而不是 raise：SDK 2.3.0 会把工具函数里 raise 的 ToolError 再包一层
    "Error executing tool X:" 前缀，破坏冻结的英文错误文案；返回 is_error 结果
    时 SDK 原样透传（convert_result 对 CallToolResult 不二次转换）。
    """
    return CallToolResult(
        content=[TextContent(type="text", text=str(exc))],
        is_error=True,
    )


def _go_json_indented(payload: dict) -> str:
    """Go json.MarshalIndent 风格文本：两空格缩进；中文原样，
    但 &<> 转成 \\u003x（encoding/json 的默认 HTML 转义）。"""
    text = json.dumps(payload, indent=2, ensure_ascii=False)
    return text.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e")


def _json_result(data: dict) -> CallToolResult:
    """Go jsonResult 等价输出：text = 上述缩进 JSON，同时挂 structured_content，
    客户端两种读法都有（Go 端两者是同一份数据）。"""
    return CallToolResult(
        content=[TextContent(type="text", text=_go_json_indented(data))],
        structured_content=data,
    )


# guard 拒绝原因 → 对外英文文案（稳定契约，不区分细类以免泄露内部分段）
_DENY_MESSAGES = {
    REASON_TOOL_DISABLED: "this tool is not enabled on this endpoint",
    REASON_PERMISSION_UNAVAILABLE: "the permission service is temporarily unavailable; retry shortly",
    # 冻结文案：Go guardTool
    REASON_RATE_LIMITED: "rate limit exceeded for this endpoint, retry shortly",
}

MODE_HYBRID = "hybrid"
MODE_SEMANTIC = "semantic"
MODE_KEYWORD = "keyword"


# ===========================================================================
# 纯函数：grep 正则与候选词（镜像 Go grepPatternAndTerms）
# ===========================================================================

# 形如 \d、\b、\s 这类转义不含字面文本，提取候选词时替换成空格
_GREP_ESCAPE_RE = re.compile(r"\\[A-Za-z]")


def grep_pattern_and_terms(query: str) -> tuple[re.Pattern[str], str]:
    """把 grep 查询拆成（正则, 关键词候选串）。

    - 正则按 (?i) 编译；编译失败则对整个查询转义，按字面量匹配
      （与 Go 一致："在数据库里本来就会失败的表达式按字面量处理"）；
    - 合法正则中的 \\d/\\s 等转义不含字面词，替换为空格后再分词；
    - 分词按大小写不敏感去重保序，用空格拼成关键词查询喂给关键词索引。
    """
    try:
        pattern = re.compile("(?i)" + query)
        source = _GREP_ESCAPE_RE.sub(" ", query)
    except re.error:
        pattern = re.compile("(?i)" + re.escape(query))
        source = query

    terms: list[str] = []
    seen: set[str] = set()
    for field in re.findall(r"[\w-]+", source, flags=re.UNICODE):
        key = field.lower()
        if key not in seen:
            seen.add(key)
            terms.append(field)
    return pattern, " ".join(terms)


# ===========================================================================
# 纯函数：检索模式、去重、输出整理
# ===========================================================================

def effective_modes(kbs: list[KBSummary], requested: str) -> dict[str, tuple[str, bool, str]]:
    """逐库决定实际检索模式（对齐 Go resolveKBSearchModes）。

    返回 {kb_id: (mode, fallback, reason)}；向量和关键词索引都没有的库直接
    不进结果（调用方据此把 wiki-only/图-only 库剔除）。
    """
    modes: dict[str, tuple[str, bool, str]] = {}
    for kb in kbs:
        has_vector, has_keyword = kb.semantic, kb.keyword
        if not has_vector and not has_keyword:
            continue
        if requested == MODE_KEYWORD and not has_keyword:
            reason = (
                "FAQ bases are indexed for semantic search only"
                if kb.type == "faq"
                else "no keyword index"
            )
            modes[kb.id] = (MODE_SEMANTIC, True, reason)
        elif requested == MODE_SEMANTIC and not has_vector:
            modes[kb.id] = (MODE_KEYWORD, True, "no vector index")
        else:
            modes[kb.id] = (requested, False, "")
    return modes


def annotate_mode_fallback(
    data: dict, requested: str, modes: dict[str, tuple[str, bool, str]]
) -> None:
    """把模式降级写进结果（对齐 Go annotateModeFallback）。"""
    data["requested_mode"] = requested
    fallbacks: list[dict] = []
    used: set[str] = set()
    for kb_id in sorted(modes):
        mode, fallback, reason = modes[kb_id]
        used.add(mode)
        if fallback:
            fallbacks.append({"knowledge_base_id": kb_id, "mode": mode, "reason": reason})
    if fallbacks:
        data["mode_fallbacks"] = fallbacks
    if len(used) == 1:
        (data["mode"],) = tuple(used)
    else:
        data["mode"] = requested


def content_signature(content: str) -> str:
    """近似重复内容签名（与 Go BuildContentSignature 一致：小写、空白归一、MD5）。"""
    normalized = " ".join(content.strip().lower().split())
    if not normalized:
        return ""
    return hashlib.md5(normalized.encode("utf-8")).hexdigest()


def dedupe_search_rows(rows: list[dict]) -> list[dict]:
    """按分块 id / 文档内位置 / 内容签名去重，高分优先（对齐 Go deduplicateResults）。"""
    ordered = sorted(rows, key=lambda r: float(r.get("score") or 0.0), reverse=True)
    seen_ids: set[str] = set()
    seen_sigs: set[str] = set()
    out: list[dict] = []
    for row in ordered:
        row_id = str(row.get("id") or "")
        keys = [row_id]
        chunk_type = str(row.get("chunk_type") or "text")
        knowledge_id = str(row.get("knowledge_id") or "")
        is_text = chunk_type in ("", "text")
        if knowledge_id and is_text:
            keys.append(f"kb:{knowledge_id}#{row.get('chunk_index')}")
        if any(key in seen_ids for key in keys if key):
            continue
        sig = content_signature(str(row.get("content") or ""))
        if sig and sig in seen_sigs:
            continue
        if sig:
            seen_sigs.add(sig)
        for key in keys:
            if key:
                seen_ids.add(key)
        out.append(row)
    return out


# ---- 片段截取（与 Go chunk_snippet.go 同策略的 Python 版） ----

_SNIPPET_CONTEXT_RUNES = 200
_SNIPPET_MAX_TOTAL_RUNES = 800


def _clip_window(text: str, start: int, end: int) -> str:
    before = text[:start]
    match = text[start:end]
    after = text[end:]
    before = before[-_SNIPPET_CONTEXT_RUNES:]
    after = after[:_SNIPPET_CONTEXT_RUNES]
    snippet = re.sub(r"\s+", " ", before + match + after).strip()
    if len(snippet) > _SNIPPET_MAX_TOTAL_RUNES:
        snippet = snippet[:_SNIPPET_MAX_TOTAL_RUNES] + "..."
    return f"... {snippet} ..."


def snippet_regex(content: str, pattern: re.Pattern[str]) -> str:
    if not content:
        return ""
    match = pattern.search(content)
    if not match:
        return ""
    return _clip_window(content, match.start(), match.end())


def snippet_for_terms(content: str, terms: list[str]) -> str:
    """在内容里找任一关键词最早出现位置做窗口；找不到则给开头摘要。"""
    content = content.strip()
    if not content:
        return ""
    lowered = content.lower()
    earliest = -1
    earliest_end = -1
    for term in terms:
        if len(term) < 2:
            continue
        index = lowered.find(term.lower())
        if index >= 0 and (earliest < 0 or index < earliest):
            earliest = index
            earliest_end = index + len(term)
    if earliest < 0:
        head = content[: _SNIPPET_CONTEXT_RUNES * 2]
        return head.strip() + (" ..." if len(content) > _SNIPPET_CONTEXT_RUNES * 2 else "")
    return _clip_window(content, earliest, earliest_end)


def _scan_haystack(chunk: dict) -> str:
    """read_document query 模式的命中面：正文 + 图片说明/OCR + FAQ 标准问。"""
    content = str(chunk.get("content") or "")
    parts = [content] if content else []
    for image in _parse_images(chunk.get("image_info")):
        if image.get("caption"):
            parts.append(f"Image Caption: {image['caption']}")
        if image.get("ocr_text"):
            parts.append(f"Image Text: {image['ocr_text']}")
    meta = parse_faq_metadata(chunk.get("metadata"))
    if meta is not None and meta["standard_question"]:
        parts.append(meta["standard_question"])
    return "\n".join(parts)


def query_terms(query: str) -> list[str]:
    """从查询里提取长度 ≥2 的词（标点分隔，大小写不敏感去重）。"""
    tokens = re.findall(r"[^\s,.;:?!()\[\]{}\"'\t\n\r]+", query)
    out: list[str] = []
    seen: set[str] = set()
    for token in tokens:
        key = token.lower()
        if len(key) < 2 or key in seen:
            continue
        seen.add(key)
        out.append(token)
    return out


# ---- FAQ 元数据与摘要（对齐 Go faq_snippet.go） ----
FAQ_SIMILAR_DISPLAY = 5
FAQ_SNIPPET_ANSWER_CHARS = 200


def parse_faq_metadata(raw: object) -> dict | None:
    """解析 FAQ 分块元数据（search 行为 chunk_metadata 字符串，chunks 行为 metadata）。

    返回 {standard_question, similar_questions, answers}；不是有效 FAQ 元数据时
    返回 None（调用方仍按 FAQ 行输出 faq_id/index，只是不带问答字段）。
    """
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return None
        try:
            meta = json.loads(text)
        except ValueError:
            return None
    elif isinstance(raw, dict):
        meta = raw
    else:
        return None
    if not isinstance(meta, dict):
        return None

    def _str_list(value: object) -> list[str]:
        if not isinstance(value, list):
            return []
        return [str(item) for item in value if str(item).strip()]

    return {
        "standard_question": str(meta.get("standard_question") or "").strip(),
        "similar_questions": _str_list(meta.get("similar_questions")),
        "answers": _str_list(meta.get("answers")),
    }


def _faq_answer_excerpt(answers: list[str]) -> str:
    parts = [answer.strip() for answer in answers if answer.strip()]
    if not parts:
        return ""
    joined = " | ".join(parts)
    return joined[:FAQ_SNIPPET_ANSWER_CHARS]


def faq_match_snippet(
    meta: dict | None, queries: list[str], pattern: re.Pattern[str] | None = None
) -> str:
    """生成 "Q: … | A: …" 摘要；优先命中小问，否则回落标准问（对齐 Go）。"""
    if meta is None:
        return ""
    standard = meta["standard_question"]
    similar = meta["similar_questions"]
    question = ""
    if pattern is not None:
        question = next((sq for sq in similar if pattern.search(sq)), "")
        if not question and pattern.search(standard):
            question = standard
    else:
        tokens = {
            token.lower()
            for query in queries
            for token in query_terms(query)
        }
        lowered_queries = [q.strip().lower() for q in queries if q.strip()]

        def _matches(text: str) -> bool:
            lowered = text.lower()
            return any(q in lowered for q in lowered_queries) or any(
                token in lowered for token in tokens
            )

        question = next((sq for sq in similar if _matches(sq)), "")
        if not question and _matches(standard):
            question = standard
    # 小问/标准问都没命中时，Go 仍回落标准问
    if not question:
        question = standard
    if not question:
        return ""
    answer = _faq_answer_excerpt(meta["answers"])
    snippet = "Q: " + question + (f" | A: {answer}" if answer else "")
    return snippet[:_SNIPPET_MAX_TOTAL_RUNES]


def enriched_content(row: dict) -> str:
    """拼接图片说明/OCR 文本供 grep 正则命中（对齐 Go getEnrichedPassage）。"""
    content = str(row.get("content") or "")
    extras: list[str] = []
    try:
        images = json.loads(row.get("image_info") or "[]")
    except (ValueError, TypeError):
        images = []
    if isinstance(images, list):
        for image in images:
            if isinstance(image, dict):
                caption = image.get("caption")
                ocr_text = image.get("ocr_text")
                if caption:
                    extras.append(f"Image Caption: {caption}")
                if ocr_text:
                    extras.append(f"Image Text: {ocr_text}")
    # FAQ 行的匹配面还包括标准问/相似问/答案（对齐 Go 渲染 passage 后再正则过滤）
    faq_meta = parse_faq_metadata(row.get("chunk_metadata"))
    if faq_meta is not None:
        if faq_meta["standard_question"]:
            extras.append(f"Question: {faq_meta['standard_question']}")
        extras.extend(f"Question: {sq}" for sq in faq_meta["similar_questions"])
        extras.extend(f"Answer: {answer}" for answer in faq_meta["answers"])
    if extras:
        return content + ("\n\n" if content else "") + "\n".join(extras)
    return content


# ===========================================================================
# 工具集
# ===========================================================================

class RetrieveToolSet:
    """注册并承载 5 个只读工具；通过构造注入 REST 客户端与裁决器。"""

    def __init__(self, client: RESTClient, guard: ToolGuard) -> None:
        self._client = client
        self._guard = guard
        self._kbs = KBService(client)
        self._docs = DocumentService(client)
        self._chunks = ChunkService(client)

    async def _authorize(self, tool_name: str, arguments: dict):
        """取当前主体并走完整 guard 裁决；返回 (principal, scope)。"""
        principal = current_principal()
        if principal is None:
            # 中间件保证 /mcp 必有主体；兜底只可能是注册路径被绕过
            raise ToolError("unauthorized")
        verdict = await self._guard.authorize(principal, tool_name, arguments)
        if not verdict.allowed:
            raise ToolError(_DENY_MESSAGES.get(verdict.deny_reason, "request denied"))
        return principal, verdict.scope

    # ------------------------------------------------------------------
    # 1. list_knowledge_bases
    # ------------------------------------------------------------------

    async def list_knowledge_bases(self) -> dict:
        _, scope = await self._authorize(TOOL_LIST_KNOWLEDGE_BASES, {})
        try:
            kbs = await self._kbs.resolve_visible(scope)
        except BackendError as exc:
            raise ToolError(f"failed to list knowledge bases: {exc.message}") from exc

        summaries = [
            {
                "id": kb.id,
                "name": kb.name,
                **({"description": kb.description} if kb.description else {}),
                "type": kb.type,
                "capabilities": [c for c in ("semantic", "keyword", "wiki") if c in kb.capabilities],
            }
            for kb in kbs
        ]
        return {"knowledge_bases": summaries, "total": len(summaries)}

    # ------------------------------------------------------------------
    # 2/3. search_knowledge / grep_chunks（共享检索主流程）
    # ------------------------------------------------------------------

    async def search_knowledge(
        self,
        query: str,
        mode: str,
        limit: int,
        knowledge_base_ids: list[str] | None,
        knowledge_ids: list[str] | None,
    ) -> dict:
        query = (query or "").strip()
        if not query:
            raise ToolError("query is required")
        mode = (mode or MODE_HYBRID).strip().lower()
        if mode not in (MODE_HYBRID, MODE_SEMANTIC, MODE_KEYWORD):
            # 冻结文案：Go normalizeSearchMode 带 (got %q) 回显
            raise ToolError(f'mode must be one of hybrid, semantic, keyword (got "{mode}")')
        limit = _clamp_int(limit, SEARCH_DEFAULT_LIMIT, 1, SEARCH_MAX_LIMIT)
        selectors = _clean_selectors(knowledge_base_ids)
        doc_ids = _clean_doc_ids(knowledge_ids) if knowledge_ids is not None else None

        arguments = {"query": query, "mode": mode, "limit": limit}
        if selectors is not None:
            arguments["knowledge_base_ids"] = selectors
        if doc_ids is not None:
            arguments["knowledge_ids"] = doc_ids
        _, scope = await self._authorize(TOOL_SEARCH_KNOWLEDGE, arguments)

        return await self._run_search(
            scope,
            query=query,
            requested_mode=mode,
            limit=limit,
            selectors=selectors,
            doc_ids=doc_ids,
            no_index_message="none of the selected knowledge bases supports semantic or keyword retrieval",
        )

    async def grep_chunks(
        self,
        query: str,
        limit: int,
        knowledge_base_ids: list[str] | None,
    ) -> dict:
        query = (query or "").strip()
        if not query:
            raise ToolError("query is required")
        limit = _clamp_int(limit, SEARCH_DEFAULT_LIMIT, 1, SEARCH_MAX_LIMIT)
        selectors = _clean_selectors(knowledge_base_ids)

        pattern, terms = grep_pattern_and_terms(query)
        if not terms:
            raise ToolError(
                'the pattern contains no literal text to look up; include at least one word, '
                'identifier or phrase (for example "timeout|deadline" instead of "^\\d+$")'
            )

        arguments = {"query": query, "limit": limit}
        if selectors is not None:
            arguments["knowledge_base_ids"] = selectors
        _, scope = await self._authorize(TOOL_GREP_CHUNKS, arguments)

        return await self._run_search(
            scope,
            query=terms,
            requested_mode=MODE_KEYWORD,
            limit=limit,
            selectors=selectors,
            doc_ids=None,
            no_index_message=(
                "none of the selected knowledge bases has a chunk index (vector or keyword)"
            ),
            pattern=pattern,
            display_pattern=query,
        )

    async def _run_search(
        self,
        scope,
        *,
        query: str,
        requested_mode: str,
        limit: int,
        selectors: list[str] | None,
        doc_ids: list[str] | None,
        no_index_message: str,
        pattern: re.Pattern[str] | None = None,
        display_pattern: str | None = None,
    ) -> dict:
        # ① 解析并收窄知识库
        try:
            kbs = await self._kbs.resolve_selectors(scope, selectors)
            modes = effective_modes(kbs, requested_mode)
            searchable = [kb for kb in kbs if kb.id in modes]
            if not searchable:
                raise ToolError(no_index_message)

            # ② 可选：收窄到指定文档（逐个反查所属库 + 越权校验）
            doc_targets: dict[str, list[str]] | None = None
            if doc_ids is not None:
                doc_targets = await self._document_targets(searchable, doc_ids)
                searchable = [kb for kb in searchable if kb.id in doc_targets]
                modes = {kb_id: modes[kb_id] for kb_id in doc_targets}
        except ScopeError as exc:
            raise ToolError(str(exc)) from exc

        # ③ 并发逐库检索
        tasks = {
            kb.id: self._search_one_kb(
                kb,
                mode=modes[kb.id][0],
                query=query,
                doc_ids=(doc_targets or {}).get(kb.id),
            )
            for kb in searchable
        }
        results_rows, failures = await self._gather(tasks)

        if tasks and len(failures) >= len(tasks):
            # 全部失败 ≠ 空结果：必须明说"库没有被搜到"，避免模型误判无答案
            raise ToolError(
                "Knowledge search failed: " + "; ".join(failures) +
                ". The knowledge bases were not searched, so this is not evidence that they lack the answer."
            )

        # ④ 合并去重 → 排序 → （grep）正则后置过滤 → 截断
        merged = dedupe_search_rows(results_rows)
        merged.sort(key=lambda r: (-float(r.get("score") or 0.0), str(r.get("knowledge_id") or "")))
        if pattern is not None:
            merged = [row for row in merged if pattern.search(enriched_content(row))]
        merged = merged[:limit]

        kb_type_map = {kb.id: kb.type for kb in searchable}
        kb_ids = [kb.id for kb in searchable]
        terms = query_terms(query)
        formatted: list[dict] = []
        for index, row in enumerate(merged, start=1):
            kb_id = str(row.get("knowledge_base_id") or "")
            kb_type = kb_type_map.get(kb_id, "")
            content = str(row.get("content") or "")
            faq_meta = parse_faq_metadata(row.get("chunk_metadata")) if kb_type == "faq" else None
            # FAQ 摘要优先在小问/答案上找；其余走正文窗口。grep 时正则优先。
            snippet = ""
            if faq_meta is not None:
                snippet = faq_match_snippet(faq_meta, [query], pattern)
            if not snippet and pattern is not None:
                snippet = snippet_regex(content, pattern)
            if not snippet:
                snippet = snippet_for_terms(content, terms)
            formatted.append(
                self._format_search_row(
                    row,
                    rank=index,
                    query=query,
                    used_mode=modes.get(kb_id, (requested_mode, False, ""))[0],
                    kb_type=kb_type,
                    snippet=snippet,
                    faq_meta=faq_meta,
                )
            )

        data: dict = {
            "knowledge_base_ids": kb_ids,
            "query": display_pattern if display_pattern is not None else query,
            "queries": [display_pattern if display_pattern is not None else query],
            "results": formatted,
            "count": len(formatted),
            "display_type": "search_results",
            "kb_counts": {
                kb_id: sum(1 for row in formatted if row["knowledge_base_id"] == kb_id)
                for kb_id in kb_ids
            },
        }
        annotate_mode_fallback(data, requested_mode, modes)
        if failures:
            data["partial_failures"] = failures
        if not formatted:
            data["message"] = self._empty_statement(query, requested_mode, kb_ids, data, failures)
        return data

    async def _document_targets(
        self, kbs: list[KBSummary], ids: list[str]
    ) -> dict[str, list[str]]:
        """把 knowledge_ids 校验并按所属库聚合成文档级检索目标。"""
        if not ids:
            raise ToolError(
                "knowledge_ids must contain at least one document id; omit it to search whole knowledge bases"
            )
        if len(ids) > SEARCH_MAX_DOCUMENTS:
            raise ToolError(f"knowledge_ids must contain at most {SEARCH_MAX_DOCUMENTS} document ids")

        kb_by_id = {kb.id: kb for kb in kbs}
        targets: dict[str, list[str]] = {}
        try:
            for doc_id in ids:
                doc = await self._docs.get(doc_id)
                if doc is None:
                    raise ToolError(f'document "{doc_id}" was not found')
                doc_kb_id = str(doc.get("knowledge_base_id") or "").strip()
                kb = kb_by_id.get(doc_kb_id)
                if kb is None or not self._same_tenant(kb, doc):
                    raise ToolError(
                        f'document "{doc_id}" is outside this endpoint\'s scope or the selected knowledge bases'
                    )
                targets.setdefault(doc_kb_id, []).append(doc_id)
        except BackendError as exc:
            raise ToolError(f"failed to verify document scope: {exc.message}") from exc
        return targets

    @staticmethod
    def _same_tenant(kb: KBSummary, doc: dict) -> bool:
        """文档租户必须与所属库租户一致（对齐 Go 的伪造属主文档防护）。"""
        try:
            doc_tenant = int(doc.get("tenant_id") or 0)
        except (TypeError, ValueError):
            return True
        return not kb.tenant_id or not doc_tenant or kb.tenant_id == doc_tenant

    async def _search_one_kb(
        self, kb: KBSummary, *, mode: str, query: str, doc_ids: list[str] | None
    ) -> list[dict]:
        """对单个库发一次 hybrid-search；文档级目标放宽阈值。"""
        payload: dict = {
            "query_text": query,
            "match_count": SEARCH_CANDIDATE_LIMIT,
            # hybrid：两个开关都 false；keyword：关向量；semantic：关关键词
            "disable_vector_match": mode == MODE_KEYWORD,
            "disable_keywords_match": mode == MODE_SEMANTIC,
            # 父/邻接块会让候选集膨胀，按需阅读交给 read_document
            "skip_context_enrichment": True,
        }
        if doc_ids:
            payload["knowledge_ids"] = doc_ids
            # 显式圈定文档：召回阈值归零（Go DisableRecallThresholds），
            # 避免"指定了文档却差一点召回不到"
            payload["vector_threshold"] = 0.0
            payload["keyword_threshold"] = 0.0

        body = await self._client.post_json(
            f"/knowledge-bases/{kb.id}/hybrid-search",
            json_body=payload,
            timeout=SEARCH_TIMEOUT_SECONDS,
        )
        rows = body.get("data")
        if not isinstance(rows, list):
            return []
        # 每行打上来源库 id（多库合并时区分归属）
        out: list[dict] = []
        for row in rows:
            if isinstance(row, dict):
                row.setdefault("knowledge_base_id", kb.id)
                out.append(row)
        return out

    async def _gather(self, tasks: dict[str, object]):
        """并发执行 {id: coroutine}，收结果与 "id: message" 失败串。"""
        results: list[dict] = []
        failures: list[str] = []
        coros = [coro for coro in tasks.values()]
        gathered = await asyncio.gather(*coros, return_exceptions=True)
        for (kb_id, _coro), outcome in zip(tasks.items(), gathered):
            if isinstance(outcome, Exception):
                message = outcome.message if isinstance(outcome, BackendError) else "search failed"
                failures.append(f"{kb_id}: {message}")
                logger.info("[search] 库检索失败 kb=%s err=%s", kb_id, message)
            else:
                results.extend(outcome)
        return results, failures

    @staticmethod
    def _format_search_row(
        row: dict,
        *,
        rank: int,
        query: str,
        used_mode: str,
        kb_type: str,
        snippet: str,
        faq_meta: dict | None = None,
    ) -> dict:
        """映射成 Go formatOutput 的逐行结构（契约稳定字段）。"""
        formatted: dict = {
            "result_index": rank,
            "content": str(row.get("content") or ""),
            "knowledge_id": str(row.get("knowledge_id") or ""),
            "knowledge_base_id": str(row.get("knowledge_base_id") or ""),
            "knowledge_title": str(row.get("knowledge_title") or ""),
            "knowledge_metadata": str(row.get("knowledge_custom_metadata") or ""),
            "match_type": str(row.get("match_type") or ""),
            "match_snippet": snippet,
            "score": float(row.get("score") or 0.0),
            "source_query": query,
            "query_type": used_mode,
            "knowledge_base_type": kb_type,
        }
        # FAQ 行用 faq_id/index 替代 chunk_id/chunk_index（Go 冻结契约）
        if kb_type == "faq":
            formatted["faq_id"] = str(row.get("id") or "")
            formatted["index"] = int(row.get("chunk_index") or 0)
            if faq_meta is not None:
                if faq_meta["standard_question"]:
                    formatted["faq_standard_question"] = faq_meta["standard_question"]
                similar = faq_meta["similar_questions"]
                if similar:
                    formatted["faq_similar_questions"] = similar[:FAQ_SIMILAR_DISPLAY]
                    if len(similar) > FAQ_SIMILAR_DISPLAY:
                        formatted["faq_similar_questions_omitted"] = (
                            len(similar) - FAQ_SIMILAR_DISPLAY
                        )
                if faq_meta["answers"]:
                    formatted["faq_answers"] = faq_meta["answers"]
        else:
            formatted["chunk_id"] = str(row.get("id") or "")
            formatted["chunk_index"] = int(row.get("chunk_index") or 0)
        # 图片信息按 REST 原值透传（有才带）
        images = _parse_images(row.get("image_info"))
        if images:
            formatted["images"] = images
        return formatted

    @staticmethod
    def _empty_statement(
        query: str,
        mode: str,
        kb_ids: list[str],
        data: dict,
        failures: list[str],
    ) -> str:
        message = f"No matching chunks for {query!r} (mode={mode}) in {len(kb_ids)} knowledge base(s)."
        for fallback in data.get("mode_fallbacks", []):
            message += (
                f" Knowledge base {fallback['knowledge_base_id']} was searched with "
                f"mode={fallback['mode']} ({fallback['reason']})."
            )
        if failures:
            message += (
                " Some knowledge bases could not be searched, so this is not evidence that they "
                "lack the answer: " + "; ".join(failures) + "."
            )
        return message

    # ------------------------------------------------------------------
    # 4. list_documents
    # ------------------------------------------------------------------

    async def list_documents(
        self,
        knowledge_base_id: str,
        keyword: str,
        page: int,
        page_size: int,
    ) -> dict:
        selector = (knowledge_base_id or "").strip()
        if not selector:
            raise ToolError("knowledge_base_id is required")
        keyword = (keyword or "").strip()
        page = max(1, int(page or 1))
        page_size = _clamp_int(page_size, LIST_DEFAULT_PAGE_SIZE, 1, LIST_MAX_PAGE_SIZE)

        arguments = {"knowledge_base_id": selector}
        _, scope = await self._authorize(TOOL_LIST_DOCUMENTS, arguments)

        try:
            kbs = await self._kbs.resolve_selectors(scope, [selector])
            result = await self._docs.list_paged(
                kbs[0].id, keyword=keyword, page=page, page_size=page_size
            )
        except ScopeError as exc:
            raise ToolError(str(exc)) from exc
        except BackendError as exc:
            raise ToolError(f"failed to list documents: {exc.message}") from exc

        return {
            "knowledge_base_id": kbs[0].id,
            "total": result.total,
            "page": result.page,
            "page_size": result.page_size,
            "documents": list(result.documents),
        }

    # ------------------------------------------------------------------
    # 5. read_document
    # ------------------------------------------------------------------

    async def read_document(
        self,
        knowledge_id: str,
        offset: int,
        limit: int,
        query: str,
    ) -> dict:
        knowledge_id = (knowledge_id or "").strip()
        if not knowledge_id:
            raise ToolError("knowledge_id is required")
        limit = _clamp_int(limit, READ_DEFAULT_LIMIT, 1, READ_MAX_LIMIT)
        offset = max(0, int(offset or 0))
        query = (query or "").strip()

        _, scope = await self._authorize(TOOL_READ_DOCUMENT, {"knowledge_id": knowledge_id})

        # ① 反查文档与所属库，做越权校验（对齐 Go knowledgeInScope）
        try:
            doc = await self._docs.get(knowledge_id)
        except BackendError as exc:
            raise ToolError(f"failed to read document: {exc.message}") from exc
        if doc is None:
            raise ToolError(f'document "{knowledge_id}" was not found')
        kb_id = str(doc.get("knowledge_base_id") or "").strip()
        if not kb_id or not scope.allows(kb_id):
            raise ToolError(
                f'document "{knowledge_id}" is outside this endpoint\'s scope or the selected knowledge bases'
            )
        try:
            kb = await self._kbs.get(kb_id)
        except BackendError as exc:
            raise ToolError(f"failed to read document: {exc.message}") from exc
        if kb is None or not self._same_tenant(kb, doc):
            raise ToolError(
                f'document "{knowledge_id}" is outside this endpoint\'s scope or the selected knowledge bases'
            )

        summary = as_dict(parse_document_summary(doc))
        try:
            if query:
                data = await self._read_by_query(doc, kb, summary, query, offset)
            else:
                data = await self._read_page(doc, kb, summary, offset, limit)
        except BackendError as exc:
            raise ToolError(f"failed to read document chunks: {exc.message}") from exc
        return data

    async def _read_page(self, doc: dict, kb: KBSummary, summary: dict, offset: int, limit: int) -> dict:
        """分页阅读：窗口 [offset, offset+limit)，REST 页大小按 limit 对齐。"""
        first_page = offset // limit + 1
        skip = offset % limit
        page1 = await self._chunks.list_paged(str(doc["id"]), page=first_page, page_size=limit)
        total = page1.total
        chunks = list(page1.chunks)
        if skip:
            chunks = chunks[skip:]
            # 窗口跨界时补下一页（与 Go fetchWindow 一致）
            if first_page * limit < total:
                page2 = await self._chunks.list_paged(
                    str(doc["id"]), page=first_page + 1, page_size=limit
                )
                chunks.extend(page2.chunks)
        chunks = chunks[:limit]

        # 越界报错（文案与 Go readPage 对齐）
        if not chunks and total > 0 and offset >= total:
            suggested = max(total - limit, 0)
            raise ToolError(
                f"offset {offset} is out of range: document has {total} chunks "
                f"(valid offsets 0..{total - 1}). Retry with offset={suggested}."
            )

        chunks = _fit_chunks_to_budget(chunks)
        data = self._build_document_data(doc, kb, summary, chunks, total)
        data["offset"] = offset
        data["page_size"] = limit
        data["page"] = first_page
        next_offset = offset + len(chunks)
        if chunks and next_offset < total:
            data["next_offset"] = next_offset
        return data

    async def _read_by_query(
        self, doc: dict, kb: KBSummary, summary: dict, query: str, offset: int
    ) -> dict:
        """query 模式：顺序扫描整篇文档，每词必含（乱序），最多 20 命中 ±1 块。"""
        matchers = [re.compile("(?i)" + re.escape(term)) for term in query.split()]
        rows: list[tuple[dict, str]] = []  # (原始 chunk, 角色)
        snippets: dict[str, str] = {}
        emitted: set[str] = set()
        match_count = 0
        truncated = False
        next_offset = -1
        total = 0
        previous: dict | None = None
        force_next = False
        used_chars = 0
        # 从 offset 前一块开始扫，保证 offset 处命中仍能拿到前一块上下文
        page = max(offset - 1, 0) // READ_SCAN_PAGE_SIZE + 1

        def emit(chunk: dict, role: str, snippet: str = "") -> None:
            nonlocal used_chars
            cid = str(chunk.get("id"))
            rows.append((chunk, role))
            if snippet:
                snippets[cid] = snippet
            emitted.add(cid)
            used_chars += len(str(chunk.get("content") or "")) + READ_CHUNK_OVERHEAD

        while True:
            chunk_page = await self._chunks.list_paged(
                str(doc["id"]), page=page, page_size=READ_SCAN_PAGE_SIZE
            )
            total = chunk_page.total
            if not chunk_page.chunks:
                break
            stop_scan = False
            for index, chunk in enumerate(chunk_page.chunks):
                position = (page - 1) * READ_SCAN_PAGE_SIZE + index
                if position < offset:
                    previous = chunk
                    continue
                # 命中面 = 正文 + 图片说明/OCR + FAQ 标准问（对齐 Go enrichChunkContent）
                haystack = _scan_haystack(chunk)
                if all(matcher.search(haystack) for matcher in matchers):
                    if match_count >= READ_MAX_MATCHES or used_chars >= READ_OUTPUT_BUDGET_CHARS:
                        truncated = True
                        next_offset = position
                        stop_scan = True
                        break
                    match_count += 1
                    if previous is not None and str(previous.get("id")) not in emitted:
                        emit(previous, "context_before")
                    if str(chunk.get("id")) not in emitted:
                        snippet = snippet_regex(haystack, matchers[0])
                        if not snippet:
                            # 多词时用任一词给窗口
                            snippet = snippet_for_terms(haystack, query.split())
                        emit(chunk, "match", snippet)
                    force_next = True
                elif force_next:
                    if str(chunk.get("id")) not in emitted and used_chars < READ_OUTPUT_BUDGET_CHARS:
                        emit(chunk, "context_after")
                    force_next = False
                previous = chunk
            if stop_scan or page * READ_SCAN_PAGE_SIZE >= total:
                break
            page += 1

        data = self._build_document_data(doc, kb, summary, [chunk for chunk, _role in rows], total)
        data["query"] = query
        data["match_count"] = match_count
        data["truncated"] = truncated
        if next_offset >= 0:
            data["next_offset"] = next_offset
        # 角色与命中文摘挂到对应 chunk 行（FAQ 行 id 键是 faq_id）
        roles = {str(chunk.get("id")): role for chunk, role in rows}
        for row in data["chunks"]:
            row_id = str(row.get("chunk_id") or row.get("faq_id") or "")
            role = roles.get(row_id)
            if role:
                row["role"] = role
            snippet_text = snippets.get(row_id)
            if snippet_text:
                row["match_snippet"] = snippet_text
        return data

    @staticmethod
    def _build_document_data(
        doc: dict, kb: KBSummary, summary: dict, chunks: list[dict], total: int
    ) -> dict:
        formatted: list[dict] = []
        for index, chunk in enumerate(chunks, start=1):
            row = chunk_data_map(index, chunk)
            # 后端行缺省 knowledge_id 时用文档头补齐
            if not row.get("knowledge_id"):
                row["knowledge_id"] = str(doc.get("id") or "")
            formatted.append(row)
        return {
            "display_type": "knowledge_chunks_list",
            "knowledge_id": str(doc.get("id") or ""),
            "knowledge_title": str(doc.get("title") or ""),
            "total_chunks": total,
            "fetched_chunks": len(formatted),
            "chunks": formatted,
            # 与 Go MCP 包装层一致：document 摘要 + 所属库简名
            "document": summary,
            "knowledge_base": {"id": kb.id, "name": kb.name},
        }

    # ------------------------------------------------------------------
    # 注册到 MCPServer
    # ------------------------------------------------------------------

    def register(self, server) -> None:
        toolset = self

        @server.tool(
            name=TOOL_LIST_KNOWLEDGE_BASES,
            description=(
                "List the knowledge bases this endpoint can access, with their id, name, description "
                "and which retrieval modes (semantic, keyword, wiki) each supports. Call this first to "
                "learn what is in scope; other tools accept either the id or the exact name."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        # 注：返回类型只标 dict；错误分支返回 CallToolResult(is_error)，
        # SDK 对 Union[CallToolResult] 的函数签名会直接拒绝注册
        async def list_knowledge_bases() -> dict:
            try:
                return _json_result(await toolset.list_knowledge_bases())
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_SEARCH_KNOWLEDGE,
            description=(
                "Search the knowledge bases in scope and return the most relevant passages with their "
                "document ids and titles. mode hybrid (default) combines semantic and keyword retrieval; "
                "semantic ranks by meaning; keyword matches exact terms, identifiers, codes or names."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def search_knowledge(
            query: Annotated[str, Field(
                description="A natural-language question, topic, or the exact terms to match",
                min_length=1,
            )],
            mode: Annotated[Literal["hybrid", "semantic", "keyword"], Field(
                description="hybrid (default), semantic, or keyword",
            )] = "hybrid",
            limit: Annotated[int, Field(
                description="Maximum passages to return, default 10, max 30", ge=1, le=30,
            )] = 10,
            knowledge_base_ids: Annotated[list[str] | None, Field(
                description="Optional knowledge base ids or names to restrict the search; defaults to "
                "every knowledge base in scope",
            )] = None,
            knowledge_ids: Annotated[list[str] | None, Field(
                min_length=1,
                max_length=SEARCH_MAX_DOCUMENTS,
                description=(
                    "Optional list of 1 to 100 document ids from list_documents or search results. "
                    "Restricts retrieval before ranking; every document must be in scope and belong to "
                    "the selected knowledge bases. Omit to search whole knowledge bases; invalid or "
                    "conflicting ids return an error."
                ),
            )] = None,
        ) -> dict:
            try:
                return _json_result(await toolset.search_knowledge(
                    query, mode, limit, knowledge_base_ids, knowledge_ids
                ))
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_GREP_CHUNKS,
            description=(
                "Case-insensitive regular-expression search over the text chunks of the knowledge bases "
                "in scope. Best for exact terms, identifiers, error codes or product names that semantic "
                "search may miss. Candidates come from the keyword index (the semantic index for bases "
                "without one) using the literal terms in the pattern, and every returned chunk matches "
                "the pattern."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def grep_chunks(
            query: Annotated[str, Field(
                description=(
                    "Regular expression (case-insensitive) to match against chunk text; text that is not "
                    "a valid regular expression is matched literally"
                ),
                min_length=1,
            )],
            limit: Annotated[int, Field(
                description="Maximum passages to return, default 10, max 30", ge=1, le=30,
            )] = 10,
            knowledge_base_ids: Annotated[list[str] | None, Field(
                description="Optional knowledge base ids or names to restrict the search",
            )] = None,
        ) -> dict:
            try:
                return _json_result(await toolset.grep_chunks(query, limit, knowledge_base_ids))
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_LIST_DOCUMENTS,
            description=(
                "List the documents inside one knowledge base with pagination and an optional title "
                "keyword filter. Returns document ids that read_document accepts."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def list_documents(
            knowledge_base_id: Annotated[str, Field(
                description="Knowledge base id or exact name", min_length=1,
            )],
            keyword: Annotated[str, Field(
                description="Optional substring to filter document titles",
            )] = "",
            page: Annotated[int, Field(
                description="1-based page number, default 1", ge=1,
            )] = 1,
            page_size: Annotated[int, Field(
                description="Documents per page, default 20, max 100", ge=1, le=100,
            )] = 20,
        ) -> dict:
            try:
                return _json_result(await toolset.list_documents(knowledge_base_id, keyword, page, page_size))
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_READ_DOCUMENT,
            description=(
                "Read a document's metadata and its text chunks in order. Page through long documents "
                "with offset and limit, or pass query to find chunks containing every whitespace-separated "
                "word, case-insensitively and in any order. Query mode returns at most 20 matches plus one "
                "chunk of context on each side, subject to the output size budget; limit does not apply. "
                "If truncated, continue with offset=next_offset from the response."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def read_document(
            knowledge_id: Annotated[str, Field(
                description="Document id from search results or list_documents", min_length=1,
            )],
            offset: Annotated[int, Field(
                description="0-based position in document reading order, default 0; use returned "
                "next_offset to continue",
                ge=0,
            )] = 0,
            limit: Annotated[int, Field(
                description="Chunks per page without query, default 20, max 100; ignored in query "
                "mode; output size budget may reduce the page",
                ge=1,
                le=100,
            )] = 20,
            query: Annotated[str, Field(
                description="Optional case-insensitive keywords split on whitespace; every word must "
                "occur in the chunk, in any order (not an exact phrase)",
            )] = "",
        ) -> dict:
            try:
                return _json_result(await toolset.read_document(knowledge_id, offset, limit, query))
            except ToolError as exc:
                return _tool_error_result(exc)


# ===========================================================================
# 模块级纯工具函数
# ===========================================================================

def _clamp_int(value: object, default: int, minimum: int, maximum: int) -> int:
    """运行时二次钳制（不信任 JSON Schema；非整数/越界一律归一）。"""
    try:
        number = int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
    if number < minimum:
        return minimum if number > 0 else default
    return min(number, maximum)


def _clean_id_list(values: list[str] | None, *, blank_message: str) -> list[str] | None:
    """规整 id 列表：去空白；显式传入但含空元素直接报错（对齐 Go fail-closed）。"""
    if values is None:
        return None
    cleaned: list[str] = []
    for value in values:
        text = str(value).strip()
        if not text:
            raise ToolError(blank_message)
        cleaned.append(text)
    return cleaned


def _clean_selectors(values: list[str] | None) -> list[str] | None:
    """库选择器；冻结文案对应 Go requireKnowledgeBaseSelectors。"""
    return _clean_id_list(
        values, blank_message="knowledge_base_ids must not contain empty selectors"
    )


def _clean_doc_ids(values: list[str] | None) -> list[str] | None:
    """文档 id；冻结文案对应 Go documentSearchTargets 的空 id 校验。"""
    return _clean_id_list(
        values, blank_message="knowledge_ids must not contain blank document ids"
    )


def _parse_images(raw_image_info: object) -> list[dict]:
    """image_info(JSON 字符串) 解析成图片列表；非 JSON/空返回空列表。"""
    if not raw_image_info:
        return []
    try:
        parsed = json.loads(raw_image_info)
    except (ValueError, TypeError):
        return []
    return parsed if isinstance(parsed, list) else []


def chunk_data_map(seq: int, chunk: dict) -> dict:
    """分块行结构（对齐 Go chunkDataMap；FAQ 行用 faq_id/index 并带问答字段）。"""
    chunk_type = str(chunk.get("chunk_type") or "text")
    row: dict = {
        "seq": seq,
        "content": str(chunk.get("content") or ""),
        "chunk_type": chunk_type,
        "knowledge_id": str(chunk.get("knowledge_id") or ""),
        "knowledge_base": str(chunk.get("knowledge_base_id") or ""),
        "start_at": int(chunk.get("start_at") or 0),
        "end_at": int(chunk.get("end_at") or 0),
        "parent_chunk_id": str(chunk.get("parent_chunk_id") or ""),
    }
    if chunk_type == "faq":
        row["faq_id"] = str(chunk.get("id") or "")
        row["index"] = int(chunk.get("chunk_index") or 0)
        meta = parse_faq_metadata(chunk.get("metadata"))
        if meta is not None:
            if meta["standard_question"]:
                row["faq_question"] = meta["standard_question"]
            similar = meta["similar_questions"]
            if similar:
                row["faq_similar_questions"] = similar[:FAQ_SIMILAR_DISPLAY]
                if len(similar) > FAQ_SIMILAR_DISPLAY:
                    row["faq_similar_questions_omitted"] = (
                        len(similar) - FAQ_SIMILAR_DISPLAY
                    )
            if meta["answers"]:
                row["faq_answers"] = meta["answers"]
    else:
        row["chunk_id"] = str(chunk.get("id") or "")
        row["chunk_index"] = int(chunk.get("chunk_index") or 0)
    return row


def _fit_chunks_to_budget(chunks: list[dict]) -> list[dict]:
    """按固定输出预算保留开头若干块（首块必保留，保证翻页有进展）。"""
    used = 0
    for index, chunk in enumerate(chunks):
        used += len(str(chunk.get("content") or "")) + READ_CHUNK_OVERHEAD
        if used > READ_OUTPUT_BUDGET_CHARS and index > 0:
            return chunks[:index]
    return chunks
