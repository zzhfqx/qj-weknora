// 几何计算独立于 SVG，因此稠密图和非连通图无需浏览器即可推演。
// 坐标采用世界单位，而非屏幕像素。
export interface GraphPoint { x: number; y: number }
export interface GraphLink { source: string; target: string }
export interface LayoutInput { slug: string; link_count: number; page_type?: string; title?: string }
export interface GraphBounds { left: number; top: number; right: number; bottom: number }
export interface LayoutPlacement extends GraphPoint {
  center: GraphPoint
  stationary?: boolean
  degree?: number
}
export interface GraphCluster extends GraphPoint, GraphBounds {
  slugs: string[]
  representative: string
  anchors: string[]
}
export interface GraphSection extends GraphBounds {
  kind: 'unlinked' | 'offscreen'
  count: number
}

export function graphNodeRadius(linkCount: number): number {
  // 平方根缩放让叶子节点保持小尺寸，从而把枢纽节点凸显为地标。
  return Math.max(5, Math.min(20, 4 + Math.sqrt(Math.max(0, linkCount)) * 1.8))
}

/** 在无向、无权图上执行确定性的多层 Louvain 社区发现。
 * 返回每个节点所属的社区编号；调用方必须以稳定顺序传入节点，
 * 因为并列情况会按索引裁决。
 */
export function graphCommunities(neighbors: readonly (readonly number[])[]): number[] {
  const n = neighbors.length
  let membership = Array.from({ length: n }, (_, i) => i)
  // 层级图：超级节点之间的带权邻接关系，加上已经折叠进各超级节点
  // 内部的边权重。
  let weights: Map<number, number>[] = neighbors.map(list => {
    const row = new Map<number, number>()
    for (const j of list) row.set(j, (row.get(j) ?? 0) + 1)
    return row
  })
  let inner = new Array<number>(n).fill(0)
  for (;;) {
    const size = weights.length
    const degree = weights.map((row, i) => [...row.values()].reduce((sum, w) => sum + w, 0) + 2 * inner[i]!)
    const total = degree.reduce((sum, d) => sum + d, 0)
    if (!total) break
    const community = Array.from({ length: size }, (_, i) => i)
    const tot = [...degree]
    let movedAny = false
    for (let pass = 0; pass < 32; pass++) {
      let moved = false
      for (let i = 0; i < size; i++) {
        const own = community[i]!
        const links = new Map<number, number>()
        for (const [j, w] of weights[i]!) links.set(community[j]!, (links.get(community[j]!) ?? 0) + w)
        tot[own] -= degree[i]!
        let best = own, bestGain = (links.get(own) ?? 0) - tot[own]! * degree[i]! / total
        for (const c of [...links.keys()].sort((a, b) => a - b)) {
          const gain = links.get(c)! - tot[c]! * degree[i]! / total
          if (gain > bestGain + 1e-9) { best = c; bestGain = gain }
        }
        tot[best] += degree[i]!
        if (best !== own) { community[i] = best; moved = true; movedAny = true }
      }
      if (!moved) break
    }
    if (!movedAny) break
    const renumber = new Map<number, number>()
    for (const c of community) if (!renumber.has(c)) renumber.set(c, renumber.size)
    const next = Array.from({ length: renumber.size }, () => new Map<number, number>())
    const nextInner = new Array<number>(renumber.size).fill(0)
    for (let i = 0; i < size; i++) {
      const ci = renumber.get(community[i]!)!
      nextInner[ci] += inner[i]!
      for (const [j, w] of weights[i]!) {
        const cj = renumber.get(community[j]!)!
        // 每条无向边在两行中各出现一次；内部只计一次。
        if (ci === cj) nextInner[ci] += w / 2
        else next[ci]!.set(cj, (next[ci]!.get(cj) ?? 0) + w)
      }
    }
    membership = membership.map(c => renumber.get(community[c]!)!)
    weights = next; inner = nextInner
    if (renumber.size === size) break
  }
  return membership
}

