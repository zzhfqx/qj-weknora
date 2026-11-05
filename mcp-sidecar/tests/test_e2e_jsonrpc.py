"""JSON-RPC over HTTP 端到端：鉴权中间件 + 真 MCP 应用 + 真 RESTClient（respx）。

对应需求.md §十.2 的四条上线验收链路，在进程内闭环，不依赖任何外部服务：
  1. 裸端点令牌 = 机器模式，只看到公共书架；
  2. 复合票 = 用户模式，看到公共书架 ∪ 个人授权（并集去重，不越权）；
  3. 坏票在传输层 401，不进 JSON-RPC、绝不降级机器；
  4. 选择器指向并集之外的库：工具 isError 冻结文案，且不发范围外详情探测。
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import time

import httpx
import jwt
import pytest
import respx

from app.auth.middleware import TICKET_TYPE, MCPAuthMiddleware
from app.backend.client import RESTClient
from app.guards import ToolGuard
from app.ratelimit import SlidingWindowLimiter
from app.stores.endpoints import EndpointRecord
from app.tools.chat import AskToolSet
from app.tools.retrieve import RetrieveToolSet
from app.tools.wiki import WikiToolSet
from mcp.server import MCPServer

BASE_URL = "https://weknora.test/api/v1"
API_KEY = "wk_test_key"
SECRET = "e2e-secret-0123456789abcdef-0123456789"
EP = "ep-1"
TENANT = 77

ALL_TOOLS = (
    "list_knowledge_bases", "search_knowledge", "grep_chunks",
    "list_documents", "read_document",
    "wiki_search", "wiki_read_page", "wiki_index", "ask",
)


def _ticket(*, user: str = "u-1", ttl: int = 300) -> str:
    now = int(time.time())
    return jwt.encode(
        {"type": TICKET_TYPE, "sub": user, "tid": TENANT,
         "aud": EP, "jti": "j", "iat": now, "exp": now + ttl},
        SECRET, algorithm="HS256",
    )


class FakeEndpointStore:
    def __init__(self) -> None:
        self.row = EndpointRecord(
            id=EP, tenant_id=TENANT, name="ep", enabled=True,
            knowledge_base_ids=("kb-pub",), tools=ALL_TOOLS,
            default_agent_id="builtin-quick-answer", rate_limit_per_minute=600,
        )

    async def get_by_token(self, token: str):
        return self.row if token == "mcp_tok" else None


class FakePermissionStore:
    def __init__(self, table: dict) -> None:
        self.table = table

    async def get_user_knowledge_bases(self, tenant_id: int, user_id: str):
        return tuple(self.table.get((tenant_id, user_id), ()))


def _kb_json(kb_id: str, *, caps=None):
    return {
        "id": kb_id, "name": f"KB {kb_id}", "description": "",
        "type": "document", "tenant_id": TENANT,
        "capabilities": caps or {"vector": True, "keyword": True},
    }


@contextlib.asynccontextmanager
async def _lifespan(app):
    """httpx ASGITransport 不跑 lifespan；MCP streamable manager 的任务组
    必须经 lifespan.startup 初始化，这里手工驱动。"""
    inbound: asyncio.Queue = asyncio.Queue()
    outbound: asyncio.Queue = asyncio.Queue()
    task = asyncio.create_task(
        app({"type": "lifespan"}, inbound.get, outbound.put)
    )
    await inbound.put({"type": "lifespan.startup"})
    started = await outbound.get()
    assert started["type"] == "lifespan.startup.complete", started
    try:
        yield
    finally:
        await inbound.put({"type": "lifespan.shutdown"})
        stopped = await outbound.get()
        assert stopped["type"] == "lifespan.shutdown.complete", stopped
        await task


@pytest.fixture
async def stack():
    server = MCPServer("e2e", version="0")
    guard = ToolGuard(
        FakePermissionStore({(TENANT, "u-1"): ("kb-mine",)}),
        SlidingWindowLimiter(None),
    )
    rest = RESTClient(BASE_URL, API_KEY, verify_ssl=False)
    RetrieveToolSet(rest, guard).register(server)
    WikiToolSet(rest, guard).register(server)
    AskToolSet(rest, guard, session_secret="e2e-session-secret", ask_timeout=10).register(server)

    mcp_app = server.streamable_http_app(
        streamable_http_path="/mcp", stateless_http=True,
    )
    app = MCPAuthMiddleware(mcp_app, endpoint_store=FakeEndpointStore(), jwt_secret=SECRET)
    transport = httpx.ASGITransport(app=app)
    async with _lifespan(app):
        async with httpx.AsyncClient(transport=transport, base_url="http://localhost:8000") as client:
            yield client
    await rest.aclose()


def _sse_payloads(response: httpx.Response) -> list[dict]:
    """从 SSE 响应里取出所有 data: JSON-RPC 消息。"""
    out = []
    for line in response.text.splitlines():
        if line.startswith("data:"):
            out.append(json.loads(line[5:].strip()))
    return out


async def _rpc(client: httpx.AsyncClient, method: str, params=None, *,
               auth: str | None = None, req_id: int = 1):
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "mcp-protocol-version": "2025-06-18",
    }
    if auth:
        headers["Authorization"] = auth
    return await client.post(
        "/mcp", headers=headers,
        json={"jsonrpc": "2.0", "id": req_id, "method": method,
              "params": params or {}},
    )


async def _tool_result(client, name: str, arguments=None, *, auth: str | None = None):
    response = await _rpc(
        client, "tools/call", {"name": name, "arguments": arguments or {}}, auth=auth,
    )
    assert response.status_code == 200, response.text
    messages = [m for m in _sse_payloads(response) if m.get("id") == 1]
    assert messages, response.text
    assert "error" not in messages[-1], messages[-1]
    return messages[-1]["result"]


@respx.mock
async def test_initialize_and_tools_list_have_nine_tools(stack):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": []}),
    )
    r = await _rpc(stack, "initialize", {
        "protocolVersion": "2025-06-18", "capabilities": {},
        "clientInfo": {"name": "e2e", "version": "0"},
    }, auth="Bearer mcp_tok")
    assert r.status_code == 200

    r = await _rpc(stack, "tools/list", {}, auth="Bearer mcp_tok")
    result = [m for m in _sse_payloads(r) if m.get("id") == 1][0]["result"]
    assert {t["name"] for t in result["tools"]} == set(ALL_TOOLS)


@respx.mock
async def test_machine_sees_only_public_shelf(stack):
    route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            _kb_json("kb-pub"), _kb_json("kb-mine"), _kb_json("kb-other"),
        ]}),
    )
    result = await _tool_result(stack, "list_knowledge_bases", auth="Bearer mcp_tok")
    assert route.called
    ids = {kb["id"] for kb in result["structuredContent"]["knowledge_bases"]}
    assert ids == {"kb-pub"}
    # 公共书架在自有列表里：不应再发详情补查
    assert not respx.get(f"{BASE_URL}/knowledge-bases/kb-pub").called


@respx.mock
async def test_user_sees_union_of_public_and_personal(stack):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            _kb_json("kb-pub"), _kb_json("kb-mine"), _kb_json("kb-other"),
        ]}),
    )
    result = await _tool_result(
        stack, "list_knowledge_bases", auth=f"Bearer mcp_tok.{_ticket()}"
    )
    ids = {kb["id"] for kb in result["structuredContent"]["knowledge_bases"]}
    assert ids == {"kb-pub", "kb-mine"}            # 并集去重；kb-other 不在任何一份书单


@respx.mock
async def test_bad_ticket_is_http_401_without_jsonrpc_downgrade(stack):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": []}),
    )
    r = await _rpc(
        stack, "tools/call",
        {"name": "list_knowledge_bases", "arguments": {}},
        auth="Bearer mcp_tok.not-a-jwt",
    )
    assert r.status_code == 401
    # 拒绝发生在中间件：后端 REST 一次都没被调用
    assert not respx.get(f"{BASE_URL}/knowledge-bases").called


@respx.mock
async def test_out_of_union_selector_fails_without_detail_probe(stack):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            _kb_json("kb-pub"), _kb_json("kb-mine"), _kb_json("kb-other"),
        ]}),
    )
    detail = respx.get(f"{BASE_URL}/knowledge-bases/kb-other").mock(
        return_value=httpx.Response(200, json={"success": True, "data": _kb_json("kb-other")}),
    )

    # 用户模式：个人授权是 kb-mine，kb-other 在并集之外
    result = await _tool_result(
        stack, "search_knowledge",
        {"query": "x", "knowledge_base_ids": ["kb-other"], "limit": 5},
        auth=f"Bearer mcp_tok.{_ticket()}",
    )
    assert result["isError"] is True
    assert "knowledge base 'kb-other' was not found or is outside this endpoint's scope" \
        in result["content"][0]["text"]
    assert not detail.called                        # 闭集之外绝不发详情探测

    # 机器模式：kb-mine 是个人授权库，机器同样拿不到（子集性质）
    result = await _tool_result(
        stack, "search_knowledge",
        {"query": "x", "knowledge_base_ids": ["kb-mine"], "limit": 5},
        auth="Bearer mcp_tok",
    )
    assert result["isError"] is True
    assert "'kb-mine'" in result["content"][0]["text"]
