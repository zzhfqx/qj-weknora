import { get, post } from '@/utils/request';

// ---------------------------------------------------------------------------
// 图片画廊 API
//
// 画廊列出某知识库内的全部图片资产。一个“资产”是从某 chunk 的 `image_info`
// 数组投影出的单张图（一个文档 chunk 可携带多张图）。后端在数据库内完成去重、
// 按关键字 / 属性 / 启停状态筛选、排序与分页，因此一页的成本与知识库持有多少
// 图片无关。
//
// 画廊展示什么（筛选器、可搜索字段、排序选项）不在此硬编码：
// GET /knowledge-bases/:id/gallery-config 返回自描述契约（属性来源、逐属性
// 已合并的 usage、调用方搜索激活状态），UI 据此渲染。因此后端新增属性无需
// 前端改动即可点亮。
// ---------------------------------------------------------------------------

/** 从某 chunk 投影出的一张图，按后端返回的形态。 */
export interface ImageAsset {
  /** 稳定 id："<chunkID>#<数组内序号>"。 */
  id: string;
  /** 所属 chunk id。 */
  chunk_id: string;
  /** 所属知识（文档）id。 */
  knowledge_id: string;
  /** 来源知识条目的可读名（文档标题）。 */
  source_name: string;
  /** 所属 chunk 类型；在文档（text）chunk 上发现的图片为空。 */
  chunk_type: string;
  /** 渲染后的图片 URL。 */
  url: string;
  /** 变换前的来源引用（若不同）。 */
  original_url: string;
  /** 模型生成的图片描述。 */
  caption: string;
  /** 抽取的 OCR 文本（若有）。 */
  ocr_text: string;
  /** 已观察的属性映射，键为来源内属性名。 */
  attrs: Record<string, unknown>;
  /** 镜像所属 chunk 的启停标志。 */
  is_enabled: boolean;
  /** 镜像所属 chunk 的索引状态。 */
  status: number;
  /** 所属 chunk 创建时间（ISO 8601）。 */
  created_at: string;
  /** 所属 chunk 最近更新时间（ISO 8601）。 */
  updated_at: string;
}

export type ImageSortOrder = 'asc' | 'desc';

/** GET /knowledge-bases/:id/images 接受的查询参数。 */
export interface ImageListParams {
  /** 对 searchIn 字段并集做不区分大小写的子串匹配。 */
  keyword?: string;
  /**
   * 参与搜索的命名空间属性 id（"builtin:caption"）。必须在契约中
   * in_searchfield=true；后端会丢弃其余。省略则用后端默认（builtin caption
   * + ocr_text）。
   */
  searchIn?: string[];
  /** 排序依据的命名空间属性 id；必须 in_sortfield=true。 */
  sortBy?: string;
  sortOrder?: ImageSortOrder;
  /** 限定为该启停状态的 chunk。省略则两者都含。 */
  isEnabled?: boolean;
  /**
   * 来源文档限定：只列出这些知识条目持有的图片。存在时经 POST body 发送
   * （GET 查询参数变体受 URL 长度限制，上限更低）。
   */
  knowledgeIds?: string[];
  /**
   * 按命名空间属性 id 索引的属性筛选。同一属性内多值 OR、属性间 AND。
   * 作为一个 `attr_filters` JSON 查询参数发送。
   */
  attrFilters?: Record<string, string[]>;
  /**
   * 按命名空间属性 id 索引的逐值裁决，每个值映射到 "off"（隐藏携带它的图片）
   * 或 "on"（无论如何显示）。未提及的值保持中立。作为一个 `attr_rules`
   * JSON 查询参数发送；当 "on" 与 "off" 谈论同一张图时，"on" 优先。
   */
  attrRules?: Record<string, Record<string, string>>;
  page?: number;
  pageSize?: number;
}

export interface ImageListResult {
  items: ImageAsset[];
  total: number;
  page: number;
  pageSize: number;
}

/**
 * 列出某知识库的图片资产。属性引用使用画廊契约中的命名空间 id；后端对照契约
 * 校验并静默丢弃不合格者。
 *
 * 携带 knowledge_ids 的请求走 POST /images，筛选条件以 JSON body 传递：否则
 * URL 长度上限会把来源选择器的“全选”限制到寥寥几个文档。
 */
