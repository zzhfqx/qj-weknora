"""Wiki 三工具（契约 v1，名字/参数/描述/输出与内置 Go MCP 冻结一致）。

  6. wiki_search     逐库扇出 GET wiki/search，渲染 <search_results>
  7. wiki_read_page  跨库消歧读页，渲染 <wiki_page>，24000 rune 输出预算
  8. wiki_index      单库目录，JSON 文本 + structuredContent

渲染逻辑逐行移植 internal/agent/tools/wiki_tools.go（片段、链接摘要、
index 页特殊处理、splitBudgetFairly 水位填充、70/30 截断）。

与 Go 版的两处有意偏差（无状态侧车无法复制工具实例内会话状态）：
  1. 链接/搜索的 "(summary omitted, already seen)" 去重不做，每次都给摘要；
  2. WikiRouteResolver 的"出处优先"排序不做，命中一律按 scope 库顺序排列。
其余文案、标签、预算算法保持一致。

regex 死参数按需求 §5 修正：Go agent 工具实际忽略 regex（恒为正则语义），
侧车实现承诺语义——省略=原样；false=POSIX 转义按字面；true=先校验正则。
"""

from __future__ import annotations

import json
import logging
import re
from typing import Annotated

from mcp.types import CallToolResult, TextContent
from pydantic import Field

from app.auth.middleware import current_principal
from app.backend.client import BackendError
from app.backend.kb import KBSummary, KBService, ScopeError
from app.backend.wiki import WikiService
from app.tools.retrieve import (
    READ_ONLY_ANNOTATIONS,
    _DENY_MESSAGES,
    ToolError,
    _clean_selectors,
    _go_json_indented,
    _tool_error_result,
)

logger = logging.getLogger("sidecar.tools.wiki")

TOOL_WIKI_SEARCH = "wiki_search"
TOOL_WIKI_READ_PAGE = "wiki_read_page"
TOOL_WIKI_INDEX = "wiki_index"

# 入参钳制（与 tools_wiki.go 常量一致）
SEARCH_DEFAULT_LIMIT = 10
SEARCH_MAX_LIMIT = 50
INDEX_DEFAULT_LIMIT = 50
INDEX_MAX_LIMIT = 200

# 渲染常量（与 wiki_tools.go / output_budget.go / truncate.go 对齐）
WIKI_OUTPUT_BUDGET = 24000
WIKI_MAX_LINK_SUMMARIES = 20
WIKI_LINK_SUMMARY_MAX_RUNES = 150
WIKI_MIN_PAGE_BODY = 400
WIKI_BUDGET_RESERVE = 600
WIKI_INDEX_AGENT_TOP_K = 20
PAGE_SEPARATOR = "\n\n"

WIKI_PAGE_TYPE_INDEX = "index"
_INDEX_TYPE_LABELS = {
    "summary": "Summary",
    "entity": "Entity",
    "concept": "Concept",
    "synthesis": "Synthesis",
    "comparison": "Comparison",
}

# 冻结错误文案
ERR_NO_WIKI_IN_SCOPE = (
    'none of the selected knowledge bases has a wiki; call list_knowledge_bases and '
    'pick one whose capabilities include "wiki"'
)
ERR_INDEX_LOAD = "failed to load wiki index"

# PostgreSQL POSIX（ARE）正则元字符：逐字符加反斜杠即按字面匹配。
# 不放字母数字——\d 这类在 PG 里是字符类，乱转义反而改变语义。
_POSIX_META = set("\\.^$*+?()[]{}|")


# ===========================================================================
# 纯函数：正则语义（侧车修正版）
# ===========================================================================

def posix_literal(query: str) -> str:
    """把查询串转义成 PG POSIX 正则的字面形式（regex=false 时使用）。"""
    return "".join(("\\" + ch) if ch in _POSIX_META else ch for ch in query)


