"""测试共用假件：端点/主体上下文、放行 FakeGuard、库摘要构造。

unit 测试（test_retrieve，内存假后端）与契约测试（test_backend_contract，
respx 打真 HTTP）共用同一套身份假件，保证两层测的是同一条裁决前提。
"""

from __future__ import annotations

from contextlib import contextmanager

from app.auth.middleware import Principal, reset_principal, set_principal
from app.backend.kb import KBSummary
from app.guards import EffectiveScope, GuardVerdict
from app.stores.endpoints import EndpointRecord

TENANT = 77

READONLY_TOOL_NAMES = (
    "list_knowledge_bases",
    "search_knowledge",
    "grep_chunks",
    "list_documents",
    "read_document",
)


def endpoint(kb_ids=(), tools=READONLY_TOOL_NAMES, *, rate: int = 60) -> EndpointRecord:
    return EndpointRecord(
        id="ep-1", tenant_id=TENANT, name="ep", enabled=True,
        knowledge_base_ids=tuple(kb_ids), tools=tuple(tools),
        default_agent_id="", rate_limit_per_minute=rate,
    )


def kb_summary(
    kb_id: str,
    *,
    name: str = "",
    kb_type: str = "document",
    caps: tuple[str, ...] = ("semantic", "keyword"),
    tenant: int = TENANT,
) -> KBSummary:
    return KBSummary(
        id=kb_id,
        name=name or f"KB {kb_id}",
        description="",
        type=kb_type,
        tenant_id=tenant,
        capabilities=frozenset(caps),
    )


# 工具单测的默认生效范围（闭集）：覆盖各用例里假后端会返回的库 id；
# 并集模型下不再有"不受限"形态，越权用例一律显式构造小 scope。
DEFAULT_TEST_SCOPE = EffectiveScope(
    kb_ids=frozenset({"kb-1", "kb-2", "kb-3", "kb-f", "kb-k", "kb-w"})
)


class FakeGuard:
    """记录调用并按构造的 scope 放行；deny 非空时一律拒绝。"""

    def __init__(self, scope: EffectiveScope | None = None, *, deny: str = "") -> None:
        self.scope = scope or DEFAULT_TEST_SCOPE
        self.deny = deny
        self.calls: list[tuple[str, dict]] = []

    async def authorize(self, principal, tool_name, arguments):
        self.calls.append((tool_name, dict(arguments)))
        if self.deny:
            return GuardVerdict(False, self.deny)
        return GuardVerdict(True, "", self.scope)


@contextmanager
def principal_context(kb_ids=()):
    """把一个机器主体（端点公共书架=kb_ids，默认空书架=零库）放进 contextvars。"""
    token = set_principal(Principal(endpoint=endpoint(kb_ids=kb_ids), user_id=None))
    try:
        yield
    finally:
        reset_principal(token)
