"""wiki 三工具单测：纯渲染函数 + fake 后端编排（真 HTTP 契约见 test_wiki_contract）。

覆盖（对照 internal/agent/tools/wiki_tools.go）：
  - POSIX 字面转义 / 正则三态、片段窗口、70/30 截断、水位填充预算；
  - index 概览渲染、单页模板、来源标签、邻居摘要前 20 条上限；
  - 逐库扇出、部分失败 <errors>、全失败 isError、跨库消歧、index 页特判；
  - guard 拒绝、无 wiki 库、冻结英文错误文案。
"""

from __future__ import annotations

import pytest

from app.backend.client import BackendError
from app.backend.kb import KBSummary, ScopeError
from app.tools import wiki as W
from app.tools.wiki import WikiToolSet
from fakes import FakeGuard, kb_summary as _kb
from mcp.server.mcpserver.exceptions import ToolError


# ---------------------------------------------------------------------------
# fake 后端
# ---------------------------------------------------------------------------

class FakeKBs:
    def __init__(self, visible: list[KBSummary]) -> None:
        self.visible = visible

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


class FakeWiki:
    """按 (kb, query) / (kb, slug) / (kb, limit) 预置结果；异常原样抛。"""

    def __init__(self) -> None:
        self.search_routes: dict[str, dict] = {}
        self.pages: dict[str, dict[str, object]] = {}
        self.indexes: dict[str, dict[int, dict]] = {}
        self.search_calls: list[tuple[str, str, int]] = []
        self.page_calls: list[tuple[str, str]] = []
        self.index_calls: list[tuple[str, int]] = []

    def add_search(self, kb_id: str, query: str, pages: list[dict] | Exception, *, limit: int = 10) -> None:
        self.search_routes.setdefault(kb_id, {})[(query, limit)] = pages

    def add_page(self, kb_id: str, slug: str, page: dict | None | Exception) -> None:
        self.pages.setdefault(kb_id, {})[slug] = page

    def add_index(self, kb_id: str, view: dict, *, limit: int = 20) -> None:
        self.indexes.setdefault(kb_id, {})[limit] = view

    async def search_pages(self, kb_id, query, limit):
        self.search_calls.append((kb_id, query, limit))
        outcome = self.search_routes.get(kb_id, {}).get((query, limit), [])
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    async def get_page(self, kb_id, slug):
        self.page_calls.append((kb_id, slug))
        outcome = self.pages.get(kb_id, {}).get(slug)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    async def get_index(self, kb_id, limit):
        self.index_calls.append((kb_id, limit))
        outcome = self.indexes[kb_id][limit]
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


def _toolset(kbs: list[KBSummary], *, guard: FakeGuard | None = None) -> tuple[WikiToolSet, FakeGuard, FakeWiki]:
    guard = guard or FakeGuard()
    toolset = WikiToolSet(client=None, guard=guard)  # type: ignore[arg-type]
    fake_wiki = FakeWiki()
    toolset._kbs = FakeKBs(kbs)
    toolset._wiki = fake_wiki
    return toolset, guard, fake_wiki


def _page(
    slug: str,
    *,
    kb_id: str = "kb-1",
    title: str = "Title",
    page_type: str = "concept",
    summary: str = "a summary",
    content: str = "body content",
    aliases: list[str] | None = None,
    out_links: list[str] | None = None,
    in_links: list[str] | None = None,
    source_refs: list[str] | None = None,
) -> dict:
    return {
        "id": "pg-" + slug,
        "slug": slug,
        "title": title,
        "knowledge_base_id": kb_id,
        "page_type": page_type,
        "summary": summary,
        "content": content,
        "aliases": aliases or [],
        "out_links": out_links or [],
        "in_links": in_links or [],
        "source_refs": source_refs or [],
    }


def _text(result) -> str:
    return result.content[0].text


# ===========================================================================
# 纯函数
# ===========================================================================

def test_posix_literal_escapes_meta_only():
    assert W.posix_literal("C++ engine? (v2)") == r"C\+\+ engine\? \(v2\)"
    assert W.posix_literal("plain文本123") == "plain文本123"
    assert W.posix_literal("a\\b") == r"a\\b"


def test_validate_regex_rejects_bad_pattern():
    W.validate_regex("a|b")
    with pytest.raises(ToolError, match="invalid regular expression"):
        W.validate_regex("(unclosed")


