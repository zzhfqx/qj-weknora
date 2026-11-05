<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  listGalleryImages,
  fetchGalleryConfig,
  type GalleryConfig,
  type GalleryResolvedAttr,
  type ImageAsset,
  type ImageListParams,
} from '@/api/image-gallery'
import EmptyState from '@/components/EmptyState.vue'
import { galleryImageRequest } from './galleryImageSrc'
import GalleryViewer, { type GalleryAttrRow } from './GalleryViewer.vue'
import GallerySourcePicker from './GallerySourcePicker.vue'

const props = defineProps<{
  knowledgeBaseId: string
}>()

const emit = defineEmits<{
  (e: 'open-source-doc', knowledgeId: string): void
}>()

const NS = 'knowledgeEditor.wikiBrowser.gallery'
const { t, te } = useI18n()

// ---------------------------------------------------------------------------
// 数据状态
// ---------------------------------------------------------------------------
const loading = ref(false)
const error = ref('')
const items = ref<ImageAsset[]>([])
const total = ref(0)
const page = ref(0)
const pageSize = 24
const exhausted = ref(false)
const hasMore = computed(() => !exhausted.value && page.value * pageSize < total.value)

const keyword = ref('')
const sortBy = ref('')
const sortOrder = ref<'asc' | 'desc'>('desc')

const filterPanelVisible = ref(false)
const sortPanelVisible = ref(false)
const scrollEl = ref<HTMLElement | null>(null)
const gridEl = ref<HTMLElement | null>(null)
const loadMoreEl = ref<HTMLElement | null>(null)
const imageRatios = ref<Record<string, number>>({})
let cardObserver: ResizeObserver | undefined
let scrollObserver: ResizeObserver | undefined

// 来源文档限定（“按文档筛选”选择器）。空数组表示知识库的全部文档；否则只拉取
// 所属文档被列出的图片。由工具栏的来源面板编辑。
const sourceDocIds = ref<string[]>([])
const sourcePickerVisible = ref(false)

// 自由文本（"keywords"）属性的属性选择：命名空间属性 id -> 字面值（同一属性内
// OR、属性间 AND）。
const attrSelections = ref<Record<string, string[]>>({})

// 逐值裁决，命名空间属性 id -> 值 -> "off" | "on"。缺键是中立位：携带该值的
// 图片保持它原本的可见性，这正是默认状态展示一切的原因。
const attrVerdicts = ref<Record<string, Record<string, string>>>({})

// ---------------------------------------------------------------------------
// 画廊契约（自描述，每次挂载拉取一次）
//
// UI 提供的一切——筛选分区、可搜索字段、排序选项——都来自契约。此处不硬编码
// 任何画廊规则，因此后端新属性与运行时策略改动在重新加载后即可点亮。
// ---------------------------------------------------------------------------
const configLoaded = ref(false)
const config = ref<GalleryConfig>({
  attribute_sources: [],
  attributes: [],
  mode: 'all',
  status: {},
})
const searchMode = ref<'all' | 'custom'>('all')
// 逐属性搜索开关，以命名空间属性 id 为键（"on"/"off"）。custom 模式下未记录的
// 字段视为 off；来源声明不携带 on/off——激活永远是用户自己的记录。
const searchStatus = ref<Record<string, string>>({})

const filterAttrs = computed(() => config.value.attributes.filter((a) => a.usage.in_filter))
const searchAttrs = computed(() => config.value.attributes.filter((a) => a.usage.in_searchfield))
const sortAttrs = computed(() => config.value.attributes.filter((a) => a.usage.in_sortfield))

// 当前实际搜索的字段："all" 模式下是每个合格字段；custom 模式下是用户记录为
// on 的字段。
const activeSearchIds = computed(() => {
  if (searchMode.value === 'all') return searchAttrs.value.map((a) => a.id)
  return searchAttrs.value.filter((a) => searchStatus.value[a.id] === 'on').map((a) => a.id)
})

// ---------------------------------------------------------------------------
// 图片 URL 解析
//
// 后端返回存储句柄（local://、minio://、resource://……），浏览器无法直接渲染。
// 它们必须经知识库文件代理（galleryImageRequest）拉取并转为 object URL。普通
// http(s) URL 原样通过。
// ---------------------------------------------------------------------------
const thumbUrls = ref<Record<string, string>>({})
const thumbBroken = ref<Record<string, boolean>>({})
// object URL 以它所拉取的原始存储 URL 为键，因此缩略图与查看器共享同一个 blob，
// 重新加载时复用屏幕上已有的，而不是再拉取（并泄漏）一份新副本。
const blobByRawUrl = new Map<string, string>()
// 飞行中的拉取，因此网格与查看器同时索要同一张图时共享一个请求。
const inflight = new Map<string, Promise<string>>()

/** 撤销每个原始 URL 不在 keep 中的缓存 object URL。 */
function releaseBlobs(keep: Set<string> = new Set()): void {
  for (const [raw, objectUrl] of blobByRawUrl) {
    if (keep.has(raw)) continue
    URL.revokeObjectURL(objectUrl)
    blobByRawUrl.delete(raw)
  }
}

async function fetchBlobUrl(rawUrl: string): Promise<string> {
  const req = galleryImageRequest(rawUrl, props.knowledgeBaseId)
  if (!req) return rawUrl
  try {
    const resp = await fetch(req.url, { headers: req.headers })
    if (!resp.ok) return rawUrl
    const objectUrl = URL.createObjectURL(await resp.blob())
    blobByRawUrl.set(rawUrl, objectUrl)
    return objectUrl
  } catch {
    return rawUrl
  }
}

