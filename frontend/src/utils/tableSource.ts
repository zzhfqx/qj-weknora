import { findNormalizedMatches, normalizeForMatch, normalizeWithPositions, type TextMatch } from './sourceLocator'

export type TableSourceMatch = TextMatch & { page: number; quote: string }
type Column = { text: string; ends: number[] }
type Row = { text: string; page: number; start: number; norm: string; pos: number[] }
type Step = { row: number; hits: TableSourceMatch[] }

function columns(scope: string): Column[] {
  return scope.split(/[|\n]/u).flatMap((cell) => {
    const words = cell
      .trim()
      .split(/\s+/u)
      .map(normalizeForMatch)
      .filter(Boolean)
    if (words.length < 4 || new Set(words.filter((w) => w.length >= 2)).size < 3) return []
    let text = ''
    const ends: number[] = []
    for (const word of words) {
      text += word
      ends.push(text.length)
    }
    return text.length >= 12 ? [{ text, ends }] : []
  })
}

function spanningLabel(scope: string): string {
  const firstCell = scope.trim().split('|')[1]?.trim() || ''
  return /^(?:\p{Script=Han}\s+){1,7}\p{Script=Han}$/u.test(firstCell) ? normalizeForMatch(firstCell) : ''
}

/**
 * 独立片段仍必须属于被引用的跨行标签。
 * 在保留偏移量的同时，把其他章节的行从兜底匹配中隐藏。
 */
export function tableFragmentPages(pages: string[], scope: string): string[] {
  const qualifier = spanningLabel(scope)
  if (!scope.includes('|') || !qualifier) return pages
  const ordinal = qualifier.match(/^[一二三四五六七八九十百]+(.+)$/u)
  const heading = ordinal ? new RegExp(`^[一二三四五六七八九十百]+${ordinal[1]}`) : null
  let active = false
  return pages.map((page) =>
    page
      .split('\n')
      .map((line) => {
        const norm = normalizeForMatch(line)
        const label = heading ? norm.match(heading)?.[0] : norm.startsWith(qualifier) ? qualifier : undefined
        if (label) active = label === qualifier
        return active ? line : ' '.repeat(line.length)
      })
      .join('\n'),
  )
}

/**
 * 通过同一原始行上的成对取值、且顺序一致，恢复按列序列化的表格。
 * 仅凭重复的课程名称永远无法确定行；只接受完整的列序列，
 * 不做相似度匹配，也不跳过原文取值。存在多组行序列时视为未解决。
 */
