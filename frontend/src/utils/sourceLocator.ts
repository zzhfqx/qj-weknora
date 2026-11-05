/**
 * 原文定位器：把被引用的 chunk 指回原始文件中的位置。
 * 后端在摄入阶段写入这些定位器（见 internal/types/source_locator.go）；
 * 序号从 1 开始，PDF 的 bbox 是相对展示页面的比例，原点位于页面左上角。
 */
export type SourceLocatorType = 'pdf' | 'docx' | 'slide' | 'sheet' | 'text' | 'time' | 'section'

export type SourceLocator = {
  type: SourceLocatorType | string
  mapping?: string
  partial?: boolean
  source_id?: string
  source_hash?: string
  page?: number
  bbox?: [number, number, number, number] | number[]
  block?: number
  slide?: number
  sheet?: string
  row_start?: number
  row_end?: number
  start?: number
  end?: number
  start_ms?: number
  end_ms?: number
  section?: number
  title?: string
  quote?: string
}

/** 预览需要揭示的内容：结构化定位目标，以及用于匹配的文本。 */
export type SourceLocateRequest = {
  /** 需要揭示的定位器，按相关度从高到低排列。 */
  locators: SourceLocator[]
  /** 当定位器无法缩小范围（或需要细化）时用于查找的文本。 */
  quotes: string[]
  /** 每次请求都会变化，确保重复点击同一条引用时仍会重新滚动。 */
  token: number
  /** 该引用所支撑的回答句子，用于缩小粗粒度定位器的范围。 */
  sentence?: string
  /** 被引用 chunk 的文本，限定细化匹配可出现的范围。 */
  scope?: string
  /** 去除标记前的原始 Markdown；代码中可能包含字面量标签。 */
  sourceMarkdown?: string
  unavailable?: boolean
  /** 夹在某个内嵌 OCR 图片前后的相邻原文段落。 */
  imageContext?: { before: string; after: string }
  imageDigest?: string
}

// ---------------------------------------------------------------------------
// 归一化只保留字母、数字和数字语境中的符号，并折叠大小写与全半角。
// 这里采用与后端正向对齐器相同的投影方式，因此解析文本与渲染原文之间
// 的 Markdown 语法、空白和标点差异不会破坏匹配。

const LETTER_OR_DIGIT = /[\p{L}\p{N}]/u

export function foldChar(ch: string): string {
  if (!LETTER_OR_DIGIT.test(ch)) return ''
  return ch.normalize('NFKC').toLowerCase()
}

/** 在保留数字和比较符号的前提下抹平排版差异。 */
export function normalizeForMatch(text: string): string {
  return normalizeWithPositions(text).norm
}

export function normalizeWithPositions(text: string): { norm: string; pos: number[] } {
  const chars = Array.from(String(text || ''))
  let norm = ''
  const pos: number[] = []
  let offset = 0
  for (let i = 0; i < chars.length; i++) {
    const ch = chars[i]!
    const value = ch.normalize('NFKC')
    const digit = (v: string | undefined) => !!v && /^\p{N}$/u.test(v.normalize('NFKC'))
    let folded = foldChar(ch)
    if (/^[.,/:+−%‰<>=≤≥≠-]$/u.test(value)) {
      let before = i - 1
      let after = i + 1
      while (before >= 0 && /^[ \t]$/.test(chars[before]!)) before--
      while (after < chars.length && /^[ \t]$/.test(chars[after]!)) after++
      const prev = digit(chars[before])
      const next = digit(chars[after])
      const symbol =
        (/[.,/:]/.test(value) && prev && next) ||
        (/^[+−-]$/.test(value) && next) ||
        (/^[%‰]$/.test(value) && prev) ||
        /^[<>=≤≥≠]$/.test(value)
      if (symbol) folded = value.replace('−', '-')
    }
    norm += folded
    // 本模块中的偏移量均为 UTF-16 单位，包括补充平面字符。
    for (let j = 0; j < folded.length; j++) pos.push(offset)
    offset += ch.length
  }
  return { norm, pos }
}

export type NormalizedMatch = { start: number; end: number; score: number }
export type TextMatch = { start: number; end: number }

