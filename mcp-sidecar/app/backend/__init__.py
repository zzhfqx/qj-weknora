"""WeKnora REST 适配层（backend/）。

纪律（见需求.md §六）：
  - 本目录只做"调 REST + 解析返回包"，不做授权裁决、不感知 MCP 协议；
  - 上游 REST 形状变化时只改本目录，tools/ 编排层不动；
  - 全部使用工作空间 X-API-Key（部署时仅授 retrieve 只读能力）。
"""
