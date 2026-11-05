import { renderDocumentPreviewMarkdown } from './documentPreviewMarkdown'
import { buildTextIndex, findTextRange } from './sourceLocatorDom'
import { findInText, normalizeForMatch, quotedSourceExcerpt } from './sourceLocator'
import { sourceOverlapLength } from './fragmentedSource'

/**
 * 在渲染后的文本中匹配 Markdown 原文，包括围栏代码块和行内代码。
 * Markdown 语法和围栏语言标记都不是可见的原文文本。
 * 渲染到惰性 template 中还能保留包含类 HTML 文本的代码。
 */
export function findMarkdownSourceRange(
  root: Element,
  markdown: string,
  sentence = '',
): { range: Range; ranges?: Range[]; narrowed: boolean; exact: boolean } | null {
  const template = root.ownerDocument.createElement('template')
  template.innerHTML = renderDocumentPreviewMarkdown(markdown)
  const source = buildTextIndex(template.content).text
  // 先确定完整的原文区域，再缩小范围。
  // 文档中其他位置的相似段落不能用来挽救缺失或有歧义的原文。
  const region = findTextRange(root, source)
  if (!region) return null
  const excerpt = quotedSourceExcerpt(source, sentence) || sentence
  if (excerpt && findInText(source, excerpt)) {
    const exact = findTextRange(root, excerpt)
    if (
      exact &&
      region.isPointInRange(exact.startContainer, exact.startOffset) &&
      region.isPointInRange(exact.endContainer, exact.endOffset)
    ) {
      return { range: exact, narrowed: true, exact: true }
    }
  }
  // 从原始 DOM（而非渲染后的 chunk）恢复段落边界。子块可能只覆盖原始段落的一半；
  // 上面的完整唯一匹配已经确定了出现位置，只有与之相交的块才能提供上下文，
  // 绝不能使用相邻的相似段落。
  const blocks = [...root.querySelectorAll('p, li, pre, td, th')]
    .filter((e) => !e.querySelector('p, li, pre, td, th') && region.intersectsNode(e))
    .flatMap((element) => {
      const quote = buildTextIndex(element).text.trim()
      const range = findTextRange(element, quote)
      if (!range) return []
      const overlap = range.cloneRange()
      if (overlap.compareBoundaryPoints(Range.START_TO_START, region) < 0) {
        overlap.setStart(region.startContainer, region.startOffset)
      }
      if (overlap.compareBoundaryPoints(Range.END_TO_END, region) > 0) {
        overlap.setEnd(region.endContainer, region.endOffset)
      }
      // 一个分隔符或两个边界字符，不足以把缺乏支撑的段落挂到被引用原文上。
      if (normalizeForMatch(overlap.toString()).length < 8) return []
      return [{ quote, range }]
    })
  // 中文分句比重复出现的命令名更有锚定力。
  // 只接受完整的原文片段，而不是猜测块中的模糊子串。
  const clauses = blocks.filter(({ quote }) =>
    [...quote.matchAll(/[\p{Script=Han}]{8,}/gu)].some((m) => findInText(sentence, m[0])),
  )
  if (clauses.length && clauses.length <= 3) {
    const ranges = clauses.map((block) => block.range)
    return { range: ranges[0]!, ranges, narrowed: true, exact: false }
  }
  // 逐字重叠只用于给与已验证原文相关的段落排序。
  // 段落上下文并不代表逐字引用覆盖了整段。
  const ranked = blocks
    .map((block) => ({ ...block, score: sourceOverlapLength(block.quote, sentence.slice(-300)) }))
    .sort((a, b) => b.score - a.score)
  const best = ranked[0]
  if (best && best.score >= 8 && best.score >= (ranked[1]?.score || 0) + 4) {
    return { range: best.range, narrowed: true, exact: false }
  }
  return { range: region, narrowed: false, exact: true }
}