function resolveImageSrc(rawUrl: string): Promise<string> {
  const cached = blobByRawUrl.get(rawUrl)
  if (cached) return Promise.resolve(cached)
  let pending = inflight.get(rawUrl)
  if (!pending) {
    pending = fetchBlobUrl(rawUrl).finally(() => inflight.delete(rawUrl))
    inflight.set(rawUrl, pending)
  }
  return pending
}

async function resolveThumbnails(imgs: ImageAsset[], token: number) {
  await Promise.all(imgs.map(async (img) => {
    const url = await resolveImageSrc(img.url)
    if (token !== listToken) return
    thumbUrls.value[img.id] = url
  }))
  if (token === listToken) releaseBlobs(new Set(items.value.map((img) => img.url)))
}

function onThumbLoad(id: string, event: Event) {
  const image = event.target as HTMLImageElement
  if (image.naturalWidth && image.naturalHeight) {
    imageRatios.value[id] = image.naturalWidth / image.naturalHeight
  }
}

// 卡片只占用其自然图片高度所需的行，同时保持 DOM 顺序（以及键盘/查看器顺序）。
// 追加时已有卡片不动。
function sizeCards(cards: HTMLElement[]) {
  const spans = cards.map((card) => Math.ceil((card.getBoundingClientRect().height + 12) / 13))
  cards.forEach((card, index) => {
    card.style.gridRowEnd = `span ${spans[index]}`
  })
}

function observeCards() {
  cardObserver?.disconnect()
  const cards = Array.from(gridEl.value?.querySelectorAll<HTMLElement>('.ig-card') ?? [])
  sizeCards(cards)
  cards.forEach((card) => cardObserver?.observe(card))
}

function checkLoadMore() {
  if (loading.value || error.value || viewerOpen.value || !hasMore.value) return
  const scroll = scrollEl.value
  const sentinel = loadMoreEl.value
  if (scroll && sentinel && scroll.clientHeight > 0 &&
      sentinel.getBoundingClientRect().top <= scroll.getBoundingClientRect().bottom + 300) {
    void loadMore()
  }
}

function onThumbError(id: string) {
  thumbBroken.value = { ...thumbBroken.value, [id]: true }
}

// ---------------------------------------------------------------------------
// 标签帮助函数
//
// 契约携带后端的默认语言措辞。语言文件按（净化后的）属性 id 叠加翻译；未翻译
// 的回退到契语文本。
//
// 每个属性与值都刻意携带两段文本：空间受限控件（筛选复选框、排序选项）用短
// `label`，有空间时用 `description` 句。下面的帮助函数一律返回短名，长名单独
// 暴露，因此紧凑面板不必拉伸成一整句。
//
// 翻译永远优先：契语文本只是语言尚不认识的属性的回退。先问语言很重要，因为来源
// 自带默认语言文本，否则会遮蔽除它自身之外所有语言的翻译。
// ---------------------------------------------------------------------------
/** 画廊自身属性措辞在语言文件中的位置。 */
const ATTR_NAMESPACE = 'knowledgeEditor.wikiBrowser.gallery.attr'

const sanitizeKey = (id: string) => id.replace(/[:.]/g, '_')

/** 管道自身对一个属性的措辞，以来源内名为键。 */
function pipelineKey(attr: GalleryResolvedAttr, suffix = ''): string {
  return `imageAttr.${(attr.name || '').replace(/[.\s]/g, '_')}${suffix}`
}

function attrLabel(attr: GalleryResolvedAttr): string {
  const key = `${ATTR_NAMESPACE}.${sanitizeKey(attr.id)}`
  if (te(key)) return t(key)
  // 存在时叠加属性管道自身的翻译。
  const legacy = pipelineKey(attr, '.label')
  if (te(legacy)) return t(legacy)
  return attr.label || attr.id
}

/** 解释一个属性的句子，用来源给出的措辞。 */
function attrDescription(attr: GalleryResolvedAttr): string {
  const key = `${ATTR_NAMESPACE}.${sanitizeKey(attr.id)}_description`
  if (te(key)) return t(key)
  // 属性管道贡献的属性不附带画廊专用文本，因此落到管道自身翻译。
  const legacy = pipelineKey(attr, '.description')
  if (te(legacy)) return t(legacy)
  return attr.description || ''
}

/** 一个允许值的短显示名。语言优先于来源自带措辞；只有语言不认识的值才回退到来源
 *  文本，而来源什么都没写明的值读作原始值。 */
function attrValueLabel(attr: GalleryResolvedAttr, value: string): string {
  const key = `${ATTR_NAMESPACE}.${sanitizeKey(attr.id)}_value_${value}`
  if (te(key)) return t(key)
  const legacy = pipelineKey(attr, `.values.${value}.label`)
  if (te(legacy)) return t(legacy)
  return attr.values?.find((v) => v.value === value)?.label || value
}

/** 解释一个允许值的句子；来源没有时为空。 */
function attrValueDescription(attr: GalleryResolvedAttr, value: string): string {
  const key = `${ATTR_NAMESPACE}.${sanitizeKey(attr.id)}_value_${value}_description`
  if (te(key)) return t(key)
  const legacy = pipelineKey(attr, `.values.${value}.description`)
  if (te(legacy)) return t(legacy)
  return attr.values?.find((v) => v.value === value)?.description || ''
}

