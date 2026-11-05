"""工具调用裁决（guard）与结构化审计。

裁决顺序对齐 Go 端 internal/mcpserver/server.go 的 guardTool：
  1. 端点工具白名单（隐藏工具按名直调同样拒绝）；
  2. 用户身份与授权（用户模式：授权表读取失败 fail-closed）；
  3. 每端点每分钟滑窗限流；
  4. 每次调用（允许或拒绝）输出一条 JSON 审计。

生效库范围（2026-11-03 修订，单端点凭证自识别 + 公共书架并集模型）：
  - 机器主体（裸端点令牌，无 JWT）：scope = 端点 knowledge_base_ids
    （"公共书架"；留空=一个库都不给，不再是"工作空间全部库"）；
  - 用户主体（复合票验签通过）：scope = 端点公共书架 ∪ 该用户在
    mcp_user_permission 中的个人授权；个人授权为空时仍可见公共书架；
    授权表读取失败 fail-closed，不降级为仅公共书架。
  - 机器范围恒为用户范围的子集：网关漏发票只缩权、不越权。
具体到"本次调用的库/文档 id 是否落在范围内"，由工具层用
EffectiveScope.allows() 逐个执行。

注意：本模型与待下线的内置 Go MCP 的交集语义刻意不同（见需求.md §4.2 修订）。
"""

from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass

from app.auth.middleware import Principal
from app.ratelimit import SlidingWindowLimiter

logger = logging.getLogger("sidecar.audit")

# 审计拒绝原因常量（与 Go 端 deny_reason 文案对齐，便于两边日志统一检索）
REASON_TOOL_DISABLED = "tool_disabled"
REASON_PERMISSION_UNAVAILABLE = "identity_or_permission_unavailable"
REASON_RATE_LIMITED = "rate_limited"


@dataclass(frozen=True)
class EffectiveScope:
    """本次请求生效的知识库范围（闭集，id 级判定）。"""

    kb_ids: frozenset[str]

    def allows(self, kb_id: str) -> bool:
        return kb_id.strip() in self.kb_ids


@dataclass(frozen=True)
class GuardVerdict:
    allowed: bool
    deny_reason: str = ""
    scope: EffectiveScope | None = None


class ToolGuard:
    """tools/call 的统一裁决器；工具包装器在执行前调用 authorize()。"""

    def __init__(self, permission_store, limiter: SlidingWindowLimiter) -> None:
        self._permissions = permission_store
        self._limiter = limiter

    async def authorize(self, principal: Principal, tool_name: str, arguments: dict) -> GuardVerdict:
        """完整裁决；任何拒绝路径都已写审计。"""
        endpoint = principal.endpoint

        # ① 端点工具白名单
        if not endpoint.has_tool(tool_name):
            self.audit(principal, tool_name, arguments, False, REASON_TOOL_DISABLED)
            return GuardVerdict(False, REASON_TOOL_DISABLED)

        # ② 身份/授权 → 生效范围
        try:
            scope = await self._scope_for(principal)
        except _PermissionUnavailable:
            self.audit(principal, tool_name, arguments, False, REASON_PERMISSION_UNAVAILABLE)
            return GuardVerdict(False, REASON_PERMISSION_UNAVAILABLE)

        # ③ 限流（预算来自端点行；失败路径由限流器内部降级，不在这里抛错）
        if not await self._limiter.allow(endpoint.id, endpoint.rate_limit_per_minute):
            self.audit(principal, tool_name, arguments, False, REASON_RATE_LIMITED)
            return GuardVerdict(False, REASON_RATE_LIMITED)

        # ④ 放行审计（scope 可能为空集：公共书架与个人授权都为空，
        #    工具层会以"本端点无可用知识库"拒绝具体调用）
        self.audit(principal, tool_name, arguments, True, "")
        return GuardVerdict(True, scope=scope)

    async def list_scope(self, principal: Principal) -> EffectiveScope | None:
        """tools/list 的可见性裁决：返回 None 表示授权服务不可用（fail-closed）。

        授权为空不再返回 None：零授权用户仍可看到公共书架上的库（并集模型）。
        """
        try:
            return await self._scope_for(principal)
        except _PermissionUnavailable:
            logger.warning(
                "[mcp-audit] tools/list denied: endpoint=%s reason=%s",
                principal.endpoint.id, REASON_PERMISSION_UNAVAILABLE,
            )
            return None

    async def _scope_for(self, principal: Principal) -> EffectiveScope:
        endpoint = principal.endpoint
        # 公共书架：任何机器/用户凭端点钥匙都能访问的库
        public_set = frozenset(kb.strip() for kb in endpoint.knowledge_base_ids if kb.strip())

        if principal.user_id is None:
            # 机器模式：只有公共书架；留空=零库（不再有 unrestricted 语义）
            return EffectiveScope(kb_ids=public_set)

        # 用户模式：公共书架 ∪ 个人授权
        try:
            user_kbs = await self._permissions.get_user_knowledge_bases(
                endpoint.tenant_id, principal.user_id,
            )
        except Exception as exc:  # noqa: BLE001 — DB 不可用必须 fail-closed
            logger.warning(
                "加载用户授权失败：endpoint=%s user=%s err=%s",
                endpoint.id, principal.user_id, exc,
            )
            raise _PermissionUnavailable from exc

        user_set = frozenset(kb.strip() for kb in user_kbs if kb.strip())
        return EffectiveScope(kb_ids=public_set | user_set)

    @staticmethod
    def audit(
        principal: Principal, tool_name: str, arguments: dict,
        allowed: bool, deny_reason: str,
    ) -> None:
        """每次 tools/call 一条 JSON 审计（字段对齐 Go auditToolCall）。"""
        arguments = arguments or {}
        payload: dict = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            "user_id": principal.user_id,
            "endpoint_id": principal.endpoint.id,
            "tool": tool_name,
            "verdict": "allow" if allowed else "deny",
        }
        # 单值/数组两种参数形态都记录，便于按库/文档追溯
        kb_id = str(arguments.get("knowledge_base_id", "")).strip()
        if kb_id:
            payload["knowledge_base_id"] = kb_id
        kb_ids = arguments.get("knowledge_base_ids")
        if isinstance(kb_ids, list) and kb_ids:
            payload["knowledge_base_ids"] = kb_ids
        doc_id = str(arguments.get("knowledge_id", "")).strip()
        if doc_id:
            payload["knowledge_id"] = doc_id
        doc_ids = arguments.get("knowledge_ids")
        if isinstance(doc_ids, list) and doc_ids:
            payload["knowledge_ids"] = doc_ids
        if not allowed and deny_reason:
            payload["deny_reason"] = deny_reason

        line = json.dumps(payload, ensure_ascii=False)
        if allowed:
            logger.info("[mcp-audit] tool call allowed %s", line)
        else:
            logger.warning("[mcp-audit] tool call denied %s", line)


class _PermissionUnavailable(Exception):
    """授权存储不可用的内部信号（对外统一文案，不泄露细节）。"""
