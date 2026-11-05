"""原文定位：把解析后的 Markdown 区间映射回原始文件中的位置。

一个源块（source block）是 ``{"start": int, "end": int, "locator": {...}}``，
其中 ``start``/``end`` 是文档内容中的 Unicode 字符偏移（end 不含），
locator 描述该段文本在原始文件中的位置。对 PDF 而言，locator 是
``{"type": "pdf", "page": n}``，并可选带 ``"bbox": [x0, y0, x1, y1]``，
坐标是相对于展示页面宽高的比例，原点位于页面左上角，因此查看器可以在任意
缩放级别下绘制它。
"""

from __future__ import annotations

import re
from collections import Counter
import statistics
import unicodedata
from typing import Iterable, List, Optional, Sequence, Tuple

# 从光标往后搜索一行的范围（规范化后的字符数）。
_SEARCH_WINDOW = 6000
# 短于该长度（规范化后）的行太模糊，无法定位。
_MIN_KEY_CHARS = 2
# 短于该长度的行只有在页面内文本唯一时才定位：图内标签（"LLM"、"EBAE"）
# 否则会抢走正文的偏移。宽字符（CJK）按 4 计权，因为每个字约等于一个词。
_SHORT_KEY_WEIGHT = 16

_LIST_START_RE = re.compile(
    r"^\s*(?:"
    r"\d{1,3}(?:\.\d{1,3}){0,4}[\.、．)）]?\s"  # 1  1.  8.14.11  2)
    r"|[（(]\d{1,3}[)）]"  # (1) （1）
    r"|[一二三四五六七八九十百]+[、．.]"  # 一、
    r"|[•·●○■□◆◇▪\-–*]\s"  # 项目符号
    r"|第[一二三四五六七八九十百零\d]+[章节条款部分篇]"  # 第三章
    r")"
)
_SENTENCE_END = tuple("。．.!?！？：:；;」』”)）")


def _fold(ch: str) -> str:
    """只保留字母和数字，转小写，全角折叠为半角。"""
    if not ch.isalnum():
        return ""
    return unicodedata.normalize("NFKC", ch).lower()


def normalize_with_positions(text: str) -> Tuple[str, List[int]]:
    """把文本投影为字母和数字。

    返回投影结果，以及投影中每个字符来自 ``text`` 中哪个字符的偏移。
    """
    out: List[str] = []
    pos: List[int] = []
    for i, ch in enumerate(text):
        value = unicodedata.normalize("NFKC", ch)
        folded = _fold(ch)
        if value in ".,:/+−-%‰<>=≤≥≠":
            before, after = i - 1, i + 1
            while before >= 0 and text[before] in " \t":
                before -= 1
            while after < len(text) and text[after] in " \t":
                after += 1
            prev = before >= 0 and text[before].isdigit()
            next_digit = after < len(text) and text[after].isdigit()
            semantic = ((value in ".,/:" and prev and next_digit)
                        or (value in "+-−" and next_digit)
                        or (value in "%‰" and prev)
                        or value in "<>=≤≥≠")
            if semantic:
                folded = value.replace("−", "-")
        for f in folded:
            out.append(f)
            pos.append(i)
    return "".join(out), pos


def normalize(text: str) -> str:
    return normalize_with_positions(text)[0]


def _key_weight(key: str) -> int:
    return sum(4 if unicodedata.east_asian_width(ch) in ("W", "F") else 1 for ch in key)


def _column_cursor(boxes, idx: int, ends: List[Optional[int]]) -> Optional[int]:
    """返回 ``boxes[idx]`` 上方同列最近一个已定位行的结束位置。"""
    x0, y0, x1, _ = boxes[idx]
    for j in range(idx - 1, max(-1, idx - 80), -1):
        if ends[j] is None:
            continue
        px0, py0, px1, _ = boxes[j]
        if px0 < x1 and x0 < px1 and py0 > y0:
            return ends[j]
    return None


def _numeric_safe_matches(text: str, key: str) -> List[int]:
    out = []
    if not key:
        return out
    start = 0
    while (at := text.find(key, start)) >= 0:
        before = text[at - 1] if at else ""
        end = at + len(key)
        after = text[end] if end < len(text) else ""
        left = key[0].isdigit() and bool(before) and (before.isdigit() or before in ".,:/+−-")
        right = key[-1].isdigit() and bool(after) and (after.isdigit() or after in ".,:/%‰")
        if not left and not right:
            out.append(at)
        start = at + 1
    return out