/** 一张图的一个已观察属性值，尽可能用措辞。 */
function displayObservedValue(attr: GalleryResolvedAttr | undefined, raw: unknown): string {
  const normalized = typeof raw === 'boolean' ? String(raw) : String(raw ?? '')
  if (!attr) return normalized
  return attrValueLabel(attr, normalized)
}

function findAttrByRawName(name: string): GalleryResolvedAttr | undefined {
  return config.value.attributes.find((a) => a.name === name)
}

// ---------------------------------------------------------------------------
// 筛选
//
// 一个属性值取三态之一：中立（不碰那些图片）、"off"（隐藏）、"on"（即使别的
// 规则隐藏也保留）。用户从未碰过的值在 map 中缺席，而不是存成一个词，也永不
// 到达服务器。
// ---------------------------------------------------------------------------
const VERDICTS = ['default', 'off', 'on'] as const
type Verdict = (typeof VERDICTS)[number]

/** 有意义的裁决，即值得上线的那些。 */
const activeRules = computed(() => {
  const out: Record<string, Record<string, string>> = {}
  for (const [id, perValue] of Object.entries(attrVerdicts.value)) {
    const clean: Record<string, string> = {}
    for (const [value, verdict] of Object.entries(perValue)) {
      if (verdict === 'off' || verdict === 'on') clean[value] = verdict
    }
    if (Object.keys(clean).length > 0) out[id] = clean
  }
  return out
})

const activeSelections = computed(() =>
  Object.fromEntries(Object.entries(attrSelections.value).filter(([, v]) => v.length)),
)

/** 多少约束收窄了列表；筛选按钮显示它。 */
const activeFilterCount = computed(() => {
  let n = Object.keys(activeSelections.value).length
  for (const perValue of Object.values(activeRules.value)) n += Object.keys(perValue).length
  if (searchMode.value === 'custom') n += 1
  return n
})

const isNarrowed = computed(
  () =>
    !!keyword.value.trim() ||
    Object.keys(activeRules.value).length > 0 ||
    Object.keys(activeSelections.value).length > 0 ||
    sourceDocIds.value.length > 0,
)

function verdictOf(attrId: string, value: string): Verdict {
  return (attrVerdicts.value[attrId]?.[value] as Verdict) || 'default'
}

function setVerdict(attrId: string, value: string, verdict: Verdict): void {
  if (verdictOf(attrId, value) === verdict) return
  const perValue = { ...(attrVerdicts.value[attrId] || {}) }
  if (verdict === 'default') delete perValue[value]
  else perValue[value] = verdict
  attrVerdicts.value = { ...attrVerdicts.value, [attrId]: perValue }
  reload()
}

function verdictLabel(v: Verdict): string {
  if (v === 'off') return t(`${NS}.verdictOff`)
  if (v === 'on') return t(`${NS}.verdictOn`)
  return t(`${NS}.verdictDefault`)
}

function onKeywordsInput(attrId: string, raw: string) {
  const values = raw
    .split(/[,，;；\n]/)
    .map((s) => s.trim())
    .filter(Boolean)
  attrSelections.value = { ...attrSelections.value, [attrId]: values }
  reload()
}

function keywordsInputValue(attrId: string): string {
  return (attrSelections.value[attrId] || []).join(', ')
}

/** 丢掉筛选面板中的每个约束，包括搜索范围。 */
function clearFilters(): void {
  attrVerdicts.value = {}
  attrSelections.value = {}
  // 本地二开不回读用户偏好：搜索范围勾选改为当次会话有效，刷新回 all。
  searchMode.value = 'all'
  reload()
}

/** 空状态的返回路径：同时忘掉关键字与来源文档。 */
function clearAll(): void {
  if (sourceTimer) clearTimeout(sourceTimer)
  keyword.value = ''
  sourceDocIds.value = []
  clearFilters()
}

/** 在来源面板勾选：把列表限定到这些文档。 */
let sourceTimer: ReturnType<typeof setTimeout> | undefined
function onSourceDocsChange(ids: string[]): void {
  sourceDocIds.value = ids
  if (sourceTimer) clearTimeout(sourceTimer)
  sourceTimer = setTimeout(() => reload(), 300)
}

// ---------------------------------------------------------------------------
// 搜索范围（mode + 逐字段开关）
//
// 勾选每个字段等价于 "all" 模式，因此面板不需要独立开关：mode 跟随勾选。
// 最后一个勾选字段不能取消——对零字段搜索什么也找不到。
// ---------------------------------------------------------------------------
function onSearchFieldToggle(attrId: string, checked: boolean) {
  const on = new Set(activeSearchIds.value)
  if (checked) on.add(attrId)
  else on.delete(attrId)
  const next: Record<string, string> = { ...searchStatus.value }
  for (const attr of searchAttrs.value) next[attr.id] = on.has(attr.id) ? 'on' : 'off'
  searchStatus.value = next
  searchMode.value = on.size === searchAttrs.value.length ? 'all' : 'custom'
  if (keyword.value.trim()) reload()
}

function isLastSearchField(attrId: string): boolean {
  const ids = activeSearchIds.value
  return ids.length === 1 && ids[0] === attrId
}

// ---------------------------------------------------------------------------
// 排序
// ---------------------------------------------------------------------------
const sortFieldLabel = computed(() => {
  const attr = sortAttrs.value.find((a) => a.id === sortBy.value)
  return attr ? attrLabel(attr) : ''
})

function selectSortField(id: string): void {
  if (sortBy.value === id) return
  sortBy.value = id
  reload()
}

