"""mcp_endpoints 只读仓储 + 全工程共用的 PG 只读连接池。

按方案 stores/ 只保留 endpoints / permissions 两个文件，连接池类 Database
也放在本文件，permissions.py 从这里导入。

按端点令牌反查端点行（单 URL 方案的关键）：内置 Go MCP 路径里带
endpoint_id，侧车是单 URL（/mcp），端点身份完全靠令牌反查——对
sha256(token) 做等值查询即可（Go 端也是 GetByTokenHash 等值查询；它额外
做的 endpoint_id 常量时间比对在侧车不存在该输入，无需做）。

软删除：Go/GORM 自动附加 deleted_at IS NULL；侧车手写 SQL 必须显式写出。
"""

from __future__ import annotations

import hashlib
import json
import logging
import time
from dataclasses import dataclass

import asyncpg

logger = logging.getLogger("sidecar.db")


class Database:
    """PostgreSQL 只读连接池（asyncpg），两个只读仓储共用一个池。

    侧车数据库账号只有 mcp_endpoints / mcp_user_permission 两张表的 SELECT
    （见 deploy/01_readonly_role.sql）；全工程不提供任何写操作入口。
    放在本文件是按方案 stores/ 只保留 endpoints / permissions 两个文件。
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._pool: asyncpg.Pool | None = None

    @property
    def configured(self) -> bool:
        return bool(self._dsn)

    async def pool(self) -> asyncpg.Pool:
        if not self._dsn:
            raise RuntimeError("SIDECAR_DB_DSN 未配置，无法访问授权表")
        if self._pool is None:
            # 池容量按侧车并发保守设置；只读短查询，3 秒语句超时与只读账号兜底一致
            self._pool = await asyncpg.create_pool(
                dsn=self._dsn,
                min_size=1,
                max_size=8,
                timeout=5,
                server_settings={"statement_timeout": "3000"},
                # asyncpg 默认把 jsonb 返回为字符串；这里注册解码器让
                # knowledge_base_ids / tools 两列自动解析为 Python list
                init=_set_jsonb_codec,
            )
            logger.info("PostgreSQL 只读连接池已建立")
        assert self._pool is not None
        return self._pool

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

# 端点行进程内缓存 TTL（秒）。端点数据变更极少；注意轮换令牌后旧 token 的
# 成功认证最多残留这么久（新 token 立即生效，因为是不同的 hash 键）。
ENDPOINT_CACHE_TTL = 30.0

# 显式列名 SELECT，与"只读账号 + 固定列"纪律一致（不写 SELECT *）
_SELECT_BY_TOKEN_HASH = """
    SELECT id, tenant_id, name, enabled, knowledge_base_ids, tools,
           default_agent_id, rate_limit_per_minute
    FROM mcp_endpoints
    WHERE token_hash = $1 AND deleted_at IS NULL
"""


@dataclass(frozen=True)
class EndpointRecord:
    """mcp_endpoints 一行（仅侧车裁决需要的列）。"""

    id: str
    tenant_id: int
    name: str
    enabled: bool
    knowledge_base_ids: tuple[str, ...]
    tools: tuple[str, ...]
    default_agent_id: str
    rate_limit_per_minute: int

    def has_tool(self, tool_name: str) -> bool:
        return tool_name in self.tools

    def allows_knowledge_base(self, kb_id: str) -> bool:
        """库白名单语义与 Go 端 AllowsKnowledgeBase 一致：空列表=工作空间全部。"""
        if not self.knowledge_base_ids:
            return True
        return kb_id in self.knowledge_base_ids


def hash_token(token: str) -> str:
    """令牌存储摘要：sha256 hex（与 Go 端 HashMCPEndpointToken 一致）。"""
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


async def _set_jsonb_codec(conn: asyncpg.Connection) -> None:
    """让 asyncpg 把 jsonb 列自动解码为 Python 对象（默认返回字符串）。"""
    await conn.set_type_codec(
        "jsonb",
        encoder=json.dumps,
        decoder=json.loads,
        schema="pg_catalog",
    )


def _normalize_jsonb_list(value: object) -> tuple[str, ...]:
    """jsonb 数组列在 asyncpg 中解析为 list；容错归一化为去空白字符串元组。"""
    if not isinstance(value, list):
        return ()
    return tuple(str(item).strip() for item in value if str(item).strip())


class EndpointStore:
    def __init__(self, db: Database, ttl: float = ENDPOINT_CACHE_TTL, *, clock=time.monotonic) -> None:
        self._db = db
        self._ttl = ttl
        self._clock = clock
        # 只缓存成功命中的行（token_hash → (记录, 过期时刻)）；不做负缓存
        self._cache: dict[str, tuple[EndpointRecord, float]] = {}

    async def get_by_token(self, token: str) -> EndpointRecord | None:
        """按明文令牌反查启用/未删除的端点行；查无返回 None，DB 错误向上抛。"""
        token_hash = hash_token(token)
        now = self._clock()
        cached = self._cache.get(token_hash)
        if cached is not None and now < cached[1]:
            return cached[0]

        pool = await self._db.pool()
        async with pool.acquire() as conn:
            row = await conn.fetchrow(_SELECT_BY_TOKEN_HASH, token_hash)
        if row is None:
            # 负结果不缓存：新建端点/刚轮换的令牌不必等 TTL 即可生效
            return None

        record = EndpointRecord(
            id=str(row["id"]),
            tenant_id=int(row["tenant_id"]),
            name=str(row["name"] or ""),
            enabled=bool(row["enabled"]),
            knowledge_base_ids=_normalize_jsonb_list(row["knowledge_base_ids"]),
            tools=_normalize_jsonb_list(row["tools"]),
            default_agent_id=str(row["default_agent_id"] or ""),
            rate_limit_per_minute=int(row["rate_limit_per_minute"] or 60),
        )
        self._cache[token_hash] = (record, now + self._ttl)
        return record
