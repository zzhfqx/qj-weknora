"""ask 工具单测：引用整理纯函数 + fake 后端编排（真 HTTP/SSE 契约见 test_chat_contract）。

覆盖（对照 internal/mcpserver/tools_ask.go）：
  - 300 rune 摘录截断与存储引用避让、图片去重合并、引用去重/score 省略；
  - SSE 聚合：answer 拼接、references 顶层/嵌套两种形状、complete 兜底、error 中断；
  - 智能体裁决（内置白名单/租户匹配/不可用冻结文案）、闭集库范围解析（空范围拒绝）；
  - 首轮建会话签票、续问验票、跨端点/用户票据拒绝；
  - smart-reasoning 走 agent-chat 且不带 web_search_enabled；整体超时；密钥缺失 fail-closed。
"""

from __future__ import annotations

import asyncio
from contextlib import contextmanager

import pytest

from app.auth.middleware import Principal, reset_principal, set_principal
from app.backend.client import BackendError
from app.backend.kb import KBSummary, ScopeError
from app.guards import EffectiveScope, REASON_TOOL_DISABLED
from app.session_token import issue_session_ticket
from app.stores.endpoints import EndpointRecord
from app.tools import chat as C
from app.tools.chat import AskToolSet
from fakes import TENANT, FakeGuard, kb_summary as _kb
from mcp.server.mcpserver.exceptions import ToolError

_AGENT_UNSET = object()

SECRET = "ask-test-secret-0123456789abcdef"


# ---------------------------------------------------------------------------
# fake 后端
# ---------------------------------------------------------------------------

class FakeKBs:
    def __init__(self, visible: list[KBSummary]) -> None:
        self.visible = visible

    async def resolve_visible(self, scope):
        # 与真 KBService 一致：闭集过滤，scope 外（含空 scope）一律不可见
        return [kb for kb in self.visible if kb.id in scope.kb_ids]

    async def resolve_selectors(self, scope, selectors):
        visible = await self.resolve_visible(scope)
        if not selectors:
            return visible
        out: list[KBSummary] = []
        for selector in selectors:
            match = next(
                (k for k in visible if k.id == selector or k.name.lower() == selector.lower()),
                None,
            )
            if match is None:
                raise ScopeError(
                    f"knowledge base {selector!r} was not found or is outside this endpoint's scope"
                )
            out.append(match)
        return out


class FakeChat:
    def __init__(self, *, agent: dict | None | Exception = None, sid: str = "sid-1") -> None:
        self._agent = agent
        self._sid = sid
        self.agent_calls: list[str] = []
        self.session_calls: list[dict] = []
        # qa_calls: [(path, payload)]；events 是预置事件列表
        self.qa_calls: list[tuple[str, dict]] = []
        self.events: list[list[dict]] = []

    async def get_agent(self, agent_id):
        self.agent_calls.append(agent_id)
        if isinstance(self._agent, Exception):
            raise self._agent
        return self._agent

    async def create_session(self, title, description):
        self.session_calls.append({"title": title, "description": description})
        return self._sid

    def qa_events(self, sid, *, query, kb_ids, agent_id, use_agent_route, timeout):
        path = f"/{'agent-chat' if use_agent_route else 'knowledge-chat'}/{sid}"
        payload = {"query": query, "channel": "mcp"}
        if kb_ids is not None:
            payload["knowledge_base_ids"] = kb_ids
        if use_agent_route:
            payload["agent_id"] = agent_id
        self.qa_calls.append((path, payload))
        events = self.events.pop(0) if self.events else []
        return _stream(events)


async def _stream(events):
    for event in events:
        yield event


class SlowFakeChat(FakeChat):
    """首事件长时间不到，用于触发整体超时。"""

    def qa_events(self, sid, *, query, kb_ids, agent_id, use_agent_route, timeout):
        self.qa_calls.append((sid, {}))

        async def _slow():
            await asyncio.sleep(1.0)
            yield {}

        return _slow()


def agent_json(
    agent_id: str,
    *,
    builtin: bool = False,
    tenant: int = TENANT,
    mode: str = "quick-answer",
    kb_mode: str = "",
    kb_list: list[str] | None = None,
) -> dict:
    return {
        "id": agent_id,
        "name": agent_id,
        "is_builtin": builtin,
        "tenant_id": tenant,
        "config": {
            "agent_mode": mode,
            "kb_selection_mode": kb_mode,
            "knowledge_bases": kb_list or [],
        },
    }


def _event(rtype: str, **kw) -> dict:
    return {"response_type": rtype, "content": "", "done": False, **kw}


