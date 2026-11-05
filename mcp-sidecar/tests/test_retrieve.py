"""只读工具 1-5 单测：纯函数 + fake 后端编排（真 HTTP 契约见 test_backend_contract）。

覆盖：grep 正则拆词、模式降级、合并去重、入参钳制，以及五个工具经 guard 后的
主流程——多库并发合并、部分失败、文档级越权校验、grep 后置过滤、分页/query 阅读。
"""

from __future__ import annotations

import pytest

from app.backend.client import BackendError
from app.backend.chunks import ChunkPage
from app.backend.documents import DocumentPage
from app.backend.kb import KBSummary, ScopeError
from app.guards import EffectiveScope, REASON_RATE_LIMITED
from app.tools.retrieve import (
    MODE_KEYWORD,
    MODE_SEMANTIC,
    SEARCH_MAX_DOCUMENTS,
    RetrieveToolSet,
    annotate_mode_fallback,
    content_signature,
    dedupe_search_rows,
    effective_modes,
    grep_pattern_and_terms,
    snippet_regex,
    _clamp_int,
)
from fakes import TENANT, FakeGuard, kb_summary as _kb
from mcp.server.mcpserver.exceptions import ToolError


class FakeKBs:
    def __init__(self, visible: list[KBSummary]) -> None:
        self.visible = visible

    async def resolve_visible(self, scope):
        return list(self.visible)

    async def resolve_selectors(self, scope, selectors):
        if not selectors:
            return list(self.visible)
        out: list[KBSummary] = []
        for selector in selectors:
            match = next(
                (k for k in self.visible if k.id == selector or k.name.lower() == selector.lower()),
                None,
            )
            if match is None:
                raise ScopeError(
                    f"knowledge base {selector!r} was not found or is outside this endpoint's scope"
                )
            out.append(match)
        return out

    async def get(self, kb_id):
        return next((k for k in self.visible if k.id == kb_id), None)


class FakeDocs:
    def __init__(self, docs: dict[str, dict] | None = None, pages=None) -> None:
        self.docs = docs or {}
        self.pages = pages or {}

    async def get(self, knowledge_id):
        return self.docs.get(knowledge_id)

    async def list_paged(self, kb_id, *, keyword, page, page_size):
        return self.pages[(kb_id, page, page_size, keyword)]


class FakeChunks:
    def __init__(self, pages: dict[tuple[int, int], ChunkPage]) -> None:
        self.pages = pages
        self.calls: list[tuple[str, int, int]] = []

    async def list_paged(self, knowledge_id, *, page, page_size, chunk_types=("text", "faq")):
        self.calls.append((knowledge_id, page, page_size))
        return self.pages[(page, page_size)]


class FakeSearchClient:
    """按库返回预置行或抛错；记录 hybrid-search 请求体用于断言。"""

    def __init__(self, routes: dict[str, list[dict] | Exception]) -> None:
        self.routes = routes
        self.payloads: dict[str, dict] = {}

    async def post_json(self, path, *, params=None, json_body=None, timeout=30):
        kb_id = path.split("/")[2]
        self.payloads[kb_id] = json_body
        outcome = self.routes[kb_id]
        if isinstance(outcome, Exception):
            raise outcome
        return {"success": True, "data": outcome}


def _row(
    row_id: str,
    content: str,
    score: float,
    *,
    kb_id: str,
    knowledge_id: str = "doc-1",
    chunk_index: int = 0,
    chunk_type: str = "text",
    match_type: str = "hybrid",
    title: str = "Doc",
) -> dict:
    return {
        "id": row_id,
        "content": content,
        "score": score,
        "knowledge_id": knowledge_id,
        "knowledge_base_id": kb_id,
        "chunk_index": chunk_index,
        "chunk_type": chunk_type,
        "match_type": match_type,
        "knowledge_title": title,
    }


def _toolset(
    kbs: list[KBSummary],
    *,
    guard: FakeGuard | None = None,
    docs: FakeDocs | None = None,
    chunks: FakeChunks | None = None,
    search_routes: dict[str, list[dict] | Exception] | None = None,
) -> tuple[RetrieveToolSet, FakeGuard, FakeSearchClient]:
    guard = guard or FakeGuard()
    toolset = RetrieveToolSet(client=None, guard=guard)  # type: ignore[arg-type]
    toolset._kbs = FakeKBs(kbs)
    toolset._docs = docs or FakeDocs()
    toolset._chunks = chunks or FakeChunks({})
    client = FakeSearchClient(search_routes or {})
    toolset._client = client
    return toolset, guard, client


