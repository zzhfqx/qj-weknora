// Wiki 反链标题解析工具
//
// 用于在反链列表中显示可读的页面标题，而不是原始 slug。
// 解析优先级：
//   1. 页面详情接口返回的 in_link_titles 映射（精确，包含未加载的页面）
//   2. 侧边栏已加载页面的标题（作为兜底）
//   3. slug 去掉类型前缀后的后半段（最后兜底）

export interface WikiBacklinkPage {
  slug: string
  title: string
}

// resolveWikiBacklinkTitle 解析反链 slug 对应的显示标题。
//
// 优先使用页面详情返回的 titles 映射（包含所有反链页面的标题，
// 包括侧边栏未加载的），其次回退到已加载页面列表，
// 最后用 slug 去掉类型前缀作为兜底显示。
export function resolveWikiBacklinkTitle(
  slug: string,
  titles: Record<string, string>,
  pages: WikiBacklinkPage[],
): string {
  // 1. 优先使用接口返回的标题映射（用 hasOwnProperty 避免原型污染）
  if (Object.prototype.hasOwnProperty.call(titles, slug)) {
    const value = titles[slug]
    if (typeof value === 'string' && value.trim()) return value.trim()
  }

  // 2. 回退到已加载页面的标题
  const loadedPage = pages.find(page => page.slug === slug)
  if (loadedPage?.title) return loadedPage.title

  // 3. 最后兜底：去掉类型前缀，显示层级 slug
  const parts = slug.split('/')
  return parts.length > 1 ? parts.slice(1).join('/') : slug
}