def _toolset(
    kbs: list[KBSummary] | None = None,
    *,
    agent=_AGENT_UNSET,
    guard: FakeGuard | None = None,
    scope: EffectiveScope | None = None,
    secret: str = SECRET,
    timeout: int = 240,
    sid: str = "sid-1",
) -> tuple[AskToolSet, FakeGuard, FakeChat]:
    # 显式传 None 表示 GET /agents 返回 404；缺省才补一个默认可用智能体
    if agent is _AGENT_UNSET:
        agent = agent_json("builtin-quick-answer", builtin=True)
    guard = guard or FakeGuard(scope=scope)
    toolset = AskToolSet(client=None, guard=guard, session_secret=secret, ask_timeout=timeout)
    fake = FakeChat(agent=agent, sid=sid)
    toolset._chat = fake
    # 默认给一个落在 DEFAULT_TEST_SCOPE 内的可见库：会话/票据/智能体用例
    # 需要先通过"解析生效范围"这一步；空范围行为另有专门用例
    toolset._kbs = FakeKBs(kbs if kbs is not None else [_kb("kb-1")])
    return toolset, guard, fake


@contextmanager
def _principal(*, agent_id: str = "", user_id: str | None = None):
    # 端点配置的 default_agent_id 必须与假后端返回的智能体 id 一致，
    # 因为实现按“请求的 id”做内置白名单裁决
    ep = EndpointRecord(
        id="ep-1", tenant_id=TENANT, name="ep", enabled=True,
        knowledge_base_ids=(), tools=("ask",),
        default_agent_id=agent_id, rate_limit_per_minute=60,
    )
    token = set_principal(Principal(endpoint=ep, user_id=user_id))
    try:
        yield
    finally:
        reset_principal(token)


# ===========================================================================
# 纯函数
# ===========================================================================

def test_truncate_excerpt_under_and_over_limit():
    short = "x" * 100
    assert C.truncate_excerpt(short) == short
    out = C.truncate_excerpt("y" * 400)
    assert len(out) == 301 and out.endswith("…")


def test_truncate_excerpt_avoids_splitting_storage_reference():
    ref = "resource://AbcDef123"
    excerpt = "z" * 295 + " " + ref + " tail"
    out = C.truncate_excerpt(excerpt)
    assert ref not in out                      # 引用跨切点：整体回退，不留半句柄
    assert out.endswith("…")
    assert out[:295].count("z") == 295


def test_summarize_reference_images_dedupe_and_merge():
    raw = '[{"url": "u1", "caption": "cap1"}, {"url": "u2"}, {"url": "", "caption": "x"}]'
    images = C.summarize_reference_images(raw)
    assert [i["url"] for i in images] == ["u1", "u2"]
    assert images[0]["caption"] == "cap1"
    # 重复 url 合并空白字段
    merged = C.summarize_reference_images('[{"url":"u1","ocr_text":"ocr"}]', images)
    u1 = next(i for i in merged if i["url"] == "u1")
    assert u1["caption"] == "cap1" and u1["ocr_text"] == "ocr"
    # 坏 JSON：已有合并结果保留（内容复制），无基线则空列表
    fallback = C.summarize_reference_images("not-json", images)
    assert [i["url"] for i in fallback] == ["u1", "u2"]
    assert C.summarize_reference_images("not-json") == []


def test_summarize_references_dedupe_score_and_fields():
    rows = [
        {"id": "c1", "knowledge_id": "d1", "knowledge_title": "Doc", "content": "a", "score": 0.9,
         "image_info": '[{"url":"i1"}]'},
        {"id": "c1", "knowledge_id": "d1", "content": "a", "score": 0.8},
        {"id": "", "knowledge_id": "d2", "chunk_index": 7, "content": "b", "score": 0.0},
    ]
    out = C.summarize_references(rows)
    assert len(out) == 2
    assert out[0]["knowledge_title"] == "Doc" and out[0]["images"] == [{"url": "i1"}]
    assert out[1]["chunk_id"] == ""
    assert "score" not in out[1]                # 0 分省略
    assert out[0]["score"] == 0.9


def test_render_answer_text_with_and_without_refs():
    no_refs = C.render_answer_text("Hello", "ticket-1", [])
    assert no_refs == "Hello\n\nsession_id: ticket-1"

    refs = [
        {"knowledge_id": "d1", "knowledge_title": "Doc One", "images": []},
        {"knowledge_id": "d2", "images": [{"url": "http://x/a&b<c>.png"}]},
    ]
    text = C.render_answer_text("Hello", "ticket-1", refs)
    assert "[1] Doc One (document d1)" in text
    # Go strconv.Quote：&<> 保持字面，双引号 JSON 风格转义
    assert '  Image: "http://x/a&b<c>.png"' in text
    assert text.endswith("session_id: ticket-1")


