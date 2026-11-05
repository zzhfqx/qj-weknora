import assert from 'node:assert/strict'
import test from 'node:test'
import {
  selectGraphAnchors, buildGraphLayout, graphNodeRadius, stepGraphLayout, visibleGraphLabels, initialGraphViewport, graphPointBounds, graphClusterOutline,
  type ForceNode, type GraphLink, type LayoutInput,
} from './wikiGraphLayout.ts'

function simulate(inputs: LayoutInput[], edges: GraphLink[], prior = new Map<string, { x: number; y: number }>()) {
  const layout = buildGraphLayout(inputs, edges, 1200, 800, prior)
  const nodes: ForceNode[] = inputs.map(n => ({
    slug: n.slug, linkCount: n.link_count, ...layout.placements.get(n.slug)!,
    vx: 0, vy: 0, pinned: false,
  }))
  const retained = new Set(prior.keys())
  for (let alpha = 1; alpha >= 0.02; alpha *= 0.985) stepGraphLayout(nodes, edges, layout.placements, alpha, retained)
  return { nodes, layout }
}

test('非连通分组彼此远离，孤立页面能区分“缺少可见边”的情况', () => {
  const inputs = [
    { slug: 'a', link_count: 1 }, { slug: 'b', link_count: 1 },
    { slug: 'c', link_count: 1 }, { slug: 'd', link_count: 1 },
    { slug: 'orphan', link_count: 0 }, { slug: 'outside', link_count: 3 },
  ]
  const edges = [{ source: 'a', target: 'b' }, { source: 'd', target: 'c' }, { source: 'outside', target: 'not-loaded' }]
  const { nodes, layout } = simulate(inputs, edges)
  assert.deepEqual(layout.sections.map(s => [s.kind, s.count]), [['offscreen', 1], ['unlinked', 1]])
  const a = nodes.find(n => n.slug === 'a')!, b = nodes.find(n => n.slug === 'b')!
  const c = nodes.find(n => n.slug === 'c')!, d = nodes.find(n => n.slug === 'd')!
  const within = Math.max(Math.hypot(a.x - b.x, a.y - b.y), Math.hypot(c.x - d.x, c.y - d.y))
  for (const first of [a, b]) for (const second of [c, d]) {
    assert.ok(Math.hypot(first.x - second.x, first.y - second.y) > within)
  }
  for (const section of layout.sections) {
    const node = nodes.find(n => n.slug === (section.kind === 'unlinked' ? 'orphan' : 'outside'))!
    assert.ok(node.x > section.left && node.x < section.right && node.y > section.top && node.y < section.bottom)
  }
})

test('初始位置在不同接口顺序和反向链接下保持稳定', () => {
  const inputs = Array.from({ length: 20 }, (_, i) => ({ slug: `p${i}`, link_count: 2 }))
  const edges = inputs.slice(1).map(n => ({ source: 'p0', target: n.slug }))
  const a = buildGraphLayout(inputs, edges, 900, 600)
  const b = buildGraphLayout([...inputs].reverse(), [...edges].reverse().map(e => ({ source: e.target, target: e.source })), 900, 600)
  for (const n of inputs) assert.deepEqual(a.placements.get(n.slug), b.placements.get(n.slug))
})

test('大量小分量打包时不重叠、也不排成行', t => {
  const inputs = Array.from({ length: 500 }, (_, i) => ({ slug: `pair${i}`, link_count: 1 }))
  const edges = inputs.flatMap((n, i) => i % 2 ? [{ source: `pair${i - 1}`, target: n.slug }] : [])
  const start = performance.now()
  const { clusters } = buildGraphLayout(inputs, edges, 1400, 900)
  t.diagnostic(`250 个分量打包耗时 ${Math.round(performance.now() - start)} ms`)
  assert.equal(clusters.length, 250)
  assert.ok(new Set(clusters.map(c => Math.round(c.y))).size > 100)
  for (let i = 0; i < clusters.length; i++) for (let j = i + 1; j < clusters.length; j++) {
    const a = clusters[i]!, b = clusters[j]!
    assert.ok(a.right <= b.left || a.left >= b.right || a.bottom <= b.top || a.top >= b.bottom)
  }
})

test('空图和完全无链接的图具有有限且不重叠的位置', () => {
  assert.equal(buildGraphLayout([], [], 800, 600).placements.size, 0)
  const inputs = Array.from({ length: 500 }, (_, i) => ({ slug: `orphan${i}`, link_count: 0 }))
  const { nodes, layout } = simulate(inputs, [])
  assert.deepEqual(layout.sections.map(s => [s.kind, s.count]), [['unlinked', 500]])
  for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
    assert.ok(Math.hypot(nodes[i]!.x - nodes[j]!.x, nodes[i]!.y - nodes[j]!.y) > 50)
  }
})

