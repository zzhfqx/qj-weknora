/**
 * 旧版智能体工具名归一化。
 *
 * 后端在 v0.8.2 把知识库检索类工具合并：
 *   - knowledge_search / grep_chunks                      -> search_knowledge
 *   - list_knowledge_chunks / get_document_info /
 *     wiki_read_source_doc（精读源文档）                   -> read_document
 *
 * 合并前保存的智能体配置，其 `allowed_tools` 里仍带着已退役的旧名字。
 * 编辑器在加载时把它们映射到新名字，工具复选框才能正确回显；
 * 聊天历史渲染继续按旧名字原样展示（见 AgentStreamDisplay.vue），
 * 因此本 helper 只用于"配置形态"的工具列表，绝不用于工具调用事件。
 *
 * 与后端 internal/agent/tools/definitions.go 的 legacyToolSuccessors 保持一致。
 */
export const LEGACY_TOOL_NAME_MAP: Readonly<Record<string, string>> = Object.freeze({
  knowledge_search: 'search_knowledge',
  grep_chunks: 'search_knowledge',
  list_knowledge_chunks: 'read_document',
  get_document_info: 'read_document',
  wiki_read_source_doc: 'read_document',
})

/** 把单个工具名映射为当前名字（非旧名原样返回）。 */
export function normalizeLegacyToolName(toolName: string): string {
  return LEGACY_TOOL_NAME_MAP[toolName] ?? toolName
}

/**
 * 归一化 `allowed_tools` 形态的列表：退役名替换为后继名，
 * 去重（先出现者保留），其余顺序不变；非字符串/空项丢弃。
 * 始终返回新数组。
 */
export function normalizeLegacyToolNames(
  tools: readonly unknown[] | undefined | null,
): string[] {
  if (!Array.isArray(tools) || tools.length === 0) return []
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of tools) {
    if (typeof raw !== 'string') continue
    const name = normalizeLegacyToolName(raw.trim())
    if (!name || seen.has(name)) continue
    seen.add(name)
    out.push(name)
  }
  return out
}

/** 判断列表是否包含某工具（按当前名或旧名都算）。 */
export function allowedToolsInclude(
  tools: readonly string[] | undefined | null,
  toolName: string,
): boolean {
  if (!tools || tools.length === 0) return false
  const target = normalizeLegacyToolName(toolName)
  return tools.some(t => normalizeLegacyToolName(t) === target)
}