function selectSortOrder(order: 'asc' | 'desc'): void {
  if (sortOrder.value === order) return
  sortOrder.value = order
  reload()
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------
function buildParams(): ImageListParams {
  const rules = activeRules.value
  const selections = activeSelections.value
  return {
    keyword: keyword.value.trim() || undefined,
    searchIn: activeSearchIds.value,
    sortBy: sortBy.value || undefined,
    sortOrder: sortOrder.value,
    attrFilters: Object.keys(selections).length ? selections : undefined,
    attrRules: Object.keys(rules).length ? rules : undefined,
    knowledgeIds: sourceDocIds.value.length ? sourceDocIds.value : undefined,
    pageSize,
  }
}

// 一个查询拥有它追加的所有页；陈旧响应无法替换它。
let listToken = 0
let queryParams: ImageListParams = {}

async function fetchNextPage(): Promise<boolean> {
  if (!props.knowledgeBaseId) return false
  const token = listToken
  const nextPage = page.value + 1
  loading.value = true
  error.value = ''
  try {
    const res = await listGalleryImages(props.knowledgeBaseId, { ...queryParams, page: nextPage })
    if (token !== listToken) return false
    const existing = new Set(items.value.map((img) => img.id))
    const incoming = res.items.filter((img) => !existing.has(img.id))
    items.value.push(...incoming)
    page.value = nextPage
    total.value = res.total
    exhausted.value = res.items.length === 0
    void resolveThumbnails(incoming, token)
    await nextTick()
    observeCards()
    return true
  } catch (e) {
    if (token === listToken) error.value = e instanceof Error ? e.message : String(e)
    return false
  } finally {
    if (token === listToken) {
      loading.value = false
      void nextTick(checkLoadMore)
    }
  }
}

function loadMore(): Promise<boolean> {
  if (loading.value || !hasMore.value) return Promise.resolve(false)
  return fetchNextPage()
}

function resetResults() {
  listToken++
  viewerOpen.value = false
  items.value = []
  total.value = 0
  page.value = 0
  exhausted.value = false
  thumbUrls.value = {}
  thumbBroken.value = {}
  imageRatios.value = {}
  scrollEl.value?.scrollTo({ top: 0 })
}

function reload() {
  resetResults()
  queryParams = buildParams()
  return fetchNextPage()
}

let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearchInput() {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => reload(), 350)
}

function onSearchNow() {
  if (searchTimer) clearTimeout(searchTimer)
  reload()
}

// ---------------------------------------------------------------------------
// 查看器
// ---------------------------------------------------------------------------
const viewerOpen = ref(false)
const viewerIndex = ref(0)

function openViewer(index: number) {
  viewerIndex.value = index
  viewerOpen.value = true
}

function closeViewer() {
  if (!viewerOpen.value) return
  viewerOpen.value = false
  // 查看器可能走到了另一张图上；把它停下的卡片带回视野，让网格从它停下的地方
  // 继续。
  const index = viewerIndex.value
  void nextTick(() => {
    scrollEl.value
      ?.querySelectorAll<HTMLElement>('.ig-card')
      [index]?.scrollIntoView({ block: 'nearest' })
  })
}

/** 到达已加载边缘时，追加下一批并保持查看器连续。 */
async function onViewerLoadMore() {
  const token = listToken
  const nextIndex = items.value.length
  if (await loadMore() && token === listToken && viewerOpen.value && items.value.length > nextIndex) {
    viewerIndex.value = nextIndex
  }
}

function onOpenSource(knowledgeId: string) {
  closeViewer()
  emit('open-source-doc', knowledgeId)
}

function attrRows(img: ImageAsset): GalleryAttrRow[] {
  return Object.entries(img.attrs || {}).map(([name, raw]) => {
    const attr = findAttrByRawName(name)
    return {
      key: name,
      label: attr ? attrLabel(attr) : name,
      value: displayObservedValue(attr, raw),
    }
  })
}

function sourceLabel(img: ImageAsset): string {
  return img.source_name || img.knowledge_id
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------
onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
  if (sourceTimer) clearTimeout(sourceTimer)
  // 遗弃任何飞行中的列表，使它无法在卸载后映射新 blob。
  listToken++
  initToken++
  cardObserver?.disconnect()
  scrollObserver?.disconnect()
  releaseBlobs()
})

// 除非契约不再提供，否则最新在前；按 id 选择使默认值独立于来源注册顺序。
const DEFAULT_SORT_ID = 'builtin:created_at'

let initToken = 0

// 为当前知识库加载契约与第一页。它也在知识库切换时运行：组件实例被复用，因此
// 先丢掉上一个知识库作用域的一切。
async function initGallery() {
  const token = ++initToken
  resetResults()
  if (searchTimer) clearTimeout(searchTimer)
  if (sourceTimer) clearTimeout(sourceTimer)
  error.value = ''
  keyword.value = ''
  sortBy.value = ''
  attrSelections.value = {}
  attrVerdicts.value = {}
  sourceDocIds.value = []
  sourcePickerVisible.value = false
  releaseBlobs()
  configLoaded.value = false
  config.value = { attribute_sources: [], attributes: [], mode: 'all', status: {} }
  loading.value = true
  try {
    const cfg = await fetchGalleryConfig(props.knowledgeBaseId)
    if (token !== initToken) return
    config.value = cfg
    searchMode.value = cfg.mode
    searchStatus.value = { ...cfg.status }
    configLoaded.value = true
  } catch {
    if (token !== initToken) return
    // 降级模式：无契约、无属性 UI——builtin 列表与默认搜索仍可用。
  }
  if (!sortBy.value && sortAttrs.value.length) {
    sortBy.value = sortAttrs.value.find((a) => a.id === DEFAULT_SORT_ID)?.id ?? sortAttrs.value[0].id
  }
  await reload()
}