/** 返回全部精确候选；调用方必须先消歧，再选择其中一个。 */
export function findNormalizedMatches(haystack: string, quote: string): NormalizedMatch[] {
  let needle = normalizeForMatch(quote)
  if (needle.length < 2 || !haystack) return []
  const search = (key: string) => {
    const matches: NormalizedMatch[] = []
    const first = Array.from(key)[0]!
    const last = Array.from(key).at(-1)!
    for (let from = 0; from <= haystack.length - key.length; ) {
      const at = haystack.indexOf(key, from)
      if (at < 0) break
      const end = at + key.length
      // 引用的数字不能是另一个数字的前缀或后缀。
      const before = haystack.slice(Math.max(0, at - 2), at)
      const after = haystack.slice(end, end + 2)
      const startsInsideNumber = /\p{N}/u.test(first) && /[\p{N}.,:/+−-]$/u.test(before)
      const endsInsideNumber = /\p{N}/u.test(last) && /^[\p{N}.,:/%‰]/u.test(after)
      if (!startsInsideNumber && !endsInsideNumber) matches.push({ start: at, end, score: 1 })
      from = at + 1
    }
    return matches
  }
  const exact = search(needle)
  if (exact.length) return exact
  // 渲染器可能省略了原文的列表序号。这是唯一允许的前缀差异，
  // 绝不能用来扩展过短的模糊锚点。
  const withoutLabel = quote.replace(/^\s*[（(][一二三四五六七八九十百\d]+[)）]\s*/u, '')
  if (withoutLabel === quote) return []
  needle = normalizeForMatch(withoutLabel)
  return needle.length >= 8 ? search(needle) : []
}

export function findNormalized(haystack: string, quote: string): NormalizedMatch | null {
  const matches = findNormalizedMatches(haystack, quote)
  return matches.length === 1 ? matches[0]! : null
}

export function findTextMatches(text: string, quote: string): TextMatch[] {
  const { norm, pos } = normalizeWithPositions(text)
  return findNormalizedMatches(norm, quote).map((m) => {
    const start = pos[m.start]!
    const last = pos[m.end - 1]!
    return { start, end: last + (text.codePointAt(last)! > 0xffff ? 2 : 1) }
  })
}

/** 在原始文本中定位引文，返回 `text` 内的 UTF-16 偏移。 */
export function findInText(text: string, quote: string): { start: number; end: number } | null {
  const hits = findTextMatches(text, quote)
  return hits.length === 1 ? hits[0]! : null
}

// ---------------------------------------------------------------------------
// 为一条引用挑选需要揭示的内容。

