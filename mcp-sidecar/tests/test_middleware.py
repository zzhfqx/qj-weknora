"""鉴权中间件裁决矩阵 + ASGI 链路单测（不连真实数据库）。

2026-11-03 起为"单端点凭证自识别"模型：不再有模式名单，
任何端点都同时接受裸令牌（机器）与复合票（用户），坏票一律 401 不降级。
"""

from __future__ import annotations

import time

import jwt
import pytest

from app.auth.middleware import (
    TICKET_TYPE,
    MCPAuthMiddleware,
    Principal,
    _Reject,
    current_principal,
)
from app.stores.endpoints import EndpointRecord

# 测试密钥故意取 ≥32 字节，贴近生产 JWT_SECRET 应有的强度
SECRET = "test-secret-0123456789abcdef-0123456789"

EP_1 = "ep-1"
EP_2 = "ep-2"
TENANT = 77


def _endpoint(endpoint_id: str, *, enabled: bool = True) -> EndpointRecord:
    return EndpointRecord(
        id=endpoint_id,
        tenant_id=TENANT,
        name="测试端点",
        enabled=enabled,
        knowledge_base_ids=(),
        tools=("list_knowledge_bases",),
        default_agent_id="",
        rate_limit_per_minute=60,
    )


class FakeEndpointStore:
    """按令牌返回端点行的假仓储；可模拟 DB 故障。"""

    def __init__(self) -> None:
        self.rows: dict[str, EndpointRecord] = {
            "mcp_token_a": _endpoint(EP_1),
            "mcp_token_b": _endpoint(EP_2),
            "mcp_disabled": _endpoint("ep-disabled", enabled=False),
        }
        self.raise_error = False

    async def get_by_token(self, token: str):
        if self.raise_error:
            raise RuntimeError("database down")
        return self.rows.get(token)


def _ticket(*, endpoint_id: str = EP_1, tenant_id: int = TENANT, user: str = "u-1",
            secret: str = SECRET, ttl: int = 300) -> str:
    now = int(time.time())
    return jwt.encode(
        {
            "type": TICKET_TYPE, "sub": user, "tid": tenant_id,
            "aud": endpoint_id, "jti": "j", "iat": now, "exp": now + ttl,
        },
        secret,
        algorithm="HS256",
    )


def _middleware(store) -> MCPAuthMiddleware:
    return MCPAuthMiddleware(app=None, endpoint_store=store, jwt_secret=SECRET)


def _auth_header(value: str) -> dict[bytes, bytes]:
    return {b"authorization": value.encode("latin-1")}


# ---------- 凭证自识别：同一个端点，两种模式 ----------

@pytest.mark.asyncio
async def test_bare_token_is_machine_on_any_endpoint():
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header("Bearer mcp_token_a"))
    assert isinstance(result, Principal)
    assert result.user_id is None
    assert result.endpoint.id == EP_1


@pytest.mark.asyncio
async def test_composite_ticket_is_user_on_any_endpoint():
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{_ticket()}"))
    assert isinstance(result, Principal)
    assert result.user_id == "u-1"
    assert result.endpoint.id == EP_1


@pytest.mark.asyncio
async def test_bare_and_composite_equally_accepted_on_other_endpoint():
    # 不存在"机器端点/用户端点"之分：EP_2 上两种凭证都合法
    mw = _middleware(FakeEndpointStore())
    bare = await mw._authenticate(_auth_header("Bearer mcp_token_b"))
    assert isinstance(bare, Principal) and bare.user_id is None
    composite = await mw._authenticate(
        _auth_header(f"Bearer mcp_token_b.{_ticket(endpoint_id=EP_2, user='u-2')}")
    )
    assert isinstance(composite, Principal) and composite.user_id == "u-2"


# ---------- 带票必验：坏票 401，绝不降级机器 ----------

@pytest.mark.asyncio
async def test_garbage_ticket_segment_returns_401():
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header("Bearer mcp_token_a.not-a-jwt"))
    assert isinstance(result, _Reject) and result.status == 401


@pytest.mark.asyncio
async def test_expired_ticket_returns_401():
    expired = _ticket(ttl=-600)
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{expired}"))
    assert isinstance(result, _Reject) and result.status == 401


@pytest.mark.asyncio
async def test_wrong_signature_ticket_returns_401():
    forged = _ticket(secret="another-secret-0123456789abcdef-0123456789")
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{forged}"))
    assert isinstance(result, _Reject) and result.status == 401


