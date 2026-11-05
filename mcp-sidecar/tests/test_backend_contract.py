"""REST 契约测试（阶段 1-D）：respx 打真 httpx，锁死侧车 ↔ WeKnora 的线上约定。

覆盖三层：
  1. RESTClient：X-API-Key 头、成功/失败信封、404→BackendNotFound、非 JSON、超时；
  2. backend 适配：库能力位解析/回落、共享库详情补查、文档与分块分页参数；
  3. 工具端到端：真实 RESTClient + FakeGuard，校验 hybrid-search 请求体、
     文档级阈值归零、chunks 重复 query 参数、越权 fail-closed。

这些用例就是需求.md §2.3 "升级冒烟清单"的可执行版本：上游换版本后先跑它们。
"""

from __future__ import annotations

import httpx
import json
import pytest
import respx

from app.backend.client import BackendError, BackendNotFound, RESTClient
from app.backend.chunks import ChunkService
from app.backend.documents import DocumentService
from app.backend.kb import KBService, ScopeError
from app.guards import EffectiveScope
from app.tools.retrieve import RetrieveToolSet
from fakes import TENANT, FakeGuard
from mcp.server.mcpserver.exceptions import ToolError

BASE_URL = "https://weknora.test/api/v1"
API_KEY = "wk_test_key"


# ===========================================================================
# 夹具
# ===========================================================================

@pytest.fixture
async def client():
    rest = RESTClient(BASE_URL, API_KEY, verify_ssl=False)
    yield rest
    await rest.aclose()


def kb_json(kb_id: str, *, name: str | None = None, kb_type: str = "document",
            caps: dict | None = "default", strategy: dict | None = None) -> dict:
    payload: dict = {
        "id": kb_id,
        "name": name or f"KB {kb_id}",
        "description": "desc",
        "type": kb_type,
        "tenant_id": TENANT,
    }
    if caps == "default":
        caps = {"vector": True, "keyword": True, "wiki": False, "graph": False, "faq": False}
    if caps is not None:
        payload["capabilities"] = caps
    if strategy is not None:
        payload["indexing_strategy"] = strategy
    return payload


def doc_json(doc_id: str, kb_id: str, *, tenant: int = TENANT, title: str = "Title") -> dict:
    return {
        "id": doc_id,
        "title": title,
        "knowledge_base_id": kb_id,
        "tenant_id": tenant,
        "parse_status": "completed",
        "file_name": "",
    }


def search_row(row_id: str, content: str, score: float, *, kb_id: str,
               knowledge_id: str = "d1", chunk_index: int = 0) -> dict:
    return {
        "id": row_id,
        "content": content,
        "score": score,
        "knowledge_id": knowledge_id,
        "knowledge_base_id": kb_id,
        "chunk_index": chunk_index,
        "chunk_type": "text",
        "match_type": "hybrid",
        "knowledge_title": "Title",
    }


def make_toolset(client, *, guard=None, kb_scope=("kb-1",)) -> RetrieveToolSet:
    # 闭集 scope 必须与各用例 mock 的库集合一致：scope 内、自有列表外的 id
    # 会触发按需详情 GET，契约用例按"自己 mock 自己的库"原则显式给范围
    return RetrieveToolSet(
        client=client,
        guard=guard or FakeGuard(scope=EffectiveScope(kb_ids=frozenset(kb_scope))),
    )


# ===========================================================================
# 1. RESTClient 信封与错误映射
# ===========================================================================

@respx.mock
async def test_get_sends_api_key_header_and_parses_envelope(client):
    route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": []})
    )
    body = await client.get_json("/knowledge-bases")
    assert body["success"] is True
    request = route.calls.last.request
    assert request.headers["X-API-Key"] == API_KEY
    assert request.headers["Accept"] == "application/json"


@respx.mock
async def test_missing_api_key_fails_without_http(client):
    bare = RESTClient(BASE_URL, "")
    route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": []})
    )
    with pytest.raises(BackendError, match="api key is not configured"):
        await bare.get_json("/knowledge-bases")
    assert not route.called


@respx.mock
async def test_404_maps_to_backend_not_found(client):
    respx.get(f"{BASE_URL}/knowledge/missing").mock(
        return_value=httpx.Response(404, json={
            "success": False, "error": {"code": "not_found", "message": "knowledge not found"},
        })
    )
    with pytest.raises(BackendNotFound) as exc_info:
        await client.get_json("/knowledge/missing")
    assert exc_info.value.status == 404
    assert exc_info.value.code == "not_found"
    assert exc_info.value.message == "knowledge not found"


@respx.mock
async def test_403_error_envelope_message_and_code(client):
    respx.get(f"{BASE_URL}/shared-knowledge-bases").mock(
        return_value=httpx.Response(403, json={
            "success": False, "error": {"code": "forbidden", "message": "manage spaces required"},
        })
    )
    with pytest.raises(BackendError) as exc_info:
        await client.get_json("/shared-knowledge-bases")
    assert exc_info.value.status == 403 and exc_info.value.code == "forbidden"