onMounted(() => {
  cardObserver = new ResizeObserver((entries) => {
    sizeCards(entries.map((entry) => entry.target as HTMLElement))
    void nextTick(checkLoadMore)
  })
  scrollObserver = new ResizeObserver(checkLoadMore)
  if (scrollEl.value) scrollObserver.observe(scrollEl.value)
  void initGallery()
})

watch(viewerOpen, (open) => {
  if (!open) void nextTick(checkLoadMore)
})

watch(
  () => props.knowledgeBaseId,
  (next, prev) => {
    if (next && next !== prev) void initGallery()
  },
)
</script>

<template>
  <div class="image-gallery">
    <!-- 与文档 tab 同形：左边列出什么，右边是收窄与排序它的控件。 -->
    <div class="ig-toolbar">
      <div class="ig-toolbar__summary">
        <span class="ig-toolbar__title">{{ t(`${NS}.allImages`) }}</span>
        <span v-if="configLoaded || total" class="ig-toolbar__count">
          {{ t(isNarrowed ? `${NS}.countFiltered` : `${NS}.count`, { count: total }) }}
        </span>
      </div>

      <div class="ig-toolbar__trailing">
        <t-input
          v-model="keyword"
          :placeholder="t(`${NS}.searchPlaceholder`)"
          :aria-label="t(`${NS}.searchPlaceholder`)"
          clearable
          class="ig-search"
          @enter="onSearchNow"
          @input="onSearchInput"
          @clear="onSearchNow"
        >
          <template #prefix-icon><t-icon name="search" size="16px" /></template>
        </t-input>

        <t-popup
          v-model:visible="sourcePickerVisible"
          trigger="click"
          placement="bottom-right"
          overlay-class-name="gallery-toolbar-popup"
          :overlay-inner-style="{ padding: 0 }"
        >
          <button
            type="button"
            class="ig-tool-btn"
            :class="{ active: sourcePickerVisible || sourceDocIds.length > 0 }"
            :aria-expanded="sourcePickerVisible"
          >
            <t-icon name="file" size="16px" />
            {{ t(`${NS}.sourceFilter`) }}
            <span v-if="sourceDocIds.length" class="ig-tool-btn__count">{{ sourceDocIds.length }}</span>
          </button>
          <template #content>
            <GallerySourcePicker
              :kb-id="props.knowledgeBaseId"
              :visible="sourcePickerVisible"
              :selected="sourceDocIds"
              @update:selected="onSourceDocsChange"
            />
          </template>
        </t-popup>

        <t-popup
          v-model:visible="filterPanelVisible"
          trigger="click"
          placement="bottom-right"
          overlay-class-name="gallery-toolbar-popup"
          :overlay-inner-style="{ padding: 0 }"
        >
          <button
            type="button"
            class="ig-tool-btn"
            :class="{ active: filterPanelVisible || activeFilterCount > 0 }"
            :aria-expanded="filterPanelVisible"
          >
            <t-icon name="filter" size="16px" />
            {{ t(`${NS}.filters`) }}
            <span v-if="activeFilterCount" class="ig-tool-btn__count">{{ activeFilterCount }}</span>
          </button>
          <template #content>
            <section class="ig-filter-panel" :aria-label="t(`${NS}.filters`)">
              <header class="ig-filter-panel__header">
                <strong>{{ t(`${NS}.filters`) }}</strong>
                <button type="button" :disabled="!activeFilterCount" @click="clearFilters">
                  {{ t(`${NS}.clearFilters`) }}
                </button>
              </header>

              <div v-if="searchAttrs.length" class="ig-filter-section">
                <div class="ig-filter-section__title">{{ t(`${NS}.searchIn`) }}</div>
                <div class="ig-filter-section__hint">{{ t(`${NS}.searchInHint`) }}</div>
                <div class="ig-search-fields">
                  <t-checkbox
                    v-for="attr in searchAttrs"
                    :key="attr.id"
                    :checked="activeSearchIds.includes(attr.id)"
                    :disabled="isLastSearchField(attr.id)"
                    :title="attrDescription(attr)"
                    @change="(checked: boolean) => onSearchFieldToggle(attr.id, checked)"
                  >
                    {{ attrLabel(attr) }}
                  </t-checkbox>
                </div>
              </div>

              <div v-if="filterAttrs.length" class="ig-filter-section">
                <div class="ig-filter-section__title">{{ t(`${NS}.attrSection`) }}</div>
                <div class="ig-filter-section__hint">{{ t(`${NS}.attrHint`) }}</div>
                <div v-for="attr in filterAttrs" :key="attr.id" class="ig-attr">
                  <div class="ig-attr__name">
                    {{ attrLabel(attr) }}
                    <t-tooltip v-if="attrDescription(attr)" :content="attrDescription(attr)">
                      <t-icon name="help-circle" size="14px" class="ig-attr__help" />
                    </t-tooltip>
                  </div>
                  <!-- 值列表每个值一行，三态并排；自由文本属性没有可定位的值，
                       因此保留一个关键字输入框。 -->
                  <template v-if="attr.type !== 'keywords'">
                    <div v-for="v in attr.values || []" :key="v.value" class="ig-attr__row">
                      <span class="ig-attr__value" :title="attrValueDescription(attr, v.value)">
                        {{ attrValueLabel(attr, v.value) }}
                      </span>
                      <div class="ig-seg" role="radiogroup" :aria-label="attrValueLabel(attr, v.value)">
                        <button
                          v-for="verdict in VERDICTS"
                          :key="verdict"
                          type="button"
                          role="radio"
                          class="ig-seg__item"
                          :class="['is-' + verdict, { active: verdictOf(attr.id, v.value) === verdict }]"
                          :aria-checked="verdictOf(attr.id, v.value) === verdict"
                          @click="setVerdict(attr.id, v.value, verdict)"
                        >
                          {{ verdictLabel(verdict) }}
                        </button>
                      </div>
                    </div>
                  </template>
                  <t-input
                    v-else
                    :value="keywordsInputValue(attr.id)"
                    clearable
                    :placeholder="t(`${NS}.keywordsPlaceholder`)"
                    @change="(v: string) => onKeywordsInput(attr.id, v)"
                    @enter="(v: string) => onKeywordsInput(attr.id, v)"
                  />
                </div>
              </div>

              <div v-if="!searchAttrs.length && !filterAttrs.length" class="ig-filter-panel__empty">
                {{ t(`${NS}.noAttrs`) }}
              </div>
            </section>
          </template>
        </t-popup>

        <t-popup
          v-if="sortAttrs.length"
          v-model:visible="sortPanelVisible"
          trigger="click"
          placement="bottom-right"
          overlay-class-name="gallery-toolbar-popup"
          :overlay-inner-style="{ padding: 0 }"
        >
          <button
            type="button"
            class="ig-tool-btn ig-sort-trigger"
            :class="{ active: sortPanelVisible }"
            :aria-label="`${t(`${NS}.sort`)}: ${sortFieldLabel}`"
          >
            <t-icon name="filter-sort" size="16px" />
            <span class="ig-sort-trigger__label">{{ t(`${NS}.sort`) }} · {{ sortFieldLabel }}</span>
            <t-icon :name="sortOrder === 'asc' ? 'arrow-up' : 'arrow-down'" size="14px" />
          </button>
          <template #content>
            <div class="ig-sort-panel" role="menu" :aria-label="t(`${NS}.sort`)">
              <section class="ig-sort-group">
                <div class="ig-sort-group__label">{{ t(`${NS}.sortField`) }}</div>
                <div class="ig-sort-group__options">
                  <button
                    v-for="attr in sortAttrs"
                    :key="attr.id"
                    type="button"
                    role="menuitemradio"
                    class="ig-sort-option"
                    :class="{ active: sortBy === attr.id }"
                    :aria-checked="sortBy === attr.id"
                    @click="selectSortField(attr.id)"
                  >
                    <span>{{ attrLabel(attr) }}</span>
                    <t-icon v-if="sortBy === attr.id" name="check" size="14px" />
                  </button>
                </div>
              </section>
              <section class="ig-sort-group">
                <div class="ig-sort-group__label">{{ t(`${NS}.sortOrder`) }}</div>
                <div class="ig-sort-group__options">
                  <button
                    v-for="order in (['desc', 'asc'] as const)"
                    :key="order"
                    type="button"
                    role="menuitemradio"
                    class="ig-sort-option"
                    :class="{ active: sortOrder === order }"
                    :aria-checked="sortOrder === order"
                    @click="selectSortOrder(order)"
                  >
                    <span>{{ t(order === 'asc' ? `${NS}.orderAsc` : `${NS}.orderDesc`) }}</span>
                    <t-icon v-if="sortOrder === order" name="check" size="14px" />
                  </button>
                </div>
              </section>
            </div>
          </template>
        </t-popup>
      </div>
    </div>

    <div ref="scrollEl" class="ig-scroll" :class="{ 'is-empty': !items.length }" @scroll.passive="checkLoadMore">
      <t-loading :loading="loading && !items.length" size="small" class="ig-loading">
        <div v-if="error && !items.length" class="ig-error" role="alert">
          {{ error }}
          <t-button variant="text" @click="reload">{{ t('common.retry') }}</t-button>
        </div>

        <EmptyState
          v-else-if="!items.length && !loading"
          icon="image"
          :title="isNarrowed ? t(`${NS}.emptyFiltered`) : t(`${NS}.empty`)"
          :description="isNarrowed ? '' : t(`${NS}.emptyHint`)"
        >
          <t-button v-if="isNarrowed" variant="outline" @click="clearAll">{{ t(`${NS}.clearFilters`) }}</t-button>
        </EmptyState>

        <div v-else ref="gridEl" class="ig-grid">
          <button
            v-for="(img, idx) in items"
            :key="img.id"
            type="button"
            class="ig-card"
            :class="{ 'is-disabled': !img.is_enabled }"
            @click="openViewer(idx)"
          >
            <div class="ig-card__thumb" :style="{ aspectRatio: imageRatios[img.id] || 4 / 3 }">
              <img
                v-if="thumbUrls[img.id] && !thumbBroken[img.id]"
                :src="thumbUrls[img.id]"
                :alt="img.caption"
                loading="lazy"
                draggable="false"
                @load="onThumbLoad(img.id, $event)"
                @error="onThumbError(img.id)"
              />
              <div v-else-if="thumbBroken[img.id]" class="ig-card__broken">
                <t-icon name="image-error" size="20px" />
                <span>{{ t(`${NS}.imageLoadError`) }}</span>
              </div>
              <span v-if="!img.is_enabled" class="ig-card__badge">
                {{ t(`${NS}.attr.builtin_is_enabled_value_false`) }}
              </span>
            </div>
            <div class="ig-card__meta">
              <div class="ig-card__caption" :class="{ 'is-empty': !img.caption }">
                {{ img.caption || t(`${NS}.noCaption`) }}
              </div>
              <div class="ig-card__source" :title="sourceLabel(img)">
                <t-icon name="file" size="14px" />
                <span>{{ sourceLabel(img) }}</span>
              </div>
            </div>
          </button>
        </div>
      </t-loading>
      <div v-if="items.length" ref="loadMoreEl" class="ig-load-more" aria-live="polite">
        <template v-if="error">
          <span class="ig-error" role="alert">{{ error }}</span>
          <t-button variant="text" @click="loadMore">{{ t('common.retry') }}</t-button>
        </template>
        <t-loading v-else-if="loading" size="small" :text="t('common.loading')" />
        <t-button v-else-if="hasMore" variant="text" @click="loadMore">{{ t('common.loadMore') }}</t-button>
        <span v-else>{{ t('common.noMoreData') }}</span>
      </div>
    </div>

    <GalleryViewer
      v-if="viewerOpen && items.length"
      :items="items"
      :index="viewerIndex"
      :total="total"
      :loading="loading"
      :thumb-urls="thumbUrls"
      :resolve-src="resolveImageSrc"
      :attr-rows="attrRows"
      @select="(i: number) => (viewerIndex = i)"
      @load-more="onViewerLoadMore"
      @close="closeViewer"
      @open-source="onOpenSource"
    />
  </div>