/** 地标标题文本：summary 类型页面沿用其源文件名。 */
export function graphLandmarkTitle(title: string): string {
  const name = title.replace(/\s*-\s*Summary$/i, '')
    .replace(/\.(pdf|docx?|pptx?|xlsx?|csv|md|txt|html?|json|ya?ml|xml|png|jpe?g)$/i, '')
    .replace(/\s*[(（]\d+[)）]$/, '').trim()
  return name || title
}

// summary 以其源文件命名，而源文件名通常能很好地指代文档，因此它只会与
// 实体、概念形成近似并列。index 页是导航性质的，即便链接了整片区域，
// 也不适合作为该区域的名字。
const anchorTypeQuality: Record<string, number> = {
  entity: 1.15, concept: 1, synthesis: 0.8, comparison: 0.8, summary: 0.9, index: 0.2,
}

/** 为一个连通分量及其各独立主题命名。
 * 首个地标是该分量中最显著、最具可读性的页面，即便它桥接了所有主题。
 * 社区即主题；每个主题至多贡献一个地标，依据其在主题内部的链接数以及
 * 标题作为名称的可读性来挑选。大多链接向已有地标的主题、以及重复某个
 * 已有名称的页面，不再额外命名。后续挑选倾向于离已有标签更远的较大
 * 主题，使名称分散开而不是堆叠。由多篇文档构成的分量，还会为它们
 * 共同链接最多的页面命名。
 */
