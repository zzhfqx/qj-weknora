"""ask 多轮会话的不透明票据（HMAC 自签名，无状态表）。

设计见 需求.md §4.2：侧车对所有端点共用一个上游 API Key，WeKnora 侧的
会话归属无法区分端点/用户，因此多轮 ask 的 session_id 绝不能下发真实值。
票据内含真实 sid + 端点 id + 用户 id（机器主体为空串），HMAC-SHA256 签名；
续问时验签并重新绑定端点/用户，即完成归属校验：
  - 篡改 → 验签失败；
  - A 端点拿到的票据拿到 B 端点用 → 绑定不一致拒绝；
  - A 用户的票据给 B 用户 → 同样拒绝。
票据不带过期时间：WeKnora 会话本身长期有效，与 Go 内置 MCP 行为一致。
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json

# 票据版本前缀，便于将来无损换算法
_VERSION = "t1"


class InvalidSessionTicket(Exception):
    """票据缺失/畸形/签名错/绑定不一致；对外统一英文文案由调用方组装。"""


def _b64encode(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _b64decode(text: str) -> bytes:
    padding = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + padding)


def issue_session_ticket(secret: str, sid: str, endpoint_id: str, user_id: str | None) -> str:
    """签发不透明票据。secret 未配置属于部署错误，直接 fail-closed。"""
    if not secret:
        raise RuntimeError("SIDECAR_SESSION_SECRET is not configured")
    payload = json.dumps(
        {"sid": sid, "ep": endpoint_id, "uid": user_id or ""},
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    body = _b64encode(payload)
    signature = hmac.new(secret.encode("utf-8"), body.encode("ascii"), hashlib.sha256).digest()
    return f"{_VERSION}.{body}.{_b64encode(signature)}"


def verify_session_ticket(
    secret: str, ticket: str, endpoint_id: str, user_id: str | None
) -> str:
    """验签并校验端点/用户绑定；成功返回真实 sid，失败抛 InvalidSessionTicket。"""
    if not secret:
        raise RuntimeError("SIDECAR_SESSION_SECRET is not configured")
    try:
        version, body, signature_b64 = ticket.split(".", 2)
    except (AttributeError, ValueError) as exc:
        raise InvalidSessionTicket("malformed session ticket") from exc
    if version != _VERSION:
        raise InvalidSessionTicket("unsupported session ticket version")

    expected = hmac.new(secret.encode("utf-8"), body.encode("ascii"), hashlib.sha256).digest()
    try:
        provided = _b64decode(signature_b64)
    except (ValueError, TypeError) as exc:
        raise InvalidSessionTicket("malformed session ticket signature") from exc
    if not hmac.compare_digest(expected, provided):
        raise InvalidSessionTicket("bad session ticket signature")

    try:
        claims = json.loads(_b64decode(body))
    except (ValueError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise InvalidSessionTicket("malformed session ticket payload") from exc
    if not isinstance(claims, dict) or not isinstance(claims.get("sid"), str):
        raise InvalidSessionTicket("malformed session ticket claims")
    if claims.get("ep") != endpoint_id or claims.get("uid", "") != (user_id or ""):
        # 票据真实但不属于本端点/本用户：与畸形同等级别拒绝，不提示差异
        raise InvalidSessionTicket("session ticket is not bound to this endpoint or user")
    sid = claims["sid"].strip()
    if not sid:
        raise InvalidSessionTicket("session ticket carries an empty session id")
    return sid