</template>

<style>
/* 弹层渲染在组件外，因此其外壳以非 scoped 样式；与文档 tab 的筛选弹层同框。 */
.gallery-toolbar-popup .t-popup__content {
  border: 1px solid var(--td-component-stroke);
  border-radius: var(--app-radius-xl);
  box-shadow: 0 8px 32px rgb(0 0 0 / 10%);
}
</style>

<style scoped lang="less">
.image-gallery {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
  min-width: 0;
  container-type: inline-size;
  container-name: image-gallery;
}

// 镜像 KnowledgeBase.vue 中的 .doc-filter-bar，因此切换 tab 时工具栏保持不动。
.ig-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-shrink: 0;
  padding: 0 0 16px;
  border-bottom: 1px solid var(--td-component-stroke);

  &__summary {
    display: flex;
    align-items: baseline;
    gap: 12px;
    min-width: 0;
    min-height: 32px;
    line-height: 32px;
  }

  &__title {
    color: var(--td-text-color-primary);
    font-size: var(--app-text-md);
    font-weight: 600;
    white-space: nowrap;
  }

  &__count {
    color: var(--td-text-color-placeholder);
    font-size: var(--app-text-sm);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  &__trailing {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
}

.ig-search {
  width: 220px;
  min-width: 100px;

  :deep(.t-input) {
    background: transparent;
    border-color: var(--td-component-stroke);
    border-radius: var(--app-radius-md);
    font-size: var(--app-text-md);
  }
}

.ig-tool-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  height: 32px;
  padding: 0 10px;
  border: 1px solid var(--td-component-stroke);
  border-radius: var(--app-radius-md);
  background: transparent;
  color: var(--td-text-color-secondary);
  font: inherit;
  font-size: var(--app-text-md);
  white-space: nowrap;
  cursor: pointer;

  &:hover,
  &.active {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-primary);
  }

  &:focus-visible {
    outline: 2px solid var(--app-focus-border);
    outline-offset: 2px;
  }

  &__count {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 17px;
    height: 17px;
    padding: 0 2px;
    color: var(--td-brand-color);
    font-weight: 600;
    font-size: var(--app-text-xs);
    font-variant-numeric: tabular-nums;
  }
}