test('500 和 1500 节点的图能稳定到有限位置且节点间留有间距', t => {
  for (const count of [500, 1500]) {
    const inputs = Array.from({ length: count }, (_, i) => ({ slug: `p${i}`, link_count: i % 50 === 0 ? 49 : 1 }))
    const edges = inputs.flatMap((n, i) => i % 50 ? [{ source: `p${i - i % 50}`, target: n.slug }] : [])
    const start = performance.now()
    const { nodes } = simulate(inputs, edges)
    t.diagnostic(`${count} 个节点稳定耗时 ${Math.round(performance.now() - start)} ms（纯布局，无 DOM）`)
    for (const n of nodes) {
      assert.ok(Number.isFinite(n.x) && Number.isFinite(n.y))
    }
    for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
      const a = nodes[i]!, b = nodes[j]!
      assert.ok(Math.hypot(a.x - b.x, a.y - b.y) >= graphNodeRadius(a.linkCount) + graphNodeRadius(b.linkCount) + 15,
        `${count}: ${a.slug} 与 ${b.slug} 重叠`)
    }
  }
})

test('增量扩展时固定已有节点，并让新邻居稳定下来', () => {
  const prior = new Map([['a', { x: 111, y: 222 }], ['b', { x: 260, y: 222 }]])
  const inputs = Array.from({ length: 30 }, (_, i) => ({ slug: i === 0 ? 'a' : i === 1 ? 'b' : `new${i}`, link_count: 1 }))
  const edges = inputs.slice(1).map(n => ({ source: 'a', target: n.slug }))
  const { nodes } = simulate(inputs, edges, prior)
  for (const [slug, point] of prior) {
    const n = nodes.find(n => n.slug === slug)!
    assert.deepEqual({ x: n.x, y: n.y }, point)
  }
  for (const n of nodes.slice(2)) {
    assert.ok(Number.isFinite(n.x) && Number.isFinite(n.y))
    assert.ok(Math.hypot(n.x - 111, n.y - 222) > 32)
  }
})

test('拥有 1499 个邻居的大枢纽能稳定且节点间留有间距', t => {
  const inputs = Array.from({ length: 1500 }, (_, i) => ({ slug: `hub${i}`, link_count: i ? 1 : 1499 }))
  const edges = inputs.slice(1).map(n => ({ source: 'hub0', target: n.slug }))
  const start = performance.now()
  const { nodes } = simulate(inputs, edges)
  t.diagnostic(`单个分量 1500 个节点稳定耗时 ${Math.round(performance.now() - start)} ms（无 DOM）`)
  for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
    const a = nodes[i]!, b = nodes[j]!
    assert.ok(Math.hypot(a.x - b.x, a.y - b.y) >= graphNodeRadius(a.linkCount) + graphNodeRadius(b.linkCount) + 14,
      `${a.slug} 与 ${b.slug} 重叠`)
  }
})

test('碰撞检测分开重合节点，同时尊重被拖拽固定的节点', () => {
  const inputs = [{ slug: 'a', link_count: 10 }, { slug: 'b', link_count: 10 }]
  const layout = buildGraphLayout(inputs, [{ source: 'a', target: 'b' }], 800, 600)
  const nodes: ForceNode[] = inputs.map(n => ({ slug: n.slug, linkCount: n.link_count, x: 400, y: 300, vx: 0, vy: 0, pinned: n.slug === 'a' }))
  for (let i = 0; i < 10; i++) stepGraphLayout(nodes, [], layout.placements, 0.02)
  assert.equal(nodes[0]!.x, 400)
  assert.equal(nodes[0]!.y, 300)
  assert.ok(Math.hypot(nodes[0]!.x - nodes[1]!.x, nodes[0]!.y - nodes[1]!.y) >= graphNodeRadius(10) * 2 + 15)
})

test('重叠标签优先留给重要节点，但被聚焦的标签始终保持可读', () => {
  const box = { left: 0, right: 100, top: 0, bottom: 20 }
  const labels = [
    { ...box, slug: 'low', priority: 1 }, { ...box, slug: 'high', priority: 20 },
    { ...box, left: 200, right: 300, slug: 'separate', priority: 0 },
  ]
  assert.deepEqual([...visibleGraphLabels(labels)], ['high', 'separate'])
  assert.deepEqual([...visibleGraphLabels([...labels, { ...box, slug: 'selected', priority: 0, required: true }])], ['selected', 'separate'])
})