# ===========================================================================
# 纯函数
# ===========================================================================

def test_grep_terms_alternation_and_dedup():
    # \d 这类纯转义替换成空格后分词；大小写不敏感去重保序
    _pattern, terms = grep_pattern_and_terms(r"Error|error|timeout\s+\d{3}")
    assert terms == "Error timeout 3"


def test_grep_invalid_regex_falls_back_to_literal():
    pattern, terms = grep_pattern_and_terms("a(b")
    assert terms == "a b"
    assert pattern.search("a(b") is not None


def test_grep_chinese_terms_kept():
    _pattern, terms = grep_pattern_and_terms("超时|重试")
    assert terms == "超时 重试"


def test_effective_modes_faq_keyword_falls_back_to_semantic():
    faq = _kb("kb-f", kb_type="faq", caps=("semantic",))
    modes = effective_modes([faq], MODE_KEYWORD)
    assert modes["kb-f"][0] == MODE_SEMANTIC and modes["kb-f"][1] is True


def test_effective_modes_vectorless_semantic_falls_back_to_keyword():
    kb = _kb("kb-k", caps=("keyword",))
    modes = effective_modes([kb], MODE_SEMANTIC)
    assert modes["kb-k"][0] == MODE_KEYWORD and modes["kb-k"][1] is True


def test_effective_modes_wiki_only_dropped():
    wiki = _kb("kb-w", caps=("wiki",))
    assert effective_modes([wiki], "hybrid") == {}


def test_annotate_mode_fallback_keys():
    kb_good = _kb("kb-1")
    kb_fb = _kb("kb-2", kb_type="faq", caps=("semantic",))
    modes = effective_modes([kb_good, kb_fb], MODE_KEYWORD)
    data: dict = {}
    annotate_mode_fallback(data, MODE_KEYWORD, modes)
    assert data["requested_mode"] == MODE_KEYWORD
    assert data["mode"] == "mixed" or data["mode"] == MODE_KEYWORD
    assert data["mode_fallbacks"][0]["knowledge_base_id"] == "kb-2"


def test_dedupe_by_id_keeps_higher_score():
    rows = [
        _row("c1", "a", 0.5, kb_id="kb-1"),
        _row("c1", "a", 0.9, kb_id="kb-1"),
    ]
    out = dedupe_search_rows(rows)
    assert len(out) == 1 and out[0]["score"] == 0.9


def test_dedupe_by_doc_position_and_content_signature():
    rows = [
        _row("c1", "same text", 0.8, kb_id="kb-1", knowledge_id="d1", chunk_index=3),
        # 不同 chunk id 但同一文档同一位置
        _row("c2", "other", 0.7, kb_id="kb-1", knowledge_id="d1", chunk_index=3),
        # 内容归一化后相同（大小写/空白差异）
        _row("c3", "  SAME   text ", 0.6, kb_id="kb-2"),
    ]
    out = dedupe_search_rows(rows)
    assert [r["id"] for r in out] == ["c1"]


def test_content_signature_normalizes():
    assert content_signature("Hello  World") == content_signature("hello world")
    assert content_signature("   ") == ""


def test_clamp_int_runtime():
    assert _clamp_int(0, 10, 1, 30) == 10
    assert _clamp_int(-5, 10, 1, 30) == 10
    assert _clamp_int(99, 10, 1, 30) == 30
    assert _clamp_int("x", 10, 1, 30) == 10
    assert _clamp_int(None, 20, 1, 100) == 20


def test_snippet_regex_window():
    text = "x" * 300 + "ERROR 500" + "y" * 300
    import re
    snippet = snippet_regex(text, re.compile("(?i)error"))
    assert "ERROR 500" in snippet and snippet.startswith("...")


# ===========================================================================
# 1. list_knowledge_bases
# ===========================================================================