/** 显式引号内容可以在句子被改写后仍然保留，无需模糊匹配。 */
export function quotedSourceExcerpt(content: string, sentence: string): string | undefined {
  const source = sourceQuoteText(content)
  const candidates = [...sentence.matchAll(/[“「『"]([^”」』"\n]{8,})[”」』"]/gu)]
    .map((m) => m[1]!)
    .filter((q) => normalizeForMatch(q).length >= 8 && findInText(source, q))
  return candidates.length === 1 ? candidates[0] : undefined
}

/** 仅当回答中包含原文的精确片段时才缩小定位范围。 */
export function selectLocatorsForSentence(locators: SourceLocator[], sentence: string): SourceLocator[] {
  const list = Array.isArray(locators) ? locators.filter(Boolean) : []
  sentence = quotedSourceExcerpt(list.map((l) => l.quote || '').join('\n'), sentence) || sentence
  const needle = normalizeForMatch(sentence)
  if (list.length <= 1 || needle.length < 4) return list
  const exact = list.filter((loc) => findTextMatches(loc.quote || '', sentence).length > 0)
  return exact.length ? exact : list
}

/** 使用精确片段；否则保留完整的被引用原文作为上下文。 */
export function selectQuoteForSentence(content: string, sentence: string): string[] {
  const source = sourceQuoteText(content)
  if (!source) return []
  // 只用确实出现在该原文中的文本做细化。即使回答是转述，
  // 也应打开其引用的 chunk，而不能错误地选中无关的第一行。
  sentence = quotedSourceExcerpt(content, sentence) || sentence
  const hit = sentence ? findInText(source, sentence) : null
  return hit ? [source.slice(hit.start, hit.end)] : [source]
}

/** 归一化 API 返回的定位器载荷，丢弃格式错误的条目。 */
export function parseSourceLocators(raw: unknown): SourceLocator[] {
  if (!Array.isArray(raw)) return []
  return raw
    .filter((item): item is SourceLocator => {
      if (!item || typeof item !== 'object' || typeof item.type !== 'string') return false
      if (item.type === 'pdf' && (!Number.isSafeInteger(item.page) || item.page < 1)) return false
      return true
    })
    .map((loc) => {
      if (loc.bbox && !validSourceBox(loc.bbox)) {
        const { bbox, ...rest } = loc
        return rest
      }
      return loc
    })
}

/** PDF 定位器涉及的 1 基页码，按首次出现顺序排列。 */
export function locatorPages(locators: SourceLocator[]): number[] {
  const pages: number[] = []
  for (const loc of locators) {
    if (loc.type === 'pdf' && loc.page && !pages.includes(loc.page)) pages.push(loc.page)
  }
  return pages
}

/** 文本片段 URL（`#:~:text=`），让浏览器滚动到并标记引文。 */
export function textFragmentUrl(url: string, quote: string): string {
  const clean = String(quote || '').replace(/\s+/g, ' ').trim()
  if (!url || !clean) return url
  const words = clean.split(' ')
  let fragment: string
  if (clean.length <= 80) {
    fragment = encodeTextFragment(clean)
  } else {
    // start,end 形式可以让长段落的 URL 保持简短。
    const head = clean.slice(0, 40).trim()
    const tail = clean.slice(-40).trim()
    fragment =
      words.length > 1 || /[　-鿿]/u.test(clean)
        ? `${encodeTextFragment(head)},${encodeTextFragment(tail)}`
        : encodeTextFragment(head)
  }
  const base = url.split('#')[0]
  return `${base}#:~:text=${fragment}`
}

function encodeTextFragment(text: string): string {
  return encodeURIComponent(text).replace(/-/g, '%2D').replace(/,/g, '%2C').replace(/&/g, '%26')
}

export function validSourceBox(box: unknown): box is [number, number, number, number] {
  return (
    Array.isArray(box) &&
    box.length === 4 &&
    box.every((n) => typeof n === 'number' && Number.isFinite(n) && n >= 0 && n <= 1) &&
    box[0] < box[2] &&
    box[1] < box[3]
  )
}

/** 原文文本，排除 Markdown 链接目标与 HTML 展示标签。 */
export function sourceQuoteText(content: string): string {
  return String(content || '')
    .replace(/!\[[^\]\n]*\]\([^\n)]*\)/g, ' ')
    .replace(/\[([^\]\n]+)\]\([^\n)]*\)/g, '$1')
    .replace(/<\/?[a-zA-Z][^>\n]*>/g, ' ')
    .replace(/&(#x[\da-f]+|#\d+|nbsp|amp|lt|gt|quot|apos);/gi, (raw, entity: string) => {
      const named: Record<string, string> = { nbsp: ' ', amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" }
      if (!entity.startsWith('#')) return named[entity.toLowerCase()] || raw
      const hex = entity[1]?.toLowerCase() === 'x'
      const point = Number.parseInt(entity.slice(hex ? 2 : 1), hex ? 16 : 10)
      return Number.isFinite(point) && point > 0 && point <= 0x10ffff ? String.fromCodePoint(point) : raw
    })
    .trim()
}

/** 按父 chunk 中的精确资源标识定位内嵌图片。 */
export function sourceImageContext(markdown: string, urls: string[]): SourceLocateRequest['imageContext'] {
  const images = [...markdown.matchAll(/!\[[^\]\n]*\]\(([^\n)]*)\)/g)]
  const selected = images.filter((m) => urls.includes(m[1]!))
  if (selected.length !== 1) return undefined
  const match = selected[0]!
  const index = images.indexOf(match)
  const previous = images[index - 1]
  const next = images[index + 1]
  const prefix = markdown.slice(previous ? previous.index! + previous[0].length : 0, match.index)
  const suffix = markdown.slice(match.index! + match[0].length, next?.index ?? markdown.length)
  const before = sourceQuoteText(prefix).trim().split(/\n\s*\n/).at(-1)?.trim() || ''
  const after = sourceQuoteText(suffix).trim().split(/\n\s*\n/)[0]?.trim() || ''
  return before || after ? { before, after } : undefined
}
