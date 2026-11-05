"""wiki REST 契约测试：respx 打真 httpx，锁死侧车 ↔ WeKnora 的 wiki 线上约定。

关键点：wiki 三接口都是裸对象（无 {success,data} 信封）；
404 是 {"error":"Wiki page not found"}；5xx 错误体同样是 {"error":"字符串"}，
逐 KB 失败文案必须保留上游原文而不是兜底 "upstream request failed"。
"""

from __future__ import annotations

import httpx
import pytest
import respx

from app.backend.client import BackendError, RESTClient
from app.backend.wiki import WikiService
from app.tools.wiki import WikiToolSet
from app.guards import EffectiveScope
from fakes import FakeGuard, TENANT
from mcp.server.mcpserver.exceptions import ToolError

BASE_URL = "https://weknora.test/api/v1"
API_KEY = "wk_test_key"


@pytest.fixture
async def client():
    rest = RESTClient(BASE_URL, API_KEY, verify_ssl=False)
    yield rest
    await rest.aclose()


def kb_json(kb_id: str, *, wiki: bool = True) -> dict:
    return {
        "id": kb_id,
        "name": f"KB {kb_id}",
        "description": "",
        "type": "document",
        "tenant_id": TENANT,
        "capabilities": {"vector": True, "keyword": True, "wiki": wiki},
    }


# ===========================================================================
# 适配层：裸响应与 URL 形状
# ===========================================================================

@respx.mock
async def test_search_bare_envelope_and_query_params(client):
    route = respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/search").mock(
        return_value=httpx.Response(200, json={"pages": [
            {"slug": "p1", "title": "P1", "page_type": "concept", "content": "x",
             "aliases": [], "summary": "s"},
        ]}),
    )
    service = WikiService(client)
    pages = await service.search_pages("kb-1", "p1|p2", 50)
    assert len(pages) == 1 and pages[0]["slug"] == "p1"
    request = route.calls.last.request
    assert request.url.path == "/api/v1/knowledgebase/kb-1/wiki/search"
    params = request.url.params
    assert params["q"] == "p1|p2" and params["limit"] == "50"


@respx.mock
async def test_get_page_keeps_slug_slash_and_404_returns_none(client):
    route = respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/pages/concept/rag").mock(
        return_value=httpx.Response(200, json={
            "slug": "concept/rag", "title": "RAG", "page_type": "concept",
            "content": "body", "summary": "s", "aliases": [],
            "out_links": [], "in_links": [], "source_refs": [],
            "in_link_titles": {},
        }),
    )
    service = WikiService(client)
    page = await service.get_page("kb-1", "concept/rag")
    assert page is not None and page["slug"] == "concept/rag"
    assert route.calls.last.request.url.path == "/api/v1/knowledgebase/kb-1/wiki/pages/concept/rag"

    respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/pages/ghost").mock(
        return_value=httpx.Response(404, json={"error": "Wiki page not found"}),
    )
    assert await service.get_page("kb-1", "ghost") is None


@respx.mock
async def test_get_page_quotes_unsafe_slug_chars(client):
    route = respx.get(url__regex=r".*/wiki/pages/a%20b%3Fc").mock(
        return_value=httpx.Response(404, json={"error": "Wiki page not found"}),
    )
    service = WikiService(client)
    await service.get_page("kb-1", "a b?c")
    assert route.called


@respx.mock
async def test_index_bare_envelope(client):
    route = respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/index").mock(
        return_value=httpx.Response(200, json={
            "intro": "Intro", "version": 1, "groups": [
                {"type": "entity", "total": 0, "items": []},
            ],
        }),
    )
    service = WikiService(client)
    view = await service.get_index("kb-1", 50)
    assert view["groups"][0]["type"] == "entity"
    assert route.calls.last.request.url.params["limit"] == "50"