test('长链保持其拉长的拓扑，并远离其他分组预留空间', () => {
  const inputs = Array.from({ length: 40 }, (_, i) => ({ slug: `chain${String(i).padStart(2, '0')}`, link_count: 2 }))
  const edges = inputs.flatMap((n, i) => i % 20 ? [{ source: inputs[i - 1]!.slug, target: n.slug }] : [])
  const { nodes, layout } = simulate(inputs, edges)
  for (const cluster of layout.clusters) {
    const members = nodes.filter(n => cluster.slugs.includes(n.slug))
    const bounds = graphPointBounds(members)
    const mean = { x: members.reduce((s, n) => s + n.x, 0) / members.length, y: members.reduce((s, n) => s + n.y, 0) / members.length }
    const xx = members.reduce((s, n) => s + (n.x - mean.x) ** 2, 0)
    const yy = members.reduce((s, n) => s + (n.y - mean.y) ** 2, 0)
    const xy = members.reduce((s, n) => s + (n.x - mean.x) * (n.y - mean.y), 0)
    const delta = Math.hypot(xx - yy, 2 * xy)
    // 用主轴表示，使其不受链的方向影响。
    assert.ok((xx + yy + delta) / Math.max(1, xx + yy - delta) > 9)
    assert.ok(Math.hypot(bounds.right - bounds.left, bounds.bottom - bounds.top) > 1500)
  }
  const first = nodes.slice(0, 20), second = nodes.slice(20)
  for (const a of first) for (const b of second) assert.ok(Math.hypot(a.x - b.x, a.y - b.y) > 250)
})

test('大画布围绕关注点以可读比例开始，而非适配每个节点', () => {
  const points = [{ x: -4000, y: -2000 }, { x: 5000, y: 3000 }]
  const focus = { x: 300, y: 400 }
  const view = initialGraphViewport(points, 1200, 800, focus)
  assert.equal(view.scale, 0.4)
  assert.equal(focus.x * view.scale + view.x, 600)
  assert.equal(focus.y * view.scale + view.y, 400)
  assert.ok(points[1]!.x * view.scale + view.x > 1200)
  assert.equal(initialGraphViewport([{ x: 0, y: 0 }, { x: 100, y: 100 }], 1200, 800).scale, 0.85)
  assert.ok(Object.values(initialGraphViewport([], 1200, 800)).every(Number.isFinite))
})


test('分组轮廓跟随位移后的节点，并能处理空或重合的输入', () => {
  assert.equal(graphClusterOutline([]), '')
  for (const points of [[{ x: 0, y: 0 }], [{ x: 0, y: 0 }, { x: 0, y: 0 }], [{ x: 0, y: 0 }, { x: 200, y: 10 }, { x: 800, y: 400 }]]) {
    const path = graphClusterOutline(points)
    assert.ok(path.startsWith('M') && path.endsWith('Z'))
    assert.ok(!path.includes('NaN') && !path.includes('Infinity'))
    assert.equal(graphClusterOutline([...points].reverse()), path)
  }
  assert.notEqual(graphClusterOutline([{ x: 0, y: 0 }, { x: 200, y: 0 }]), graphClusterOutline([{ x: 0, y: 0 }, { x: 400, y: 0 }]))
})


test('地标覆盖各自独立的稠密邻域，而不只覆盖最大的那个', () => {
  const sizes = [40, 30, 20]
  const slugs = sizes.flatMap((size, group) => Array.from({ length: size }, (_, i) => `region${group}-${i}`))
  const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
  const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a) }
  sizes.forEach((size, group) => {
    for (let i = 0; i < size; i++) for (let j = i + 1; j < size; j++) link(`region${group}-${i}`, `region${group}-${j}`)
  })
  link('region0-0', 'region1-0'); link('region1-0', 'region2-0')
  const anchors = selectGraphAnchors(slugs, adjacency)
  assert.equal(anchors.length, 3)
  assert.equal(new Set(anchors.map(slug => slug.split('-')[0])).size, 3)
  const reversed = new Map([...adjacency].reverse().map(([slug, neighbors]) => [slug, new Set([...neighbors].reverse())]))
  assert.deepEqual(selectGraphAnchors([...slugs].reverse(), reversed), anchors)
})

test('单个枢纽不会人为产生额外主题，且外部邻居不影响地标', () => {
  const slugs = Array.from({ length: 200 }, (_, i) => `page${i}`)
  const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
  for (const slug of slugs.slice(1)) { adjacency.get('page0')!.add(slug); adjacency.get(slug)!.add('page0') }
  adjacency.get('page199')!.add('outside-the-component')
  assert.deepEqual(selectGraphAnchors(slugs, adjacency), ['page0'])
  assert.deepEqual(selectGraphAnchors([], adjacency), [])
})