def locate_lines(
    page_text: str,
    line_texts: Sequence[str],
    boxes: Optional[Sequence[Tuple[float, float, float, float]]] = None,
) -> List[Optional[int]]:
    """找出每个视觉行在页面文本中的起始位置。

    各行在上一处命中之后按顺序搜索，因此重复出现的短语能落到正确的位置。
    提供 ``boxes``（PDF 点坐标，左下原点）时，搜索优先从同列上一行之后继续，
    因为并排栏的字形行是交错到达的。两处光标都找不到的行会从页面开头再搜，
    以容忍文本提取器与字形分组的栏顺序不一致。每个行返回一个偏移（或 None）。
    """
    norm, pos = normalize_with_positions(page_text)
    hits: List[Optional[int]] = []
    ends: List[Optional[int]] = []
    cursor = 0
    counts = Counter(normalize(line) for line in line_texts)
    occurrences = {key: _numeric_safe_matches(norm, key) for key in counts}
    for idx, line in enumerate(line_texts):
        key = normalize(line)
        if len(key) < _MIN_KEY_CHARS or len(occurrences[key]) != counts[key] or (
            _key_weight(key) < _SHORT_KEY_WEIGHT and len(occurrences[key]) > 1
        ):
            hits.append(None)
            ends.append(None)
            continue
        cursors = [cursor]
        if boxes is not None:
            column = _column_cursor(boxes, idx, ends)
            if column is not None:
                cursors.insert(0, column)
        at = -1
        for start_at in cursors:
            at = next((hit for hit in occurrences[key] if start_at <= hit <= start_at + _SEARCH_WINDOW), -1)
            if at >= 0:
                break
        if at < 0:
            at = occurrences[key][0] if len(occurrences[key]) == 1 else -1
            if at < 0:
                hits.append(None)
                ends.append(None)
                continue
        else:
            cursor = at + len(key)
        ends.append(at + len(key))
        start = pos[at]
        # 把同一文本行前面被规范化跳过的标点或标记（括号、"## "、项目符号）
        # 一并纳入。
        while start > 0 and page_text[start - 1] != "\n" and not page_text[start - 1].isalnum():
            start -= 1
        hits.append(start)
    return hits


class PageGeometry:
    """把 PDF 用户空间点坐标转换为展示页面的比例坐标。"""

    def __init__(self, cropbox: Tuple[float, float, float, float], rotation: int = 0):
        left, bottom, right, top = cropbox
        self.left, self.bottom = min(left, right), min(bottom, top)
        self.width = abs(right - left) or 1.0
        self.height = abs(top - bottom) or 1.0
        self.rotation = int(rotation or 0) % 360

    def bbox(self, box: Tuple[float, float, float, float]) -> List[float]:
        x0, y0, x1, y1 = box
        # 未旋转、左上角为原点。
        ux0 = (x0 - self.left) / self.width
        ux1 = (x1 - self.left) / self.width
        uy0 = 1.0 - (y1 - self.bottom) / self.height
        uy1 = 1.0 - (y0 - self.bottom) / self.height
        corners = [self._rotate(ux0, uy0), self._rotate(ux1, uy1)]
        xs = [c[0] for c in corners]
        ys = [c[1] for c in corners]
        return [
            round(_clamp(min(xs)), 4),
            round(_clamp(min(ys)), 4),
            round(_clamp(max(xs)), 4),
            round(_clamp(max(ys)), 4),
        ]

    def _rotate(self, x: float, y: float) -> Tuple[float, float]:
        # 查看器按 /Rotate 顺时针展示。
        if self.rotation == 90:
            return 1.0 - y, x
        if self.rotation == 180:
            return 1.0 - x, 1.0 - y
        if self.rotation == 270:
            return y, 1.0 - x
        return x, y


def _clamp(v: float) -> float:
    return 0.0 if v < 0 else 1.0 if v > 1 else v


def _union(boxes: Iterable[Tuple[float, float, float, float]]):
    boxes = list(boxes)
    return (
        min(b[0] for b in boxes),
        min(b[1] for b in boxes),
        max(b[2] for b in boxes),
        max(b[3] for b in boxes),
    )


def _line_gap(prev: dict, cur: dict) -> Optional[float]:
    """当 ``prev`` 与 ``cur`` 在同一列时，返回两者之间的垂直间距。"""
    px0, py0, px1, _ = prev["bbox"]
    cx0, _, cx1, cy1 = cur["bbox"]
    if cx1 < px0 or cx0 > px1 or cy1 > py0:
        return None
    return py0 - cy1


