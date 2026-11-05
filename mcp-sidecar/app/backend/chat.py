"""ask 链路的 REST 适配：智能体解析、会话创建、QA SSE 聚合。

对齐 Go 内置 MCP（internal/mcpserver/tools_ask.go）走 REST 的等价路径：
  - GET  /agents/:id            → 标准信封 {success,data:CustomAgent}
  - POST /sessions              → 201 {success,data:{id,...}}
  - POST /knowledge-chat/:sid   → text/event-stream（普通 RAG）
  - POST /agent-chat/:sid       → text/event-stream（smart-reasoning 智能体）

侧车不像 Go 内部调用那样预建 user/assistant 消息——HTTP QA 处理器会自行
建消息；请求体只发 query / knowledge_base_ids / agent_id，绝不携带
web_search_enabled（内置 quick-answer 默认会开联网，见 Go 注释安全说明）。
"""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator

from app.backend.client import BackendError, RESTClient

logger = logging.getLogger("sidecar.backend.chat")

AGENT_MODE_SMART_REASONING = "smart-reasoning"
BUILTIN_QUICK_ANSWER = "builtin-quick-answer"

# 用户可见的内置智能体白名单（Go GetBuiltinAgentIDs 固定顺序）；
# wiki-fixer / skill-installer 这类内部内置不在其中，端点无法点名。
BUILTIN_AGENT_IDS = (
    BUILTIN_QUICK_ANSWER,
    "builtin-smart-reasoning",
    "builtin-wiki-researcher",
    "builtin-deep-researcher",
    "builtin-data-analyst",
    "builtin-knowledge-graph-expert",
    "builtin-document-assistant",
)

ASK_SESSION_TITLE_MAX = 60
ASK_CHANNEL = "mcp"


class ChatService:
    """ask 所需的三个 REST 面。"""

    def __init__(self, client: RESTClient) -> None:
        self._client = client

    async def get_agent(self, agent_id: str) -> dict | None:
        """GET /agents/:id；404 返回 None。返回 CustomAgent 原始 dict。"""
        try:
            body = await self._client.get_json(f"/agents/{agent_id}")
        except BackendError as exc:
            if exc.status == 404:
                return None
            raise
        data = body.get("data")
        return data if isinstance(data, dict) else None

    async def create_session(self, title: str, description: str) -> str:
        """创建会话并返回 sid；标题截 60 rune 由调用方保证。"""
        body = await self._client.post_json(
            "/sessions",
            json_body={"title": title, "description": description},
        )
        data = body.get("data")
        sid = str(data.get("id") or "") if isinstance(data, dict) else ""
        if not sid:
            raise BackendError("upstream session response is missing an id")
        return sid

    def qa_events(
        self,
        sid: str,
        *,
        query: str,
        kb_ids: list[str] | None,
        agent_id: str | None,
        use_agent_route: bool,
        timeout: float,
    ) -> AsyncIterator[dict]:
        """发起一轮 QA，返回 SSE 事件 JSON 的异步迭代器。

        普通模式走 /knowledge-chat；smart-reasoning 走 /agent-chat 并带 agent_id。
        kb_ids 为 None 时不下发该字段（交由智能体自身配置决定）。
        """
        payload: dict = {"query": query, "channel": ASK_CHANNEL}
        if kb_ids is not None:
            payload["knowledge_base_ids"] = kb_ids
        if use_agent_route:
            payload["agent_id"] = agent_id
        path = f"/{'agent-chat' if use_agent_route else 'knowledge-chat'}/{sid}"
        return self._client.stream_sse_events(
            "POST", path, json_body=payload, timeout=timeout
        )


def truncate_title(question: str, max_runes: int = ASK_SESSION_TITLE_MAX) -> str:
    """问题截前 60 rune 作为会话标题（与 Go askSessionTitleMaxLen 一致）。"""
    return question[:max_runes]
