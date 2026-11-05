"""9 个端点工具的 annotations 冻结测试。

对照 Go internal/mcpserver/tools_annotations_test.go：
  - 8 个只读工具：readOnly=true、destructive=false、idempotent=true、openWorld=false；
  - ask：readOnly=false，且保持 mcp-go SDK 默认（destructive=true、openWorld=true），
    只有 idempotent 缺省 false。
Python SDK 的 ToolAnnotations 字段缺省为 None（序列化省略），因此这里按
"显式声明值"断言，缺省即失败，防止有人无意软化 ask 的默认提示。
"""

from __future__ import annotations

import pytest

from app.guards import ToolGuard
from app.ratelimit import SlidingWindowLimiter
from app.tools.chat import AskToolSet
from app.tools.retrieve import RetrieveToolSet
from app.tools.wiki import WikiToolSet
from mcp.server import MCPServer


class _FakePermissionStore:
    async def get_user_knowledge_bases(self, tenant_id: int, user_id: str):
        return ()

READONLY_TOOLS = {
    "list_knowledge_bases",
    "search_knowledge",
    "grep_chunks",
    "list_documents",
    "read_document",
    "wiki_search",
    "wiki_read_page",
    "wiki_index",
}


@pytest.fixture
def registered():
    server = MCPServer("annotations-test", version="0")
    guard = ToolGuard(_FakePermissionStore(), SlidingWindowLimiter(None))
    RetrieveToolSet(client=None, guard=guard).register(server)
    WikiToolSet(client=None, guard=guard).register(server)
    AskToolSet(client=None, guard=guard, session_secret="x", ask_timeout=1).register(server)
    return server


async def test_all_nine_tools_registered_with_frozen_names(registered):
    tools = await registered.list_tools()
    assert {t.name for t in tools} == READONLY_TOOLS | {"ask"}
    # 名字与 Go constants 逐字一致，且每个都有描述（冻结契约的一部分）
    for tool in tools:
        assert tool.description and tool.description.strip()


async def test_readonly_tools_carry_all_four_hints(registered):
    tools = {t.name: t for t in await registered.list_tools()}
    for name in READONLY_TOOLS:
        ann = tools[name].annotations
        assert ann is not None, name
        assert ann.read_only_hint is True, name
        assert ann.destructive_hint is False, name
        assert ann.idempotent_hint is True, name
        assert ann.open_world_hint is False, name


async def test_ask_matches_go_sdk_default_hints(registered):
    tools = {t.name: t for t in await registered.list_tools()}
    ann = tools["ask"].annotations
    assert ann is not None
    # Go TestMutatingEndpointToolsStayDestructive 冻结的取值
    assert ann.read_only_hint is False
    assert ann.destructive_hint is True
    assert ann.open_world_hint is True
