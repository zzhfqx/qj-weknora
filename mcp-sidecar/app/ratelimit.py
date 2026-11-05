"""Redis 滑窗限流器（按端点），Redis 不可用时内存兜底。

算法与 Go 端 internal/ratelimit/limiter.go 对齐：
  - 每端点一个 ZSET，成员是"本次命中"，分值为毫秒时间戳；
  - 每次调用先砍掉窗口外旧成员，ZCARD 计数，未超额才 ZADD 新成员并刷新过期；
  - 四步用一段 Lua 原子完成（Go 侧 rateLimitScript 同款逻辑）；
  - Redis 未配置或报错时退化为进程内内存滑窗（与 Go 的 localLimiter 一致，
    多副本部署时兜底窗口只在本副本计数，仅作可用性保护，不追求精确）。

预算取端点行 rate_limit_per_minute（<=0 表示不限，与 Go 的 max<=0 放行一致）。
"""

from __future__ import annotations

import logging
import time
import uuid
from collections import defaultdict, deque
from typing import Protocol

logger = logging.getLogger("sidecar.ratelimit")

# 独立前缀，不与内置 Go MCP 的 "mcp:endpoint:ratelimit:" 计数串用
KEY_PREFIX = "mcp:sidecar:ratelimit:"
WINDOW_SECONDS = 60

# 与 Go rateLimitScript 等价的原子脚本：清旧成员→计数→未超额才记录
_LUA_SLIDING_WINDOW = """
local key     = KEYS[1]
local now     = tonumber(ARGV[1])
local window  = tonumber(ARGV[2])
local maxReq  = tonumber(ARGV[3])
local member  = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)
if count < maxReq then
    redis.call('ZADD', key, now, member)
    redis.call('PEXPIRE', key, window + 1000)
    return 1
end
return 0
"""


class RedisBackend(Protocol):
    """限流器需要的最小 Redis 接口（redis.asyncio.Redis 天然满足）。"""

    async def eval(self, script: str, numkeys: int, *keys_and_args): ...


class LocalSlidingWindow:
    """进程内兜底滑窗：每键一个时间戳 deque（对齐 Go localLimiter）。"""

    def __init__(self) -> None:
        self._hits: dict[str, deque[float]] = defaultdict(deque)

    def allow(self, key: str, window_seconds: int, max_requests: int, now: float | None = None) -> bool:
        now = time.monotonic() if now is None else now
        cutoff = now - window_seconds
        hits = self._hits[key]
        while hits and hits[0] <= cutoff:
            hits.popleft()
        if len(hits) >= max_requests:
            return False
        hits.append(now)
        return True


class SlidingWindowLimiter:
    """按端点的每分钟滑窗限流；Redis 主路径 + 内存兜底。"""

    def __init__(self, redis_client, *, local: LocalSlidingWindow | None = None) -> None:
        self._redis = redis_client
        self._local = local or LocalSlidingWindow()

    async def allow(self, endpoint_id: str, max_per_minute: int) -> bool:
        """本次调用是否在预算内。max<=0 表示不限（与 Go 语义一致）。"""
        if max_per_minute <= 0:
            return True

        if self._redis is not None:
            allowed = await self._redis_allow(endpoint_id, max_per_minute)
            if allowed is not None:
                return allowed
            logger.warning("[ratelimit] Redis 不可用，本端点改用进程内内存兜底：%s", endpoint_id)
        return self._local.allow(endpoint_id, WINDOW_SECONDS, max_per_minute)

    async def _redis_allow(self, endpoint_id: str, max_per_minute: int) -> bool | None:
        """返回 None 表示 Redis 路径失败，调用方应走内存兜底。"""
        now_ms = int(time.time() * 1000)
        window_ms = WINDOW_SECONDS * 1000
        # uuid 保证同毫秒并发的成员也不互相覆盖（Go 用 实例id:毫秒，极端同毫秒会撞）
        member = f"{uuid.uuid4().hex}:{now_ms}"
        try:
            result = await self._redis.eval(
                _LUA_SLIDING_WINDOW, 1,
                KEY_PREFIX + endpoint_id,
                str(now_ms), str(window_ms), str(max_per_minute), member,
            )
        except Exception:  # noqa: BLE001 — 任何 Redis 异常都降级，不把请求打死
            logger.exception("[ratelimit] Redis 调用失败")
            return None
        return int(result) == 1