# ===========================================================================
# SSE 聚合
# ===========================================================================

async def test_aggregate_answer_chunks_and_top_level_references():
    events = _stream([
        _event("agent_query"),
        _event("answer", content="Hel"),
        _event("answer", content="lo"),
        _event("references", knowledge_references=[{"id": "c1", "knowledge_id": "d1", "content": "x"}]),
        _event("thinking", content="ignore me"),
        _event("complete", data={"final_content": "Hello", "total_steps": 3}),
    ])
    answer, refs, error_text = await C.aggregate_qa(events)
    assert answer == "Hello"
    assert len(refs) == 1 and refs[0]["id"] == "c1"
    assert error_text == ""


async def test_aggregate_references_nested_in_data():
    events = _stream([
        _event("references", data={"references": [{"id": "c2", "knowledge_id": "d9", "content": "y"}]}),
        _event("answer", content="A", done=True),
        _event("complete", data={}),
    ])
    _answer, refs, _err = await C.aggregate_qa(events)
    assert refs[0]["id"] == "c2"


async def test_aggregate_complete_final_content_fallback():
    # 没有任何 answer 块时，complete.final_content 兜底
    events = _stream([_event("complete", data={"final_content": "fallback answer"})])
    answer, _refs, _err = await C.aggregate_qa(events)
    assert answer == "fallback answer"


async def test_aggregate_error_event_breaks():
    events = _stream([
        _event("answer", content="partial"),
        _event("error", content="retrieval failed", data={"stage": "knowledge_qa_execution"}),
        _event("answer", content="never"),
    ])
    answer, _refs, error_text = await C.aggregate_qa(events)
    assert answer == "partial" and error_text == "retrieval failed"


async def test_aggregate_error_falls_back_to_data_error():
    events = _stream([_event("error", data={"error": "boom"})])
    _answer, _refs, error_text = await C.aggregate_qa(events)
    assert error_text == "boom"


# ===========================================================================
# ask 主流程
# ===========================================================================

def test_ask_requires_question():
    toolset, _g, _f = _toolset()
    with _principal():
        with pytest.raises(ToolError, match="question is required"):
            asyncio.run(toolset.ask("   ", "", None))


def test_ask_guard_denied():
    guard = FakeGuard(deny=REASON_TOOL_DISABLED)
    toolset, _g, _f = _toolset(guard=guard)
    with _principal():
        with pytest.raises(ToolError, match="not enabled on this endpoint"):
            asyncio.run(toolset.ask("q", "", None))


def test_ask_agent_unavailable_frozen_text():
    toolset, _g, fake = _toolset(agent=None)
    with _principal():
        with pytest.raises(ToolError, match='the agent configured.*builtin-quick-answer.*is not available'):
            asyncio.run(toolset.ask("q", "", None))
    assert fake.agent_calls == ["builtin-quick-answer"]


def test_ask_agent_upstream_error_uses_same_frozen_text():
    toolset, _g, _f = _toolset(agent=BackendError("forbidden", status=403))
    with _principal():
        with pytest.raises(ToolError, match="is not available"):
            asyncio.run(toolset.ask("q", "", None))


def test_ask_internal_builtin_agent_rejected():
    # wiki-fixer 是内置但不在用户可见白名单：即便后端返回了也拒绝
    toolset, _g, fake = _toolset(agent=agent_json("builtin-wiki-fixer", builtin=True))
    with _principal(agent_id="builtin-wiki-fixer"):
        with pytest.raises(ToolError, match="is not available"):
            asyncio.run(toolset.ask("q", "", None))
    assert fake.agent_calls == ["builtin-wiki-fixer"]


def test_ask_custom_agent_tenant_mismatch_rejected():
    toolset, _g, fake = _toolset(agent=agent_json("agent-x", builtin=False, tenant=TENANT + 1))
    with _principal(agent_id="agent-x"):
        with pytest.raises(ToolError, match="is not available"):
            asyncio.run(toolset.ask("q", "", None))
    assert fake.agent_calls == ["agent-x"]


def test_ask_custom_agent_same_tenant_allowed():
    toolset, _g, fake = _toolset(
        agent=agent_json("agent-ok", builtin=False, tenant=TENANT, kb_mode="none"),
    )
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal(agent_id="agent-ok"):
        result = asyncio.run(toolset.ask("q", "", None))
    assert result.structured_content["agent_id"] == "agent-ok"


