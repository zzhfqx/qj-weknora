import { findTableSource, tableFragmentPages } from './tableSource'
import { findSourceFragments, sourceOverlapLength } from './fragmentedSource'
import {
  findInText,
  findTextMatches,
  normalizeForMatch,
  parseSourceLocators,
  validSourceBox,
  type SourceLocateRequest,
} from './sourceLocator'

export type PdfTarget = {
  page: number
  quote?: string
  occurrence?: number
  bbox?: [number, number, number, number]
  granularity: 'text' | 'block' | 'page'
}
export type PdfResolution = {
  targets: PdfTarget[]
  reason?: 'ambiguous' | 'missing' | 'unavailable' | 'cancelled' | 'partial'
}

/** 在接触 DOM 之前先解析证据；绝不能直接选择第一个重复项。 */
export async function resolvePdfSource(
  request: SourceLocateRequest,
  pageCount: number,
  getText: (page: number) => Promise<string>,
  cancelled: () => boolean = () => false,
): Promise<PdfResolution> {
  if (request.unavailable) return { targets: [], reason: 'unavailable' }
  const locators = parseSourceLocators(request.locators).filter((l) => l.type === 'pdf' && l.page! <= pageCount)
  const invalidLocators = request.locators.filter((l) => l.type === 'pdf').length !== locators.length
  const targets: PdfTarget[] = []
  for (const loc of locators) {
    if (cancelled()) return { targets: [], reason: 'cancelled' }
    // 历史 bbox 由启发式对齐产生，可能指向另一页。
    // 因此需要在整份原文中重新校验。
    if (loc.mapping !== 'exact') continue
    const page = loc.page!
    const quote = loc.quote || ''
    const sentence = request.sentence && findInText(quote, request.sentence) ? request.sentence : ''
    if (loc.mapping === 'exact' && validSourceBox(loc.bbox)) {
      // 文本细化发生在这个已知区域内；扫描页即使没有 PDF 文本层，
      // 也能直接使用解析器给出的几何信息。
      targets.push({ page, bbox: loc.bbox, quote: sentence || quote, granularity: 'block' })
      continue
    }
    const text = await getText(page)
    if (cancelled()) return { targets: [], reason: 'cancelled' }
    const scope = quote ? findInText(text, quote) : null
    if (scope) {
      const sub = sentence ? findInText(text.slice(scope.start, scope.end), sentence) : null
      const actual = sub ? text.slice(scope.start + sub.start, scope.start + sub.end) : text.slice(scope.start, scope.end)
      const at = sub ? scope.start + sub.start : scope.start
      const occurrence = findTextMatches(text, actual).findIndex((hit) => hit.start === at)
      targets.push({ page, quote: actual, occurrence, granularity: 'text' })
    } else if (loc.mapping === 'exact') {
      targets.push({ page, granularity: 'page' })
    }
  }
  if (
    targets.length === locators.length &&
    targets.length &&
    !invalidLocators &&
    !locators.some((l) => l.partial)
  ) {
    return { targets }
  }

  // 旧记录可能包含错误页码。需要把完整 chunk 与原文逐页比对，
  // 包括跨页摘录。文档级搜索必须先检查所有出现位置，才能判定唯一匹配。
  const pages: string[] = []
  const starts: number[] = []
  let full = ''
  for (let page = 1; page <= pageCount; page++) {
    if (cancelled()) return { targets: [], reason: 'cancelled' }
    const text = await getText(page)
    starts.push(full.length)
    pages.push(text)
    full += text + '\n'
  }
  if (cancelled()) return { targets: [], reason: 'cancelled' }
  // 完整 chunk 用于消解重复句子；不要用无关的开头或更短的模糊锚点
  // 替代缺失的完整范围。
  const candidates = request.scope ? [request.scope] : request.quotes.length ? request.quotes : locators.map((l) => l.quote || '')
  let ambiguous = false
  for (const quote of candidates) {
    if (normalizeForMatch(quote).length < 4) continue
    const matches = findTextMatches(full, quote)
    if (matches.length > 1) {
      ambiguous = true
      continue
    }
    if (matches.length !== 1) continue
    let { start, end } = matches[0]!
    const sentence = request.sentence ? findInText(full.slice(start, end), request.sentence) : null
    if (sentence) {
      end = start + sentence.end
      start += sentence.start
    }
    const resolved: PdfTarget[] = []
    for (let i = 0; i < pages.length; i++) {
      const lo = Math.max(start, starts[i]!)
      const hi = Math.min(end, starts[i]! + pages[i]!.length)
      if (hi <= lo) continue
      const actual = full.slice(lo, hi)
      if (normalizeForMatch(actual).length < 2) continue
      const occurrence = findTextMatches(pages[i]!, actual).findIndex((m) => m.start === lo - starts[i]!)
      resolved.push({ page: i + 1, quote: actual, occurrence, granularity: 'text' })
    }
    if (resolved.length) return { targets: resolved }
  }
  if (!ambiguous && request.scope) {
    const table = findTableSource(pages, request.scope)
    // 独立片段必须遵守相同的年级/章节约束。
    // 当解析器伪影导致列不完整时，它们仍可验证部分文本。
    const fragments = table.length
      ? table
      : findSourceFragments(tableFragmentPages(pages, request.scope), request.scope)
    if (request.sentence) {
      fragments.sort(
        (a, b) => sourceOverlapLength(b.quote, request.sentence!) - sourceOverlapLength(a.quote, request.sentence!),
      )
    }
    if (fragments.length) {
      return {
        targets: fragments.map((hit) => ({
          page: hit.page,
          quote: hit.quote,
          granularity: 'text' as const,
          occurrence: findTextMatches(pages[hit.page - 1]!, hit.quote).findIndex((m) => m.start === hit.start),
        })),
        reason: 'partial',
      }
    }
  }
  // 保留经过独立校验的区域，但同时暴露结果不完整。
  return { targets, reason: ambiguous ? 'ambiguous' : 'missing' }
}

export type SourceRect = { left: number; top: number; width: number; height: number }

/** 只合并相邻的字形片段；绝不能跨越双栏之间的装订线。 */
export function mergeSourceRects(rects: SourceRect[]): SourceRect[] {
  const lines: SourceRect[] = []
  for (const r of [...rects].sort((a, b) => a.top - b.top || a.left - b.left)) {
    if (r.width < 0.5 || r.height < 0.5) continue
    const line = lines.find((l) => {
      const overlap = Math.min(l.top + l.height, r.top + r.height) - Math.max(l.top, r.top)
      const gap = Math.max(r.left - l.left - l.width, l.left - r.left - r.width)
      return overlap > Math.min(l.height, r.height) * 0.5 && gap <= Math.min(l.height, r.height) * 0.5
    })
    if (line) {
      const right = Math.max(line.left + line.width, r.left + r.width)
      const bottom = Math.max(line.top + line.height, r.top + r.height)
      line.left = Math.min(line.left, r.left)
      line.top = Math.min(line.top, r.top)
      line.width = right - line.left
      line.height = bottom - line.top
    } else {
      lines.push({ left: r.left, top: r.top, width: r.width, height: r.height })
    }
  }
  return lines
}