def test_extract_snippet_window_and_newlines():
    content = "x" * 100 + "NEEDLE" + "y" * 100
    out = W.extract_snippet(content, "needle")
    assert out.startswith("... ") and out.endswith(" ...")
    assert "NEEDLE" in out
    assert "x" * 61 not in out  # 前文最多 60 字符
    assert "\n" not in out


def test_extract_snippet_invalid_regex_empty():
    assert W.extract_snippet("content", "(?P<") == ""
    assert W.extract_snippet("", "x") == ""


def test_truncate_tool_output_head_tail_marker():
    out = W.truncate_tool_output("abcdefghij" * 300, 500)
    assert "[output truncated:" in out
    head = out.split("... [output truncated:")[0].rstrip("\n")
    assert len(head) == int((500 - 200) * 0.7)
    # 结尾是保留的尾部原文（最后 90 字符），省略号只在中间标记里
    source = "abcdefghij" * 300
    assert out.endswith(source[-90:])
    assert "first 210 + last 90" in out


def test_split_budget_fairly_water_filling():
    # 小条目拿全长并让渡余量给大条目
    caps = W.split_budget_fairly(10, [2, 100])
    assert caps == [2, 8]
    assert W.split_budget_fairly(0, [1, 2]) == [0, 0]
    # 总量充裕：各取所需
    caps = W.split_budget_fairly(1000, [3, 5])
    assert caps == [3, 5]


def test_render_index_overview_clips_legacy_directory():
    view = {"intro": "Wiki intro\n## legacy inline dir\nshould be clipped", "groups": []}
    out = W.render_index_overview(view)
    assert "Wiki intro" in out and "legacy inline dir" not in out
    assert "No wiki pages yet" in out


def test_render_index_overview_groups_and_footer():
    view = {
        "intro": "Intro",
        "groups": [
            {"type": "summary", "total": 2, "items": [
                {"slug": "s1", "title": "S1", "summary": "sum1"},
                {"slug": "s2", "title": "", "summary": ""},
            ]},
            {"type": "weird", "total": 1, "items": [{"slug": "w1", "title": "W1", "summary": ""}]},
            {"type": "entity", "total": 0, "items": []},
        ],
    }
    out = W.render_index_overview(view)
    assert "## Summary (2)" in out
    assert "[[s1|S1]] — sum1" in out
    assert "[[s2|s2]]\n" in out           # 空标题回落 slug
    assert "## weird (1)" in out           # 未知类型原样
    assert out.strip().endswith("read a specific slug directly._")


def test_render_index_overview_pagination_header():
    view = {"intro": "", "groups": [
        {"type": "entity", "total": 50, "items": [{"slug": "e1", "title": "E1", "summary": ""}]},
    ]}
    out = W.render_index_overview(view)
    assert "## Entity (50 total, showing top 1)" in out


def test_build_sources_both_shapes():
    page = {"source_refs": ["uuid-1", "uuid-2|Doc Title"]}
    assert W.build_sources(page) == [
        '<source knowledge_id="uuid-1"/>',
        '<source knowledge_id="uuid-2">Doc Title</source>',
    ]


def test_render_page_template():
    text = W.render_page("kb-1", _page("concept/rag", aliases=["RAG"], source_refs=["u1"]),
                         out_links=["[[x]]"], in_links=["(none)"],
                         sources=['<source knowledge_id="u1"/>'], body="BODY")
    assert text.startswith("<wiki_page>\n<metadata>")
    assert "<knowledge_base_id>kb-1</knowledge_base_id>" in text
    assert "<link>[[concept/rag|Title]]</link>" in text
    assert "<type>concept</type>" in text
    assert "<aliases>RAG</aliases>" in text
    assert "<links_to>[[x]]</links_to>" in text
    assert '<source knowledge_id="u1"/>' in text
    assert text.endswith("<content>\nBODY\n</content>\n</wiki_page>")


def test_render_pages_within_budget_fit_all():
    page = _page("p1", content="short")
    pending = [{"page": page, "kb_id": "kb-1", "out_links": [], "in_links": ["(none)"],
                "sources": [], "body": "short"}]
    out, truncated, omitted = W.render_pages_within_budget(pending, W.WIKI_OUTPUT_BUDGET)
    assert "<wiki_page>" in out and truncated is None and omitted is None


