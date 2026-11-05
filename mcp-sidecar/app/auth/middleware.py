"""ASGI 鉴权中间件：端点反查、用户票验签、凭证自识别、请求身份注入。

本文件按方案 §六 集中三块内容（不再拆文件）：
  1. 千机用户票 JWT 验签（与 Go 端 mcp_user_ticket.go 对齐）；
  2. 请求主体 Principal 与 contextvars 注入；
  3. 纯 ASGI 鉴权中间件（参照官方 Python server 的 MCPAuthMiddleware 写法，
     不使用 BaseHTTPMiddleware，避免对流式响应的缓冲副作用）。

凭证自识别（2026-11-03 修订，取代 identity.yaml 模式名单）：
  同一个 /mcp 端点同时服务两类客户端，现场按凭证形态识别，不再按端点配置模式：
  - 仅 mcp_xxx（无票）= 机器模式，Principal.user_id=None，范围=公共书架；
  - mcp_xxx.<合法 JWT> = 用户模式，范围=公共书架 ∪ 个人授权；
  - 带了票但验签失败/过期/交叉绑定不符 = 一律 401，绝不降级为机器模式
    （否则攻击者可塞乱票探测、蹭公共书架）。
  过渡期票也可来自旧 X-User-Credential 头（同规则：坏票 401）。
机器范围恒为用户范围的真子集，网关漏发票只缩权不越权（见需求.md §4.2 修订）。
所有错误对客户端统一 401 invalid credential（不区分是哪一段错，防分段探测）。
"""

from __future__ import annotations

import contextvars
import json
import logging
from dataclasses import dataclass
from typing import Any

import jwt

from app.auth.credentials import split_bearer
from app.stores.endpoints import EndpointRecord, EndpointStore

logger = logging.getLogger("sidecar.auth")

# ---------------------------------------------------------------------------
# 1) 用户票验签（对齐 Go: internal/application/service/mcp_user_ticket.go）
# ---------------------------------------------------------------------------

# 与 Go 端 mcpUserTicketType 常量一致
TICKET_TYPE = "mcp_user_identity"
# 容忍的签发/到达时钟偏差（秒）
DEFAULT_TICKET_LEEWAY_SECONDS = 30
# 过渡期旧头名（Go 端 mcpUserCredentialHeader）
LEGACY_TICKET_HEADER = b"x-user-credential"


class TicketError(Exception):
    """票据校验失败统一异常（对外不区分子类，防探测）。"""


@dataclass(frozen=True)
class TicketClaims:
    user_id: str
    tenant_id: int
    endpoint_id: str


def verify_user_ticket(
    raw: str,
    *,
    secret: str,
    expected_endpoint_id: str,
    expected_tenant_id: int,
    leeway_seconds: int = DEFAULT_TICKET_LEEWAY_SECONDS,
) -> TicketClaims:
    """校验 HS256 用户票并做端点/工作空间交叉比对；任何不符抛 TicketError。"""
    if not secret:
        # 侧车未配置 JWT_SECRET 时绝不放行（Go 端缺省会随机生成，两侧必然不一致）
        raise TicketError("jwt secret not configured")
    try:
        # 显式算法白名单，防 alg=none / RS→HS 混淆攻击
        claims = jwt.decode(
            raw,
            secret,
            algorithms=["HS256"],
            # aud 在此关闭自动校验：aud 是端点 id 而非常规受众字符串，
            # 由本函数在下方与 expected_endpoint_id 手工交叉比对
            options={"require": ["exp", "iat", "sub"], "verify_aud": False},
            leeway=leeway_seconds,
        )
    except jwt.PyJWTError as exc:
        raise TicketError(str(exc)) from exc

    # 用途隔离：拒绝登录 access token 等其它类型票据
    if claims.get("type") != TICKET_TYPE:
        raise TicketError("unexpected ticket type")

    user_id = str(claims.get("sub", "")).strip()
    endpoint_id = str(claims.get("aud", "")).strip()
    if not user_id or not endpoint_id:
        raise TicketError("missing sub/aud")
    try:
        tenant_id = int(claims.get("tid", 0))
    except (TypeError, ValueError):
        tenant_id = 0
    if tenant_id <= 0:
        raise TicketError("invalid tid")

    # 交叉校验：票必须签给"这个工作空间的这个端点"（Go 中间件同款比对）
    if tenant_id != expected_tenant_id or endpoint_id != expected_endpoint_id:
        raise TicketError("ticket scope mismatch")

    return TicketClaims(user_id=user_id, tenant_id=tenant_id, endpoint_id=endpoint_id)


# ---------------------------------------------------------------------------
# 2) 请求主体与 contextvars
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Principal:
    """一次 /mcp 请求经鉴权后确定的调用主体。"""

    endpoint: EndpointRecord
    # 机器调用为 None；用户调用为已验签的外部用户 id
    user_id: str | None

    @property
    def is_user(self) -> bool:
        return bool(self.user_id)


_current: contextvars.ContextVar[Principal | None] = contextvars.ContextVar(
    "mcp_sidecar_principal", default=None
)


def set_principal(principal: Principal) -> contextvars.Token:
    return _current.set(principal)


def reset_principal(token: contextvars.Token) -> None:
    _current.reset(token)