def test_ask_default_agent_and_explicit_ids_route_to_knowledge_chat():
    kbs = [_kb("kb-1"), _kb("kb-2")]
    toolset, _g, fake = _toolset(
        kbs, agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    fake.events = [[
        _event("answer", content="The answer."),
        _event("complete", data={"final_content": "The answer."}),
    ]]
    with _principal():
        result = asyncio.run(toolset.ask("why?", "", None))

    path, payload = fake.qa_calls[0]
    assert path == "/knowledge-chat/sid-1"
    assert payload["query"] == "why?"
    assert set(payload["knowledge_base_ids"]) == {"kb-1", "kb-2"}  # 闭集范围：可见库即生效 scope 内的库
    assert "web_search_enabled" not in payload
    assert "agent_id" not in payload
    assert payload["channel"] == "mcp"
    # 会话标题截 60 字符，描述带端点名
    assert fake.session_calls[0]["title"] == "why?"
    assert fake.session_calls[0]["description"] == "MCP endpoint ep"

    text = result.content[0].text
    ticket = result.structured_content["session_id"]
    assert ticket.startswith("t1.") and "sid-1" not in ticket
    assert text.endswith(f"session_id: {ticket}")
    assert result.structured_content["agent_id"] == "builtin-quick-answer"


def test_ask_smart_reasoning_routes_to_agent_chat():
    toolset, _g, fake = _toolset(
        [_kb("kb-1")],
        agent=agent_json("agent-smart", builtin=False, mode="smart-reasoning", kb_mode="none"),
    )
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal(agent_id="agent-smart"):
        asyncio.run(toolset.ask("q", "", None))
    path, payload = fake.qa_calls[0]
    assert path == "/agent-chat/sid-1"
    assert payload["agent_id"] == "agent-smart"
    assert "web_search_enabled" not in payload


def test_ask_agent_own_kb_list_ignored_scope_pins_payload():
    # 智能体自称 selected kb-9 也无用：侧车只下发 guard 生效闭集内的可见库
    toolset, _g, fake = _toolset(
        [_kb("kb-1"), _kb("kb-2")],
        agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="selected", kb_list=["kb-9"]),
    )
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal():
        asyncio.run(toolset.ask("q", "", None))
    _, payload = fake.qa_calls[0]
    assert set(payload["knowledge_base_ids"]) == {"kb-1", "kb-2"}


