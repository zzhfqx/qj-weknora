import { findTextMatches, normalizeForMatch, type TextMatch } from './sourceLocator'

/**
 * 针对「解析器以不同顺序序列化表格列」的精确片段匹配。
 * 返回的每个范围都经过独立校验；调用方必须报告覆盖不完整，
 * 未能匹配的文本绝不能纳入高亮。
 */
export function findSourceFragments(pages: string[], scope: string): Array<TextMatch & { page: number; quote: string }> {
  const table = scope.includes('|')
  const parts = table
    ? scope.split(/[|\n]/u)
    : scope.split(/(?<=[。！？；!?;])|\n+|(?<=[\p{Script=Han}】）])\s*[à→\uE000-\uF8FF]\s*/u)
  const candidates = new Set<string>()
  for (const part of parts) {
    const clean = part.trim()
    if (!clean || /^[-: ]+$/.test(clean)) continue
    if (!table) {
      candidates.add(clean)
      continue
    }
    // 优先保留完整单元格。被拍平的单元格可能包含课程列表，
    // 因此使用完整的空白分隔条目，绝不使用任意 n-gram。
    candidates.add(clean)
    const words = clean.split(/\s+/u)
    if (words.length > 1) for (const word of words) candidates.add(word)
  }
  const evidence = [...candidates]
    .filter((q) => normalizeForMatch(q).length >= (table ? 5 : 8))
    .map((quote) => ({
      quote,
      hits: pages.flatMap((text, i) => findTextMatches(text, quote).map((hit) => ({ ...hit, page: i + 1 }))),
    }))
  const unique = evidence.filter((e) => e.hits.length === 1)
  // 必须有相互独立的锚点。仅共享一个开头并不足以确定位置。
  const anchors = unique.filter(
    (e) => !unique.some((other) => other !== e && normalizeForMatch(other.quote).includes(normalizeForMatch(e.quote))),
  )
  let anchorPages = new Set(anchors.map((e) => e.hits[0]!.page))
  if (anchors.length < 2) {
    // 重复的表格条目仍可通过交集确定页码：
    // 所有匹配单元格必须一致，而不只是得分最高的那一页。
    const present = evidence.filter((e) => e.hits.length)
    if (!table || present.length < 2) return []
    let common = new Set(present[0]!.hits.map((hit) => hit.page))
    for (const entry of present.slice(1)) {
      common = new Set([...common].filter((page) => entry.hits.some((hit) => hit.page === page)))
    }
    if (common.size !== 1) return []
    anchorPages = common
  }
  const ranges: Array<TextMatch & { page: number; quote: string }> = []
  for (const { quote, hits } of evidence) {
    const scoped = hits.length === 1 ? hits : hits.filter((hit) => anchorPages.has(hit.page))
    if (scoped.length !== 1) continue
    const hit = scoped[0]!
    ranges.push({ ...hit, quote: pages[hit.page - 1]!.slice(hit.start, hit.end) })
  }
  const nonOverlapping = ranges.filter(
    (hit, i) =>
      !ranges.some(
        (other, j) =>
          j !== i &&
          other.page === hit.page &&
          other.start <= hit.start &&
          other.end >= hit.end &&
          (other.start < hit.start || other.end > hit.end || j < i),
      ),
  )
  if (!table) {
    // 图片和 OCR 伪影可能占据 chunk 的大部分内容。覆盖百分比并不是定位证据：
    // 要求同一局部原文顺序中出现多个完整且唯一的句子，并把结果报告为部分匹配。
    for (let i = 1; i < nonOverlapping.length; i++) {
      const previous = nonOverlapping[i - 1]!
      const current = nonOverlapping[i]!
      if (current.page < previous.page || (current.page === previous.page && current.start < previous.end)) return []
    }
    if (nonOverlapping.length < 2 || nonOverlapping.at(-1)!.page - nonOverlapping[0]!.page > 2) return []
  }
  return nonOverlapping.sort((a, b) => a.page - b.page || a.start - b.start)
}

/**
 * 按与引用句子的逐字重叠度对已验证范围排序。
 * 这里只改变导航顺序，不能凭空创造匹配或消除歧义。
 */
export function sourceOverlapLength(quote: string, sentence: string): number {
  const source = normalizeForMatch(quote)
  const answer = normalizeForMatch(sentence)
  let previous = new Uint32Array(answer.length + 1)
  let best = 0
  for (const character of source) {
    const current = new Uint32Array(answer.length + 1)
    for (let j = 0; j < answer.length; j++) {
      if (character === answer[j]) {
        current[j + 1] = previous[j]! + 1
        best = Math.max(best, current[j + 1]!)
      }
    }
    previous = current
  }
  return best
}
