"""WeKnora MCP 侧车（mcp-sidecar）。

独立 Python 进程：对 MCP 客户端提供 Streamable HTTP 服务，对 WeKnora 后端
只走公开 REST（/api/v1），并只读直连 mcp_endpoints / mcp_user_permission
两张二开自有表。v1 只提供只读 9 工具，不做任何写库操作。

设计文档：指南/变更文档/MCP/需求.md
"""

__version__ = "0.1.0"