def validate_regex(query: str) -> None:
    """regex=true 的前置校验；非法直接给英文 ToolError，避免上游 500。

    Python re 与 PG ARE 语法并非完全等价（如 Python 支持环视而 PG 不支持），
    这里只能做尽力校验：Python 不通过的一定非法；Python 通过但 PG 拒绝的
    极少数构造仍会走到逐 KB 错误文案，绝不假成功。
    """
    try:
        re.compile(query)
    except re.error as exc:
        raise ToolError(f"invalid regular expression in 'query': {exc}") from exc


def _clamp_limit(value: object, default: int, maximum: int) -> int:
    """wiki 限幅：非整数/<1 归默认值，>max 截断（与 Go GetInt 分支一致）。"""
    try:
        number = int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
    if number < 1:
        return default
    return min(number, maximum)


def _go_quote(text: str) -> str:
    """模拟 Go %q：双引号包裹、JSON 风格转义、可打印 Unicode 原样保留。"""
    return json.dumps(text, ensure_ascii=False)


# ===========================================================================
# 纯函数：片段与截断（移植 extractSnippet / truncateRunes / TruncateToolOutput）
# ===========================================================================

def extract_snippet(content: str, query: str) -> str:
    """命中处前后各 60 rune 的片段；正则非法返回空串（与 Go 一致）。"""
    if not content or not query:
        return ""
    try:
        match = re.search(query, content, flags=re.IGNORECASE)
    except re.error:
        return ""
    if match is None:
        return ""

    before = content[: match.start()]
    after = content[match.end()]
    matched = content[match.start(): match.end()]

    if len(before) > 60:
        before = before[-60:]
    if len(after) > 60:
        after = after[:60]
    if len(matched) > 100:
        matched = matched[:100] + "..."

    snippet = before + matched + after
    snippet = snippet.replace("\n", " ")
    while "  " in snippet:
        snippet = snippet.replace("  ", " ")
    return "... " + snippet.strip() + " ..."


def truncate_runes(text: str, max_runes: int) -> str:
    """超长按 rune（Python 字符即码点）截断并补省略号。"""
    if len(text) <= max_runes:
        return text
    return text[:max_runes] + "..."


def truncate_tool_output(output: str, max_chars: int) -> str:
    """整体输出超预算时保头 70% / 保尾 30%，中间放截断标记（移植 Go）。"""
    rune_count = len(output)
    if max_chars <= 0 or rune_count <= max_chars:
        return output

    usable = max_chars - 200
    if usable <= 0:
        return output[:max_chars]

    head_size = int(usable * 0.7)
    tail_size = usable - head_size
    if tail_size < 0:
        tail_size = 0

    marker = (
        f"\n\n... [output truncated: {rune_count} → {max_chars} chars, "
        f"showing first {head_size} + last {tail_size}] ...\n\n"
    )
    if tail_size == 0:
        return output[:head_size] + marker
    return output[:head_size] + marker + output[len(output) - tail_size:]


def split_budget_fairly(total: int, sizes: list[int]) -> list[int]:
    """max-min 公平（水位填充）分配：小条目保留全长并把余量让给大条目。"""
    caps = [0] * len(sizes)
    if not sizes or total <= 0:
        return caps
    settled = [False] * len(sizes)
    remaining, unsettled = total, len(sizes)
    while unsettled > 0:
        share = remaining // unsettled
        if share <= 0:
            break
        progressed = False
        for i, size in enumerate(sizes):
            if settled[i] or size > share:
                continue
            caps[i], settled[i] = size, True
            remaining -= size
            unsettled -= 1
            progressed = True
        if not progressed:
            # 剩余条目都超过均份，余量平分后结束
            for i in range(len(sizes)):
                if not settled[i]:
                    caps[i] = share
            break
    return caps


# ===========================================================================
# 纯函数：index 目录概览（移植 renderIndexOverviewForAgent）
# ===========================================================================