export function findTableSource(pages: string[], scope: string): TableSourceMatch[] {
  if (!scope.includes('|')) return []
  // 纵向排版的跨行标签会被序列化为独立字形（例如院系或年级），
  // 这里把它保留为作用域约束。
  const qualifier = spanningLabel(scope)
  const cols = columns(scope)
  if (cols.length < 2 || cols.length > 12) return []
  let operations = 0
  const rows: Row[] = pages.flatMap((page, i) => {
    let start = 0
    return page.split('\n').map((text) => {
      const row = { text, page: i + 1, start, ...normalizeWithPositions(text) }
      start += text.length + 1
      return row
    })
  })
  const ordinalLabel = qualifier.match(/^[一二三四五六七八九十百]+(.+)$/u)
  const heading = ordinalLabel ? new RegExp(`^[一二三四五六七八九十百]+${ordinalLabel[1]}`) : null
  let active = !qualifier
  const allowed = rows.map((row) => {
    const label = heading ? row.norm.match(heading)?.[0] : row.norm.startsWith(qualifier) ? qualifier : undefined
    if (label) active = label === qualifier
    return active
  })
  const solutions: Array<{ steps: Step[]; weight: number }> = []
  for (let a = 0; a < cols.length; a++) {
    for (let b = a + 1; b < cols.length; b++) {
      const first = cols[a]!
      const second = cols[b]!
      let states = new Map<string, Step[][]>([['0:0', [[]]]])
      let overflow = false
      for (let r = 0; r < rows.length; r++) {
        const row = rows[r]!
        if (row.norm.length < 4 || (qualifier && !allowed[r])) continue
        const prefix = (column: Column, offset: number) => {
          // 只在原文词元边界处结束，包括由多个词组成的单元格取值。
          for (let i = column.ends.length - 1; i >= 0; i--) {
            const end = column.ends[i]!
            const size = end - offset
            if (size < 2 || size > row.norm.length) continue
            const hits = findNormalizedMatches(row.norm, column.text.slice(offset, end))
            if (hits.length !== 1) continue
            const hit = hits[0]!
            const start = row.pos[hit.start]!
            const last = row.pos[hit.end - 1]!
            const finish = last + (row.text.codePointAt(last)! > 0xffff ? 2 : 1)
            return {
              end,
              hit: { page: row.page, start: row.start + start, end: row.start + finish, quote: row.text.slice(start, finish) },
            }
          }
          return null
        }
        const left = new Map<number, ReturnType<typeof prefix>>()
        const right = new Map<number, ReturnType<typeof prefix>>()
        const containsValue = (column: Column) =>
          column.ends.some((end, i) => {
            const word = column.text.slice(column.ends[i - 1] || 0, end)
            return word.length >= 2 && findNormalizedMatches(row.norm, word).length > 0
          })
        const dataRow = containsValue(first) && containsValue(second)
        const nextStates = new Map<string, Step[][]>()
        const put = (key: string, paths: Step[][]) => {
          const list = nextStates.get(key) || []
          for (const path of paths) {
            if (!list.some((have) => have.map((s) => s.row).join(',') === path.map((s) => s.row).join(','))) list.push(path)
            if (list.length >= 2) break
          }
          nextStates.set(key, list.slice(0, 2))
        }
        for (const [key, paths] of states) {
          if (++operations > 100000) return []
          const [x, y] = key.split(':').map(Number) as [number, number]
          if ((x === 0 && y === 0) || !dataRow || (x === first.text.length && y === second.text.length)) put(key, paths)
          if (x === first.text.length || y === second.text.length) continue
          if (!left.has(x)) left.set(x, prefix(first, x))
          if (!right.has(y)) right.set(y, prefix(second, y))
          const p = left.get(x)
          const q = right.get(y)
          if (!p || !q) continue
          // 不同列必须占据不同的单元格，不能是同一段重复文本。
          if (p.hit.start < q.hit.end && q.hit.start < p.hit.end) continue
          put(
            `${p.end}:${q.end}`,
            paths.map((path) => [...path, { row: r, hits: [p.hit, q.hit] }]),
          )
        }
        states = nextStates
        // 限制病态或高度重复的输入；预算耗尽时绝不臆测结果。
        if (states.size > 4096) {
          overflow = true
          break
        }
      }
      const complete = states.get(`${first.text.length}:${second.text.length}`) || []
      if (!overflow && complete.length === 1 && complete[0]!.length >= 3) {
        const opening = rows[complete[0]![0]!.row]!
        if (qualifier && !normalizeForMatch(pages[opening.page - 1]!.slice(0, opening.start)).includes(qualifier)) continue
        solutions.push({ steps: complete[0]!, weight: first.text.length + second.text.length })
      }
    }
  }
  if (!solutions.length) return []
  solutions.sort((a, b) => b.steps.length - a.steps.length || b.weight - a.weight)
  const best = solutions[0]!
  // 相互独立的完整列对必须在行序列上一致。较短的序列可以描述子集，
  // 但不能把原文位置移到其他行。
  const selectedRows = new Set(best.steps.map((step) => step.row))
  if (solutions.some((s) => s.steps.some((step) => !selectedRows.has(step.row)))) return []
  const hits = best.steps.flatMap((step) => step.hits)
  return hits.sort((a, b) => a.page - b.page || a.start - b.start)
}