def test_render_pages_within_budget_truncates_big_body():
    page = _page("p1", content="字" * 30000)
    pending = [{"page": page, "kb_id": "kb-1", "out_links": [], "in_links": ["(none)"],
                "sources": [], "body": "字" * 30000}]
    out, truncated, omitted = W.render_pages_within_budget(pending, W.WIKI_OUTPUT_BUDGET)
    assert truncated == ["p1"]
    assert "[output truncated:" in out


def test_render_pages_within_budget_omits_trailing_pages():
    # 3 个超大页：固定开销都放不下时，尾部页进 omitted
    pending = []
    for slug in ("p1", "p2", "p3"):
        page = _page(slug, title="T" * 20000, summary="S" * 20000, content="x" * 20000)
        pending.append({"page": page, "kb_id": "kb-1", "out_links": ["(none)"],
                        "in_links": ["(none)"], "sources": [], "body": "x" * 20000})
    out, truncated, omitted = W.render_pages_within_budget(pending, W.WIKI_OUTPUT_BUDGET)
    assert omitted and "p3" in omitted
    assert "<omitted_pages" not in out  # 拖车在工具层拼接，纯函数只管返回 slug 列表


def test_go_json_indented_escapes_amp_lt_gt():
    text = W._go_json_indented({"intro": "a & b < c > d"})
    assert "\\u0026" in text and "\\u003c" in text and "\\u003e" in text
    assert text.startswith("{\n  ")


# ===========================================================================
# wiki_search
# ===========================================================================

@pytest.mark.usefixtures("principal_context")
async def test_search_renders_block_and_found_kbs():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    page = _page("concept/rag", title="RAG", aliases=["Retrieval"],
                 out_links=["concept/embedding"], content="RAG means retrieval augmented")
    fake.add_search("kb-1", "rag", [page])
    result = await toolset.wiki_search("rag", None, None, 10)

    text = _text(result)
    assert '<search_results count="1" query="rag">' in text
    assert "<knowledge_base_id>kb-1</knowledge_base_id>" in text
    assert "<link>[[concept/rag|RAG]]</link>" in text
    assert "<aliases>Retrieval</aliases>" in text
    assert "<match_snippet>" in text
    assert text.endswith("</search_results>")
    assert result.structured_content["found_kbs"]["concept/rag"] == ["kb-1"]
    # 邻居 slug 也登记到 found_kbs
    assert result.structured_content["found_kbs"]["concept/embedding"] == ["kb-1"]
    assert result.structured_content["content"] == text
    assert fake.search_calls == [("kb-1", "rag", 10)]


@pytest.mark.usefixtures("principal_context")
async def test_search_zero_hits_self_closing_tag():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_search("kb-1", "nope", [])
    result = await toolset.wiki_search("nope", None, None, 10)
    assert _text(result) == '<search_results count="0" query="nope" />'


@pytest.mark.usefixtures("principal_context")
async def test_search_partial_failure_appends_errors():
    kb1, kb2 = _kb("kb-1", caps=("wiki",)), _kb("kb-2", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb1, kb2])
    fake.add_search("kb-1", "rag", BackendError("db regex error", status=500))
    fake.add_search("kb-2", "rag", [_page("p1", kb_id="kb-2")])
    result = await toolset.wiki_search("rag", None, None, 10)

    text = _text(result)
    assert result.is_error is False
    assert "<errors>" in text
    assert 'Wiki search "rag" failed in KB kb-1: db regex error' in text
    assert "kb-2" in text


@pytest.mark.usefixtures("principal_context")
async def test_search_all_failed_is_error():
    kb1, kb2 = _kb("kb-1", caps=("wiki",)), _kb("kb-2", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb1, kb2])
    fake.add_search("kb-1", "rag", BackendError("boom", status=500))
    fake.add_search("kb-2", "rag", BackendError("boom2", status=500))
    with pytest.raises(ToolError, match='failed in KB kb-1.*failed in KB kb-2'):
        await toolset.wiki_search("rag", None, None, 10)


@pytest.mark.usefixtures("principal_context")
async def test_search_regex_false_sends_escaped_literal():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_search("kb-1", r"C\+\+", [_page("p1")], limit=10)
    result = await toolset.wiki_search("C++", False, None, 10)
    assert result.structured_content["found_kbs"]["p1"] == ["kb-1"]
    assert fake.search_calls[0][1] == r"C\+\+"


@pytest.mark.usefixtures("principal_context")
async def test_search_regex_true_invalid_fails_before_http():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    with pytest.raises(ToolError, match="invalid regular expression"):
        await toolset.wiki_search("(unclosed", True, None, 10)
    assert fake.search_calls == []