def render_index_overview(resp: dict) -> str:
    """把 WikiIndexResponse 渲染成读 index 页时塞入 <content> 的紧凑概览。"""
    parts: list[str] = []
    intro = str(resp.get("intro") or "").strip()
    clip = intro.find("\n## ")
    if clip >= 0:
        intro = intro[:clip].strip()
    if intro:
        parts.append(intro + "\n")

    non_empty = 0
    groups = resp.get("groups")
    if isinstance(groups, list):
        for group in groups:
            if not isinstance(group, dict):
                continue
            total = int(group.get("total") or 0)
            if total == 0:
                continue
            gtype = str(group.get("type") or "")
            label = _INDEX_TYPE_LABELS.get(gtype, gtype)
            items = group.get("items")
            items = items if isinstance(items, list) else []
            if len(items) < total:
                parts.append(f"\n## {label} ({total} total, showing top {len(items)})\n\n")
            else:
                parts.append(f"\n## {label} ({total})\n\n")
            for item in items:
                if not isinstance(item, dict):
                    continue
                slug = str(item.get("slug") or "")
                display = str(item.get("title") or "") or slug
                summary = str(item.get("summary") or "")
                if summary:
                    parts.append(f"[[{slug}|{display}]] — {summary}\n")
                else:
                    parts.append(f"[[{slug}|{display}]]\n")
            non_empty += 1

    if non_empty == 0:
        parts.append("\n*No wiki pages yet. Upload documents to get started.*\n")
    else:
        parts.append(
            "\n_To explore more pages under any category, use wiki_search with a query, "
            "or read a specific slug directly._\n"
        )
    return "".join(parts)


# ===========================================================================
# 纯函数：页渲染与预算（移植 pendingWikiPage / renderWikiPagesWithinBudget）
# ===========================================================================

def _register_found(found_kbs: dict[str, list[str]], slug: str, kb_id: str) -> None:
    """登记 slug→[kb...]（去重保序）；含邻居 slug，供前端跨库解析。"""
    if not slug or not kb_id:
        return
    owners = found_kbs.setdefault(slug, [])
    if kb_id not in owners:
        owners.append(kb_id)


def render_page(kb_id: str, page: dict, out_links: list[str], in_links: list[str],
                sources: list[str], body: str) -> str:
    """单页 <wiki_page> 文本（模板逐字对齐 Go fmt.Sprintf）。"""
    aliases = page.get("aliases")
    aliases_text = ", ".join(aliases) if isinstance(aliases, list) else ""
    return (
        "<wiki_page>\n"
        "<metadata>\n"
        f"<knowledge_base_id>{kb_id}</knowledge_base_id>\n"
        f"<link>[[{page.get('slug', '')}|{page.get('title', '')}]]</link>\n"
        f"<type>{page.get('page_type', '')}</type>\n"
        f"<aliases>{aliases_text}</aliases>\n"
        "</metadata>\n"
        "<relationships>\n"
        f"<links_to>{', '.join(out_links)}</links_to>\n"
        f"<linked_from>{', '.join(in_links)}</linked_from>\n"
        "</relationships>\n"
        "<sources>\n"
        f"{chr(10).join(sources)}\n"
        "</sources>\n"
        "<summary>\n"
        f"{page.get('summary', '')}\n"
        "</summary>\n"
        "<content>\n"
        f"{body}\n"
        "</content>\n"
        "</wiki_page>"
    )


def build_sources(page: dict) -> list[str]:
    """source_refs（"uuid" 或 "uuid|title"）→ <source> 标签行。"""
    sources: list[str] = []
    refs = page.get("source_refs")
    if not isinstance(refs, list):
        return sources
    for ref in refs:
        text = str(ref or "")
        kid, title = text, ""
        pipe = text.find("|")
        if pipe > 0:
            kid, title = text[:pipe], text[pipe + 1:]
        if title:
            sources.append(f'<source knowledge_id="{kid}">{title}</source>')
        else:
            sources.append(f'<source knowledge_id="{kid}"/>')
    return sources


