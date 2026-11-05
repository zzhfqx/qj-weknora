"""知识库 REST 适配：列表、详情、能力位解析、选择器解析。

对齐 Go：
  - GET /knowledge-bases         → 工作空间自有库（X-API-Key 上下文）
  - GET /knowledge-bases/:id     → 单库详情（retrieve 能力可调；组织共享库
                                   经 KBAccessRead 同样可读）
  - GET /shared-knowledge-bases  需要 manage_spaces 能力，retrieve-only 钥匙
                                   调不通（路由策略实测如此），因此侧车不调它，
                                   scope 点名的共享库改为按需 GET 详情补入。

scope 为闭集（2026-11-03 修订）：机器=端点公共书架，用户=公共书架∪个人授权，
不再有 unrestricted 形态；范围之外的库 id 绝不发详情请求（fail-closed）。

能力位：REST 的 KnowledgeBase JSON 自带计算字段 capabilities
（vector/keyword/wiki/...），缺失时回落到 indexing_strategy 的三个布尔位。
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass

from app.backend.client import BackendError, BackendNotFound, RESTClient
from app.guards import EffectiveScope

logger = logging.getLogger("sidecar.backend.kb")

# 库列表/详情短缓存：库集合变化极慢；只缓存成功结果（不做负缓存，
# 新分享/新建的库最多等一个 TTL 自然可见）
KB_CACHE_TTL = 30.0

# capabilities 英文词 → 工具契约词（与 Go summarizeKnowledgeBase 一致）
_CAP_MAP = (("vector", "semantic"), ("keyword", "keyword"), ("wiki", "wiki"))


class ScopeError(ValueError):
    """选择器/范围类用户错误；tools 层捕获后转 ToolError（isError=true）。"""


@dataclass(frozen=True)
class KBSummary:
    """侧车内部使用的库摘要。"""

    id: str
    name: str
    description: str
    type: str
    tenant_id: int
    capabilities: frozenset[str]

    @property
    def semantic(self) -> bool:
        return "semantic" in self.capabilities

    @property
    def keyword(self) -> bool:
        return "keyword" in self.capabilities

    @property
    def wiki(self) -> bool:
        return "wiki" in self.capabilities

    @property
    def retrievable(self) -> bool:
        """有向量或关键词索引（与 Go retrievableKnowledgeBases 一致）。"""
        return self.semantic or self.keyword


def parse_kb(raw: dict) -> KBSummary:
    """把 REST 的 KnowledgeBase JSON 归一化成 KBSummary。"""
    caps: set[str] = set()
    raw_caps = raw.get("capabilities")
    if isinstance(raw_caps, dict):
        for rest_name, tool_name in _CAP_MAP:
            if raw_caps.get(rest_name):
                caps.add(tool_name)
    else:
        # 老版本/异常返回缺少 capabilities 时的回落：读 indexing_strategy
        strategy = raw.get("indexing_strategy")
        if isinstance(strategy, dict):
            if strategy.get("vector_enabled"):
                caps.add("semantic")
            if strategy.get("keyword_enabled"):
                caps.add("keyword")
            if strategy.get("wiki_enabled"):
                caps.add("wiki")
    try:
        tenant_id = int(raw.get("tenant_id") or 0)
    except (TypeError, ValueError):
        tenant_id = 0
    return KBSummary(
        id=str(raw.get("id") or "").strip(),
        name=str(raw.get("name") or ""),
        description=str(raw.get("description") or ""),
        type=str(raw.get("type") or "document") or "document",
        tenant_id=tenant_id,
        capabilities=frozenset(caps),
    )


class KBService:
    def __init__(self, client: RESTClient, ttl: float = KB_CACHE_TTL, *, clock=time.monotonic) -> None:
        self._client = client
        self._ttl = ttl
        self._clock = clock
        self._own_cache: tuple[list[KBSummary], float] | None = None
        self._detail_cache: dict[str, tuple[KBSummary, float]] = {}

    async def list_own(self) -> list[KBSummary]:
        """工作空间自有库（REST 已按 API Key 的库白名单过滤）。"""
        now = self._clock()
        if self._own_cache is not None and now < self._own_cache[1]:
            return self._own_cache[0]

        body = await self._client.get_json("/knowledge-bases")
        rows = body.get("data")
        out: list[KBSummary] = []
        if isinstance(rows, list):
            for row in rows:
                if isinstance(row, dict):
                    summary = parse_kb(row)
                    if summary.id:
                        out.append(summary)
        self._own_cache = (out, now + self._ttl)
        return out

    async def get(self, kb_id: str) -> KBSummary | None:
        """单库详情；404 返回 None，其他上游错误照常抛。"""
        kb_id = kb_id.strip()
        now = self._clock()
        cached = self._detail_cache.get(kb_id)
        if cached is not None and now < cached[1]:
            return cached[0]
        try:
            body = await self._client.get_json(f"/knowledge-bases/{kb_id}")
        except BackendNotFound:
            return None
        raw = body.get("data")
        if not isinstance(raw, dict):
            return None
        summary = parse_kb(raw)
        if not summary.id:
            return None
        self._detail_cache[kb_id] = (summary, now + self._ttl)
        return summary

    async def resolve_visible(self, scope: EffectiveScope) -> list[KBSummary]:
        """返回本次主体实际可见且落在生效范围（闭集）内的库。

        自有库先按 scope 过滤；scope 里不在自有列表的 id（公共书架或个人
        授权点名的组织共享库）并发 GET 详情补入，查不到的静默丢弃（与 Go
        端 warn-and-continue 一致，失效 id 无需同步清理；后端 KBAccessRead
        是最终裁判，未共享给本空间的库补不进来）。
        """
        own = await self.list_own()

        own_by_id = {kb.id: kb for kb in own}
        visible = [kb for kb in own if kb.id in scope.kb_ids]

        missing = [kb_id for kb_id in scope.kb_ids if kb_id not in own_by_id]
        if missing:
            details = await asyncio.gather(*(self.safe_get(kb_id) for kb_id in missing))
            for kb_id, detail in zip(missing, details):
                if detail is not None:
                    visible.append(detail)
                else:
                    logger.info("[scope] 生效范围内的库不可读或不存在：%s", kb_id)
        return visible

    async def safe_get(self, kb_id: str) -> KBSummary | None:
        """get() 的不抛错版本（上游 5xx 等记日志后按"不可见"处理）。"""
        try:
            return await self.get(kb_id)
        except BackendError as exc:
            logger.info("[scope] 取库详情失败，按不可见处理 kb=%s err=%s", kb_id, exc.message)
            return None

    async def resolve_selectors(
        self, scope: EffectiveScope, selectors: list[str] | None
    ) -> list[KBSummary]:
        """把工具入参里的库 id/精确库名解析成授权范围内的库。

        语义对齐 Go selectKnowledgeBases：未传选择器返回全部可见库；
        先按 id 精确匹配，再按大小写不敏感的精确库名匹配；查不到或越权
        统一报 ScopeError（不区分"不存在"与"越权"，防探测）。
        """
        visible = await self.resolve_visible(scope)
        cleaned = [s.strip() for s in (selectors or []) if s and s.strip()]
        if not cleaned:
            if not visible:
                raise ScopeError("no knowledge base is available on this endpoint")
            return visible

        # scope 是闭集：resolve_visible 已把范围内（含共享库按需补详情）
        # 的库全部补齐，范围外选择器绝不补查（fail-closed）。
        out: list[KBSummary] = []
        seen: set[str] = set()
        for selector in cleaned:
            kb = _match_one(visible, selector)
            if kb is None:
                raise ScopeError(
                    f"knowledge base {selector!r} was not found or is outside this endpoint's "
                    "scope; call list_knowledge_bases to see what is available"
                )
            if kb.id not in seen:
                seen.add(kb.id)
                out.append(kb)
        return out


def _match_one(kbs: list[KBSummary], selector: str) -> KBSummary | None:
    """先 id 精确，再大小写不敏感的精确库名（与 Go matchKnowledgeBase 一致）。"""
    for kb in kbs:
        if kb.id == selector:
            return kb
    for kb in kbs:
        if kb.name.strip().lower() == selector.lower():
            return kb
    return None
