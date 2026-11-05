"""ask 会话票据单测：HMAC 签名、端点/用户绑定、篡改与跨上下文拒绝。"""

from __future__ import annotations

import pytest

from app import session_token as st


SECRET = "sidecar-session-secret-0123456789abcdef"
SID = "weknora-session-uuid"
EP = "ep-1"
USER = "user-123"


def test_round_trip_machine_and_user_principal():
    ticket = st.issue_session_ticket(SECRET, SID, EP, None)
    assert st.verify_session_ticket(SECRET, ticket, EP, None) == SID
    # 机器主体的 uid 空串不能被用户主体冒用
    with pytest.raises(st.InvalidSessionTicket):
        st.verify_session_ticket(SECRET, ticket, EP, USER)

    user_ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    assert st.verify_session_ticket(SECRET, user_ticket, EP, USER) == SID
    with pytest.raises(st.InvalidSessionTicket):
        st.verify_session_ticket(SECRET, user_ticket, EP, None)


def test_ticket_is_opaque_and_does_not_leak_sid():
    ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    # 不透明票据：真实 sid 不以明文出现（b64 只是编码，签名防篡改）
    assert SID not in ticket
    assert ticket.count(".") == 2


def test_wrong_secret_rejected():
    ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    with pytest.raises(st.InvalidSessionTicket, match="bad session ticket signature"):
        st.verify_session_ticket("another-secret-aaaaaaaaaaaaaaaa", ticket, EP, USER)


def test_tampered_body_rejected():
    ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    version, body, signature = ticket.split(".")
    tampered = f"{version}.{body}x.{signature}"
    with pytest.raises(st.InvalidSessionTicket):
        st.verify_session_ticket(SECRET, tampered, EP, USER)
    # 只改签名同样拒绝
    with pytest.raises(st.InvalidSessionTicket):
        st.verify_session_ticket(SECRET, f"{version}.{body}.AAAA", EP, USER)


def test_cross_endpoint_and_cross_user_rejected():
    ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    with pytest.raises(st.InvalidSessionTicket, match="not bound"):
        st.verify_session_ticket(SECRET, ticket, "ep-2", USER)
    with pytest.raises(st.InvalidSessionTicket, match="not bound"):
        st.verify_session_ticket(SECRET, ticket, EP, "user-other")


def test_malformed_tickets_rejected():
    for bad in ("", "garbage", "t1.abc", "v9.abc.def", "t1.###.def"):
        with pytest.raises(st.InvalidSessionTicket):
            st.verify_session_ticket(SECRET, bad, EP, USER)


def test_missing_secret_fails_closed():
    with pytest.raises(RuntimeError):
        st.issue_session_ticket("", SID, EP, USER)
    ticket = st.issue_session_ticket(SECRET, SID, EP, USER)
    with pytest.raises(RuntimeError):
        st.verify_session_ticket("", ticket, EP, USER)