.ig-sort-trigger {
  max-width: 220px;

  &__label {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
}

// 筛选面板——与 .doc-filter-panel 同一框体与字号刻度。
.ig-filter-panel {
  width: 360px;
  max-width: calc(100vw - 32px);
  max-height: min(600px, 80vh);
  overflow-y: auto;
  padding: 16px;
  box-sizing: border-box;
  color: var(--td-text-color-primary);
  font-size: var(--app-text-md);

  button {
    font: inherit;
    cursor: pointer;
  }

  &__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;

    strong {
      font-size: var(--app-text-base);
      font-weight: 600;
    }

    button {
      border: 0;
      padding: 0;
      background: transparent;
      color: var(--td-text-color-secondary);
      font-size: var(--app-text-sm);

      &:hover:not(:disabled) { color: var(--td-brand-color); }
      &:disabled { color: var(--td-text-color-disabled); cursor: default; }
    }
  }

  &__empty {
    padding: 8px 0;
    color: var(--td-text-color-placeholder);
  }
}

.ig-filter-section {
  & + & {
    margin-top: 16px;
    padding-top: 14px;
    border-top: 1px solid var(--td-component-stroke);
  }

  &__title {
    font-weight: 600;
    line-height: 20px;
  }

  &__hint {
    margin: 2px 0 10px;
    color: var(--td-text-color-placeholder);
    font-size: var(--app-text-xs);
    line-height: 17px;
  }
}

.ig-search-fields {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 16px;
}

