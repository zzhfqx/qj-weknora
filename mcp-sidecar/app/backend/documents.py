"""文档（knowledge）REST 适配：文档详情与某库下的分页文档列表。

  - GET /knowledge/:id                          文档详情（含 knowledge_base_id，
                                                文档级越权校验就靠它反查所属库）
  - GET /knowledge-bases/:id/knowledge         某库文档分页（keyword/page/page_size）
"""

from __future__ import annotations

from dataclasses import dataclass

from app.backend.client import BackendNotFound, RESTClient


@dataclass(frozen=True)
class DocumentSummary:
    """与 Go documentSummary 字段一致（list_documents 输出契约）。"""

    id: str
    title: str
    file_name: str
    file_type: str
    source: str
    parse_status: str
    description: str
    created_at: str
    updated_at: str


def parse_document_summary(raw: dict) -> DocumentSummary:
    def s(key: str) -> str:
        value = raw.get(key)
        return "" if value is None else str(value)

    return DocumentSummary(
        id=s("id"),
        title=s("title"),
        file_name=s("file_name"),
        file_type=s("file_type"),
        source=s("source"),
        parse_status=s("parse_status"),
        description=s("description"),
        created_at=s("created_at"),
        updated_at=s("updated_at"),
    )


def as_dict(summary: DocumentSummary) -> dict:
    """序列化为工具输出（空值字段省略，与 Go omitempty 对齐）。"""
    out = {
        "id": summary.id,
        "title": summary.title,
        "parse_status": summary.parse_status,
        "created_at": summary.created_at,
        "updated_at": summary.updated_at,
    }
    for key in ("file_name", "file_type", "source", "description"):
        value = getattr(summary, key)
        if value:
            out[key] = value
    return out


@dataclass(frozen=True)
class DocumentPage:
    documents: tuple[dict, ...]
    total: int
    page: int
    page_size: int


class DocumentService:
    def __init__(self, client: RESTClient) -> None:
        self._client = client

    async def get(self, knowledge_id: str) -> dict | None:
        """文档详情原始 JSON；404 返回 None。tools 层用它反查所属库做越权校验。"""
        try:
            body = await self._client.get_json(f"/knowledge/{knowledge_id.strip()}")
        except BackendNotFound:
            return None
        data = body.get("data")
        return data if isinstance(data, dict) else None

    async def list_paged(
        self, kb_id: str, *, keyword: str, page: int, page_size: int
    ) -> DocumentPage:
        """某知识库下的文档分页（GET .../knowledge）。"""
        params: list[tuple[str, str]] = [
            ("page", str(page)),
            ("page_size", str(page_size)),
        ]
        if keyword:
            params.append(("keyword", keyword))
        body = await self._client.get_json(
            f"/knowledge-bases/{kb_id}/knowledge", params=params
        )
        rows = body.get("data")
        documents: list[dict] = []
        if isinstance(rows, list):
            for row in rows:
                if isinstance(row, dict) and row.get("id"):
                    documents.append(as_dict(parse_document_summary(row)))
        return DocumentPage(
            documents=tuple(documents),
            total=int(body.get("total") or 0),
            page=int(body.get("page") or page),
            page_size=int(body.get("page_size") or page_size),
        )