@respx.mock
async def test_non_json_upstream_body(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(502, text="bad gateway")
    )
    with pytest.raises(BackendError, match="non-JSON"):
        await client.get_json("/knowledge-bases")


@respx.mock
async def test_success_false_at_200_is_error(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={
            "success": False, "error": {"code": "weird", "message": "backend degraded"},
        })
    )
    with pytest.raises(BackendError, match="backend degraded"):
        await client.get_json("/knowledge-bases")


@respx.mock
async def test_timeout_maps_to_backend_error(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(side_effect=httpx.ConnectTimeout("slow"))
    with pytest.raises(BackendError, match="timed out"):
        await client.get_json("/knowledge-bases")


# ===========================================================================
# 2. backend 适配层
# ===========================================================================

@respx.mock
async def test_kb_list_parses_capabilities_and_short_caches(client):
    route = respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1", caps={"vector": True, "keyword": False, "wiki": False,
                                  "graph": False, "faq": False}),
        ]})
    )
    clock = [100.0]
    kbs = KBService(client, ttl=5.0, clock=lambda: clock[0])
    first = await kbs.list_own()
    assert first[0].semantic is True and first[0].keyword is False
    await kbs.list_own()
    assert route.call_count == 1  # TTL 内命中缓存
    clock[0] += 6
    await kbs.list_own()
    assert route.call_count == 2  # 过期后重新拉取


@respx.mock
async def test_kb_capabilities_fallback_to_indexing_strategy(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1", caps=None, strategy={
                "vector_enabled": True, "keyword_enabled": True, "wiki_enabled": False,
            }),
        ]})
    )
    kbs = KBService(client)
    own = await kbs.list_own()
    assert own[0].semantic is True and own[0].keyword is True and own[0].wiki is False


@respx.mock
async def test_resolve_visible_fetches_missing_scope_ids_by_detail(client):
    # 自有列表只含 kb-a；端点白名单含共享库 shared-x 与已失效 gone-y
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-a"),
        ]})
    )
    detail = respx.get(f"{BASE_URL}/knowledge-bases/shared-x").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("shared-x")})
    )
    respx.get(f"{BASE_URL}/knowledge-bases/gone-y").mock(
        return_value=httpx.Response(404, json={"success": False, "error": {"message": "nf"}})
    )
    kbs = KBService(client)
    scope = EffectiveScope(kb_ids=frozenset({"kb-a", "shared-x", "gone-y"}))
    visible = await kbs.resolve_visible(scope)
    assert {kb.id for kb in visible} == {"kb-a", "shared-x"}
    assert detail.called  # 白名单内缺失的 id 逐个补详情


@respx.mock
async def test_restricted_scope_never_probes_outside_selector(client):
    # 限定范围 kb-a；入参点名范围外的 kb-x：绝不发详情请求（fail-closed）
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-a")]})
    )
    detail = respx.get(f"{BASE_URL}/knowledge-bases/kb-x").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("kb-x")})
    )
    kbs = KBService(client)
    scope = EffectiveScope(kb_ids=frozenset({"kb-a"}))
    with pytest.raises(ScopeError, match="outside this endpoint's scope"):
        await kbs.resolve_selectors(scope, ["kb-x"])
    assert not detail.called


@respx.mock
async def test_empty_closed_scope_denies_selector_without_detail_probe(client):
    # 空公共书架端点（机器零库）：显式选择器直接 ScopeError，
    # 绝不"懒探测"范围外 id 的详情（旧 unrestricted 通道已删除）
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-a")]})
    )
    detail = respx.get(f"{BASE_URL}/knowledge-bases/shared-x").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("shared-x")})
    )
    kbs = KBService(client)
    scope = EffectiveScope(kb_ids=frozenset())
    with pytest.raises(ScopeError, match="outside this endpoint's scope"):
        await kbs.resolve_selectors(scope, ["shared-x"])
    assert not detail.called


@respx.mock
async def test_selector_matches_case_insensitive_name(client):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1", name="Policy Book"),
        ]})
    )
    kbs = KBService(client)
    scope = EffectiveScope(kb_ids=frozenset({"kb-1"}))
    resolved = await kbs.resolve_selectors(scope, ["policy book"])
    assert resolved[0].id == "kb-1"


@respx.mock
async def test_document_get_404_returns_none(client):
    respx.get(f"{BASE_URL}/knowledge/missing").mock(
        return_value=httpx.Response(404, json={"success": False, "error": {"message": "nf"}})
    )
    assert await DocumentService(client).get("missing") is None


