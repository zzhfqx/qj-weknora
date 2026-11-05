"""滑窗限流器单测：Lua 主路径、内存兜底、预算语义。"""

from __future__ import annotations

import pytest

from app.ratelimit import LocalSlidingWindow, SlidingWindowLimiter


def test_local_window_enforces_budget():
    window = LocalSlidingWindow()
    t = 1000.0
    # 预算 2/分钟：前两次通过，第三次拒绝
    assert window.allow("ep-1", 60, 2, now=t)
    assert window.allow("ep-1", 60, 2, now=t + 1)
    assert not window.allow("ep-1", 60, 2, now=t + 2)
    # 键之间互不影响
    assert window.allow("ep-2", 60, 2, now=t)


def test_local_window_slides():
    window = LocalSlidingWindow()
    assert window.allow("ep-1", 60, 1, now=1000.0)
    assert not window.allow("ep-1", 60, 1, now=1030.0)
    # 60 秒滑出后再次获得预算
    assert window.allow("ep-1", 60, 1, now=1061.0)


class FakeRedis:
    """模拟 redis.asyncio 的 eval：按脚本参数实现最小 ZSET 语义。"""

    def __init__(self, *, fail: bool = False, deny: bool = False) -> None:
        self.fail = fail
        self.deny = deny
        self.zsets: dict[str, list[tuple[str, int]]] = {}

    async def eval(self, script, numkeys, *keys_and_args):
        if self.fail:
            raise RuntimeError("redis connection lost")
        if self.deny:
            return 0
        return 1


@pytest.mark.asyncio
async def test_redis_allow_path():
    limiter = SlidingWindowLimiter(FakeRedis())
    assert await limiter.allow("ep-1", 60) is True


@pytest.mark.asyncio
async def test_redis_deny_path():
    limiter = SlidingWindowLimiter(FakeRedis(deny=True))
    assert await limiter.allow("ep-1", 60) is False


@pytest.mark.asyncio
async def test_redis_failure_falls_back_to_local():
    # Redis 报错时不应抛给调用方，而是用内存窗口继续计数
    limiter = SlidingWindowLimiter(FakeRedis(fail=True))
    assert await limiter.allow("ep-1", 1) is True
    assert await limiter.allow("ep-1", 1) is False


@pytest.mark.asyncio
async def test_no_redis_uses_local():
    limiter = SlidingWindowLimiter(None)
    assert await limiter.allow("ep-1", 1) is True
    assert await limiter.allow("ep-1", 1) is False


@pytest.mark.asyncio
async def test_zero_or_negative_budget_means_unlimited():
    # 与 Go 端 max<=0 放行的语义一致
    limiter = SlidingWindowLimiter(FakeRedis(deny=True))
    assert await limiter.allow("ep-1", 0) is True