@pytest.mark.usefixtures("principal_context")
async def test_search_requires_query_and_clamps_limit():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    with pytest.raises(ToolError, match="query is required"):
        await toolset.wiki_search("   ", None, None, 10)
    fake.add_search("kb-1", "x", [])
    await toolset.wiki_search("x", None, None, 999)
    assert fake.search_calls[0][2] == 50
    await toolset.wiki_search("x", None, None, 0)
    assert fake.search_calls[1][2] == 10


@pytest.mark.usefixtures("principal_context")
async def test_search_no_wiki_in_scope():
    kb = _kb("kb-1", caps=("semantic", "keyword"))
    toolset, _guard, _fake = _toolset([kb])
    with pytest.raises(ToolError, match='capabilities include "wiki"'):
        await toolset.wiki_search("x", None, None, 10)


@pytest.mark.usefixtures("principal_context")
async def test_search_guard_denied():
    kb = _kb("kb-1", caps=("wiki",))
    from app.guards import REASON_TOOL_DISABLED

    guard = FakeGuard(deny=REASON_TOOL_DISABLED)
    toolset, _g, fake = _toolset([kb], guard=guard)
    with pytest.raises(ToolError, match="not enabled on this endpoint"):
        await toolset.wiki_search("x", None, None, 10)
    assert fake.search_calls == []


# ===========================================================================
# wiki_read_page
# ===========================================================================

@pytest.mark.usefixtures("principal_context")
async def test_read_page_full_render_with_link_summaries():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_page("kb-1", "concept/rag", _page(
        "concept/rag", source_refs=["u1", "u2|Doc"], out_links=["n1"], in_links=["n2"],
    ))
    fake.add_page("kb-1", "n1", _page("n1", summary="neighbour one summary"))
    fake.add_page("kb-1", "n2", _page("n2", summary=""))  # 空摘要 → 裸 slug

    result = await toolset.wiki_read_page("concept/rag")
    text = _text(result)
    assert "<links_to>[[n1]] (neighbour one summary)</links_to>" in text
    assert "<linked_from>[[n2]]</linked_from>" in text
    assert '<source knowledge_id="u1"/>' in text
    assert '<source knowledge_id="u2">Doc</source>' in text
    data = result.structured_content
    assert data["found_kbs"]["concept/rag"] == ["kb-1"]
    assert data["ambiguous_slugs"] == {}
    assert data["truncated_slugs"] is None and data["omitted_slugs"] is None
    assert data["content"] == text


@pytest.mark.usefixtures("principal_context")
async def test_read_page_empty_link_sections_say_none():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_page("kb-1", "p1", _page("p1"))
    result = await toolset.wiki_read_page("p1")
    text = _text(result)
    assert "<links_to>(none)</links_to>" in text
    assert "<linked_from>(none)</linked_from>" in text


@pytest.mark.usefixtures("principal_context")
async def test_read_page_link_summary_cap_is_20():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    neighbours = [f"n{i}" for i in range(25)]
    page = _page("hub", out_links=neighbours)
    fake.add_page("kb-1", "hub", page)
    for n in neighbours:
        fake.add_page("kb-1", n, _page(n, summary="S"))
    result = await toolset.wiki_read_page("hub")
    text = _text(result)
    assert text.count("(S)") == 20
    assert "[[n24]]" in text and "(S)" not in text.split("[[n24]]")[1]
    # 前 20 条之外不再请求邻居页
    assert ("kb-1", "n24") not in fake.page_calls


@pytest.mark.usefixtures("principal_context")
async def test_read_page_not_found_frozen_text():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_page("kb-1", "ghost", None)
    with pytest.raises(ToolError, match="Wiki page 'ghost' not found"):
        await toolset.wiki_read_page("ghost")


@pytest.mark.usefixtures("principal_context")
async def test_read_page_upstream_error_frozen_text():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_page("kb-1", "p1", BackendError("internal failure", status=500))
    with pytest.raises(ToolError, match="Failed to read Wiki page 'p1' in KB kb-1: internal failure"):
        await toolset.wiki_read_page("p1")


