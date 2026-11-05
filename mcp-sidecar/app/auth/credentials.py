"""复合 Bearer 凭证切分（纯函数，无 I/O，先有单测钉死）。

凭证两种形态（需求.md §4.1）：
  1. 机器凭证：   mcp_xxxx                         （无点）
  2. 复合凭证：   mcp_xxxx.<JWT>                   （第一个点之后为用户票）

无歧义的依据：端点令牌由 Go 侧用 32 字节随机数做 base64.RawURLEncoding 生成
（字符集仅 A-Za-z0-9-_，见 Go: generateMCPEndpointToken），恒不含 "."；
而 JWT 序列化形态恒为 "头.载荷.签名" 三段。因此在第一个 "." 处切分即可。
"""

from __future__ import annotations

from dataclasses import dataclass

# 与 Go 端 types.MCPEndpointTokenPrefix 保持一致
ENDPOINT_TOKEN_PREFIX = "mcp_"
_BEARER_PREFIX = "bearer "


@dataclass(frozen=True)
class SplitCredential:
    """切分后的凭证：端点令牌必有；用户票在机器调用时为 None。"""

    endpoint_token: str
    user_ticket: str | None


def split_bearer(raw: str | None) -> SplitCredential | None:
    """从 Authorization 头值切出端点令牌与可选用户票。

    返回 None 表示凭证形态非法（调用方一律按 401 处理，fail-closed）：
      - 头缺失/为空；
      - 不是 Bearer 方案；
      - Bearer 后为空或含空白；
      - 端点令牌部分不是 mcp_ 前缀；
      - 复合票的 JWT 段为空。
    """
    if not raw:
        return None
    value = raw.strip()
    if len(value) <= 7 or value[:7].lower() != _BEARER_PREFIX:
        return None

    credential = value[7:].strip()
    if not credential or any(ch.isspace() for ch in credential):
        return None
    if not credential.startswith(ENDPOINT_TOKEN_PREFIX):
        return None

    # 端点令牌不含点；第一个点必是 "端点票.据" 的分界
    if "." not in credential:
        return SplitCredential(endpoint_token=credential, user_ticket=None)

    endpoint_token, user_ticket = credential.split(".", 1)
    user_ticket = user_ticket.strip()
    # 左段仍必须是合法形态的端点令牌；右段不能为空
    if not endpoint_token.startswith(ENDPOINT_TOKEN_PREFIX) or not user_ticket:
        return None
    return SplitCredential(endpoint_token=endpoint_token, user_ticket=user_ticket)