test('地标预算从小分量到大分量逐级增长，最多五个', () => {
  for (const [size, expected] of [[10, 1], [30, 2], [75, 3], [150, 4], [500, 5]]) {
    const slugs = Array.from({ length: size! }, (_, i) => `p${i}`)
    const adjacency = new Map(slugs.map((slug, i) => [slug, new Set([slugs[i - 1], slugs[i + 1]].filter((s): s is string => !!s))]))
    const anchors = selectGraphAnchors(slugs, adjacency)
    assert.equal(anchors.length, expected)
    assert.equal(new Set(anchors).size, expected)
    if (size! >= 30) {
      const indices = anchors.map(slug => Number(slug.slice(1)))
      assert.ok(Math.max(...indices) - Math.min(...indices) > size! * 0.7)
    }
    const inputs = slugs.map(slug => ({ slug, link_count: adjacency.get(slug)!.size }))
    const edges = slugs.slice(1).map((slug, i) => ({ source: slugs[i]!, target: slug }))
    assert.deepEqual(buildGraphLayout(inputs, edges, 1200, 800).clusters[0]!.anchors, anchors)
  }
})

test('主题由主角领衔，独立的配置主题会被命名，而不是给同一主题起第二个名字', () => {
  const names = ['platform', 'enterprise', 'configuration',
    ...Array.from({ length: 12 }, (_, i) => `shared${i}`),
    ...Array.from({ length: 10 }, (_, i) => `platformDetail${i}`),
    ...Array.from({ length: 4 }, (_, i) => `enterpriseDetail${i}`),
    ...Array.from({ length: 6 }, (_, i) => `configurationDetail${i}`)]
  const adjacency = new Map(names.map(slug => [slug, new Set<string>()]))
  const edges: GraphLink[] = []
  const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a); edges.push({ source: a, target: b }) }
  for (let i = 0; i < 12; i++) { link('platform', `shared${i}`); link('enterprise', `shared${i}`) }
  for (let i = 0; i < 10; i++) link('platform', `platformDetail${i}`)
  for (let i = 0; i < 4; i++) link('enterprise', `enterpriseDetail${i}`)
  for (let i = 0; i < 6; i++) link('configuration', `configurationDetail${i}`)
  for (let i = 0; i < 3; i++) link('configuration', `shared${i}`)
  link('platform', 'enterprise'); link('enterprise', 'configuration')
  const nodes = names.map(slug => ({ slug, link_count: adjacency.get(slug)!.size,
    page_type: ['platform', 'enterprise'].includes(slug) ? 'entity' : 'concept' }))
  const layout = buildGraphLayout(nodes, edges, 1200, 800)
  // enterprise 与 platform 共享十二个邻居：一个主题、一个名字。
  assert.deepEqual(layout.clusters[0]!.anchors, ['platform', 'configuration'])
  assert.deepEqual(buildGraphLayout([...nodes].reverse(), [...edges].reverse(), 1200, 800).clusters[0]!.anchors, layout.clusters[0]!.anchors)
})

test('链接了每个主题大多数页面的主角，独自为该分量命名', () => {
  const slugs = ['hub', ...Array.from({ length: 3 }, (_, g) => Array.from({ length: 10 }, (_, i) => `t${g}-${i}`)).flat()]
  const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
  const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a) }
  for (let g = 0; g < 3; g++) {
    for (let i = 0; i < 10; i++) for (let j = i + 1; j < 10; j++) link(`t${g}-${i}`, `t${g}-${j}`)
    // 一篇文档：其主角链接了每个分区中几乎每个页面。
    for (let i = 0; i < 8; i++) link('hub', `t${g}-${i}`)
  }
  assert.deepEqual(selectGraphAnchors(slugs, adjacency), ['hub'])
})

test('标题重复已有标签的页面，不会再为主题命名一次', () => {
  const sizes = [30, 20]
  const slugs = sizes.flatMap((size, g) => Array.from({ length: size }, (_, i) => `r${g}-${i}`))
  const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
  const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a) }
  sizes.forEach((size, g) => {
    for (let i = 0; i < size; i++) for (let j = i + 1; j < size; j++) link(`r${g}-${i}`, `r${g}-${j}`)
  })
  link('r0-0', 'r1-0')
  const pages = new Map([
    ['r0-0', { page_type: 'concept', title: '第三代社保卡' }],
    ['r1-0', { page_type: 'entity', title: '北京市第三代社保卡' }],
    ['r1-1', { page_type: 'concept', title: '线上申领' }],
  ])
  assert.deepEqual(selectGraphAnchors(slugs, adjacency, pages), ['r0-0', 'r1-1'])
})