def _starts_paragraph(
    prev: dict, cur: dict, line_h: float, usual_gap: float, col_right: float
) -> bool:
    """判断 ``cur`` 是否在 ``prev`` 之后开启一个新的视觉段落。

    bbox 是 PDF 点坐标，原点在左下角，因此向下阅读意味着 y 递减。
    ``usual_gap`` 是页面中同一段落相邻行的典型间距，可以吸收双倍行距的排版。
    """
    px0, py0, px1, _ = prev["bbox"]
    cx0, _, cx1, cy1 = cur["bbox"]
    if cx1 < px0 or cx0 > px1:
        return True  # 换到了另一栏
    if cy1 > py0 + line_h * 0.5:
        return True  # 回到页面上方：新的一栏或新区域
    if py0 - cy1 > usual_gap * 1.5 + line_h * 0.3:
        return True  # 垂直间距明显大于常规行距
    if _LIST_START_RE.match(cur["text"]):
        return True
    ends_sentence = prev["text"].rstrip().endswith(_SENTENCE_END)
    short = col_right - px1 > line_h * 2
    return ends_sentence and short


def page_blocks(
    page_text: str,
    offset: int,
    page_number: int,
    lines: Sequence[dict],
    geometry: Optional[PageGeometry],
) -> List[dict]:
    """返回放在文档 ``offset`` 位置的一页文本的源块。

    ``lines`` 是视觉行 ``{"text", "bbox"}``（bbox 为 PDF 点坐标）。这些行会
    在 ``page_text`` 中定位，再按几何位置分组成段落，每段生成一个只覆盖其
    已匹配行的块，并用这些行的并集作为区域框。未匹配的间隙保留整页来源，
    绝不会继承前一段的几何位置。
    """
    end = offset + len(page_text)
    page_only = {"type": "pdf", "page": page_number, "mapping": "exact"}
    if not page_text:
        return []
    placed = []
    if lines and geometry is not None:
        starts = locate_lines(
            page_text, [ln["text"] for ln in lines], [ln["bbox"] for ln in lines]
        )
        placed = sorted(
            (
                (at, ln)
                for at, ln in zip(starts, lines)
                if at is not None
            ),
            key=lambda item: item[0],
        )
    if not placed:
        return [{"start": offset, "end": end, "locator": dict(page_only)}]

    heights = [ln["bbox"][3] - ln["bbox"][1] for _, ln in placed if ln["bbox"][3] > ln["bbox"][1]]
    line_h = statistics.median(heights) if heights else 10.0
    gaps = [
        g
        for g in (_line_gap(a[1], b[1]) for a, b in zip(placed, placed[1:]))
        if g is not None and g >= 0
    ]
    usual_gap = min(statistics.median(gaps), line_h * 1.5) if gaps else line_h * 0.5

    norm, positions = normalize_with_positions(page_text)
    def line_end(at, line):
        key = normalize(line["text"])
        norm_start = next((i for i, p in enumerate(positions) if p >= at), len(norm))
        hit = norm.find(key, norm_start)
        if hit < 0:
            return at
        end_at = positions[hit + len(key) - 1] + 1
        while end_at < len(page_text) and not page_text[end_at].isalnum():
            if page_text[end_at] == "\n":
                return end_at + 1
            end_at += 1
        return end_at

    paragraphs: List[Tuple[int, List[dict]]] = []
    for at, ln in placed:
        ln = {**ln, "_source_end": line_end(at, ln)}
        if paragraphs:
            prev = paragraphs[-1][1][-1]
            col_right = max(p["bbox"][2] for p in paragraphs[-1][1])
            col_right = max(col_right, ln["bbox"][2])
            previous_end = prev["_source_end"]
            gap_is_empty = not normalize(page_text[previous_end:at])
            if gap_is_empty and not _starts_paragraph(prev, ln, line_h, usual_gap, col_right):
                paragraphs[-1][1].append(ln)
                continue
        paragraphs.append((at, [ln]))

    blocks: List[dict] = []
    cursor = 0
    for at, members in paragraphs:
        stop = members[-1]["_source_end"]
        if stop <= at:
            continue
        if at > cursor and page_text[cursor:at].strip():
            blocks.append({"start": offset + cursor, "end": offset + at, "locator": dict(page_only)})
        locator = dict(page_only)
        locator["bbox"] = geometry.bbox(_union(m["bbox"] for m in members))
        locator["source_id"] = f"pdf:{page_number}:{at}:{stop}"
        blocks.append({"start": offset + at, "end": offset + stop, "locator": locator})
        cursor = stop
    if cursor < len(page_text) and page_text[cursor:].strip():
        blocks.append({"start": offset + cursor, "end": end, "locator": dict(page_only)})
    return blocks
