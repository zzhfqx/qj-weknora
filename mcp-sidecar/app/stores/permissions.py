"""mcp_user_permission 只读仓储：按 (工作空间, 外部用户) 点查授权库列表。

语义与 Go 端 mcpUserPermissionService 对齐：
  - 进程内缓存 TTL 45 秒（Go: MCPUserPermissionCacheTTL）；
  - 空结果不缓存（无负缓存），收权/删行后下一次请求即生效；
  - 返回副本，调用方修改不污染缓存；
  - 定时 sweep 清理过期项，避免外部用户无限增长撑爆 dict。
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass

from app.stores.endpoints import Database

logger = logging.getLogger("sidecar.permissions")

PERMISSION_CACHE_TTL = 45.0

_SELECT_BY_USER = """
    SELECT knowledge_base_ids
    FROM mcp_user_permission
    WHERE tenant_id = $1 AND user_id = $2
"""


@dataclass(frozen=True)
class _CacheEntry:
    kb_ids: tuple[str, ...]
    exp: float


class PermissionStore:
    def __init__(
        self,
        db: Database,
        ttl: float = PERMISSION_CACHE_TTL,
        *,
        clock=time.monotonic,
    ) -> None:
        self._db = db
        self._ttl = ttl
        self._clock = clock
        self._cache: dict[tuple[int, str], _CacheEntry] = {}
        self._sweeper: asyncio.Task | None = None

    async def get_user_knowledge_bases(self, tenant_id: int, user_id: str) -> tuple[str, ...]:
        """返回该用户授权库 id 元组（去空白、去重、保序）；无行/空列表返回空元组。

        DB 错误向上抛，由调用方 fail-closed 拒绝，绝不放行。
        """
        user_id = user_id.strip()
        key = (tenant_id, user_id)
        now = self._clock()

        entry = self._cache.get(key)
        if entry is not None and now < entry.exp:
            return entry.kb_ids

        pool = await self._db.pool()
        async with pool.acquire() as conn:
            row = await conn.fetchrow(_SELECT_BY_USER, tenant_id, user_id)

        kb_ids = _normalize_jsonb_list(row["knowledge_base_ids"]) if row is not None else ()
        if kb_ids:
            self._cache[key] = _CacheEntry(kb_ids=kb_ids, exp=now + self._ttl)
        else:
            # 空结果不缓存，并淘汰旧授权快照：撤销授权立即生效，不必等 TTL
            self._cache.pop(key, None)
        return kb_ids

    def start_sweeper(self) -> None:
        """启动过期项定时清理（随应用生命周期常驻）。"""
        if self._sweeper is None:
            self._sweeper = asyncio.create_task(self._sweep_loop())

    async def stop_sweeper(self) -> None:
        if self._sweeper is not None:
            self._sweeper.cancel()
            self._sweeper = None

    async def _sweep_loop(self) -> None:
        try:
            while True:
                await asyncio.sleep(self._ttl)
                now = self._clock()
                expired = [key for key, entry in self._cache.items() if not now < entry.exp]
                for key in expired:
                    self._cache.pop(key, None)
        except asyncio.CancelledError:  # 停服时正常退出
            pass


def _normalize_jsonb_list(value: object) -> tuple[str, ...]:
    """jsonb 数组归一化：去空白、去重、保持首次出现顺序（对齐 Go normalizeMCPUserKBIDs）。"""
    if not isinstance(value, list):
        return ()
    out: list[str] = []
    seen: set[str] = set()
    for item in value:
        kb_id = str(item).strip()
        if not kb_id or kb_id in seen:
            continue
        seen.add(kb_id)
        out.append(kb_id)
    return tuple(out)