@pytest.mark.usefixtures("principal_context")
async def test_read_page_ambiguous_renders_both():
    kb1, kb2 = _kb("kb-1", caps=("wiki",)), _kb("kb-2", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb1, kb2])
    fake.add_page("kb-1", "shared", _page("shared", kb_id="kb-1", title="A"))
    fake.add_page("kb-2", "shared", _page("shared", kb_id="kb-2", title="B"))
    result = await toolset.wiki_read_page("shared")
    text = _text(result)
    assert text.count("<wiki_page>") == 2
    assert "kb-1" in text and "kb-2" in text
    assert result.structured_content["ambiguous_slugs"]["shared"] == ["kb-1", "kb-2"]


@pytest.mark.usefixtures("principal_context")
async def test_read_page_partial_upstream_failure_keeps_hit_with_errors():
    kb1, kb2 = _kb("kb-1", caps=("wiki",)), _kb("kb-2", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb1, kb2])
    fake.add_page("kb-1", "p1", BackendError("boom", status=500))
    fake.add_page("kb-2", "p1", _page("p1", kb_id="kb-2"))
    result = await toolset.wiki_read_page("p1")
    text = _text(result)
    assert "<wiki_page>" in text
    assert "<errors>" in text and "Failed to read Wiki page 'p1' in KB kb-1: boom" in text


@pytest.mark.usefixtures("principal_context")
async def test_read_page_index_type_uses_overview():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_page("kb-1", "index", _page(
        "index", page_type="index", content="legacy intro only",
    ))
    fake.add_index("kb-1", {"intro": "Generated intro", "groups": [
        {"type": "summary", "total": 1, "items": [{"slug": "s1", "title": "S1", "summary": "z"}]},
    ]}, limit=20)
    result = await toolset.wiki_read_page("index")
    text = _text(result)
    assert "Generated intro" in text and "## Summary (1)" in text
    assert fake.index_calls == [("kb-1", 20)]


@pytest.mark.usefixtures("principal_context")
async def test_read_page_requires_slug():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _g, _fake = _toolset([kb])
    with pytest.raises(ToolError, match="slug is required"):
        await toolset.wiki_read_page("  ")


@pytest.mark.usefixtures("principal_context")
async def test_read_page_selector_restricts_scope():
    kb1, kb2 = _kb("kb-1", caps=("wiki",)), _kb("kb-2", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb1, kb2])
    fake.add_page("kb-2", "p1", _page("p1", kb_id="kb-2"))
    result = await toolset.wiki_read_page("p1", knowledge_base_id="kb-2")
    assert _text(result).count("<wiki_page>") == 1
    # kb-1 未被查询
    assert all(kb_id == "kb-2" for kb_id, _ in fake.page_calls)


# ===========================================================================
# wiki_index
# ===========================================================================

@pytest.mark.usefixtures("principal_context")
async def test_index_returns_indented_json_and_structured():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    view = {"intro": "Intro", "version": 3, "groups": [
        {"type": "entity", "total": 1, "items": [{"slug": "e1", "title": "E1", "summary": ""}],
         "next_cursor": ""},
    ]}
    fake.add_index("kb-1", view, limit=50)
    result = await toolset.wiki_index("kb-1", 50)

    text = _text(result)
    assert '"knowledge_base_id": "kb-1"' in text
    assert '"intro": "Intro"' in text
    assert '"groups"' in text
    data = result.structured_content
    assert data["knowledge_base_id"] == "kb-1"
    assert data["groups"][0]["type"] == "entity"
    # 与 Go jsonResult 一致：structured 里不复制 content
    assert "content" not in data
    assert fake.index_calls == [("kb-1", 50)]


@pytest.mark.usefixtures("principal_context")
async def test_index_requires_selector_and_clamps():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    with pytest.raises(ToolError, match="knowledge_base_id is required"):
        await toolset.wiki_index("", 50)
    fake.add_index("kb-1", {"intro": "", "groups": []}, limit=200)
    fake.add_index("kb-1", {"intro": "", "groups": []}, limit=50)
    await toolset.wiki_index("kb-1", 9999)
    assert fake.index_calls[-1][1] == 200
    await toolset.wiki_index("kb-1", 0)
    assert fake.index_calls[-1][1] == 50


@pytest.mark.usefixtures("principal_context")
async def test_index_upstream_failure_frozen_prefix():
    kb = _kb("kb-1", caps=("wiki",))
    toolset, _guard, fake = _toolset([kb])
    fake.add_index("kb-1", BackendError("db down", status=500), limit=50)
    with pytest.raises(ToolError, match="failed to load wiki index: db down"):
        await toolset.wiki_index("kb-1", 50)