def test_ask_explicit_selectors_win():
    kbs = [_kb("kb-1"), _kb("kb-2")]
    toolset, _g, fake = _toolset(kbs, agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"))
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal():
        asyncio.run(toolset.ask("q", "", ["kb-2"]))
    _, payload = fake.qa_calls[0]
    assert payload["knowledge_base_ids"] == ["kb-2"]


def test_ask_restricted_scope_pins_endpoint_kbs():
    kbs = [_kb("kb-1"), _kb("kb-2"), _kb("kb-3")]
    scope = EffectiveScope(kb_ids=frozenset({"kb-2"}))
    # 即便智能体自带别的库，闭集范围仍钉死自己的库集合
    toolset, _g, fake = _toolset(
        kbs, scope=scope,
        agent=agent_json("agent-x", builtin=False, kb_mode="selected", kb_list=["kb-9"]),
    )
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal(agent_id="agent-x"):
        asyncio.run(toolset.ask("q", "", None))
    _, payload = fake.qa_calls[0]
    assert payload["knowledge_base_ids"] == ["kb-2"]


def test_ask_empty_scope_rejected_before_upstream():
    # guard 可以放行空 scope（公共书架与个人授权都为空），工具层必须拒绝具体调用，
    # 且绝不向上游发起 QA
    toolset, _g, fake = _toolset(
        [_kb("kb-1")], scope=EffectiveScope(kb_ids=frozenset()),
        agent=agent_json("builtin-quick-answer", builtin=True),
    )
    with _principal():
        with pytest.raises(ToolError, match="no knowledge base is available"):
            asyncio.run(toolset.ask("q", "", None))
    assert fake.qa_calls == []


def test_ask_followup_verifies_ticket_and_reuses_sid():
    toolset, _g, fake = _toolset(agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"))
    fake.events = [[_event("answer", content="ok"), _event("complete", data={"final_content": "ok"})]]
    with _principal():
        ticket = issue_session_ticket(SECRET, "sid-existing", "ep-1", None)
        result = asyncio.run(toolset.ask("again", ticket, None))
    assert not fake.session_calls                    # 续问不建新会话
    path, _payload = fake.qa_calls[0]
    assert path == "/knowledge-chat/sid-existing"
    assert result.structured_content["session_id"] == ticket


def test_ask_followup_ticket_wrong_endpoint_rejected():
    toolset, _g, fake = _toolset(agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"))
    foreign = issue_session_ticket(SECRET, "sid-x", "ep-other", None)
    with _principal():
        with pytest.raises(ToolError, match='session ".*" was not found on this endpoint'):
            asyncio.run(toolset.ask("q", foreign, None))
    assert fake.qa_calls == []


def test_ask_followup_ticket_wrong_user_rejected():
    toolset, _g, fake = _toolset(
        agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    foreign = issue_session_ticket(SECRET, "sid-x", "ep-1", "user-other")
    with _principal(user_id="user-1"):
        with pytest.raises(ToolError, match="was not found on this endpoint"):
            asyncio.run(toolset.ask("q", foreign, None))
    assert fake.qa_calls == []


def test_ask_tampered_ticket_rejected():
    toolset, _g, fake = _toolset(agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"))
    with _principal():
        with pytest.raises(ToolError, match="was not found on this endpoint"):
            asyncio.run(toolset.ask("q", "t1.garbage.sig", None))


def test_ask_missing_secret_fails_closed():
    toolset, _g, fake = _toolset(secret="", agent=agent_json("builtin-quick-answer", builtin=True))
    with _principal():
        with pytest.raises(ToolError, match="session ticket secret is not configured"):
            asyncio.run(toolset.ask("q", "", None))
    assert fake.session_calls == []


def test_ask_error_event_without_answer_fails():
    toolset, _g, fake = _toolset(
        [_kb("kb-1")], agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    fake.events = [[
        _event("references", knowledge_references=[]),
        _event("error", content="model unavailable"),
    ]]
    with _principal():
        with pytest.raises(ToolError, match="failed to answer: model unavailable"):
            asyncio.run(toolset.ask("q", "", None))


def test_ask_partial_answer_with_error_still_returned():
    # 与 Go runQA 一致：答案非空时 error 事件不翻盘，部分答案照常返回
    toolset, _g, fake = _toolset(
        [_kb("kb-1")], agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    fake.events = [[
        _event("answer", content="partial"),
        _event("error", content="model unavailable"),
    ]]
    with _principal():
        result = asyncio.run(toolset.ask("q", "", None))
    assert result.structured_content["answer"] == "partial"


def test_ask_empty_answer_uses_fallback_sentence():
    toolset, _g, fake = _toolset(
        [_kb("kb-1")], agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    fake.events = [[_event("complete", data={})]]
    with _principal():
        result = asyncio.run(toolset.ask("q", "", None))
    assert result.structured_content["answer"] == C.NO_ANSWER_TEXT
    assert result.content[0].text.startswith(C.NO_ANSWER_TEXT)


def test_ask_references_render_sources():
    toolset, _g, fake = _toolset(
        [_kb("kb-1")], agent=agent_json("builtin-quick-answer", builtin=True, kb_mode="none"),
    )
    fake.events = [[
        _event("answer", content="A"),
        _event("references", knowledge_references=[
            {"id": "c1", "knowledge_id": "d1", "knowledge_title": "Doc", "content": "see d1", "score": 0.5},
        ]),
        _event("complete", data={"final_content": "A"}),
    ]]
    with _principal():
        result = asyncio.run(toolset.ask("q", "", None))
    text = result.content[0].text
    assert "Sources:\n[1] Doc (document d1)" in text
    refs = result.structured_content["references"]
    assert refs[0]["excerpt"] == "see d1" and refs[0]["score"] == 0.5


def test_ask_create_session_failure_frozen_prefix():
    toolset, _g, fake = _toolset(agent=agent_json("builtin-quick-answer", builtin=True))

    async def _boom(title, description):
        raise BackendError("db unavailable", status=500)

    fake.create_session = _boom
    with _principal():
        with pytest.raises(ToolError, match="failed to create session: db unavailable"):
            asyncio.run(toolset.ask("q", "", None))


def test_ask_overall_timeout():
    guard = FakeGuard()
    toolset = AskToolSet(client=None, guard=guard, session_secret=SECRET, ask_timeout=1)
    fake = SlowFakeChat(agent=agent_json("builtin-quick-answer", builtin=True))
    toolset._chat = fake
    toolset._kbs = FakeKBs([_kb("kb-1")])
    with _principal():
        with pytest.raises(ToolError, match="failed to answer: answer timed out"):
            asyncio.run(toolset.ask("q", "", None))
