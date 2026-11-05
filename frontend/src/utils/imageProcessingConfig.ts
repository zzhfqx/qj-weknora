/** 一条 OCR 触发条件：属性 `prop` 等于 `is`。 */
export interface ImageAttrConditionLike {
  prop: string
  is: string
}

/** 管线 id。两个对话框都拿它们比较，而非写字面量。 */
export const IMAGE_PIPELINE_DEFAULT = 'default'
export const IMAGE_PIPELINE_SMARTOCR = 'smartocr'
/** 已存配置可能仍携带的旧拼写；归一化处理，但不再被遵循。 */
export const LEGACY_PIPELINE_CAPTION_OCR = 'caption_ocr'
export const LEGACY_PIPELINE_OB_CAP_OCR = 'ob_cap_ocr'

/** KB 编辑器中喂给 image_processing_config 的字段。 */
export interface ImageProcessingEdits {
  imageAttrsEnabled: boolean
  onUnobserved: boolean
  /** 注册表当前的默认 OCR 条件（GET /image-attrs/schema）。 */
  defaultOn: ImageAttrConditionLike[]
  /** 面板里选中的管线；空表示多模态关闭、相关字段消失。 */
  pipelineId: string
  /** 该管线的私有可调项，按字段键索引。 */
  pipelineParams: Record<string, unknown>
}

/** 覆盖负载携带的管线字段，空值已剔除。 */
export interface PipelineFields {
  image_pipeline?: string
  image_pipeline_params?: Record<string, unknown>
}

/**
 * 把一个已存管线 id 映射到面板提供的 id。两个改名都折叠在这里，以便 KB 编辑器、
 * 上传对话框与重新解析对话框对一个已存值的含义达成一致；未知 id 原样穿过，由
 * 调用方回退。镜像后端 types.NormalizeImagePipelineID。
 */
export function normalizeImagePipelineId(id: string | null | undefined): string {
  if (!id) return ''
  if (id === LEGACY_PIPELINE_CAPTION_OCR) return IMAGE_PIPELINE_DEFAULT
  if (id === LEGACY_PIPELINE_OB_CAP_OCR) return IMAGE_PIPELINE_SMARTOCR
  return id
}

/**
 * 构造负载的管线那一半。一个空选择本身不是一种值：字段缺失时后端从
 * image_attrs_enabled 解析，因此这一对被丢弃，而非写成一个空对。参数同理——一个
 * 没有可调项的选择绝不能发一个空对象，否则会读作「调用方把所有东西都关了」。
 *
 * 这是唯一决定空含义的地方，因此 KB 编辑器（整体替换配置）与上传/重新解析对话框
 * （逐任务覆盖）不会在「没有设置」意味着什么上产生分歧。
 */
export function buildPipelineFields(
  pipelineId: string,
  pipelineParams: Record<string, unknown> | null | undefined,
): PipelineFields {
  if (!pipelineId) return {}
  const fields: PipelineFields = {}
  if (pipelineId) fields.image_pipeline = pipelineId
  if (pipelineParams && Object.keys(pipelineParams).length > 0) {
    fields.image_pipeline_params = pipelineParams
  }
  return fields
}

/**
 * 构造 KB 编辑器保存的 image_processing_config；若无任何改动则返回 null。后端整体
 * 替换该对象，因此编辑器不拥有的每个字段（model_id 等）都从快照带过来。
 *
 * 编辑器只编辑观察开关与 on_unobserved，但 on_unobserved 必须随一个非空 `on` 同行：
 * 后端只有在 `on` 被设置时才采纳自定义 OCR 子句。一个 `on` 经 API 自定义过的知识库
 * 保留它；没有自定义列表的知识库拿到注册表当前默认，从而仍随默认演进。
 *
 * 管线与其参数同样写入。它们是面板自己的设置；观察开关与选择保持同步，以便一个经
 * UI 编辑过的知识库经旧开关读起来仍一致。
 */
export function buildImageProcessingConfig(
  snapshot: Record<string, unknown> | null | undefined,
  edits: ImageProcessingEdits,
): Record<string, unknown> | null {
  const snap = snapshot || {}
  const storedOn = (snap.image_actions as { ocr?: { on?: unknown } } | undefined)?.ocr?.on
  const on = Array.isArray(storedOn) && storedOn.length > 0 ? storedOn : edits.defaultOn
  const built: Record<string, unknown> = {
    ...snap,
    image_attrs_enabled: edits.imageAttrsEnabled,
    image_actions: {
      ocr: {
        on,
        on_unobserved: edits.onUnobserved,
      },
    },
  }
  // 选择携带面板自己的答案，因此它替换快照里的任何东西——包括把它移除。
  const pipeline = buildPipelineFields(edits.pipelineId, edits.pipelineParams)
  if (pipeline.image_pipeline) built.image_pipeline = pipeline.image_pipeline
  else delete built.image_pipeline
  if (pipeline.image_pipeline_params) built.image_pipeline_params = pipeline.image_pipeline_params
  else delete built.image_pipeline_params
  return JSON.stringify(built) === JSON.stringify(snap) ? null : built
}

/**
 * 按后端 ResolveImagePipelineID 遵循的同一规则，解析一个知识库已存配置指向的管线
 * id：显式存储的 id 优先（改名前的 "caption_ocr" 归一为 "default"，"ob_cap_ocr"
 * 归一为 "smartocr"），否则旧观察开关在开启时选 smartocr、否则 default。选择器出现
 * 之前保存的知识库完全没有配置，落到 default，因此选择器总展示一个真实选项。
 */
export function resolveImagePipelineFromKb(
  kb: Record<string, any> | null | undefined,
): string {
  const cfg = kb?.image_processing_config
  const stored = normalizeImagePipelineId((cfg?.image_pipeline as string | undefined) || '')
  if (stored) return stored
  return cfg?.image_attrs_enabled ? IMAGE_PIPELINE_SMARTOCR : IMAGE_PIPELINE_DEFAULT
}