def current_principal() -> Principal | None:
    return _current.get()


def current_user_id() -> str | None:
    """工具层取当前外部用户 id；机器调用返回 None。

    user_id 的唯一可信来源是验签通过的票据，绝不从工具参数/URL 取。
    """
    principal = _current.get()
    return principal.user_id if principal else None


# ---------------------------------------------------------------------------
# 3) ASGI 鉴权中间件
# ---------------------------------------------------------------------------

# 对外统一错误文案（与 Go 端风格接近，但不暴露具体失败环节）
_MSG_UNAUTHORIZED = "unauthorized: invalid credential"
_MSG_MISSING = "unauthorized: missing bearer token"
_MSG_DISABLED = "mcp endpoint disabled"
_MSG_UNAVAILABLE = "authentication unavailable"
# 注：工作空间停用检查在内置 Go MCP 中查 tenants 表完成；侧车只读账号没有该
# 表权限，停用工作空间在调 REST 时由后端自身拒绝（X-API-Key 校验兜底）。


class MCPAuthMiddleware:
    """包裹 MCP ASGI 应用：在请求进入工具层之前完成全部身份裁决。"""

    def __init__(
        self,
        app: Any,
        *,
        endpoint_store: EndpointStore,
        jwt_secret: str,
        protected_path: str = "/mcp",
        leeway_seconds: int = DEFAULT_TICKET_LEEWAY_SECONDS,
    ) -> None:
        self.app = app
        self._endpoints = endpoint_store
        self._jwt_secret = jwt_secret
        self._protected_path = protected_path
        self._leeway = leeway_seconds

    async def __call__(self, scope: dict, receive, send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        # 健康检查等非 MCP 路径直接放行（custom_route 挂的 /healthz）
        if not scope.get("path", "").startswith(self._protected_path):
            await self.app(scope, receive, send)
            return

        headers = {name: value for name, value in scope.get("headers", [])}
        principal = await self._authenticate(headers)
        if isinstance(principal, _Reject):
            await _send_reject(send, principal)
            return

        context_token = set_principal(principal)
        try:
            await self.app(scope, receive, send)
        finally:
            reset_principal(context_token)

    async def _authenticate(self, headers: dict[bytes, bytes]) -> "Principal | _Reject":
        """裁决的纯逻辑主体；返回 Principal 或拒绝 Response。"""
        raw_auth = headers.get(b"authorization")
        credential = split_bearer(_decode_header(raw_auth))
        if credential is None:
            if not raw_auth:
                return _Reject(401, _MSG_MISSING, "weknora-mcp")
            return _Reject(401, _MSG_UNAUTHORIZED, "weknora-mcp")

        # ① 端点令牌反查（单 URL：身份由令牌哈希决定）
        try:
            endpoint = await self._endpoints.get_by_token(credential.endpoint_token)
        except Exception:  # noqa: BLE001 — DB 不可用时 fail-closed，细节只进日志
            logger.exception("[mcp-auth] endpoint lookup failed")
            return _Reject(503, _MSG_UNAVAILABLE)
        if endpoint is None:
            return _Reject(401, _MSG_UNAUTHORIZED, "weknora-mcp")
        if not endpoint.enabled:
            return _Reject(403, _MSG_DISABLED)

        # ② 凭证自识别：无票=机器模式；带票=用户模式（坏票一律 401，不降级）。
        #    票来源：复合 Bearer 段优先；过渡期也接受旧 X-User-Credential 头。
        ticket = credential.user_ticket
        if ticket is None:
            ticket = _decode_header(headers.get(LEGACY_TICKET_HEADER)) or None

        if ticket is None:
            return Principal(endpoint=endpoint, user_id=None)

        claims = self._verify(ticket, endpoint)
        if claims is None:
            return _Reject(401, _MSG_UNAUTHORIZED, "weknora-mcp-user")
        return Principal(endpoint=endpoint, user_id=claims.user_id)

    def _verify(self, ticket: str, endpoint: EndpointRecord) -> TicketClaims | None:
        try:
            return verify_user_ticket(
                ticket,
                secret=self._jwt_secret,
                expected_endpoint_id=endpoint.id,
                expected_tenant_id=endpoint.tenant_id,
                leeway_seconds=self._leeway,
            )
        except TicketError as exc:
            logger.info("[mcp-auth] user ticket rejected: endpoint=%s reason=%s", endpoint.id, exc)
            return None


@dataclass(frozen=True)
class _Reject:
    """裁决拒绝结果（鉴权内部使用，不暴露给工具层）。"""

    status: int
    message: str
    realm: str | None = None


async def _send_reject(send, rejection: _Reject) -> None:
    """直接以 ASGI 原语发拒绝响应，不经 Starlette 响应栈，对流式面零副作用。"""
    body = json.dumps({"error": rejection.message}).encode("utf-8")
    headers = [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode())]
    if rejection.realm:
        headers.append((b"www-authenticate", f'Bearer realm="{rejection.realm}"'.encode("ascii")))
    await send({"type": "http.response.start", "status": rejection.status, "headers": headers})
    await send({"type": "http.response.body", "body": body})


def _decode_header(value: bytes | None) -> str:
    if value is None:
        return ""
    return value.decode("latin-1", errors="replace").strip()