@respx.mock
async def test_bare_string_error_body_preserved(client):
    respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/search").mock(
        return_value=httpx.Response(500, json={"error": "invalid regular expression: quantifier operand invalid"}),
    )
    service = WikiService(client)
    with pytest.raises(BackendError, match="quantifier operand invalid"):
        await service.search_pages("kb-1", "(", 10)


# ===========================================================================
# 工具端到端：真实 RESTClient + FakeGuard
# ===========================================================================

@respx.mock
@pytest.mark.usefixtures("principal_context")
async def test_search_fan_out_over_real_rest(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1"), kb_json("kb-2"),
        ]}),
    )
    respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/search").mock(
        return_value=httpx.Response(200, json={"pages": [
            {"slug": "p1", "title": "P1", "page_type": "concept", "summary": "s1",
             "content": "rag content", "aliases": [], "out_links": [], "in_links": []},
        ]}),
    )
    respx.get(f"{BASE_URL}/knowledgebase/kb-2/wiki/search").mock(
        return_value=httpx.Response(500, json={"error": "db is down"}),
    )

    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1", "kb-2"})))
    toolset = WikiToolSet(client=client, guard=guard)
    result = await toolset.wiki_search("rag", None, None, 10)

    text = result.content[0].text
    assert "kb-1" in text and "<match_snippet>" in text
    assert "Wiki search" in text and "db is down" in text
    assert result.structured_content["found_kbs"]["p1"] == ["kb-1"]


@respx.mock
@pytest.mark.usefixtures("principal_context")
async def test_read_page_not_found_frozen_text_over_real_rest(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]}),
    )
    respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/pages/ghost").mock(
        return_value=httpx.Response(404, json={"error": "Wiki page not found"}),
    )
    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
    toolset = WikiToolSet(client=client, guard=guard)
    with pytest.raises(ToolError, match="Wiki page 'ghost' not found"):
        await toolset.wiki_read_page("ghost")


@respx.mock
@pytest.mark.usefixtures("principal_context")
async def test_wiki_index_over_real_rest(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]}),
    )
    index_route = respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/index").mock(
        return_value=httpx.Response(200, json={
            "intro": "Wiki intro", "version": 3,
            "groups": [{"type": "entity", "total": 1, "items": [{"slug": "a", "title": "A"}]}],
        }),
    )
    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
    toolset = WikiToolSet(client=client, guard=guard)
    result = await toolset.wiki_index("kb-1", 50)
    # limit 钳制默认 50；响应是裸对象，不套信封
    assert index_route.calls.last.request.url.params["limit"] == "50"
    assert result.structured_content["knowledge_base_id"] == "kb-1"
    assert result.structured_content["groups"][0]["type"] == "entity"
    # text 是 Go 风格缩进 JSON
    assert '"intro": "Wiki intro"' in result.content[0].text


@respx.mock
@pytest.mark.usefixtures("principal_context")
async def test_wiki_index_load_error_frozen_prefix(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]}),
    )
    respx.get(f"{BASE_URL}/knowledgebase/kb-1/wiki/index").mock(
        return_value=httpx.Response(500, json={"error": "pg exploded"}),
    )
    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
    toolset = WikiToolSet(client=client, guard=guard)
    with pytest.raises(ToolError, match="^failed to load wiki index: pg exploded$"):
        await toolset.wiki_index("kb-1", 50)


@respx.mock
@pytest.mark.usefixtures("principal_context")
async def test_wiki_index_outside_scope_fails_without_probe(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]}),
    )
    detail = respx.get(f"{BASE_URL}/knowledge-bases/kb-other").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("kb-other")}),
    )
    index_route = respx.get(f"{BASE_URL}/knowledgebase/kb-other/wiki/index")
    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-1"})))
    toolset = WikiToolSet(client=client, guard=guard)
    with pytest.raises(ToolError, match="kb-other"):
        await toolset.wiki_index("kb-other", 50)
    assert not detail.called and not index_route.called
