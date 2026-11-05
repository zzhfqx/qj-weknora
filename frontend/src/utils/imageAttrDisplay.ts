import type {
  ImageAttrCondition,
  ImageAttrSchema,
  ImageAttrSpec,
  ImageAttrValue,
} from '@/api/knowledge-base'

// ---------------------------------------------------------------------------
// 图片属性面板的展示文案。
//
// GET /image-attrs/schema 返回的注册表是唯一事实源——它携带属性名、度量什么、每个
// 值意味着什么。正因如此，一个非技术运营者能读到「图片中的文字：一段正文」而非
// "contain.text: block"，也正因如此，往后端加一个属性无需前端改动就出现在这里。
//
// 翻译是按属性名做键的叠加层，盖在注册表文案之上。尚未翻译的回落注册表措辞，因此
// 一个全新属性仍读作文字而非裸键。（i18n key 在运行时由属性名构造，这正是
// localeKeyAudit 的 EXTRA_PREFIXES 里注册 `imageAttr.` 前缀的原因。）
// ---------------------------------------------------------------------------

type Translate = (key: string) => string
type HasTranslation = (key: string) => boolean

/** 属性的一个可选值，连同它的含义文字。 */
export interface ImageAttrValueDisplay {
  /** 注册表与处理 trace 中出现的原始值。 */
  value: string
  /** 该值屏幕上的短名。 */
  label: string
  /** 解释该值的整句；注册表没有时为空。 */
  description: string
}

/** 设置面板所见的一个属性。 */
export interface ImageAttrDisplay {
  /** 稳定属性键，例如 "contain.text"。小字展示，以便与 trace 对齐。 */
  name: string
  /** 人类可读名，例如 "图片中的文字"。 */
  label: string
  /** 一行说明该属性度量什么。不可用时为空。 */
  description: string
  /** 按注册表顺序排列的每个可选值及其含义。 */
  values: ImageAttrValueDisplay[]
}

/** 一条 OCR 策略条件，文字形式加原始对。 */
export interface ImageAttrConditionDisplay {
  /** 例如「图片中的文字 = 一段正文」 */
  label: string
  /** 例如 "contain.text = block"——让面板与 trace 保持对齐。 */
  raw: string
}

/** 一个 spec 声明的值；presence 属性隐式列出 true/false。 */
function specValues(attr: ImageAttrSpec): ImageAttrValue[] {
  if (attr.values && attr.values.length) return attr.values
  return attr.type === 'presence' ? [{ value: 'true', label: 'true' }, { value: 'false', label: 'false' }] : []
}

/**
 * 一个属性的 i18n 命名空间。
 *
 * vue-i18n 按点逐段解析 key，因此字面量 `'contain.text'` 这个消息 key 经
 * `imageAttr.contain.text.label` 是触达不到的——查找会找 imageAttr → contain →
 * text 然后停下。属性名因此转义它的点（contain.text → contain_text）成为一段可解析
 * 的段。同一映射在每个 locale 包里套用，因此一个新属性只需把名字拼对。
 */
export function imageAttrKeyBase(name: string): string {
  return `imageAttr.${name.replace(/\./g, '_')}`
}

/** 查 `key`，回落注册表自己的文案。 */
function pick(key: string, fallback: string, t: Translate, te: HasTranslation): string {
  return te(key) ? t(key) : fallback
}

/**
 * 为设置面板渲染一个注册表属性。从不返回空 label：一个到达时不带展示文案的属性仍
 * 展示它的名字，因此面板退化为标识符而非空白。
 */
export function imageAttrDisplay(
  attr: ImageAttrSpec,
  t: Translate,
  te: HasTranslation,
): ImageAttrDisplay {
  const base = imageAttrKeyBase(attr.name)
  return {
    name: attr.name,
    label: pick(`${base}.label`, attr.label || attr.name, t, te),
    description: pick(`${base}.description`, attr.description ?? '', t, te),
    values: specValues(attr).map((value) => ({
      value: value.value,
      label: pick(
        `${base}.values.${value.value}.label`,
        value.label || value.value,
        t,
        te,
      ),
      description: pick(
        `${base}.values.${value.value}.description`,
        value.description ?? '',
        t,
        te,
      ),
    })),
  }
}

/**
 * 用与属性列表相同的文字渲染一条 OCR 策略条件，同时保留原始 `prop = value` 对，
 * 以便运营者仍能把面板与处理 trace 对上。
 */
export function imageAttrConditionDisplay(
  cond: ImageAttrCondition,
  schema: ImageAttrSchema,
  t: Translate,
  te: HasTranslation,
): ImageAttrConditionDisplay {
  const raw = `${cond.prop} = ${cond.is}`
  const attr = schema.attributes.find((candidate) => candidate.name === cond.prop)
  if (!attr) return { label: raw, raw }

  const display = imageAttrDisplay(attr, t, te)
  const value = display.values.find((candidate) => candidate.value === cond.is)
  return { label: `${display.label} = ${value ? value.label : cond.is}`, raw }
}