@respx.mock
async def test_document_list_paging_params_and_shape(client):
    route = respx.get(f"{BASE_URL}/knowledge-bases/kb-1/knowledge").mock(
        return_value=httpx.Response(200, json={
            "success": True,
            "data": [{"id": "d1", "title": "T1", "parse_status": "completed",
                      "file_name": "a.pdf", "file_type": "pdf"}],
            "total": 1, "page": 2, "page_size": 50,
        })
    )
    page = await DocumentService(client).list_paged("kb-1", keyword="alpha", page=2, page_size=50)
    assert page.total == 1 and page.page == 2 and page.page_size == 50
    assert page.documents[0]["file_name"] == "a.pdf"
    params = route.calls.last.request.url.params
    assert params["page"] == "2" and params["page_size"] == "50" and params["keyword"] == "alpha"


@respx.mock
async def test_chunks_uses_repeated_chunk_type_params(client):
    route = respx.get(f"{BASE_URL}/chunks/d1").mock(
        return_value=httpx.Response(200, json={
            "success": True,
            "data": [{"id": "c0", "chunk_index": 0, "content": "body", "chunk_type": "text"}],
            "total": 1, "page": 1, "page_size": 20,
        })
    )
    page = await ChunkService(client).list_paged("d1", page=1, page_size=20)
    assert page.total == 1 and page.chunks[0]["id"] == "c0"
    assert route.calls.last.request.url.params.get_list("chunk_type") == ["text", "faq"]


# ===========================================================================
# 3. 工具端到端（真实 HTTP 出站 + FakeGuard）
# ===========================================================================

@respx.mock
async def test_tool_list_knowledge_bases_over_http(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1", caps={"vector": True, "keyword": True, "wiki": True,
                                  "graph": False, "faq": False}),
        ]})
    )
    data = await make_toolset(client).list_knowledge_bases()
    assert data["total"] == 1
    assert data["knowledge_bases"][0]["capabilities"] == ["semantic", "keyword", "wiki"]


@respx.mock
async def test_tool_search_fanout_request_bodies_and_merge(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            kb_json("kb-1"), kb_json("kb-2"),
        ]})
    )
    search = respx.post(url__regex=r".*/hybrid-search$").mock(
        return_value=httpx.Response(200, json={"success": True, "data": []})
    )

    async def _side_effect(request):
        kb_id = request.url.path.split("/")[4]
        rows = [search_row(f"{kb_id}-a", f"hit from {kb_id}", 0.9, kb_id=kb_id,
                           knowledge_id=f"doc-{kb_id}", chunk_index=1)]
        return httpx.Response(200, json={"success": True, "data": rows})

    search.side_effect = _side_effect
    data = await make_toolset(client, kb_scope=("kb-1", "kb-2")).search_knowledge(
        "hit", "hybrid", 10, None, None
    )

    assert data["count"] == 2
    bodies = {call.request.url.path.split("/")[4]: call.request.content for call in search.calls}
    for kb_id, raw in bodies.items():
        payload = json.loads(raw)
        assert payload["query_text"] == "hit"
        assert payload["match_count"] == 30
        assert payload["skip_context_enrichment"] is True
        assert payload["disable_vector_match"] is False
        assert payload["disable_keywords_match"] is False
        # 非文档级检索不显式发阈值，用服务端默认（0.6/0.5）
        assert "vector_threshold" not in payload


@respx.mock
async def test_tool_search_document_targets_ownership_and_zero_thresholds(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]})
    )
    doc_route = respx.get(f"{BASE_URL}/knowledge/d1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": doc_json("d1", "kb-1")})
    )
    search = respx.post(f"{BASE_URL}/knowledge-bases/kb-1/hybrid-search").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": [search_row("a", "hit", 0.5, kb_id="kb-1", knowledge_id="d1")],
        })
    )
    data = await make_toolset(client).search_knowledge("q", "hybrid", 10, None, ["d1"])
    assert doc_route.called and data["count"] == 1
    payload = json.loads(search.calls.last.request.content)
    assert payload["knowledge_ids"] == ["d1"]
    assert payload["vector_threshold"] == 0.0
    assert payload["keyword_threshold"] == 0.0


@respx.mock
async def test_tool_search_document_cross_tenant_denied(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]})
    )
    respx.get(f"{BASE_URL}/knowledge/d9").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": doc_json("d9", "kb-1", tenant=999),
        })
    )
    search = respx.post(url__regex=r".*/hybrid-search$")
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await make_toolset(client).search_knowledge("q", "hybrid", 10, None, ["d9"])
    assert not search.called


