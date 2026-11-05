"""Wiki REST 适配：搜索、读页、目录三个裸响应接口。

对齐 Go：
  - GET /knowledgebase/:kb_id/wiki/search?q=&limit=
      → 裸对象 {"pages": [WikiPage...]}（无 success/data 信封）；
        q 缺失 400；正则非法时上游返回 500（由调用方决定是整错还是部分失败）。
  - GET /knowledgebase/:kb_id/wiki/pages/*slug
      → 裸 WikiPageDetail（WikiPage 全字段打平 + in_link_titles）；
        404 表示该 KB 内无此 slug（跨 KB 消歧时不算失败）。
  - GET /knowledgebase/:kb_id/wiki/index?limit=
      → 裸 WikiIndexResponse {intro, version, groups:[...]}。

侧车只依赖这些公开 REST，不碰 Go 内部服务；响应字段名以后端 JSON tag 为准。
"""

from __future__ import annotations

import logging
from urllib.parse import quote

from app.backend.client import BackendError, BackendNotFound, RESTClient

logger = logging.getLogger("sidecar.backend.wiki")


class WikiService:
    """wiki 三个只读接口的异步封装。"""

    def __init__(self, client: RESTClient) -> None:
        self._client = client

    async def search_pages(self, kb_id: str, query: str, limit: int) -> list[dict]:
        """单库 wiki 搜索；返回 WikiPage dict 列表（含 content，供片段提取）。"""
        body = await self._client.get_json(
            f"/knowledgebase/{kb_id}/wiki/search",
            params={"q": query, "limit": str(limit)},
        )
        pages = body.get("pages")
        return [p for p in pages if isinstance(p, dict)] if isinstance(pages, list) else []

    async def get_page(self, kb_id: str, slug: str) -> dict | None:
        """按 slug 取页；404 返回 None（与 Go ErrWikiPageNotFound 同义）。"""
        # slug 自带 "concept/rag" 这类斜杠，保留 "/"，其余特殊字符做百分号转义
        quoted = quote(slug.strip(), safe="/")
        try:
            body = await self._client.get_json(f"/knowledgebase/{kb_id}/wiki/pages/{quoted}")
        except BackendNotFound:
            return None
        return body if isinstance(body, dict) else None

    async def get_index(self, kb_id: str, limit: int) -> dict:
        """取目录视图（Go GetIndexView：types 空 = 全类型，cursor 空 = 首页）。

        与内置 MCP 一样只取首页，不追 next_cursor；limit 是每类型条数。
        """
        body = await self._client.get_json(
            f"/knowledgebase/{kb_id}/wiki/index",
            params={"limit": str(limit)},
        )
        if not isinstance(body, dict):
            raise BackendError("upstream wiki index response is not an object")
        return body
