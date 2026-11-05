import { findInText, findTextMatches } from './sourceLocator'

/** 用于标记被引用文本的 CSS 自定义高亮名称。 */
export const SOURCE_HIGHLIGHT_NAME = 'source-locate'
/** 加到被引用的整块元素（段落、表格行、幻灯片）上的类名。 */
export const SOURCE_BLOCK_CLASS = 'source-locate-block'
const MARK_CLASS = 'source-locate-mark'

type TextIndex = {
  text: string
  nodes: Array<{ node: Text; start: number }>
}

const SKIP_TAGS = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE'])

/** 拼接 `root` 下的所有文本节点，并记录每个节点的起始位置。 */
export function buildTextIndex(root: Node): TextIndex {
  const nodes: TextIndex['nodes'] = []
  let text = ''
  const doc = root.ownerDocument || (root as Document)
  const walker = doc.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      let el = node.parentElement
      while (el && el !== root) {
        if (SKIP_TAGS.has(el.tagName)) return NodeFilter.FILTER_REJECT
        el = el.parentElement
      }
      return node.nodeValue ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT
    },
  })
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const t = n as Text
    nodes.push({ node: t, start: text.length })
    // 分隔符可避免相邻块的词语粘连；它不是字母或数字，匹配时会忽略。
    text += t.nodeValue + ' '
  }
  return { text, nodes }
}

function pointAt(index: TextIndex, offset: number): { node: Text; offset: number } | null {
  if (!index.nodes.length) return null
  let lo = 0
  let hi = index.nodes.length - 1
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1
    if (index.nodes[mid].start <= offset) lo = mid
    else hi = mid - 1
  }
  const entry = index.nodes[lo]
  const len = entry.node.nodeValue?.length || 0
  return { node: entry.node, offset: Math.max(0, Math.min(len, offset - entry.start)) }
}

/** `root` 中匹配 `quote` 的最佳文本范围，找不到时返回 null。 */
export function findTextRange(root: Node, quote: string, index = buildTextIndex(root)): Range | null {
  const hit = findInText(index.text, quote)
  if (!hit) return null
  return rangeAt(root, hit, index)
}

export function findTextRanges(root: Node, quote: string): Range[] {
  const index = buildTextIndex(root)
  return findTextMatches(index.text, quote)
    .map((hit) => rangeAt(root, hit, index))
    .filter((r): r is Range => !!r)
}

function rangeAt(root: Node, hit: { start: number; end: number }, index: TextIndex): Range | null {
  const from = pointAt(index, hit.start)
  const to = pointAt(index, hit.end)
  if (!from || !to) return null
  const range = (root.ownerDocument || (root as Document)).createRange()
  range.setStart(from.node, from.offset)
  range.setEnd(to.node, to.offset)
  return range
}

/**
 * 覆盖 `root` 文本内容中码点区间 [start, end) 的范围
 * （后端为纯文本原文上报的正是该单位）。
 */
export function rangeForOffsets(root: Node, startCp: number, endCp: number): Range | null {
  const index = buildTextIndex(root)
  if (!index.nodes.length) return null
  const full = index.nodes.map((n) => n.node.nodeValue || '').join('')
  const toUnits = (cp: number) => {
    let units = 0
    let count = 0
    for (const ch of full) {
      if (count >= cp) break
      units += ch.length
      count++
    }
    return units
  }
  const start = toUnits(startCp)
  const end = toUnits(endCp)
  let consumed = 0
  let startPoint: { node: Text; offset: number } | null = null
  let endPoint: { node: Text; offset: number } | null = null
  for (const { node } of index.nodes) {
    const len = node.nodeValue?.length || 0
    if (!startPoint && start <= consumed + len) startPoint = { node, offset: Math.max(0, start - consumed) }
    if (!endPoint && end <= consumed + len) {
      endPoint = { node, offset: Math.max(0, end - consumed) }
      break
    }
    consumed += len
  }
  if (!startPoint) return null
  if (!endPoint) {
    const last = index.nodes[index.nodes.length - 1].node
    endPoint = { node: last, offset: last.nodeValue?.length || 0 }
  }
  const range = (root.ownerDocument || (root as Document)).createRange()
  range.setStart(startPoint.node, startPoint.offset)
  range.setEnd(endPoint.node, endPoint.offset)
  return range
}

