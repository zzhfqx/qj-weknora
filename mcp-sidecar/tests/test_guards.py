"""工具裁决 guard 单测：工具白名单/公共书架并集/空并集/DB 故障/限流/审计。"""

from __future__ import annotations

import json
import logging

import pytest

from app.auth.middleware import Principal
from app.guards import (
    REASON_PERMISSION_UNAVAILABLE,
    REASON_RATE_LIMITED,
    REASON_TOOL_DISABLED,
    EffectiveScope,
    ToolGuard,
)
from app.stores.endpoints import EndpointRecord

TENANT = 77


def _endpoint(*, tools=("search_knowledge",), kb_ids=(), rate=60) -> EndpointRecord:
    return EndpointRecord(
        id="ep-1", tenant_id=TENANT, name="ep", enabled=True,
        knowledge_base_ids=tuple(kb_ids), tools=tuple(tools),
        default_agent_id="", rate_limit_per_minute=rate,
    )


class FakePermissionStore:
    def __init__(self, table=None, *, fail: bool = False) -> None:
        self.table = table or {}
        self.fail = fail

    async def get_user_knowledge_bases(self, tenant_id: int, user_id: str):
        if self.fail:
            raise RuntimeError("database down")
        return tuple(self.table.get((tenant_id, user_id), ()))


class FakeLimiter:
    def __init__(self, allowed: bool = True) -> None:
        self.calls: list[tuple[str, int]] = []
        self.allowed = allowed

    async def allow(self, endpoint_id: str, max_per_minute: int) -> bool:
        self.calls.append((endpoint_id, max_per_minute))
        return self.allowed


# ---------- 机器主体：只有公共书架 ----------

@pytest.mark.asyncio
async def test_machine_empty_public_shelf_is_zero_scope():
    # 留空 = 一个库都不给（不再是"工作空间全部库"）
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    principal = Principal(endpoint=_endpoint(), user_id=None)
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert verdict.allowed
    assert verdict.scope == EffectiveScope(kb_ids=frozenset())
    assert not verdict.scope.allows("kb-a")


@pytest.mark.asyncio
async def test_machine_scope_is_public_shelf():
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    principal = Principal(endpoint=_endpoint(kb_ids=("kb-a", "kb-b")), user_id=None)
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert verdict.allowed
    assert verdict.scope == EffectiveScope(kb_ids=frozenset({"kb-a", "kb-b"}))
    assert verdict.scope.allows("kb-a") and not verdict.scope.allows("kb-c")


# ---------- 用户主体：公共书架 ∪ 个人授权 ----------

@pytest.mark.asyncio
async def test_user_scope_is_union_deduplicated():
    # 公共书架 {a,b}，个人授权 {b,c} → 并集 {a,b,c}；b 同时在两侧只出现一次
    store = FakePermissionStore({(TENANT, "u-1"): ("kb-b", "kb-c")})
    guard = ToolGuard(store, FakeLimiter())
    principal = Principal(endpoint=_endpoint(kb_ids=("kb-a", "kb-b")), user_id="u-1")
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert verdict.allowed
    assert verdict.scope == EffectiveScope(kb_ids=frozenset({"kb-a", "kb-b", "kb-c"}))
    assert len(verdict.scope.kb_ids) == 3  # frozenset 并集天然去重


@pytest.mark.asyncio
async def test_user_empty_permission_still_sees_public_shelf():
    # 零个人授权 + 合法票：放行，范围=公共书架（并集语义，不再拒绝）
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    principal = Principal(endpoint=_endpoint(kb_ids=("kb-a",)), user_id="u-1")
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert verdict.allowed
    assert verdict.scope == EffectiveScope(kb_ids=frozenset({"kb-a"}))


@pytest.mark.asyncio
async def test_user_empty_permission_empty_shelf_is_empty_scope_but_allowed():
    # 两侧皆空：guard 仍放行（空 scope 由工具层以"无可用知识库"拒绝具体调用）
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    principal = Principal(endpoint=_endpoint(), user_id="u-1")
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert verdict.allowed
    assert verdict.scope == EffectiveScope(kb_ids=frozenset())


