"""源块的传输形式（见 docreader.parser.source_locator）。

与 docreader.main 分开存放，便于在不加载解析器注册表的情况下导入。
"""

import json

from docreader.proto.docreader_pb2 import SourceBlock


def source_blocks_to_proto(document) -> list:
    """把文档的源块转换为传输形式。

    畸形条目直接跳过：位置信息是可选附加项，绝不能导致解析失败。
    """
    out = []
    for block in getattr(document, "source_blocks", None) or []:
        try:
            start, end = int(block["start"]), int(block["end"])
            locator = block["locator"]
            if end <= start or start < 0 or not locator.get("type"):
                continue
            out.append(
                SourceBlock(
                    start=start,
                    end=end,
                    locator_json=json.dumps(locator, ensure_ascii=False),
                )
            )
        except (KeyError, TypeError, ValueError, AttributeError):
            continue
    return out