type HighlightRegistry = { set(name: string, h: unknown): void; delete(name: string): void }

function highlightRegistry(): { registry: HighlightRegistry; Highlight: new (...r: Range[]) => unknown } | null {
  const css = (globalThis as unknown as { CSS?: { highlights?: HighlightRegistry } }).CSS
  const Highlight = (globalThis as unknown as { Highlight?: new (...r: Range[]) => unknown }).Highlight
  if (css?.highlights && Highlight) return { registry: css.highlights, Highlight }
  return null
}

/**
 * 若存在 CSS 自定义高亮 API，则在不改动文档结构的前提下高亮范围；
 * 否则把文本包进 <mark>。返回一个用于撤销高亮的函数。
 */
export function highlightRanges(ranges: Range[]): () => void {
  const live = ranges.filter((r) => r && !r.collapsed)
  if (!live.length) return () => {}
  const api = highlightRegistry()
  if (api) {
    api.registry.set(SOURCE_HIGHLIGHT_NAME, new api.Highlight(...live))
    return () => api.registry.delete(SOURCE_HIGHLIGHT_NAME)
  }
  const marks: HTMLElement[] = []
  for (const range of live) marks.push(...wrapRange(range))
  return () => {
    for (const mark of marks) {
      const parent = mark.parentNode
      if (!parent) continue
      while (mark.firstChild) parent.insertBefore(mark.firstChild, mark)
      parent.removeChild(mark)
      parent.normalize()
    }
  }
}

function wrapRange(range: Range): HTMLElement[] {
  const doc = range.startContainer.ownerDocument
  if (!doc) return []
  const texts: Text[] = []
  const walker = doc.createTreeWalker(range.commonAncestorContainer, NodeFilter.SHOW_TEXT)
  for (let n = walker.currentNode as Node | null; n; n = walker.nextNode()) {
    if (n.nodeType === Node.TEXT_NODE && range.intersectsNode(n)) texts.push(n as Text)
  }
  const marks: HTMLElement[] = []
  for (const node of texts) {
    let target = node
    const start = node === range.startContainer ? range.startOffset : 0
    const end = node === range.endContainer ? range.endOffset : node.length
    if (end <= start) continue
    if (end < target.length) target.splitText(end)
    if (start > 0) target = target.splitText(start)
    const mark = doc.createElement('mark')
    mark.className = MARK_CLASS
    target.parentNode?.insertBefore(mark, target)
    mark.appendChild(target)
    marks.push(mark)
  }
  return marks
}

/** 把整块元素标记为被引用，返回撤销标记的函数。 */
export function highlightElements(elements: Element[]): () => void {
  const list = elements.filter(Boolean)
  for (const el of list) el.classList.add(SOURCE_BLOCK_CLASS)
  return () => {
    for (const el of list) el.classList.remove(SOURCE_BLOCK_CLASS)
  }
}

/**
 * 把 `container` 滚动到使 `rect`（视口坐标）位于约三分之一高度处，
 * 且不移动任何外层滚动容器。滚动是瞬时完成的：预览尚未稳定时，
 * 平滑滚动会被焦点处理和重排打断。
 */
export function scrollRectIntoContainer(container: Element, rect: DOMRect | { top: number; bottom: number }) {
  const box = container.getBoundingClientRect()
  const target = container.scrollTop + (rect.top - box.top) - Math.max(24, box.height / 3)
  container.scrollTop = Math.max(0, target)
}

/** `el` 最近的可滚动祖先（找不到时返回 `fallback`）。 */
export function scrollParent(el: Element | null, fallback: Element): Element {
  for (let node = el?.parentElement; node; node = node.parentElement) {
    const style = getComputedStyle(node)
    if (/(auto|scroll)/.test(style.overflowY) && node.scrollHeight > node.clientHeight) return node
    if (node === fallback) break
  }
  return fallback
}
