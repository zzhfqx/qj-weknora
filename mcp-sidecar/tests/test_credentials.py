"""复合 Bearer 切分单测：端点令牌无点、JWT 恒含点，第一个点切分。"""

from __future__ import annotations

from app.auth.credentials import split_bearer


def test_machine_token_no_dot():
    result = split_bearer("Bearer mcp_Az09-_Ab")
    assert result is not None
    assert result.endpoint_token == "mcp_Az09-_Ab"
    assert result.user_ticket is None


def test_composite_token_splits_at_first_dot():
    # JWT 自身含两个点；必须只在第一个点切一次，右段原样保留
    result = split_bearer("Bearer mcp_Az09.aaa.bbb.ccc")
    assert result is not None
    assert result.endpoint_token == "mcp_Az09"
    assert result.user_ticket == "aaa.bbb.ccc"


def test_scheme_case_insensitive_and_padding():
    result = split_bearer("  bearer mcp_Az09  ")
    assert result is not None
    assert result.endpoint_token == "mcp_Az09"


def test_reject_non_bearer():
    assert split_bearer("Basic xyz") is None
    assert split_bearer("mcp_Az09") is None
    assert split_bearer("") is None
    assert split_bearer(None) is None


def test_reject_wrong_prefix_and_whitespace():
    assert split_bearer("Bearer sk_abc") is None
    assert split_bearer("Bearer mcp_abc def") is None
    assert split_bearer("Bearer mcp_abc.") is None
    assert split_bearer("Bearer .jwtpart") is None
