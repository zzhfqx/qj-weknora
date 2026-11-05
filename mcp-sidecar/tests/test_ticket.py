"""用户票验签单测：与 Go 端票据 claims 约定逐字段对齐。"""

from __future__ import annotations

import time

import jwt
import pytest

from app.auth.middleware import TICKET_TYPE, TicketError, verify_user_ticket

# 测试密钥故意取 ≥32 字节，贴近生产 JWT_SECRET 应有的强度
SECRET = "test-secret-0123456789abcdef-0123456789"
ENDPOINT_ID = "ep-1"
TENANT_ID = 77


def _make_ticket(**overrides) -> str:
    now = int(time.time())
    payload = {
        "type": TICKET_TYPE,
        "sub": "user-123",
        "tid": TENANT_ID,
        "aud": ENDPOINT_ID,
        "jti": "jti-1",
        "iat": now,
        "exp": now + 300,
    }
    payload.update(overrides)
    return jwt.encode(payload, SECRET, algorithm="HS256")


def test_valid_ticket_passes():
    claims = verify_user_ticket(
        _make_ticket(),
        secret=SECRET,
        expected_endpoint_id=ENDPOINT_ID,
        expected_tenant_id=TENANT_ID,
    )
    assert claims.user_id == "user-123"
    assert claims.tenant_id == TENANT_ID
    assert claims.endpoint_id == ENDPOINT_ID


def test_wrong_secret_rejected():
    token = _make_ticket()
    with pytest.raises(TicketError):
        verify_user_ticket(
            token, secret="other-secret-0123456789abcdef-0123",
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=TENANT_ID,
        )


def test_expired_ticket_rejected():
    past = int(time.time()) - 600
    token = _make_ticket(iat=past, exp=past + 300)
    with pytest.raises(TicketError):
        verify_user_ticket(
            token, secret=SECRET,
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=TENANT_ID,
        )


def test_wrong_type_rejected():
    # 模拟登录 access token 等其它票据混用
    token = _make_ticket(type="access")
    with pytest.raises(TicketError):
        verify_user_ticket(
            token, secret=SECRET,
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=TENANT_ID,
        )


def test_cross_endpoint_and_tenant_rejected():
    token = _make_ticket()
    with pytest.raises(TicketError):  # aud 不符（跨端点重放）
        verify_user_ticket(
            token, secret=SECRET,
            expected_endpoint_id="ep-other", expected_tenant_id=TENANT_ID,
        )
    with pytest.raises(TicketError):  # tid 不符（跨工作空间）
        verify_user_ticket(
            token, secret=SECRET,
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=78,
        )


def test_missing_secret_fails_closed():
    with pytest.raises(TicketError):
        verify_user_ticket(
            _make_ticket(), secret="",
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=TENANT_ID,
        )


def test_alg_none_rejected():
    # 手工构造 alg=none 的票据，算法白名单必须拒绝
    import base64

    def b64(raw: bytes) -> str:
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

    header = b64(b'{"alg":"none","typ":"JWT"}')
    now = int(time.time())
    import json
    payload = b64(json.dumps({
        "type": TICKET_TYPE, "sub": "u", "tid": TENANT_ID,
        "aud": ENDPOINT_ID, "iat": now, "exp": now + 300,
    }).encode())
    token = f"{header}.{payload}."
    with pytest.raises(TicketError):
        verify_user_ticket(
            token, secret=SECRET,
            expected_endpoint_id=ENDPOINT_ID, expected_tenant_id=TENANT_ID,
        )
