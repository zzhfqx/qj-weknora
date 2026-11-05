"""ask 链路 REST/SSE 契约测试：respx 打真 httpx，锁死侧车 ↔ WeKnora 线上约定。

  - GET  /agents/:id      标准信封解包、404→None、403 不透出上游细节；
  - POST /sessions        201 信封取 data.id，标题/描述/请求头形状；
  - POST /knowledge-chat/:sid 与 /agent-chat/:sid 是 gin SSEvent 成帧
    （event: message + data: JSON + 空行分发），建流前 403 是普通 JSON 错误包；
  - 工具端到端：只发 query/channel/kb_ids(/agent_id)，绝不发 web_search_enabled。
"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from app.auth.middleware import Principal, reset_principal, set_principal
from app.backend.chat import ChatService
from app.backend.client import BackendError, RESTClient
from app.session_token import verify_session_ticket
from app.stores.endpoints import EndpointRecord
from app.guards import EffectiveScope
from app.tools.chat import AskToolSet
from fakes import TENANT, FakeGuard
from mcp.server.mcpserver.exceptions import ToolError

BASE_URL = "https://weknora.test/api/v1"
API_KEY = "wk_test_key"
SECRET = "contract-secret-0123456789abcdef"


@pytest.fixture
async def client():
    rest = RESTClient(BASE_URL, API_KEY, verify_ssl=False)
    yield rest
    await rest.aclose()


def agent_json(agent_id: str, *, mode: str = "quick-answer", kb_mode: str = "none", kb_list=None):
    return {
        "id": agent_id,
        "name": agent_id,
        "is_builtin": agent_id.startswith("builtin-"),
        "tenant_id": TENANT,
        "config": {
            "agent_mode": mode,
            "kb_selection_mode": kb_mode,
            "knowledge_bases": kb_list or [],
        },
    }


def sse_body(events: list[dict]) -> str:
    frames = [
        f"event: message\ndata: {json.dumps(event, ensure_ascii=False)}\n"
        for event in events
    ]
    return "\n".join(frames) + "\n"


def sse_response(events: list[dict]) -> httpx.Response:
    return httpx.Response(
        200,
        content=sse_body(events),
        headers={"Content-Type": "text/event-stream"},
    )


@pytest.fixture
def ask_principal():
    ep = EndpointRecord(
        id="ep-1", tenant_id=TENANT, name="ep", enabled=True,
        knowledge_base_ids=(), tools=("ask",),
        default_agent_id="builtin-quick-answer", rate_limit_per_minute=60,
    )
    token = set_principal(Principal(endpoint=ep, user_id=None))
    yield ep
    reset_principal(token)


# ===========================================================================
# ChatService 适配层
# ===========================================================================

@respx.mock
async def test_get_agent_envelope_and_api_key_header(client):
    route = respx.get(f"{BASE_URL}/agents/builtin-quick-answer").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": agent_json("builtin-quick-answer"),
        }),
    )
    service = ChatService(client)
    agent = await service.get_agent("builtin-quick-answer")
    assert agent["id"] == "builtin-quick-answer"
    assert agent["config"]["agent_mode"] == "quick-answer"
    assert route.calls.last.request.headers["X-API-Key"] == API_KEY


@respx.mock
async def test_get_agent_404_returns_none(client):
    respx.get(f"{BASE_URL}/agents/ghost").mock(
        return_value=httpx.Response(404, json={"success": False, "error": {"message": "not found"}}),
    )
    assert await ChatService(client).get_agent("ghost") is None


@respx.mock
async def test_get_agent_403_propagates_as_backend_error(client):
    # 403 不吞：工具层负责统一成“agent not available”冻结文案
    respx.get(f"{BASE_URL}/agents/builtin-quick-answer").mock(
        return_value=httpx.Response(403, json={"success": False, "error": {"message": "forbidden"}}),
    )
    with pytest.raises(BackendError) as exc:
        await ChatService(client).get_agent("builtin-quick-answer")
    assert exc.value.status == 403


@respx.mock
async def test_create_session_request_shape(client):
    route = respx.post(f"{BASE_URL}/sessions").mock(
        return_value=httpx.Response(201, json={
            "success": True, "data": {"id": "sid-77", "title": "q"},
        }),
    )
    # 截断在工具层（truncate_title），适配层只负责原样发送
    from app.backend.chat import truncate_title
    title = truncate_title("q" * 80)
    assert title == "q" * 60
    sid = await ChatService(client).create_session(title, "MCP endpoint ep")
    assert sid == "sid-77"
    body = json.loads(route.calls.last.request.content)
    assert body["title"] == "q" * 60
    assert body["description"] == "MCP endpoint ep"
    assert route.calls.last.request.headers["X-API-Key"] == API_KEY


@respx.mock
async def test_knowledge_chat_sse_framing_and_payload(client):
    route = respx.post(f"{BASE_URL}/knowledge-chat/sid-77").mock(
        return_value=sse_response([
            {"response_type": "answer", "content": "Hel"},
            {"response_type": "answer", "content": "lo"},
            {"response_type": "complete", "done": True, "data": {"final_content": "Hello"}},
        ]),
    )
    events = []
    stream = ChatService(client).qa_events(
        "sid-77", query="hi", kb_ids=["kb-1"], agent_id=None,
        use_agent_route=False, timeout=5.0,
    )
    async for event in stream:
        events.append(event)

    assert [e["response_type"] for e in events] == ["answer", "answer", "complete"]
    body = json.loads(route.calls.last.request.content)
    assert body == {"query": "hi", "channel": "mcp", "knowledge_base_ids": ["kb-1"]}
    assert "web_search_enabled" not in body


@respx.mock
async def test_agent_chat_carries_agent_id_and_no_kb_field(client):
    route = respx.post(f"{BASE_URL}/agent-chat/sid-77").mock(
        return_value=sse_response([
            {"response_type": "answer", "content": "ok"},
            {"response_type": "complete", "done": True, "data": {"final_content": "ok"}},
        ]),
    )
    stream = ChatService(client).qa_events(
        "sid-77", query="hi", kb_ids=None, agent_id="builtin-smart-reasoning",
        use_agent_route=True, timeout=5.0,
    )
    events = [event async for event in stream]
    assert events[0]["content"] == "ok"
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "query": "hi", "channel": "mcp", "agent_id": "builtin-smart-reasoning",
    }


@respx.mock
async def test_qa_pre_stream_json_error_raises_backend_error(client):
    respx.post(f"{BASE_URL}/knowledge-chat/sid-77").mock(
        return_value=httpx.Response(403, json={
            "success": False, "error": {"message": "api key requires chat capability"},
        }),
    )
    stream = ChatService(client).qa_events(
        "sid-77", query="hi", kb_ids=[], agent_id=None,
        use_agent_route=False, timeout=5.0,
    )
    with pytest.raises(BackendError, match="chat capability"):
        async for _ in stream:
            pass


@respx.mock
async def test_sse_skips_heartbeats_and_bad_json(client):
    # 心跳注释行 + 坏 data 不应拖垮整条流
    raw = (
        ": ping\n\n"
        "event: message\ndata: {\"response_type\": \"answer\", \"content\": \"a\"}\n\n"
        "event: message\ndata: not-json\n\n"
        "event: message\ndata: {\"response_type\": \"answer\", \"content\": \"b\"}\n\n"
    )
    respx.post(f"{BASE_URL}/knowledge-chat/sid-77").mock(
        return_value=httpx.Response(
            200, content=raw, headers={"Content-Type": "text/event-stream"},
        ),
    )
    stream = ChatService(client).qa_events(
        "sid-77", query="hi", kb_ids=None, agent_id=None,
        use_agent_route=False, timeout=5.0,
    )
    events = [event async for event in stream]
    assert [e["content"] for e in events] == ["a", "b"]


# ===========================================================================
# 工具端到端：真实 RESTClient + 真实 SSE
# ===========================================================================

@respx.mock
async def test_ask_end_to_end_first_turn_mints_bound_ticket(client, ask_principal):
    respx.get(f"{BASE_URL}/agents/builtin-quick-answer").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": agent_json("builtin-quick-answer", kb_mode="none"),
        }),
    )
    kb_route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": [{
                "id": "kb-1", "name": "KB kb-1", "description": "", "type": "document",
                "tenant_id": TENANT, "capabilities": {"vector": True, "keyword": True},
            }],
        }),
    )
    session_route = respx.post(f"{BASE_URL}/sessions").mock(
        return_value=httpx.Response(201, json={
            "success": True, "data": {"id": "sid-9"},
        }),
    )
    qa_route = respx.post(f"{BASE_URL}/knowledge-chat/sid-9").mock(
        return_value=sse_response([
            {"response_type": "answer", "content": "The answer."},
            {"response_type": "references", "knowledge_references": [
                {"id": "c1", "knowledge_id": "d1", "knowledge_title": "Doc",
                 "content": "see d1", "score": 0.42},
            ]},
            {"response_type": "complete", "done": True,
             "data": {"final_content": "The answer."}},
        ]),
    )

    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
    toolset = AskToolSet(client, guard, session_secret=SECRET, ask_timeout=10)
    result = await toolset.ask("why?", "", None)

    # 请求体三连：智能体 → 可见库 → 建会话 → QA，全部带 X-API-Key
    for route in (kb_route, session_route, qa_route):
        assert route.calls.last.request.headers["X-API-Key"] == API_KEY

    qa_body = json.loads(qa_route.calls.last.request.content)
    assert qa_body["query"] == "why?" and qa_body["channel"] == "mcp"
    assert qa_body["knowledge_base_ids"] == ["kb-1"]
    assert "web_search_enabled" not in qa_body and "agent_id" not in qa_body

    ticket = result.structured_content["session_id"]
    assert "sid-9" not in ticket
    assert verify_session_ticket(SECRET, ticket, "ep-1", None) == "sid-9"
    assert result.structured_content["answer"] == "The answer."
    assert result.structured_content["references"][0]["knowledge_title"] == "Doc"
    assert "Sources:\n[1] Doc (document d1)" in result.content[0].text
    # 建会话描述带端点名
    assert json.loads(session_route.calls.last.request.content)["description"] == "MCP endpoint ep"


@respx.mock
async def test_ask_smart_reasoning_agent_route_over_real_sse(client):
    ep = EndpointRecord(
        id="ep-1", tenant_id=TENANT, name="ep", enabled=True,
        knowledge_base_ids=(), tools=("ask",),
        default_agent_id="builtin-smart-reasoning", rate_limit_per_minute=60,
    )
    token = set_principal(Principal(endpoint=ep, user_id=None))
    try:
        respx.get(f"{BASE_URL}/agents/builtin-smart-reasoning").mock(
            return_value=httpx.Response(200, json={
                "success": True,
                "data": agent_json("builtin-smart-reasoning", mode="smart-reasoning",
                                   kb_mode="selected", kb_list=["kb-9"]),
            }),
        )
        kb_route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
            return_value=httpx.Response(200, json={
                "success": True,
                "data": [{
                    "id": "kb-1", "name": "KB kb-1", "description": "", "type": "document",
                    "tenant_id": TENANT, "capabilities": {"vector": True, "keyword": True},
                }],
            }),
        )
        respx.post(f"{BASE_URL}/sessions").mock(
            return_value=httpx.Response(201, json={"success": True, "data": {"id": "sid-2"}}),
        )
        qa_route = respx.post(f"{BASE_URL}/agent-chat/sid-2").mock(
            return_value=sse_response([
                {"response_type": "answer", "content": "reasoned"},
                {"response_type": "complete", "done": True, "data": {"final_content": "reasoned"}},
            ]),
        )

        # 闭集模型：即便智能体自称 selected kb-9，侧车仍只下发 guard 范围内的可见库
        guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
        toolset = AskToolSet(client, guard, session_secret=SECRET, ask_timeout=10)
        result = await toolset.ask("deep?", "", None)

        assert kb_route.called
        body = json.loads(qa_route.calls.last.request.content)
        assert body["agent_id"] == "builtin-smart-reasoning"
        assert body["knowledge_base_ids"] == ["kb-1"]
        assert "web_search_enabled" not in body
        assert result.structured_content["agent_id"] == "builtin-smart-reasoning"
    finally:
        reset_principal(token)


@respx.mock
async def test_ask_agent_403_becomes_frozen_unavailable_text(client, ask_principal):
    respx.get(f"{BASE_URL}/agents/builtin-quick-answer").mock(
        return_value=httpx.Response(403, json={
            "success": False, "error": {"message": "api key lacks capability 'chat'"},
        }),
    )
    toolset = AskToolSet(client, FakeGuard(), session_secret=SECRET, ask_timeout=10)
    with pytest.raises(ToolError) as exc:
        await toolset.ask("q", "", None)
    # 不泄露上游 403 细节，冻结文案点名配置的智能体
    assert "builtin-quick-answer" in str(exc.value) and "is not available" in str(exc.value)
    assert "chat" not in str(exc.value)