.ig-attr {
  & + & { margin-top: 12px; }

  &__name {
    display: flex;
    align-items: center;
    gap: 4px;
    margin-bottom: 4px;
    color: var(--td-text-color-secondary);
    font-size: var(--app-text-sm);
  }

  &__help {
    color: var(--td-text-color-placeholder);
    cursor: help;
  }

  &__row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 32px;
  }

  &__value {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
}

// 三态开关，画成文档 tab 的视图切换。
.ig-seg {
  display: inline-flex;
  flex-shrink: 0;
  padding: 2px;
  border-radius: var(--app-radius-md);
  background: var(--td-bg-color-secondarycontainer);

  &__item {
    height: 24px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--app-radius-xs);
    background: transparent;
    color: var(--td-text-color-secondary);
    font-size: var(--app-text-sm);
    white-space: nowrap;
    transition: background-color var(--app-motion-fast) ease, color var(--app-motion-fast) ease;

    &:hover { color: var(--td-text-color-primary); }

    &.active {
      background: var(--td-bg-color-container);
      color: var(--td-text-color-primary);
      box-shadow: 0 1px 3px rgb(0 0 0 / 8%);
    }

    &.is-off.active { color: var(--td-error-color); }
    &.is-on.active { color: var(--td-brand-color); }

    &:focus-visible {
      outline: 2px solid var(--app-focus-border);
      outline-offset: 1px;
    }
  }
}

// 排序面板——与 .document-sort-panel 同形。
.ig-sort-panel {
  width: 280px;
  max-width: calc(100vw - 32px);
  padding: 6px;
  box-sizing: border-box;
  color: var(--td-text-color-primary);
}

.ig-sort-group {
  padding: 7px 6px 8px;

  & + & { border-top: 1px solid var(--td-component-stroke); }

  &__label {
    padding: 0 4px 6px;
    font-size: var(--app-text-md);
    font-weight: 600;
    line-height: 20px;
  }

  &__options {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 4px;
  }
}

.ig-sort-option {
  display: inline-flex;
  align-items: center;
  justify-content: space-between;
  min-width: 0;
  height: 32px;
  padding: 0 10px;
  border: 0;
  border-radius: var(--app-radius-sm);
  background: transparent;
  color: var(--td-text-color-primary);
  font-family: var(--app-font-family);
  font-size: var(--app-text-md);
  cursor: pointer;

  &:hover { background: var(--td-bg-color-secondarycontainer); }

  &.active {
    background: var(--td-brand-color-light);
    color: var(--td-brand-color);
    font-weight: 500;
  }
}

// 网格
.ig-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
  padding: 16px 0;

  &.is-empty {
    display: flex;
    flex-direction: column;
  }
}

.ig-loading {
  display: flex;
  flex-direction: column;

  .is-empty & { min-height: 100%; }
}

.ig-error {
  padding: 16px 0;
  color: var(--td-error-color);
}

.ig-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(180px, 100%), 1fr));
  grid-auto-rows: 1px;
  gap: 12px;
  align-items: start;
  align-content: start;
}

.ig-card {
  display: flex;
  flex-direction: column;
  min-width: 0;
  padding: 0;
  overflow: hidden;
  border: 1px solid var(--td-component-border);
  border-radius: var(--app-radius-md);
  background: var(--td-bg-color-container);
  box-shadow: 0 1px 2px rgb(0 0 0 / 6%);
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: border-color var(--app-motion-base) ease, box-shadow var(--app-motion-base) ease;

  &:hover {
    border-color: color-mix(in srgb, var(--td-component-stroke) 55%, var(--td-brand-color));
    box-shadow: 0 4px 14px rgb(0 0 0 / 7%);

    .ig-card__thumb img { transform: scale(1.03); }
  }

  &:focus-visible {
    outline: 2px solid var(--app-focus-border);
    outline-offset: 2px;
  }

  &.is-disabled .ig-card__thumb img { opacity: 0.55; }

  &__thumb {
    position: relative;
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    overflow: hidden;
    background: var(--td-bg-color-secondarycontainer);

    img {
      width: 100%;
      height: 100%;
      object-fit: contain;
      transition: transform var(--app-motion-slow) ease;
    }
  }

  &__broken {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 4px;
    padding: 0 8px;
    color: var(--td-text-color-placeholder);
    font-size: var(--app-text-sm);
    text-align: center;
  }

  &__badge {
    position: absolute;
    top: 8px;
    left: 8px;
    padding: 0 6px;
    border-radius: var(--app-radius-xs);
    background: rgb(0 0 0 / 55%);
    color: #fff;
    font-size: var(--app-text-xs);
    line-height: 18px;
  }

  &__meta {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 12px 12px;
  }

  &__caption {
    display: -webkit-box;
    min-height: calc(2 * 1.5em);
    overflow: hidden;
    color: var(--td-text-color-primary);
    font-size: var(--app-text-md);
    line-height: 1.5;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;

    &.is-empty { color: var(--td-text-color-placeholder); }
  }

  &__source {
    display: flex;
    align-items: center;
    gap: 4px;
    min-width: 0;
    color: var(--td-text-color-placeholder);
    font-size: var(--app-text-sm);

    .t-icon { flex-shrink: 0; }

    span {
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
  }
}

.ig-load-more {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: 56px;
  padding: 16px 0;
  color: var(--td-text-color-placeholder);
  font-size: var(--app-text-sm);
}

@container image-gallery (max-width: 780px) {
  .ig-toolbar { flex-wrap: wrap; gap: 12px; }
  .ig-toolbar__trailing { width: 100%; }
  .ig-search { flex: 1; width: auto; }
}
</style>