export async function listGalleryImages(
  kbId: string,
  params: ImageListParams = {},
): Promise<ImageListResult> {
  if (params.knowledgeIds && params.knowledgeIds.length) {
    const res = await post<{
      success: boolean;
      data: ImageAsset[];
      total: number;
      page: number;
      page_size: number;
    }>(`/api/v1/knowledge-bases/${kbId}/images`, {
      keyword: params.keyword,
      search_in: params.searchIn,
      sort_by: params.sortBy,
      sort_order: params.sortOrder,
      is_enabled: params.isEnabled,
      knowledge_ids: params.knowledgeIds,
      attr_filters: params.attrFilters,
      attr_rules: params.attrRules,
      page: params.page,
      page_size: params.pageSize,
    });
    return {
      items: res.data,
      total: res.total,
      page: res.page,
      pageSize: res.page_size,
    };
  }

  const query = new URLSearchParams();
  if (params.keyword) query.set('keyword', params.keyword);
  if (params.searchIn && params.searchIn.length) {
    query.set('search_in', params.searchIn.join(','));
  }
  if (params.sortBy) query.set('sort_by', params.sortBy);
  if (params.sortOrder) query.set('sort_order', params.sortOrder);
  if (typeof params.isEnabled === 'boolean') {
    query.set('is_enabled', String(params.isEnabled));
  }
  if (params.attrFilters && Object.keys(params.attrFilters).length) {
    query.set('attr_filters', JSON.stringify(params.attrFilters));
  }
  if (params.attrRules && Object.keys(params.attrRules).length) {
    query.set('attr_rules', JSON.stringify(params.attrRules));
  }
  if (params.page) query.set('page', String(params.page));
  if (params.pageSize) query.set('page_size', String(params.pageSize));

  const qs = query.toString();
  const res = await get<{
    success: boolean;
    data: ImageAsset[];
    total: number;
    page: number;
    page_size: number;
  }>(`/api/v1/knowledge-bases/${kbId}/images${qs ? `?${qs}` : ''}`);

  return {
    items: res.data,
    total: res.total,
    page: res.page,
    pageSize: res.page_size,
  };
}

// ---------------------------------------------------------------------------
// 画廊契约（GET /knowledge-bases/:id/gallery-config）
// ---------------------------------------------------------------------------

/** 各配置层合并后的逐属性 usage 标志。 */
export interface GalleryAttrUsage {
  in_filter: boolean;
  in_searchfield: boolean;
  in_sortfield: boolean;
}

/**
 * 属性的一个允许值。
 *
 * 显示文本刻意拆开：`label` 是塞进筛选复选框的短名，`description` 是有空间时
 * （tooltip）展示的句子。`value` 保留原始机器值——它才是被筛选与搜索的对象，
 * 永不本地化。
 */
export interface GalleryAttrValue {
  value: string;
  label: string;
  description?: string;
}

/**
 * 契约下发的一个属性：来源声明已合并各配置层覆盖的结果。`id` 是命名空间的
 * （"<来源ID>:<名称>"）；`usage_from` 命名最后改动 usage 的最高层
 * （"source" | "system" | "kb" | "user"）。
 */
export interface GalleryResolvedAttr {
  id: string;
  source: string;
  /** 来源内属性名，按 image_info attrs 中存储的形态。 */
  name: string;
  /** "text" | "date" | "extent" | "presence" | "keywords" */
  type: string;
  values?: GalleryAttrValue[];
  /** 屏幕短名；缺失时回退到属性 id。 */
  label: string;
  /** 解释该属性的句子；有空间时展示。 */
  description?: string;
  usage: GalleryAttrUsage;
  usage_from: string;
}

/** 画廊 UI 渲染所依据的自描述契约。 */
export interface GalleryConfig {
  /** 按优先顺序列出的在线来源 id。 */
  attribute_sources: string[];
  attributes: GalleryResolvedAttr[];
  /** 搜索激活模式："all"（默认）或 "custom"。 */
  mode: 'all' | 'custom';
  /** 逐属性搜索开关（"on"/"off"）；custom 模式下生效。 */
  status: Record<string, string>;
}

/** 拉取某知识库的画廊契约。 */
export async function fetchGalleryConfig(kbId: string): Promise<GalleryConfig> {
  const res = await get<{ success: boolean; data: GalleryConfig }>(
    `/api/v1/knowledge-bases/${kbId}/gallery-config`,
  );
  return {
    attribute_sources: res.data.attribute_sources ?? [],
    attributes: res.data.attributes ?? [],
    mode: res.data.mode === 'custom' ? 'custom' : 'all',
    status: res.data.status ?? {},
  };
}