@pytest.mark.asyncio
async def test_list_knowledge_bases_shape(principal_context):
    toolset, _guard, _client = _toolset([
        _kb("kb-1", name="Policy"),
        _kb("kb-2", name="FAQ", kb_type="faq", caps=("semantic",)),
    ])
    data = await toolset.list_knowledge_bases()
    assert data["total"] == 2
    first = data["knowledge_bases"][0]
    assert set(first.keys()) >= {"id", "name", "type", "capabilities"}
    assert first["capabilities"] == ["semantic", "keyword"]


@pytest.mark.asyncio
async def test_guard_denial_becomes_tool_error(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")], guard=FakeGuard(deny=REASON_RATE_LIMITED))
    with pytest.raises(ToolError, match="rate limit"):
        await toolset.list_knowledge_bases()


@pytest.mark.asyncio
async def test_registered_tool_returns_clean_iserror_without_sdk_prefix(principal_context):
    """闭包必须把 ToolError 转成 isError 结果，避免 SDK 套 "Error executing tool" 前缀。"""
    from mcp.server import MCPServer

    server = MCPServer("test", version="0")
    toolset, _g, _c = _toolset([_kb("kb-1")], guard=FakeGuard(deny=REASON_RATE_LIMITED))
    toolset.register(server)
    result = await server.call_tool("list_knowledge_bases", {})
    assert result.is_error is True
    assert result.content[0].text == "rate limit exceeded for this endpoint, retry shortly"


# ===========================================================================
# 2. search_knowledge
# ===========================================================================

@pytest.mark.asyncio
async def test_search_merges_two_kbs_and_clamps_limit(principal_context):
    toolset, _guard, client = _toolset(
        [_kb("kb-1"), _kb("kb-2")],
        search_routes={
            "kb-1": [_row("a", "alpha", 0.9, kb_id="kb-1")],
            "kb-2": [
                _row("b", "beta", 0.8, kb_id="kb-2", knowledge_id="d2", chunk_index=0),
                _row("c", "gamma", 0.7, kb_id="kb-2", knowledge_id="d2", chunk_index=1),
            ],
        },
    )
    data = await toolset.search_knowledge("alpha beta", "hybrid", 99, None, None)
    # limit 运行时钳到 30；候选池每库固定 30
    assert client.payloads["kb-1"]["match_count"] == 30
    assert client.payloads["kb-1"]["skip_context_enrichment"] is True
    assert [r["chunk_id"] for r in data["results"]] == ["a", "b", "c"]
    assert data["count"] == 3
    assert data["kb_counts"] == {"kb-1": 1, "kb-2": 2}
    assert data["display_type"] == "search_results"
    row = data["results"][0]
    assert set(row.keys()) >= {
        "result_index", "content", "knowledge_id", "knowledge_base_id",
        "knowledge_title", "score", "source_query", "query_type",
        "chunk_id", "chunk_index",
    }


@pytest.mark.asyncio
async def test_search_partial_failure_recorded(principal_context):
    toolset, _g, _c = _toolset(
        [_kb("kb-1"), _kb("kb-2")],
        search_routes={
            "kb-1": [_row("a", "alpha", 0.9, kb_id="kb-1")],
            "kb-2": BackendError("boom", status=502),
        },
    )
    data = await toolset.search_knowledge("q", "hybrid", 10, None, None)
    assert data["count"] == 1
    assert any("kb-2" in f and "boom" in f for f in data["partial_failures"])


@pytest.mark.asyncio
async def test_search_all_failure_is_not_empty_evidence(principal_context):
    toolset, _g, _c = _toolset(
        [_kb("kb-1"), _kb("kb-2")],
        search_routes={
            "kb-1": BackendError("boom", status=500),
            "kb-2": BackendError("deadline", status=504),
        },
    )
    with pytest.raises(ToolError, match="not evidence that they lack the answer"):
        await toolset.search_knowledge("q", "hybrid", 10, None, None)


@pytest.mark.asyncio
async def test_search_wiki_only_kb_errors(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-w", caps=("wiki",))])
    with pytest.raises(ToolError, match="semantic or keyword retrieval"):
        await toolset.search_knowledge("q", "hybrid", 10, None, None)


@pytest.mark.asyncio
async def test_search_selector_outside_scope_errors(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await toolset.search_knowledge("q", "hybrid", 10, ["kb-x"], None)


@pytest.mark.asyncio
async def test_search_mode_flags_in_payload(principal_context):
    toolset, _g, client = _toolset(
        [_kb("kb-1")],
        search_routes={"kb-1": []},
    )
    await toolset.search_knowledge("q", "keyword", 10, None, None)
    assert client.payloads["kb-1"]["disable_vector_match"] is True
    assert client.payloads["kb-1"]["disable_keywords_match"] is False


@pytest.mark.asyncio
async def test_search_empty_query_rejected(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="query is required"):
        await toolset.search_knowledge("   ", "hybrid", 10, None, None)


# ---- 文档级 knowledge_ids ----

def _doc(doc_id: str, kb_id: str, *, tenant: int = TENANT, title: str = "Doc") -> dict:
    return {
        "id": doc_id,
        "title": title,
        "knowledge_base_id": kb_id,
        "tenant_id": tenant,
        "parse_status": "completed",
    }


@pytest.mark.asyncio
async def test_search_document_targets_relax_thresholds(principal_context):
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, client = _toolset(
        [_kb("kb-1")],
        docs=docs,
        search_routes={"kb-1": [_row("a", "hit", 0.5, kb_id="kb-1", knowledge_id="d1")]},
    )
    data = await toolset.search_knowledge("q", "hybrid", 10, None, ["d1"])
    assert client.payloads["kb-1"]["knowledge_ids"] == ["d1"]
    assert client.payloads["kb-1"]["vector_threshold"] == 0.0
    assert client.payloads["kb-1"]["keyword_threshold"] == 0.0
    assert data["count"] == 1


@pytest.mark.asyncio
async def test_search_document_in_other_kb_denied(principal_context):
    # 端点范围只有 kb-1；文档属于 kb-2（伪造属主文档防护）
    scope = EffectiveScope(kb_ids=frozenset({"kb-1"}))
    docs = FakeDocs({"d9": _doc("d9", "kb-2")})
    toolset, _g, _c = _toolset(
        [_kb("kb-1")], guard=FakeGuard(scope), docs=docs,
    )
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await toolset.search_knowledge("q", "hybrid", 10, None, ["d9"])


@pytest.mark.asyncio
async def test_search_document_cross_tenant_denied(principal_context):
    docs = FakeDocs({"d9": _doc("d9", "kb-1", tenant=999)})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs)
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await toolset.search_knowledge("q", "hybrid", 10, None, ["d9"])


@pytest.mark.asyncio
async def test_search_missing_document_not_found(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=FakeDocs({}))
    with pytest.raises(ToolError, match='document "d-missing" was not found'):
        await toolset.search_knowledge("q", "hybrid", 10, None, ["d-missing"])


@pytest.mark.asyncio
async def test_search_too_many_documents_rejected(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match=f"at most {SEARCH_MAX_DOCUMENTS}"):
        await toolset.search_knowledge("q", "hybrid", 10, None, ["d"] * 101)


@pytest.mark.asyncio
async def test_search_empty_document_list_rejected(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="at least one document id"):
        await toolset.search_knowledge("q", "hybrid", 10, None, [])


# ---- FAQ 行契约 ----

@pytest.mark.asyncio
async def test_search_faq_row_uses_faq_id_and_qa_fields(principal_context):
    import json
    faq_meta = {
        "standard_question": "怎么重置密码？",
        "similar_questions": ["忘记密码怎么办"] + [f"问{i}" for i in range(6)],
        "answers": ["在设置里重置。"],
    }
    row = _row("f1", "", 0.9, kb_id="kb-f", chunk_index=0, match_type="vector")
    row["chunk_metadata"] = json.dumps(faq_meta, ensure_ascii=False)
    toolset, _g, _c = _toolset(
        [_kb("kb-f", kb_type="faq", caps=("semantic",))],
        search_routes={"kb-f": [row]},
    )
    data = await toolset.search_knowledge("忘记密码", "keyword", 10, None, None)
    # keyword 在 FAQ 库自动降级 semantic
    assert data["mode_fallbacks"][0]["knowledge_base_id"] == "kb-f"
    out = data["results"][0]
    assert out["faq_id"] == "f1" and out["index"] == 0
    assert "chunk_id" not in out and "chunk_index" not in out
    assert out["faq_standard_question"] == "怎么重置密码？"
    assert out["faq_answers"] == ["在设置里重置。"]
    assert len(out["faq_similar_questions"]) == 5
    assert out["faq_similar_questions_omitted"] == 2
    assert out["match_snippet"].startswith("Q: ")
    assert "A: 在设置里重置。" in out["match_snippet"]


@pytest.mark.asyncio
async def test_read_document_faq_chunk_normalized(principal_context):
    import json
    faq = _chunk("f0", 0, "", ctype="faq")
    faq["metadata"] = json.dumps({
        "standard_question": "Q?", "similar_questions": ["S?"], "answers": ["A."],
    })
    chunks = FakeChunks({(1, 20): ChunkPage((faq,), 1, 1, 20)})
    docs = FakeDocs({"d1": _doc("d1", "kb-f")})
    toolset, _g, _c = _toolset(
        [_kb("kb-f", kb_type="faq", caps=("semantic",))], docs=docs, chunks=chunks,
    )
    data = await toolset.read_document("d1", 0, 20, "")
    row = data["chunks"][0]
    assert row["faq_id"] == "f0" and row["index"] == 0
    assert "chunk_id" not in row and "chunk_index" not in row
    assert row["faq_question"] == "Q?"
    assert row["faq_similar_questions"] == ["S?"]
    assert row["faq_answers"] == ["A."]


# ===========================================================================
# 3. grep_chunks
# ===========================================================================

@pytest.mark.asyncio
async def test_grep_post_filters_nonmatching_candidates(principal_context):
    # 关键词候选池拿回 3 行，只有 1 行真正匹配正则
    toolset, _g, client = _toolset(
        [_kb("kb-1")],
        search_routes={"kb-1": [
            _row("a", "ERROR 500 exploded", 0.9, kb_id="kb-1", chunk_index=0),
            _row("b", "errors were considered", 0.8, kb_id="kb-1", chunk_index=1),
            _row("c", "no digits here", 0.7, kb_id="kb-1", chunk_index=2),
        ]},
    )
    data = await toolset.grep_chunks(r"ERROR\s+\d{3}", 10, None)
    assert [r["chunk_id"] for r in data["results"]] == ["a"]
    # 关键词查询是拆出的词面量，走关键词索引
    assert client.payloads["kb-1"]["disable_vector_match"] is True
    assert "ERROR" in client.payloads["kb-1"]["query_text"]
    assert data["query"] == r"ERROR\s+\d{3}"
    assert data["results"][0]["match_snippet"]


@pytest.mark.asyncio
async def test_grep_pattern_without_literal_terms_rejected(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    # \d+ 合法但没有任何可喂给关键词索引的词面量
    with pytest.raises(ToolError, match="no literal text"):
        await toolset.grep_chunks(r"\d+", 10, None)


# ===========================================================================
# 4. list_documents
# ===========================================================================

@pytest.mark.asyncio
async def test_list_documents_paging_and_shape(principal_context):
    page = DocumentPage(
        documents=(
            {"id": "d1", "title": "T1", "parse_status": "completed",
             "created_at": "", "updated_at": ""},
        ),
        total=1, page=1, page_size=20,
    )
    docs = FakeDocs(pages={("kb-1", 1, 20, ""): page})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs)
    data = await toolset.list_documents("kb-1", "", 1, 20)
    assert data == {
        "knowledge_base_id": "kb-1",
        "total": 1, "page": 1, "page_size": 20,
        "documents": list(page.documents),
    }


@pytest.mark.asyncio
async def test_list_documents_clamps_page_size(principal_context):
    page = DocumentPage(documents=(), total=0, page=1, page_size=100)
    docs = FakeDocs(pages={("kb-1", 1, 100, ""): page})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs)
    data = await toolset.list_documents("kb-1", "", 0, 999)
    assert data["page_size"] == 100


@pytest.mark.asyncio
async def test_list_documents_requires_selector(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="knowledge_base_id is required"):
        await toolset.list_documents("  ", "", 1, 20)


# ===========================================================================
# 5. read_document
# ===========================================================================

def _chunk(cid: str, index: int, content: str, *, ctype: str = "text") -> dict:
    return {
        "id": cid, "chunk_index": index, "content": content, "chunk_type": ctype,
        "knowledge_id": "d1", "knowledge_base_id": "kb-1",
        "start_at": 0, "end_at": 0, "parent_chunk_id": "",
    }


@pytest.mark.asyncio
async def test_read_document_page_with_next_offset(principal_context):
    chunks = FakeChunks({
        (1, 20): ChunkPage(tuple(_chunk(f"c{i}", i, f"body {i}") for i in range(20)), 45, 1, 20),
    })
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs, chunks=chunks)
    data = await toolset.read_document("d1", 0, 20, "")
    assert data["display_type"] == "knowledge_chunks_list"
    assert data["total_chunks"] == 45 and data["fetched_chunks"] == 20
    assert data["next_offset"] == 20
    assert data["document"]["id"] == "d1"
    assert data["knowledge_base"] == {"id": "kb-1", "name": "KB kb-1"}
    assert data["chunks"][0]["seq"] == 1
    assert data["chunks"][0]["chunk_id"] == "c0"


