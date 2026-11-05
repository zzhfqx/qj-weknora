/**
 * 引用标记所支撑的回答段落：标记所在块（段落、列表项、表格单元）中
 * 标记之前的文本，去掉其他引用标记，并从上一条标记之后开始。
 * 如果标记独占一行（如“依据：[1]”），则借用它前面的若干块。
 */

const BLOCK_SELECTOR = 'li, p, td, th, blockquote, h1, h2, h3, h4, h5, h6, dd, dt'
const CITATION_SELECTOR = '.citation, .citation-kb, .citation-web, .citation-wiki'
const MIN_SENTENCE = 8
const MAX_LOOKBACK = 3
// 回溯永远不会离开当前渲染出来的回答区域。
const ANSWER_ROOT_SELECTOR = '.markdown-content, body, [contenteditable]'

function letterCount(text: string): number {
  return text.replace(/[\s\p{P}\p{S}]/gu, '').length
}

/** `text` 的最后一句；若过短则向前扩展，直到足以用于对齐。 */
export function lastSentence(text: string): string {
  const pieces = String(text || '')
    .replace(/\s+/g, ' ')
    .split(/(?<=[。！？!?；;])/u)
    .map((s) => s.trim())
    .filter(Boolean)
  let out = ''
  for (let i = pieces.length - 1; i >= 0; i--) {
    out = pieces[i] + out
    if (letterCount(out) >= MIN_SENTENCE) break
  }
  return out.slice(-300)
}

/**
 * 当标记自身文本 `own` 过短、不足以对齐时，依次把前面的块文本
 * （由近及远）拼到前面，形成锚点。
 */
export function anchorWithContext(own: string, previous: string[]): string {
  let text = own
  for (const prev of previous) {
    if (letterCount(lastSentence(text)) >= MIN_SENTENCE) break
    text = `${prev} ${text}`
  }
  return lastSentence(text)
}

function textWithoutCitations(node: Node): string {
  const clone = node.cloneNode(true) as Element | DocumentFragment
  clone.querySelectorAll?.(CITATION_SELECTOR).forEach((n) => n.remove())
  return clone.textContent || ''
}

/**
 * `block` 各外层上下文中位于它之前的文本，由近及远：
 * 前面的兄弟节点；若是嵌套列表，则还包括承载它的列表项文本。
 */
function previousBlockTexts(block: Element): string[] {
  const out: string[] = []
  for (let node: Element | null = block; node?.parentElement && out.length < MAX_LOOKBACK; node = node.parentElement) {
    if (node.matches(ANSWER_ROOT_SELECTOR)) break
    const range = node.ownerDocument.createRange()
    range.setStart(node.parentElement, 0)
    range.setEndBefore(node)
    const text = textWithoutCitations(range.cloneContents()).trim()
    if (text) out.push(text)
  }
  return out
}

export function citationAnchorText(marker: Element): string {
  try {
    const block = marker.closest(BLOCK_SELECTOR) || marker.parentElement
    if (!block) return ''
    const range = marker.ownerDocument.createRange()
    range.setStart(block, 0)
    // 一条引用覆盖自上一条引用以来的段落，同一段落内可能包含多个句子。
    range.setEndBefore(marker)
    const preceding = [...block.querySelectorAll(CITATION_SELECTOR)].filter(
      (previous) => previous.compareDocumentPosition(marker) & Node.DOCUMENT_POSITION_FOLLOWING,
    )
    for (const previous of preceding.reverse()) {
      const candidate = range.cloneRange()
      candidate.setStartAfter(previous)
      // 相邻引用支撑同一段落；跳过只包含引用的空白间隙。
      if (letterCount(textWithoutCitations(candidate.cloneContents())) > 0) {
        range.setStartAfter(previous)
        break
      }
    }
    const own = textWithoutCitations(range.cloneContents())
    const enough = letterCount(lastSentence(own)) >= MIN_SENTENCE
    return enough ? own.replace(/\s+/g, ' ').trim().slice(-300) : anchorWithContext(own, previousBlockTexts(block))
  } catch {
    // 对齐只用于缩小高亮范围；没有句子时展示整个 chunk。
    return ''
  }
}