test('summary 为其主导的文档命名，但几乎同样中心的实体仍会胜出', () => {
  const pages = new Map([
    ['summary', { page_type: 'summary', title: '初中63组语文古诗词高频考点(2).pdf - Summary' }],
    ['subject', { page_type: 'entity', title: '范仲淹' }],
  ])
  const anchorsWhenSubjectLinks = (details: number) => {
    const slugs = ['summary', 'subject', ...Array.from({ length: 10 }, (_, i) => `detail${i}`)]
    const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
    const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a) }
    for (const slug of slugs.slice(1)) link('summary', slug)
    for (let i = 0; i < details; i++) link('subject', `detail${i}`)
    return selectGraphAnchors(slugs, adjacency, pages)
  }
  assert.deepEqual(anchorsWhenSubjectLinks(5), ['summary'])
  assert.deepEqual(anchorsWhenSubjectLinks(9), ['subject'])
})

test('由多篇文档构成的分量，还会为它们共享的页面命名', () => {
  const anchorsWithBridgeDetails = (details: number) => {
    const slugs: string[] = ['bridge']
    const adjacency = new Map<string, Set<string>>([['bridge', new Set()]])
    const link = (a: string, b: string) => {
      for (const s of [a, b]) if (!adjacency.has(s)) { adjacency.set(s, new Set()); slugs.push(s) }
      adjacency.get(a)!.add(b); adjacency.get(b)!.add(a)
    }
    const pages = new Map<string, { page_type: string; title: string }>([['bridge', { page_type: 'concept', title: '强化学习' }]])
    for (let d = 0; d < 3; d++) {
      pages.set(`doc${d}`, { page_type: 'summary', title: `论文${d}.pdf - Summary` })
      for (let i = 0; i < 8; i++) {
        link(`doc${d}`, `d${d}-${i}`)
        if (i > 0) link(`d${d}-${i}`, `d${d}-${i - 1}`)
      }
      link('bridge', `doc${d}`)
      for (let i = 0; i < details; i++) link('bridge', `d${d}-${i}`)
    }
    return selectGraphAnchors(slugs, adjacency, pages)
  }
  const named = anchorsWithBridgeDetails(1)
  assert.equal(named.at(-1), 'bridge')
  assert.ok(named.slice(0, -1).every(slug => slug.startsWith('doc')), named.join())
  // 被每篇文档共享但几乎没有链接：只是细节页，不是把它们连起来的东西。
  assert.ok(!anchorsWithBridgeDetails(0).includes('bridge'))
})

test('标识符式的文件名不会胜过可读页面而为主题命名', () => {
  const slugs = ['summary', 'subject', ...Array.from({ length: 10 }, (_, i) => `detail${i}`)]
  const adjacency = new Map(slugs.map(slug => [slug, new Set<string>()]))
  const link = (a: string, b: string) => { adjacency.get(a)!.add(b); adjacency.get(b)!.add(a) }
  for (const slug of slugs.slice(1)) link('summary', slug)
  for (let i = 0; i < 5; i++) link('subject', `detail${i}`)
  const pages = new Map([
    ['summary', { page_type: 'summary', title: '2412.18925v1.pdf - Summary' }],
    ['subject', { page_type: 'entity', title: '香港中文大学（深圳）' }],
  ])
  assert.deepEqual(selectGraphAnchors(slugs, adjacency, pages), ['subject'])
})

test('实体偏好在接近的排名中起决定作用，但不会挤掉明显更强的概念', () => {
  const inputs = Array.from({ length: 12 }, (_, i) => ({ slug: `page${i}`, link_count: 0, page_type: i === 1 ? 'entity' : 'concept' }))
  const edges = inputs.slice(1).map(n => ({ source: 'page0', target: n.slug }))
  assert.deepEqual(buildGraphLayout(inputs, edges, 1200, 800).clusters[0]!.anchors, ['page0'])
  // 两个连接相当的页面：实体是更有用的总览锚点。
  const pair = [{ slug: 'a-concept', link_count: 1, page_type: 'concept' }, { slug: 'z-entity', link_count: 1, page_type: 'entity' }]
  assert.deepEqual(buildGraphLayout(pair, [{ source: pair[0]!.slug, target: pair[1]!.slug }], 1200, 800).clusters[0]!.anchors, ['z-entity'])
})