@respx.mock
async def test_tool_grep_post_filters_with_regex(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]})
    )
    search = respx.post(f"{BASE_URL}/knowledge-bases/kb-1/hybrid-search").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [
            search_row("a", "ERR 500 boom", 0.9, kb_id="kb-1", chunk_index=0),
            search_row("b", "no code here", 0.8, kb_id="kb-1", chunk_index=1),
        ]})
    )
    data = await make_toolset(client).grep_chunks(r"ERR\s+\d{3}", 10, None)
    assert [r["chunk_id"] for r in data["results"]] == ["a"]
    payload = json.loads(search.calls.last.request.content)
    # 候选走关键词索引；\s/\d 转义按 Go 规则替换为空格后分词，原正则不上线路
    assert payload["disable_vector_match"] is True
    assert payload["query_text"] == "ERR 3"
    assert "\\" not in payload["query_text"]
    # 工具输出里仍保留用户原始正则
    assert data["query"] == r"ERR\s+\d{3}"


@respx.mock
async def test_tool_search_all_kbs_failing_is_not_empty(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]})
    )
    respx.post(f"{BASE_URL}/knowledge-bases/kb-1/hybrid-search").mock(
        return_value=httpx.Response(502, json={
            "success": False, "error": {"message": "upstream down"},
        })
    )
    with pytest.raises(ToolError, match="not evidence that they lack the answer"):
        await make_toolset(client).search_knowledge("q", "hybrid", 10, None, None)


@respx.mock
async def test_tool_list_documents_over_http(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge-bases").mock(
        return_value=httpx.Response(200, json={"success": True, "data": [kb_json("kb-1")]})
    )
    respx.get(f"{BASE_URL}/knowledge-bases/kb-1/knowledge").mock(
        return_value=httpx.Response(200, json={
            "success": True, "data": [{"id": "d1", "title": "T", "parse_status": "ready"}],
            "total": 1, "page": 1, "page_size": 20,
        })
    )
    data = await make_toolset(client).list_documents("kb-1", "", 1, 20)
    assert data["documents"][0]["id"] == "d1"


@respx.mock
async def test_tool_read_document_page_over_http(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge/d1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": doc_json("d1", "kb-1")})
    )
    respx.get(f"{BASE_URL}/knowledge-bases/kb-1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("kb-1")})
    )
    chunks_route = respx.get(f"{BASE_URL}/chunks/d1").mock(
        return_value=httpx.Response(200, json={
            "success": True,
            "data": [{"id": f"c{i}", "chunk_index": i, "content": f"body {i}",
                      "chunk_type": "text", "knowledge_id": "d1",
                      "knowledge_base_id": "kb-1", "start_at": 0, "end_at": 0,
                      "parent_chunk_id": ""} for i in range(20)],
            "total": 45, "page": 1, "page_size": 20,
        })
    )
    data = await make_toolset(client).read_document("d1", 0, 20, "")
    assert data["total_chunks"] == 45 and data["fetched_chunks"] == 20
    assert data["next_offset"] == 20
    assert chunks_route.calls.last.request.url.params.get_list("chunk_type") == ["text", "faq"]


@respx.mock
async def test_tool_read_document_scope_violation_is_fail_closed(client, principal_context):
    # guard 给的范围不含文档所属库：文档详情可以拿（REST 权限内），但侧车自己拒绝
    respx.get(f"{BASE_URL}/knowledge/d1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": doc_json("d1", "kb-1")})
    )
    kb_detail = respx.get(f"{BASE_URL}/knowledge-bases/kb-1")
    guard = FakeGuard(scope=EffectiveScope(kb_ids=frozenset({"kb-other"})))
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await make_toolset(client, guard=guard).read_document("d1", 0, 20, "")
    assert not kb_detail.called  # 范围判定在反查库详情之前


@respx.mock
async def test_tool_read_document_query_mode_scans_and_matches(client, principal_context):
    respx.get(f"{BASE_URL}/knowledge/d1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": doc_json("d1", "kb-1")})
    )
    respx.get(f"{BASE_URL}/knowledge-bases/kb-1").mock(
        return_value=httpx.Response(200, json={"success": True, "data": kb_json("kb-1")})
    )

    def _chunk(cid, index, content):
        return {"id": cid, "chunk_index": index, "content": content, "chunk_type": "text",
                "knowledge_id": "d1", "knowledge_base_id": "kb-1",
                "start_at": 0, "end_at": 0, "parent_chunk_id": ""}

    respx.get(f"{BASE_URL}/chunks/d1").mock(
        return_value=httpx.Response(200, json={
            "success": True,
            "data": [
                _chunk("c0", 0, "intro"),
                _chunk("c1", 1, "needle appears"),
                _chunk("c2", 2, "tail"),
            ],
            "total": 3, "page": 1, "page_size": 100,
        })
    )
    data = await make_toolset(client).read_document("d1", 0, 20, "needle")
    roles = {c["chunk_id"]: c["role"] for c in data["chunks"]}
    assert roles == {"c0": "context_before", "c1": "match", "c2": "context_after"}
    assert data["match_count"] == 1 and data["truncated"] is False