def render_pages_within_budget(
    pending: list[dict], budget: int
) -> tuple[str, list[str] | None, list[str] | None]:
    """多页共享预算渲染；返回（文本, 被裁剪正文的 slugs, 整本省略的 slugs）。"""
    if not pending:
        return "", None, None

    rendered: list[str] = []
    body_sizes: list[int] = []
    overheads: list[int] = []
    total = len(PAGE_SEPARATOR) * (len(pending) - 1)
    for item in pending:
        full = render_page(
            item["kb_id"], item["page"], item["out_links"], item["in_links"],
            item["sources"], item["body"],
        )
        rendered.append(full)
        body_size = len(item["body"])
        body_sizes.append(body_size)
        overheads.append(len(full) - body_size)
        total += len(full)

    usable = budget - WIKI_BUDGET_RESERVE
    if usable <= 0 or total <= usable:
        return PAGE_SEPARATOR.join(rendered), None, None

    def fixed_cost(keep: int) -> int:
        cost = len(PAGE_SEPARATOR) * (keep - 1)
        for i in range(keep):
            cost += overheads[i]
        return cost

    keep = len(pending)
    while keep > 1 and fixed_cost(keep) + keep * WIKI_MIN_PAGE_BODY > usable:
        keep -= 1
    omitted = [str(item["page"].get("slug") or "") for item in pending[keep:]]

    caps = split_budget_fairly(usable - fixed_cost(keep), body_sizes[:keep])
    outputs: list[str] = []
    truncated: list[str] = []
    for i in range(keep):
        if caps[i] >= body_sizes[i]:
            outputs.append(rendered[i])
            continue
        body = "(body omitted: output budget exhausted)"
        if caps[i] > 0:
            body = truncate_tool_output(str(pending[i]["body"]), caps[i])
        outputs.append(render_page(
            pending[i]["kb_id"], pending[i]["page"], pending[i]["out_links"],
            pending[i]["in_links"], pending[i]["sources"], body,
        ))
        truncated.append(str(pending[i]["page"].get("slug") or ""))
    return PAGE_SEPARATOR.join(outputs), truncated, omitted


# ===========================================================================
# 工具集
# ===========================================================================