export function selectGraphAnchors(
  slugs: string[], adjacency: ReadonlyMap<string, ReadonlySet<string>>,
  pages: ReadonlyMap<string, Pick<LayoutInput, 'page_type' | 'title'>> = new Map(),
): string[] {
  if (!slugs.length) return []
  const order = [...new Set(slugs)].sort()
  const index = new Map(order.map((slug, i) => [slug, i]))
  const neighbors = order.map(slug => [...(adjacency.get(slug) ?? [])]
    .flatMap(n => index.has(n) && n !== slug ? [index.get(n)!] : []).sort((a, b) => a - b))
  const membership = graphCommunities(neighbors)
  const groups = new Map<number, number[]>()
  membership.forEach((c, i) => { const group = groups.get(c) ?? []; group.push(i); groups.set(c, group) })

  const quality = (i: number) => {
    const page = pages.get(order[i]!)
    const title = graphLandmarkTitle(page?.title ?? '')
    // 像 arXiv 文件名（"2412.18925v1"）这样的标识符没有可读的词。
    // 含至少两个 CJK 汉字、或三个拉丁字母，才认为标题可读
    const readable = /[一-鿿]{2}|[a-z]{3}/i.test(title) ? 1 : 0.3
    return (anchorTypeQuality[page?.page_type ?? ''] ?? 1) * (title.length > 18 ? 0.6 : 1) * readable
  }
  const best = (group: number[], score: (i: number) => number) => group.reduce((top, i) => {
    const a = score(i), b = score(top)
    return a > b + 1e-9 || (Math.abs(a - b) <= 1e-9 && neighbors[i]!.length > neighbors[top]!.length) ? i : top
  })
  // 主题由身处其中的页面命名；指向主题外部的链接仍计入一定的显著性。
  const themes = [...groups.values()].map(group => {
    const inside = new Set(group)
    const score = (i: number) => {
      const internal = neighbors[i]!.filter(j => inside.has(j)).length
      return (internal + 0.5 * (neighbors[i]!.length - internal) + 1) * quality(i)
    }
    const ranked = [...group].sort((a, b) => score(b) - score(a) || neighbors[b]!.length - neighbors[a]!.length || a - b)
    return { size: group.length, ranked, members: inside }
  }).sort((a, b) => b.size - a.size || a.ranked[0]! - b.ranked[0]!)

  const name = best(order.map((_, i) => i), i => (neighbors[i]!.length + 1) * quality(i))
  const home = themes.find(theme => theme.members.has(name))!
  // 一个主题的碎片不单独算作主题。
  const minimum = Math.max(3, themes[0]!.size * 0.3)
  const candidates = themes.filter(theme => theme !== home && theme.size >= minimum)
  // 对已命名事物的第二个名称：邻域几乎相同（作者紧挨着其论文），
  // 或标题包含对方（"北京市第三代社保卡"挨着"第三代社保卡"）。
  const closed = (i: number) => new Set([i, ...neighbors[i]!])
  const normalized = (i: number) => graphLandmarkTitle(pages.get(order[i]!)?.title ?? '')
    .toLowerCase().replace(/[\s《》“”"'‘’（）()·・]/g, '')
  const redundant = (i: number, chosen: number[]) => chosen.some(s => {
    const a = closed(i), b = closed(s)
    const shared = [...a].filter(x => b.has(x)).length
    if (shared / (a.size + b.size - shared) >= 0.5) return true
    const x = normalized(i), y = normalized(s)
    return !!x && !!y && (x.includes(y) || y.includes(x))
  })
  const budget = order.length < 16 ? 1 : order.length < 50 ? 2 : order.length < 100 ? 3 : order.length < 200 ? 4 : 5
  const bfs = (start: number) => {
    const distance = new Array<number>(order.length).fill(Infinity)
    distance[start] = 0
    const queue = [start]
    for (let q = 0; q < queue.length; q++) {
      for (const j of neighbors[queue[q]!]!) {
        if (distance[j] !== Infinity) continue
        distance[j] = distance[queue[q]!]! + 1; queue.push(j)
      }
    }
    return distance
  }
  // 一旦标签之间相隔分量直径的合理份额，就视为完全分开；比这更近时，
  // 更大的主题仍优先。
  const nearest = bfs(name)
  const far = nearest.reduce((top, d, i) => d !== Infinity && d > nearest[top]! ? i : top, name)
  const diameter = Math.max(...bfs(far).filter(Number.isFinite))
  const reach = Math.max(2, diameter / budget)
  const selected = [name]
  const open = new Set(candidates)
  while (selected.length < budget && open.size) {
    let pick: { theme: typeof candidates[number]; label: number } | undefined, bestScore = -1
    for (const theme of open) {
      // 主题的大部分已经挂在某个已有标签下（一篇文档围绕其主题）：
      // 再命名只会是一个细节页。
      const covered = selected.some(s => neighbors[s]!.filter(j => theme.members.has(j)).length * 2 >= theme.size)
      const label = covered ? undefined : theme.ranked.find(i => !redundant(i, selected))
      if (label === undefined) { open.delete(theme); continue }
      const score = Math.sqrt(theme.size) * Math.min(nearest[label]!, reach) / reach
      if (score > bestScore + 1e-9 || (Math.abs(score - bestScore) <= 1e-9 && pick && nearest[label]! > nearest[pick.label]!)) {
        pick = { theme, label }; bestScore = score
      }
    }
    if (!pick) break
    open.delete(pick.theme)
    selected.push(pick.label)
    bfs(pick.label).forEach((d, i) => { nearest[i] = Math.min(nearest[i]!, d) })
  }

  // 主题大多随文档而定，因此它们的名字能说明这里有哪些文档，却说明不
  // 了它们为何聚在一起。一个被大多数文档共享的页面能说明这一点；除非
  // 已有标签是这样的页面，否则补上它。
  const documents = new Set(order.flatMap((slug, i) => pages.get(slug)?.page_type === 'summary' ? [i] : []))
  if (documents.size >= 2) {
    const spans = (i: number) => documents.has(i) ? 0 : neighbors[i]!.filter(j => documents.has(j)).length
    const needed = Math.max(2, Math.ceil(documents.size * 2 / 3))
    if (!selected.some(s => spans(s) >= needed)) {
      const weakest = Math.min(...selected.map(s => neighbors[s]!.length))
      const prominence = (i: number) => (neighbors[i]!.length + 1) * quality(i)
      const bridge = order.map((_, i) => i)
        .filter(i => spans(i) >= needed && neighbors[i]!.length >= weakest * 0.6 && !redundant(i, selected))
        .reduce<number | undefined>((top, i) => top === undefined || spans(i) > spans(top)
          || (spans(i) === spans(top) && prominence(i) > prominence(top) + 1e-9) ? i : top, undefined)
      if (bridge !== undefined) selected.push(bridge)
    }
  }
  return selected.map(i => order[i]!)
}

/** 各连通分量分别打包；孤立节点放到两个明确的“货架”上。
 * 缺少可见边并不代表该页面在知识库中没有链接。
 */
export function buildGraphLayout(
  nodes: LayoutInput[], edges: GraphLink[], width: number, height: number,
  prior: ReadonlyMap<string, GraphPoint> = new Map(),
) {
  const nodeBySlug = new Map(nodes.map(n => [n.slug, n]))
  const adjacency = new Map(nodes.map(n => [n.slug, new Set<string>()]))
  for (const e of edges) {
    if (e.source === e.target || !nodeBySlug.has(e.source) || !nodeBySlug.has(e.target)) continue
    adjacency.get(e.source)!.add(e.target)
    adjacency.get(e.target)!.add(e.source)
  }
  const components: string[][] = []
  const unlinked: string[] = []
  const offscreen: string[] = []
  const visited = new Set<string>()
  for (const slug of [...nodeBySlug.keys()].sort()) {
    if (visited.has(slug)) continue
    visited.add(slug)
    if (!adjacency.get(slug)!.size) {
      ;(nodeBySlug.get(slug)!.link_count > 0 ? offscreen : unlinked).push(slug)
      continue
    }
    const component = [slug]
    for (let i = 0; i < component.length; i++) {
      for (const neighbor of adjacency.get(component[i]!)!) {
        if (visited.has(neighbor)) continue
        visited.add(neighbor)
        component.push(neighbor)
      }
    }
    components.push(component.sort())
  }
  components.sort((a, b) => b.length - a.length || (a[0]! < b[0]! ? -1 : 1))

  // 依据每个分量的拓扑预留空间，而非圆形边界。这些包围盒只负责初始摆放
  // 各分组；模拟过程从不把节点裁剪在盒内，也不把单个节点拉向共同中心。
  const aspect = Math.min(1.8, Math.max(0.8, width / Math.max(height, 1)))
  const stretch = Math.sqrt(aspect)
  const goldenAngle = Math.PI * (3 - Math.sqrt(5))
  const clusters: GraphCluster[] = []
  const seeds = new Map<string, GraphPoint>()
  const cellSize = 500
  const cells = new Map<string, GraphCluster[]>()
  const keysFor = (bounds: GraphBounds) => {
    const keys: string[] = []
    for (let gx = Math.floor(bounds.left / cellSize); gx <= Math.floor(bounds.right / cellSize); gx++) {
      for (let gy = Math.floor(bounds.top / cellSize); gy <= Math.floor(bounds.bottom / cellSize); gy++) keys.push(`${gx},${gy}`)
    }
    return keys
  }
  for (const slugs of components) {
    const representative = [...slugs].sort((a, b) => adjacency.get(b)!.size - adjacency.get(a)!.size || (a < b ? -1 : 1))[0]!
    // 从连接最多的页面为生成树播种。各分支顺着父节点方向延伸，因此链和
    // 树不会一生成就是圆盘状。
    const local = new Map<string, GraphPoint & { heading: number }>()
    local.set(representative, { x: 0, y: 0, heading: 0 })
    const queue = [representative]
    for (let cursor = 0; cursor < queue.length; cursor++) {
      const parentSlug = queue[cursor]!
      const parent = local.get(parentSlug)!
      const children = [...adjacency.get(parentSlug)!].filter(slug => !local.has(slug)).sort()
      children.forEach((slug, i) => {
        const heading = parent.heading + (cursor === 0 ? i * goldenAngle : ((i + 0.5) / children.length - 0.5) * Math.PI * 0.8)
        const distance = 150 + (children.length > 6 ? Math.sqrt(i) * 30 : (i % 3) * 12)
        local.set(slug, { x: parent.x + Math.cos(heading) * distance, y: parent.y + Math.sin(heading) * distance, heading })
        queue.push(slug)
      })
    }
    const mean = { x: 0, y: 0 }
    for (const point of local.values()) { mean.x += point.x / local.size; mean.y += point.y / local.size }
    for (const [slug, point] of local) seeds.set(slug, { x: point.x - mean.x, y: point.y - mean.y })
    const extent = graphPointBounds(slugs.map(slug => seeds.get(slug)!))
    // 为排斥力和标题预留空间；这是间距，而非力的上限。
    const padding = 160
    let x = 0, y = 0, bounds: GraphBounds
    for (let attempt = 0; ; attempt++) {
      const distance = Math.sqrt(attempt) * 48
      x = Math.cos(attempt * goldenAngle) * distance * stretch
      y = Math.sin(attempt * goldenAngle) * distance / stretch
      bounds = { left: x + extent.left - padding, right: x + extent.right + padding,
        top: y + extent.top - padding, bottom: y + extent.bottom + padding }
      const neighbors = new Set(keysFor(bounds).flatMap(key => cells.get(key) ?? []))
      if ([...neighbors].every(c => bounds.right <= c.left || bounds.left >= c.right || bounds.bottom <= c.top || bounds.top >= c.bottom)) break
    }
    const cluster = { x, y, ...bounds, slugs, representative, anchors: selectGraphAnchors(slugs, adjacency, nodeBySlug) }
    clusters.push(cluster)
    for (const key of keysFor(bounds)) {
      const bucket = cells.get(key) ?? []
      bucket.push(cluster); cells.set(key, bucket)
    }
  }
  const left = Math.min(0, ...clusters.map(c => c.left))
  const top = Math.min(0, ...clusters.map(c => c.top))
  const placements = new Map<string, LayoutPlacement>()
  const sections: GraphSection[] = []
  let right = 0, bottom = 0
  for (const cluster of clusters) {
    cluster.x -= left; cluster.y -= top
    cluster.left -= left; cluster.right -= left; cluster.top -= top; cluster.bottom -= top
    const center = { x: cluster.x, y: cluster.y }
    for (const slug of cluster.slugs) {
      const point = seeds.get(slug)!
      placements.set(slug, { x: center.x + point.x, y: center.y + point.y,
        center, degree: adjacency.get(slug)!.size })
    }
    right = Math.max(right, cluster.right)
    bottom = Math.max(bottom, cluster.bottom)
  }
  const gap = 80
  // 独立页面放在可读的网格上，而不是聚集到共同的力学中心。这些货架并非
  // 语义上的社区。
  for (const [kind, slugs] of [['offscreen', offscreen], ['unlinked', unlinked]] as const) {
    if (!slugs.length) continue
    const shelfWidth = right || Math.sqrt(slugs.length * 180 * 100 * aspect)
    const columns = Math.min(slugs.length, Math.max(3, Math.floor(shelfWidth / 180)))
    const top = bottom ? bottom + gap : 0
    const sectionWidth = columns * 180
    const sectionHeight = Math.ceil(slugs.length / columns) * 100 + 50
    const left = Math.max(0, (right - sectionWidth) / 2)
    slugs.forEach((slug, i) => {
      const point = { x: left + (i % columns) * 180 + 90, y: top + 70 + Math.floor(i / columns) * 100 }
      placements.set(slug, { ...point, center: point, stationary: true })
    })
    sections.push({ kind, count: slugs.length, left, top, right: left + sectionWidth, bottom: top + sectionHeight })
    bottom = top + sectionHeight
    right = Math.max(right, left + sectionWidth)
  }

  const dx = (width - right) / 2, dy = (height - bottom) / 2
  for (const placement of placements.values()) {
    placement.x += dx; placement.y += dy
    // 中心由一个分量共享；在下面只平移一次。
  }
  for (const center of new Set([...placements.values()].map(p => p.center))) { center.x += dx; center.y += dy }
  for (const cluster of clusters) {
    cluster.x += dx; cluster.y += dy
    cluster.left += dx; cluster.right += dx; cluster.top += dy; cluster.bottom += dy
  }
  for (const section of sections) {
    section.left += dx; section.right += dx; section.top += dy; section.bottom += dy
  }

  // 扩展期间保留既有的心理地图。新节点从某个已可见邻居附近开始；
  // 旧节点由模拟保持静止。
  if (prior.size) {
    for (const slugs of components) {
      const existing = slugs.flatMap(slug => prior.has(slug) ? [prior.get(slug)!] : [])
      if (!existing.length) continue
      const center = { x: existing.reduce((sum, p) => sum + p.x, 0) / existing.length, y: existing.reduce((sum, p) => sum + p.y, 0) / existing.length }
      slugs.forEach((slug, i) => {
        const neighbor = [...adjacency.get(slug)!].find(nb => prior.has(nb))
        const anchor = prior.get(slug) ?? (neighbor ? prior.get(neighbor)! : center)
        const angle = i * Math.PI * (3 - Math.sqrt(5))
        const point = prior.has(slug) ? anchor : { x: anchor.x + Math.cos(angle) * 90, y: anchor.y + Math.sin(angle) * 90 }
        placements.set(slug, { ...point, center, degree: adjacency.get(slug)!.size })
      })
    }
    for (const [slug, point] of prior) {
      const placement = placements.get(slug)
      if (placement) { placement.x = point.x; placement.y = point.y }
    }
  }
  return { placements, sections: prior.size ? [] : sections, clusters: prior.size ? [] : clusters }
}

export interface ForceNode extends GraphPoint {
  slug: string; vx: number; vy: number; linkCount: number; pinned: boolean
}

/** 局部排斥采用二维空间网格；碰撞检测包含外圈。 */
export function stepGraphLayout(
  nodes: ForceNode[], edges: GraphLink[], placements: ReadonlyMap<string, LayoutPlacement>,
  alpha: number, retained: ReadonlySet<string> = new Set(),
) {
  const fixed = (n: ForceNode) => n.pinned || retained.has(n.slug) || !!placements.get(n.slug)?.stationary
  if (nodes.every(fixed)) return
  const reach = 300
  const buckets = new Map<string, ForceNode[]>()
  const nodeMap = new Map(nodes.map(n => [n.slug, n]))
  for (const n of nodes) {
    const cx = Math.floor(n.x / reach), cy = Math.floor(n.y / reach)
    for (let gx = cx - 1; gx <= cx + 1; gx++) for (let gy = cy - 1; gy <= cy + 1; gy++) {
      for (const other of buckets.get(`${gx},${gy}`) ?? []) {
        let dx = n.x - other.x, dy = n.y - other.y
        let distance = Math.hypot(dx, dy)
        if (distance > reach) continue
        if (distance < 0.001) { dx = 1; dy = 0; distance = 1 }
        const force = 12000 * alpha / Math.max(distance * distance, 100)
        const fx = dx / distance * force, fy = dy / distance * force
        if (!fixed(n)) { n.vx += fx; n.vy += fy }
        if (!fixed(other)) { other.vx -= fx; other.vy -= fy }
      }
    }
    const key = `${cx},${cy}`
    const bucket = buckets.get(key) ?? []
    bucket.push(n); buckets.set(key, bucket)
  }
  // 一条互惠的 Wiki 链接只算一根视觉弹簧，而非双倍吸引力。
  const pairs = new Set<string>()
  for (const edge of edges) {
    const s = nodeMap.get(edge.source), t = nodeMap.get(edge.target)
    if (!s || !t || s === t) continue
    const key = JSON.stringify([s.slug, t.slug].sort())
    if (pairs.has(key)) continue
    pairs.add(key)
    const dx = t.x - s.x, dy = t.y - s.y, distance = Math.hypot(dx, dy) || 1
    // 稠密页面不应把整组拉成一个实心球。按可见度数（而非知识库范围的
    // 链接数）归一化吸引力。
    const degree = Math.min(placements.get(s.slug)?.degree ?? 1, placements.get(t.slug)?.degree ?? 1)
    const force = (distance - 140) * 0.003 * alpha / Math.sqrt(Math.max(1, degree))
    if (!fixed(s)) { s.vx += dx / distance * force; s.vy += dy / distance * force }
    if (!fixed(t)) { t.vx -= dx / distance * force; t.vy -= dy / distance * force }
  }
  for (const n of nodes) {
    if (fixed(n)) continue
    n.vx *= 0.6
    n.vy *= 0.6
    const speed = Math.hypot(n.vx, n.vy)
    if (speed > 20) { n.vx *= 20 / speed; n.vy *= 20 / speed }
    n.x += n.vx; n.y += n.vy
  }
  // 积分后解决重叠，即便力已经冷却也一样。这里用 X 方向剪枝，因为碰撞
  // 半径较小（至多 64 单位）。
  for (let pass = 0; pass < 3; pass++) {
    const sorted = [...nodes].sort((a, b) => a.x - b.x)
    for (let i = 0; i < sorted.length; i++) {
      const a = sorted[i]!
      for (let j = i + 1; j < sorted.length; j++) {
        const b = sorted[j]!
        if (b.x - a.x > 64) break
        let dx = b.x - a.x, dy = b.y - a.y
        let distance = Math.hypot(dx, dy)
        const minimum = graphNodeRadius(a.linkCount) + graphNodeRadius(b.linkCount) + 16
        if (distance >= minimum || (fixed(a) && fixed(b))) continue
        if (distance < 0.001) { dx = 1; dy = 0; distance = 1 }
        const shift = (minimum - distance) / (fixed(a) || fixed(b) ? 1 : 2)
        if (!fixed(a)) { a.x -= dx / distance * shift; a.y -= dy / distance * shift }
        if (!fixed(b)) { b.x += dx / distance * shift; b.y += dy / distance * shift }
      }
    }
  }
  // 只纠正整个分量的漂移。对每个成员施加相同的平移；没有节点被拉向共同
  // 中心，也没有被裁剪进圆内。被固定/保留的分量维持用户的坐标不变。
  const groups = new Map<GraphPoint, ForceNode[]>()
  for (const node of nodes) {
    const center = placements.get(node.slug)?.center
    if (!center) continue
    const group = groups.get(center) ?? []
    group.push(node); groups.set(center, group)
  }
  for (const [center, group] of groups) {
    if (group.some(fixed)) continue
    const dx = (center.x - group.reduce((sum, n) => sum + n.x, 0) / group.length) * 0.03 * alpha
    const dy = (center.y - group.reduce((sum, n) => sum + n.y, 0) / group.length) * 0.03 * alpha
    for (const n of group) { n.x += dx; n.y += dy }
  }
}

export interface GraphLabel extends GraphBounds { slug: string; priority: number; required?: boolean }
/** 贪心的屏幕空间标签摆放。被聚焦的标签永不隐藏。 */
export function visibleGraphLabels(labels: GraphLabel[]): Set<string> {
  const visible = new Set<string>()
  const occupied = new Map<string, GraphLabel[]>()
  const cells = (label: GraphLabel) => {
    const keys: string[] = []
    for (let x = Math.floor(label.left / 160); x <= Math.floor(label.right / 160); x++) {
      for (let y = Math.floor(label.top / 32); y <= Math.floor(label.bottom / 32); y++) keys.push(`${x},${y}`)
    }
    return keys
  }
  for (const label of [...labels].sort((a, b) => Number(!!b.required) - Number(!!a.required) || b.priority - a.priority)) {
    const keys = cells(label)
    const overlaps = keys.some(key => (occupied.get(key) ?? []).some(other => label.left < other.right && label.right > other.left && label.top < other.bottom && label.bottom > other.top))
    if (overlaps && !label.required) continue
    visible.add(label.slug)
    for (const key of keys) {
      const bucket = occupied.get(key) ?? []
      bucket.push(label); occupied.set(key, bucket)
    }
  }
  return visible
}


export function graphPointBounds(points: GraphPoint[]): GraphBounds {
  if (!points.length) return { left: 0, top: 0, right: 0, bottom: 0 }
  return {
    left: Math.min(...points.map(p => p.x)), right: Math.max(...points.map(p => p.x)),
    top: Math.min(...points.map(p => p.y)), bottom: Math.max(...points.map(p => p.y)),
  }
}

/** 以可读的缩放比例开始。适配整图是一个显式动作。 */
export function initialGraphViewport(points: GraphPoint[], width: number, height: number, focus?: GraphPoint) {
  const bounds = graphPointBounds(points)
  const fit = Math.min(Math.max(100, width - 120) / Math.max(100, bounds.right - bounds.left),
    Math.max(100, height - 120) / Math.max(100, bounds.bottom - bounds.top))
  const scale = Math.max(0.4, Math.min(0.85, fit))
  const center = fit < 0.4 && focus ? focus : { x: (bounds.left + bounds.right) / 2, y: (bounds.top + bounds.bottom) / 2 }
  return { scale, x: width / 2 - center.x * scale, y: height / 2 - center.y * scale }
}


/** 节点圆盘的平滑包络，带稳定的屏幕间距。
 * 凸包取自已经按间距膨胀过的圆盘，再用闭合样条描边，其切线受相邻
 * 线段长度限制，因此它向凸包外侧鼓起，不会出现环或尖点。
 */
export function graphClusterOutline(points: (GraphPoint & { radius?: number })[], scale = 1): string {
  if (!points.length) return ''
  const zoom = Number.isFinite(scale) && scale > 0 ? scale : 1
  // 以世界单位封顶，避免在缩得很远的层级把不同分组合并。
  const padding = Math.min(22 / zoom, 60)
  const samples = 24
  const expanded = points.flatMap(p => {
    const radius = Math.max(0, p.radius ?? 0) + padding
    return Array.from({ length: samples }, (_, i) => ({
      x: p.x + Math.cos(i * 2 * Math.PI / samples) * radius,
      y: p.y + Math.sin(i * 2 * Math.PI / samples) * radius,
    }))
  }).sort((a, b) => a.x - b.x || a.y - b.y)
  const unique = expanded.filter((p, i) => !i || p.x !== expanded[i - 1]!.x || p.y !== expanded[i - 1]!.y)
  const cross = (o: GraphPoint, a: GraphPoint, b: GraphPoint) => (a.x - o.x) * (b.y - o.y) - (a.y - o.y) * (b.x - o.x)
  const half = (list: GraphPoint[]) => {
    const hull: GraphPoint[] = []
    for (const point of list) {
      while (hull.length > 1 && cross(hull[hull.length - 2]!, hull[hull.length - 1]!, point) <= 0) hull.pop()
      hull.push(point)
    }
    hull.pop()
    return hull
  }
  const hull = [...half(unique), ...half([...unique].reverse())]
  const at = (i: number) => hull[(i + hull.length) % hull.length]!
  const fmt = (p: GraphPoint) => `${Math.round(p.x * 10) / 10},${Math.round(p.y * 10) / 10}`
  const tangent = (from: GraphPoint, to: GraphPoint, length: number) => {
    const dx = to.x - from.x, dy = to.y - from.y, norm = Math.hypot(dx, dy) || 1
    return { x: dx / norm * length, y: dy / norm * length }
  }
  const path = [`M${fmt(hull[0]!)}`]
  hull.forEach((p, i) => {
    const next = at(i + 1)
    const reach = Math.hypot(next.x - p.x, next.y - p.y) * 0.36
    const out = tangent(at(i - 1), next, reach), into = tangent(p, at(i + 2), reach)
    path.push(`C${fmt({ x: p.x + out.x, y: p.y + out.y })} ${fmt({ x: next.x - into.x, y: next.y - into.y })} ${fmt(next)}`)
  })
  return path.join(' ') + ' Z'
}