@pytest.mark.asyncio
async def test_read_document_last_page_has_no_next_offset(principal_context):
    chunks = FakeChunks({
        (3, 20): ChunkPage(tuple(_chunk(f"c{i}", i, "x") for i in range(40, 45)), 45, 3, 20),
    })
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs, chunks=chunks)
    data = await toolset.read_document("d1", 40, 20, "")
    assert "next_offset" not in data
    assert data["fetched_chunks"] == 5


@pytest.mark.asyncio
async def test_read_document_out_of_range_message(principal_context):
    chunks = FakeChunks({
        (6, 20): ChunkPage((), 45, 6, 20),
    })
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs, chunks=chunks)
    with pytest.raises(ToolError, match="offset 100 is out of range: document has 45 chunks"):
        await toolset.read_document("d1", 100, 20, "")


@pytest.mark.asyncio
async def test_read_document_missing_doc_not_found(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=FakeDocs({}))
    with pytest.raises(ToolError, match='document "d-x" was not found'):
        await toolset.read_document("d-x", 0, 20, "")


@pytest.mark.asyncio
async def test_read_document_scope_violation_denied(principal_context):
    scope = EffectiveScope(kb_ids=frozenset({"kb-other"}))
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, _c = _toolset([_kb("kb-1")], guard=FakeGuard(scope), docs=docs)
    with pytest.raises(ToolError, match="outside this endpoint's scope"):
        await toolset.read_document("d1", 0, 20, "")