class WikiToolSet:
    """注册并承载 wiki_search / wiki_read_page / wiki_index。"""

    def __init__(self, client, guard) -> None:
        self._guard = guard
        self._kbs = KBService(client)
        self._wiki = WikiService(client)
        # 单次工具调用内的链接摘要缓存 {(kb_id, slug): summary or ""}；
        # 省重复请求，不跨调用保留（侧车无状态）。
        self._summary_cache: dict[tuple[str, str], str] = {}

    async def _authorize(self, tool_name: str, arguments: dict):
        """与检索工具一致：主体只取 contextvars，过完整 guard 裁决。"""
        principal = current_principal()
        if principal is None:
            # 中间件保证 /mcp 必有主体；兜底只可能是注册路径被绕过
            raise ToolError("unauthorized")
        verdict = await self._guard.authorize(principal, tool_name, arguments)
        if not verdict.allowed:
            raise ToolError(_DENY_MESSAGES.get(verdict.deny_reason, "request denied"))
        return principal, verdict.scope

    async def _resolve_wiki_kbs(self, scope, selectors: list[str] | None) -> list[KBSummary]:
        try:
            kbs = await self._kbs.resolve_selectors(scope, selectors)
        except ScopeError as exc:
            raise ToolError(str(exc)) from exc
        wiki_kbs = [kb for kb in kbs if kb.wiki]
        if not wiki_kbs:
            raise ToolError(ERR_NO_WIKI_IN_SCOPE)
        return wiki_kbs

    # ------------------------------------------------------------------
    # 6. wiki_search
    # ------------------------------------------------------------------

    async def wiki_search(
        self,
        query: str,
        regex: bool | None,
        knowledge_base_ids: list[str] | None,
        limit: int,
    ) -> CallToolResult:
        query = (query or "").strip()
        if not query:
            raise ToolError("query is required")
        selectors = _clean_selectors(knowledge_base_ids)
        limit = _clamp_limit(limit, SEARCH_DEFAULT_LIMIT, SEARCH_MAX_LIMIT)

        # regex 三态：省略保持历史正则语义；false 字面化；true 先校验
        effective_query = query
        if regex is False:
            effective_query = posix_literal(query)
        elif regex is True:
            validate_regex(query)

        arguments: dict = {"query": query, "limit": limit}
        if regex is not None:
            arguments["regex"] = regex
        if selectors is not None:
            arguments["knowledge_base_ids"] = selectors
        _, scope = await self._authorize(TOOL_WIKI_SEARCH, arguments)
        wiki_kbs = await self._resolve_wiki_kbs(scope, selectors)

        hits: list[tuple[str, dict]] = []
        errors: list[str] = []
        successful_calls = 0
        found_kbs: dict[str, list[str]] = {}

        for kb in wiki_kbs:
            try:
                pages = await self._wiki.search_pages(kb.id, effective_query, limit)
            except BackendError as exc:
                # 与 Go 一致：%q 里放的是实际发给 SearchPages 的查询（regex=false 时为转义后串）
                errors.append(
                    f"Wiki search {_go_quote(effective_query)} failed in KB {kb.id}: {exc.message}"
                )
                continue
            successful_calls += 1
            for page in pages:
                slug = str(page.get("slug") or "")
                returned_kb = str(page.get("knowledge_base_id") or "")
                if returned_kb and returned_kb != kb.id:
                    errors.append(
                        f"Wiki search result {_go_quote(slug)} returned KB {returned_kb} "
                        f"while resolving allowed KB {kb.id}"
                    )
                    continue
                hits.append((kb.id, page))
                _register_found(found_kbs, slug, kb.id)
                for neighbour in self._link_slugs(page):
                    _register_found(found_kbs, neighbour, kb.id)

        if successful_calls == 0 and errors:
            raise ToolError("; ".join(errors))

        block = self._render_search_block(hits, effective_query)
        output = block
        if errors:
            output += "\n\n<errors>\n" + "\n".join(errors) + "\n</errors>"

        data: dict = {"found_kbs": found_kbs, "content": output}
        return CallToolResult(
            content=[TextContent(type="text", text=output)],
            structured_content=data,
        )

    @staticmethod
    def _link_slugs(page: dict) -> list[str]:
        out: list[str] = []
        for key in ("out_links", "in_links"):
            value = page.get(key)
            if isinstance(value, list):
                out.extend(str(s) for s in value)
        return out

    @staticmethod
    def _render_search_block(hits: list[tuple[str, dict]], query: str) -> str:
        if not hits:
            return f'<search_results count="0" query="{query}" />'
        parts = [f'<search_results count="{len(hits)}" query="{query}">']
        for kb_id, page in hits:
            aliases = page.get("aliases")
            aliases_tag = ""
            if isinstance(aliases, list) and aliases:
                aliases_tag = "\n<aliases>" + ", ".join(str(a) for a in aliases) + "</aliases>"

            snippet = extract_snippet(str(page.get("content") or ""), query)
            snippet_tag = f"\n<match_snippet>{snippet}</match_snippet>" if snippet else ""

            # 侧车无跨调用 seen 状态，摘要恒完整给出（有意偏差，见模块头注释）
            parts.append(
                "<page>\n"
                f"<knowledge_base_id>{kb_id}</knowledge_base_id>\n"
                f"<link>[[{page.get('slug', '')}|{page.get('title', '')}]]</link>\n"
                f"<type>{page.get('page_type', '')}</type>{aliases_tag}\n"
                f"<summary>{page.get('summary', '')}</summary>{snippet_tag}\n"
                "</page>\n"
            )
        parts.append("</search_results>")
        return "".join(parts)

    # ------------------------------------------------------------------
    # 7. wiki_read_page
    # ------------------------------------------------------------------

    async def wiki_read_page(self, slug: str, knowledge_base_id: str = "") -> CallToolResult:
        slug = (slug or "").strip()
        if not slug:
            raise ToolError("slug is required")
        selector = (knowledge_base_id or "").strip()
        selectors = [selector] if selector else None

        arguments: dict = {"slug": slug}
        if selector:
            arguments["knowledge_base_id"] = selector
        _, scope = await self._authorize(TOOL_WIKI_READ_PAGE, arguments)
        wiki_kbs = await self._resolve_wiki_kbs(scope, selectors)

        hits: list[tuple[str, dict]] = []
        errors: list[str] = []
        lookup_failed = False
        found_kbs: dict[str, list[str]] = {}

        for kb in wiki_kbs:
            try:
                page = await self._wiki.get_page(kb.id, slug)
            except BackendError as exc:
                lookup_failed = True
                errors.append(
                    f"Failed to read Wiki page '{slug}' in KB {kb.id}: {exc.message}"
                )
                continue
            if page is None:
                continue
            returned_kb = str(page.get("knowledge_base_id") or "")
            if returned_kb and returned_kb != kb.id:
                lookup_failed = True
                errors.append(
                    f"Wiki page '{slug}' returned KB {returned_kb} "
                    f"while resolving allowed KB {kb.id}"
                )
                continue
            hits.append((kb.id, page))
            _register_found(found_kbs, slug, kb.id)
            for neighbour in self._link_slugs(page):
                _register_found(found_kbs, neighbour, kb.id)

        if not hits:
            if not lookup_failed:
                errors.append(f"Wiki page '{slug}' not found")
            raise ToolError("; ".join(errors))

        self._summary_cache = {}
        pending = [await self._resolve_page(kb_id, page) for kb_id, page in hits]
        output, truncated, omitted = render_pages_within_budget(pending, WIKI_OUTPUT_BUDGET)
        if omitted:
            output += (
                '\n\n<omitted_pages reason="output budget exceeded">\n'
                + "\n".join(omitted)
                + "\n</omitted_pages>\n<hint>These pages were resolved but not rendered. "
                "Call wiki_read_page again with fewer slugs to read them.</hint>"
            )
        if errors:
            output += "\n\n<errors>\n" + "\n".join(errors) + "\n</errors>"

        ambiguous = {s: owners for s, owners in found_kbs.items() if len(owners) > 1}
        data: dict = {
            "found_kbs": found_kbs,
            "ambiguous_slugs": ambiguous,
            # None 对齐 Go nil 切片序列化出的 null
            "truncated_slugs": truncated,
            "omitted_slugs": omitted,
            "content": output,
        }
        return CallToolResult(
            content=[TextContent(type="text", text=output)],
            structured_content=data,
        )

    async def _resolve_page(self, kb_id: str, page: dict) -> dict:
        """取齐渲染所需的邻居摘要/来源/index 概览，正文裁剪延后统一做。"""
        out_links = await self._format_links(page.get("out_links"), kb_id)
        in_links = await self._format_links(page.get("in_links"), kb_id)
        body = str(page.get("content") or "")
        if str(page.get("page_type") or "") == WIKI_PAGE_TYPE_INDEX:
            try:
                overview = await self._wiki.get_index(kb_id, WIKI_INDEX_AGENT_TOP_K)
                body = render_index_overview(overview)
            except BackendError as exc:
                logger.info("[wiki] index 概览加载失败，回落正文 kb=%s err=%s", kb_id, exc.message)
        return {
            "page": page,
            "kb_id": kb_id,
            "out_links": out_links,
            "in_links": in_links,
            "sources": build_sources(page),
            "body": body,
        }

    async def _format_links(self, raw_slugs: object, kb_id: str) -> list[str]:
        """链接区：前 20 条拿到非空摘要的内联 (summary...)，其余只给 slug。"""
        if not isinstance(raw_slugs, list):
            return ["(none)"]
        descs: list[str] = []
        inlined = 0
        for raw in raw_slugs:
            link_slug = str(raw or "")
            if not link_slug:
                continue
            if inlined >= WIKI_MAX_LINK_SUMMARIES:
                descs.append(f"[[{link_slug}]]")
                continue
            summary = await self._link_summary(kb_id, link_slug)
            if not summary:
                descs.append(f"[[{link_slug}]]")
                continue
            descs.append(f"[[{link_slug}]] ({truncate_runes(summary, WIKI_LINK_SUMMARY_MAX_RUNES)})")
            inlined += 1
        return descs or ["(none)"]

    async def _link_summary(self, kb_id: str, slug: str) -> str:
        """取邻居页摘要（404/上游错误/空摘要一律按无摘要处理）。"""
        key = (kb_id, slug)
        if key in self._summary_cache:
            return self._summary_cache[key]
        try:
            page = await self._wiki.get_page(kb_id, slug)
        except BackendError:
            page = None
        summary = str(page.get("summary") or "") if page else ""
        self._summary_cache[key] = summary
        return summary

    # ------------------------------------------------------------------
    # 8. wiki_index
    # ------------------------------------------------------------------

    async def wiki_index(self, knowledge_base_id: str, limit: int) -> CallToolResult:
        selector = (knowledge_base_id or "").strip()
        if not selector:
            raise ToolError("knowledge_base_id is required")
        limit = _clamp_limit(limit, INDEX_DEFAULT_LIMIT, INDEX_MAX_LIMIT)

        arguments = {"knowledge_base_id": selector, "limit": limit}
        _, scope = await self._authorize(TOOL_WIKI_INDEX, arguments)
        wiki_kbs = await self._resolve_wiki_kbs(scope, [selector])

        try:
            view = await self._wiki.get_index(wiki_kbs[0].id, limit)
        except BackendError as exc:
            raise ToolError(f"{ERR_INDEX_LOAD}: {exc.message}") from exc

        payload = {
            "knowledge_base_id": wiki_kbs[0].id,
            "intro": str(view.get("intro") or ""),
            "groups": view.get("groups") if isinstance(view.get("groups"), list) else [],
        }
        # 与 Go jsonResult 一致：文本是缩进 JSON；structuredContent 同构但不复制 content
        return CallToolResult(
            content=[TextContent(type="text", text=_go_json_indented(payload))],
            structured_content=payload,
        )

    # ------------------------------------------------------------------
    # 注册
    # ------------------------------------------------------------------

    def register(self, server) -> None:
        toolset = self

        @server.tool(
            name=TOOL_WIKI_SEARCH,
            description=(
                "Search the generated wiki pages of the knowledge bases in scope by title, alias and "
                "content. Returns page slugs that wiki_read_page accepts. Only knowledge bases with a "
                "wiki are searched."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def wiki_search(
            query: Annotated[str, Field(
                description="Search terms or a regular expression (case-insensitive POSIX); text that "
                "is not a valid regular expression is matched literally",
                min_length=1,
            )],
            regex: Annotated[bool | None, Field(
                description="false forces a literal match; true requires a valid regular expression; "
                "omit for the default behaviour",
            )] = None,
            knowledge_base_ids: Annotated[list[str] | None, Field(
                description="Optional knowledge base ids or names to restrict the search",
            )] = None,
            limit: Annotated[int, Field(
                description="Maximum pages to return, default 10, max 50", ge=1, le=50,
            )] = 10,
        ) -> dict:
            try:
                return await toolset.wiki_search(query, regex, knowledge_base_ids, limit)
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_WIKI_READ_PAGE,
            description="Read one wiki page by slug, including its content and outgoing links.",
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def wiki_read_page(
            slug: Annotated[str, Field(
                description="Page slug from wiki_search or wiki_index, e.g. concept/rag",
                min_length=1,
            )],
            knowledge_base_id: Annotated[str, Field(
                description="Knowledge base id or name, required only when the same slug exists in "
                "several wikis",
            )] = "",
        ) -> dict:
            try:
                return await toolset.wiki_read_page(slug, knowledge_base_id)
            except ToolError as exc:
                return _tool_error_result(exc)

        @server.tool(
            name=TOOL_WIKI_INDEX,
            description=(
                "Browse the table of contents of one knowledge base's wiki: the intro plus pages "
                "grouped by type (summary, entity, concept, ...)."
            ),
            annotations=READ_ONLY_ANNOTATIONS,
        )
        async def wiki_index(
            knowledge_base_id: Annotated[str, Field(
                description="Knowledge base id or exact name", min_length=1,
            )],
            limit: Annotated[int, Field(
                description="Maximum entries per page type, default 50, max 200", ge=1, le=200,
            )] = 50,
        ) -> dict:
            try:
                return await toolset.wiki_index(knowledge_base_id, limit)
            except ToolError as exc:
                return _tool_error_result(exc)