@pytest.mark.asyncio
async def test_cross_endpoint_ticket_returns_401():
    # 票签给 EP_2，却配 EP_1 的令牌：aud 交叉绑定不符
    foreign = _ticket(endpoint_id=EP_2)
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{foreign}"))
    assert isinstance(result, _Reject) and result.status == 401


@pytest.mark.asyncio
async def test_cross_tenant_ticket_returns_401():
    foreign = _ticket(tenant_id=TENANT + 1)
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{foreign}"))
    assert isinstance(result, _Reject) and result.status == 401


@pytest.mark.asyncio
async def test_wrong_type_ticket_returns_401():
    now = int(time.time())
    access_token = jwt.encode(
        {"sub": "u-1", "tid": TENANT, "aud": EP_1, "iat": now, "exp": now + 300},
        SECRET, algorithm="HS256",
    )
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header(f"Bearer mcp_token_a.{access_token}"))
    assert isinstance(result, _Reject) and result.status == 401


# ---------- 过渡期旧头 X-User-Credential ----------

@pytest.mark.asyncio
async def test_legacy_header_valid_ticket_is_user():
    mw = _middleware(FakeEndpointStore())
    headers = {
        b"authorization": b"Bearer mcp_token_a",
        b"x-user-credential": _ticket().encode(),
    }
    result = await mw._authenticate(headers)
    assert isinstance(result, Principal) and result.user_id == "u-1"


@pytest.mark.asyncio
async def test_legacy_header_bad_ticket_returns_401():
    mw = _middleware(FakeEndpointStore())
    headers = {
        b"authorization": b"Bearer mcp_token_a",
        b"x-user-credential": b"garbage",
    }
    result = await mw._authenticate(headers)
    assert isinstance(result, _Reject) and result.status == 401


# ---------- 通用失败路径 ----------

@pytest.mark.asyncio
async def test_missing_and_malformed_authorization():
    mw = _middleware(FakeEndpointStore())
    assert (await mw._authenticate({})).status == 401
    assert (await mw._authenticate(_auth_header("Basic abc"))).status == 401
    assert (await mw._authenticate(_auth_header("Bearer sk_apikey"))).status == 401


@pytest.mark.asyncio
async def test_unknown_token_generic_401():
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header("Bearer mcp_unknown"))
    assert isinstance(result, _Reject) and result.status == 401
    # 不区分"端点不存在"与"令牌错误"，防探测
    assert "invalid credential" in result.message


@pytest.mark.asyncio
async def test_disabled_endpoint_403():
    mw = _middleware(FakeEndpointStore())
    result = await mw._authenticate(_auth_header("Bearer mcp_disabled"))
    assert isinstance(result, _Reject) and result.status == 403


@pytest.mark.asyncio
async def test_db_outage_fails_closed_503():
    store = FakeEndpointStore()
    store.raise_error = True
    mw = _middleware(store)
    result = await mw._authenticate(_auth_header("Bearer mcp_token_a"))
    assert isinstance(result, _Reject) and result.status == 503


# ---------- ASGI 全链路 ----------

@pytest.mark.asyncio
async def test_asgi_healthz_passthrough_and_mcp_401():
    from httpx import ASGITransport, AsyncClient

    async def inner(scope, receive, send):
        body = b'{"ok":true}'
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/json")]})
        await send({"type": "http.response.body", "body": body})

    app = MCPAuthMiddleware(inner, endpoint_store=FakeEndpointStore(), jwt_secret=SECRET)
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        # 非 /mcp 路径不带凭证也放行
        r = await client.get("/healthz")
        assert r.status_code == 200
        # /mcp 无凭证 → 401 JSON
        r = await client.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        assert r.status_code == 401
        assert r.json()["error"].startswith("unauthorized")


@pytest.mark.asyncio
async def test_asgi_principal_visible_to_inner_and_reset():
    from httpx import ASGITransport, AsyncClient

    seen: list[str | None] = []

    async def inner(scope, receive, send):
        principal = current_principal()
        seen.append(principal.user_id if principal else None)
        body = b"{}"
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/json")]})
        await send({"type": "http.response.body", "body": body})

    app = MCPAuthMiddleware(inner, endpoint_store=FakeEndpointStore(), jwt_secret=SECRET)
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        r = await client.post(
            "/mcp",
            headers={"Authorization": f"Bearer mcp_token_a.{_ticket()}"},
            content=b"{}",
        )
        assert r.status_code == 200

    assert seen == ["u-1"]
    # 请求结束后 contextvars 已复位，不向后续请求泄漏身份
    assert current_principal() is None