@pytest.mark.asyncio
async def test_read_document_query_mode_match_with_context(principal_context):
    rows = [_chunk("c0", 0, "intro paragraph"), _chunk("c1", 1, "needle appears here"),
            _chunk("c2", 2, "following text"), _chunk("c3", 3, "unrelated tail")]
    chunks = FakeChunks({(1, 100): ChunkPage(tuple(rows), 4, 1, 100)})
    docs = FakeDocs({"d1": _doc("d1", "kb-1")})
    toolset, _g, _c = _toolset([_kb("kb-1")], docs=docs, chunks=chunks)
    data = await toolset.read_document("d1", 0, 20, "needle")
    assert data["query"] == "needle"
    assert data["match_count"] == 1
    assert data["truncated"] is False
    assert "next_offset" not in data
    roles = {c["chunk_id"]: c["role"] for c in data["chunks"]}
    assert roles == {"c0": "context_before", "c1": "match", "c2": "context_after"}
    match_row = next(c for c in data["chunks"] if c["role"] == "match")
    assert "needle" in match_row["match_snippet"]


@pytest.mark.asyncio
async def test_read_document_requires_doc_id(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="knowledge_id is required"):
        await toolset.read_document("  ", 0, 20, "")


# ===========================================================================
# 冻结文案回归（与 Go 逐字对齐）
# ===========================================================================

@pytest.mark.asyncio
async def test_search_invalid_mode_echoes_frozen_text(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match='^mode must be one of hybrid, semantic, keyword \\(got "bogus"\\)$'):
        await toolset.search_knowledge("q", "bogus", 10, None, None)


@pytest.mark.asyncio
async def test_search_blank_doc_id_frozen_text(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="^knowledge_ids must not contain blank document ids$"):
        await toolset.search_knowledge("q", "hybrid", 10, None, ["  "])


@pytest.mark.asyncio
async def test_search_blank_selector_frozen_text(principal_context):
    toolset, _g, _c = _toolset([_kb("kb-1")])
    with pytest.raises(ToolError, match="^knowledge_base_ids must not contain empty selectors$"):
        await toolset.search_knowledge("q", "hybrid", 10, ["  "], None)