@pytest.mark.asyncio
async def test_machine_scope_always_subset_of_user_scope():
    # 安全性质：同一端点上机器范围恒为任意用户范围的子集
    store = FakePermissionStore({(TENANT, "u-1"): ("kb-c",)})
    guard = ToolGuard(store, FakeLimiter())
    endpoint = _endpoint(kb_ids=("kb-a", "kb-b"))
    machine = await guard.authorize(Principal(endpoint=endpoint, user_id=None), "search_knowledge", {})
    user = await guard.authorize(Principal(endpoint=endpoint, user_id="u-1"), "search_knowledge", {})
    assert machine.scope.kb_ids <= user.scope.kb_ids


@pytest.mark.asyncio
async def test_user_permission_db_failure_fails_closed():
    # 授权表读取失败：拒绝，绝不降级为仅公共书架
    guard = ToolGuard(FakePermissionStore(fail=True), FakeLimiter())
    principal = Principal(endpoint=_endpoint(kb_ids=("kb-a",)), user_id="u-1")
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert not verdict.allowed and verdict.deny_reason == REASON_PERMISSION_UNAVAILABLE


# ---------- 白名单与限流 ----------

@pytest.mark.asyncio
async def test_tool_not_in_endpoint_allowlist_denied():
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    principal = Principal(endpoint=_endpoint(), user_id=None)
    verdict = await guard.authorize(principal, "delete_document", {})
    assert not verdict.allowed and verdict.deny_reason == REASON_TOOL_DISABLED


@pytest.mark.asyncio
async def test_rate_limited_denied():
    guard = ToolGuard(FakePermissionStore(), FakeLimiter(allowed=False))
    principal = Principal(endpoint=_endpoint(), user_id=None)
    verdict = await guard.authorize(principal, "search_knowledge", {})
    assert not verdict.allowed and verdict.deny_reason == REASON_RATE_LIMITED


# ---------- tools/list 可见性 ----------

@pytest.mark.asyncio
async def test_list_scope_user_with_perms():
    store = FakePermissionStore({(TENANT, "u-1"): ("kb-b",)})
    guard = ToolGuard(store, FakeLimiter())
    scope = await guard.list_scope(Principal(endpoint=_endpoint(kb_ids=("kb-a",)), user_id="u-1"))
    assert scope is not None and scope.kb_ids == frozenset({"kb-a", "kb-b"})


@pytest.mark.asyncio
async def test_list_scope_empty_perms_returns_public_shelf():
    guard = ToolGuard(FakePermissionStore(), FakeLimiter())
    scope = await guard.list_scope(Principal(endpoint=_endpoint(kb_ids=("kb-a",)), user_id="u-1"))
    assert scope is not None and scope.kb_ids == frozenset({"kb-a"})


@pytest.mark.asyncio
async def test_list_scope_db_failure_returns_none():
    guard = ToolGuard(FakePermissionStore(fail=True), FakeLimiter())
    scope = await guard.list_scope(Principal(endpoint=_endpoint(), user_id="u-1"))
    assert scope is None


# ---------- 审计 ----------

def test_audit_json_fields(caplog):
    ep = _endpoint(kb_ids=("kb-a",))
    principal = Principal(endpoint=ep, user_id="u-9")
    with caplog.at_level(logging.INFO, logger="sidecar.audit"):
        ToolGuard.audit(
            principal, "search_knowledge",
            {"knowledge_base_ids": ["kb-a", "kb-x"], "knowledge_id": "doc-1"},
            allowed=False, deny_reason=REASON_RATE_LIMITED,
        )
    record = next(r for r in caplog.records if "[mcp-audit]" in r.getMessage())
    # 日志正文里的 JSON 片段解析（前缀之后的部分）
    message = record.getMessage()
    payload = json.loads(message[message.index("{"):])
    assert payload["verdict"] == "deny"
    assert payload["user_id"] == "u-9"
    assert payload["endpoint_id"] == "ep-1"
    assert payload["knowledge_base_ids"] == ["kb-a", "kb-x"]
    assert payload["knowledge_id"] == "doc-1"
    assert payload["deny_reason"] == REASON_RATE_LIMITED
