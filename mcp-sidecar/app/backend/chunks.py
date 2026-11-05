"""分块（chunk）REST 适配：按文档分页拉取分块。

  - GET /chunks/:knowledge_id?page=&page_size=&chunk_type=text&chunk_type=faq

Go 内置 read_document 同时取 text+faq 两类分块（见 read_document.go 的
listChunks），侧车保持一致；REST 返回已按文档顺序分页。
"""

from __future__ import annotations

from dataclasses import dataclass

from app.backend.client import RESTClient


@dataclass(frozen=True)
class ChunkPage:
    chunks: tuple[dict, ...]
    total: int
    page: int
    page_size: int


class ChunkService:
    def __init__(self, client: RESTClient) -> None:
        self._client = client

    async def list_paged(
        self,
        knowledge_id: str,
        *,
        page: int,
        page_size: int,
        chunk_types: tuple[str, ...] = ("text", "faq"),
    ) -> ChunkPage:
        # 重复 query 参数（chunk_type=text&chunk_type=faq）
        params: list[tuple[str, str]] = [("page", str(page)), ("page_size", str(page_size))]
        for chunk_type in chunk_types:
            params.append(("chunk_type", chunk_type))
        body = await self._client.get_json(
            f"/chunks/{knowledge_id.strip()}", params=params
        )
        rows = body.get("data")
        chunks: list[dict] = []
        if isinstance(rows, list):
            for row in rows:
                if isinstance(row, dict) and row.get("id"):
                    chunks.append(row)
        return ChunkPage(
            chunks=tuple(chunks),
            total=int(body.get("total") or 0),
            page=int(body.get("page") or page),
            page_size=int(body.get("page_size") or page_size),
        )
