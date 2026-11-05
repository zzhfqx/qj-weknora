<script setup lang="ts">
/**
 * 图片管线面板。
 *
 * 管线列表与其字段都来自后端（GET /api/v1/image-pipelines），因此本组件不点任何
 * 管线的名、也不认识任何字段：在后端加一条，这里就作为一个新选项与一组新控件到达。
 * 一个字段按其声明的类型渲染，当本 locale 没有对应文案时回落后端自己的措辞。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  fetchImagePipelines,
  validateImagePipelines,
  type ImagePipelineField,
  type ImagePipelineSpec,
  type ImagePipelineViolation,
} from '@/api/knowledge-base'

const props = defineProps<{
  /** 选中的管线 id。 */
  pipelineId: string
  /** 该管线的私有可调项；缺键时读字段默认值。 */
  params: Record<string, unknown>
}>()

const emit = defineEmits<{
  'update:pipelineId': [value: string]
  'update:params': [value: Record<string, unknown>]
  /** 当前选择被配成什么都不做时置为 on。 */
  'update:invalid': [value: boolean]
}>()

const { t, te } = useI18n()

const pipelines = ref<ImagePipelineSpec[]>([])
const loading = ref(false)
const loadError = ref('')

/** 当前选中的管线（若注册表仍携带它）。 */
const current = computed<ImagePipelineSpec | undefined>(() =>
  pipelines.value.find((pipeline) => pipeline.id === props.pipelineId),
)

/** 它的字段，按后端声明顺序。 */
const fields = computed<ImagePipelineField[]>(() => current.value?.fields ?? [])

// 所选管线破坏的每条规则，由后端按它自己的措辞上报：每条违规携带自己文案的 i18n key
// 与要指向的控件名。这里不断定什么被禁止——规则随 spec 同行，因此给一条管线加一条
// 规则无需改本文件；而动作不由用户开关的管线不声明任何规则，永远不会进这个列表。
const violations = ref<ImagePipelineViolation[]>([])
const validationFailed = ref(false)

const invalid = computed<boolean>(() => violations.value.length > 0)

watch(invalid, (value) => emit('update:invalid', value), { immediate: true })

/** 防抖避免一次细碎改动在每次击键时都发一次请求。 */
let validationTimer: ReturnType<typeof setTimeout> | undefined
let validationGeneration = 0
let disposed = false

function scheduleValidation() {
  if (disposed) return
  // 输入一变就作废在途响应，包括下一次防抖请求发起前的那段间隔。
  validationGeneration++
  violations.value = []
  validationFailed.value = false
  if (validationTimer) clearTimeout(validationTimer)
  validationTimer = setTimeout(() => void validate(), 300)
}

async function validate() {
  if (disposed) return
  const generation = ++validationGeneration
  const spec = current.value
  if (!spec) {
    violations.value = []
    return
  }
  try {
    const result = await validateImagePipelines(spec.id, props.params ?? {})
    if (generation !== validationGeneration) return
    violations.value = result.violations ?? []
    validationFailed.value = false
  } catch (error) {
    if (generation !== validationGeneration) return
    // 一次打不通的检查不是判决。面板绝不能因一次网络抖动拦住保存，况且保存路径本来
    // 还会再问后端一遍。
    violations.value = []
    validationFailed.value = true
    console.error('[ImagePipelineSettings] validation request failed', error)
  }
}

watch(() => [props.pipelineId, props.params], scheduleValidation)

onBeforeUnmount(() => {
  disposed = true
  validationGeneration++
  if (validationTimer) clearTimeout(validationTimer)
})

/**
 * 一个字段声明的默认值。后端发 JSON，因此一个未设的键到达时是 undefined；下面的
 * 开关仍要读到一个值而非空白。
 */
function defaultValue(field: ImagePipelineField): unknown {
  return field.default ?? false
}

function paramValue(field: ImagePipelineField): unknown {
  const own = props.params?.[field.key]
  return own === undefined || own === null ? defaultValue(field) : own
}

/** 一条管线一个字段的 i18n 叠加 key。 */
function fieldKey(field: ImagePipelineField, part: 'label' | 'description'): string {
  return `imagePipeline.${current.value?.id ?? ''}.${field.key}.${part}`
}

function fieldLabel(field: ImagePipelineField): string {
  const key = fieldKey(field, 'label')
  return te(key) ? t(key) : field.label || field.key
}

function fieldDescription(field: ImagePipelineField): string {
  const key = fieldKey(field, 'description')
  return te(key) ? t(key) : field.description ?? ''
}

function pipelineName(pipeline: ImagePipelineSpec): string {
  const key = `imagePipeline.${pipeline.id}.name`
  return te(key) ? t(key) : pipeline.name
}

/** 所选管线如何工作，展示在选择项下方。 */
function pipelineDescription(pipeline: ImagePipelineSpec): string {
  const key = `imagePipeline.${pipeline.id}.description`
  return te(key) ? t(key) : pipeline.description ?? ''
}

function onPipelineChange(value: string) {
  emit('update:pipelineId', value)
  // 一条管线的参数只对那条管线有意义。切换时丢弃另一条的参数，而非带一个它会静默
  // 忽略的陈旧键过去。
  emit('update:params', {})
}

function onParamChange(key: string, value: unknown) {
  emit('update:params', { ...props.params, [key]: value })
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    pipelines.value = await fetchImagePipelines()
  } catch (error) {
    loadError.value = error instanceof Error ? error.message : String(error)
  } finally {
    loading.value = false
  }
  // 注册表到达了，当前选择现在可对照它检查；在那之前没有规则可对照。
  scheduleValidation()
}

onMounted(load)
// 一个对话框可能在它要编辑的知识库加载之前就挂载，因此选择可能在它要对照校验的
// 注册表之后才到达。
watch(() => props.pipelineId, () => {
  if (!loading.value && pipelines.value.length === 0) load()
})
</script>

<template>
  <div class="image-pipeline-settings">
    <div v-if="loading" class="image-pipeline-hint">
      {{ $t('knowledgeEditor.advanced.multimodal.imagePipelineLoading') }}
    </div>

    <div v-else-if="loadError" class="image-pipeline-hint image-pipeline-hint--error">
      {{ $t('knowledgeEditor.advanced.multimodal.imagePipelineLoadError') }}：{{ loadError }}
    </div>

    <template v-else>
      <div class="image-pipeline-row">
        <div class="image-pipeline-info">
          <label>{{ $t('knowledgeEditor.advanced.multimodal.imagePipelineLabel') }}</label>
        </div>
        <div class="image-pipeline-control">
          <t-select
            :value="pipelineId"
            :placeholder="$t('knowledgeEditor.advanced.multimodal.imagePipelinePlaceholder')"
            @change="onPipelineChange"
          >
            <t-option
              v-for="pipeline in pipelines"
              :key="pipeline.id"
              :value="pipeline.id"
              :label="pipelineName(pipeline)"
            />
          </t-select>
        </div>
      </div>
      <p v-if="current" class="image-pipeline-desc">{{ pipelineDescription(current) }}</p>

      <!-- 运行管线声明的每个字段一行。没有字段的管线在这里什么都不显示，这是「没有
           可调项」的诚实呈现，而非一个空分区。 -->
      <div v-for="field in fields" :key="field.key" class="image-pipeline-row">
        <div class="image-pipeline-info">
          <label>{{ fieldLabel(field) }}</label>
          <p v-if="fieldDescription(field)" class="desc">{{ fieldDescription(field) }}</p>
        </div>
        <div class="image-pipeline-control">
          <t-switch
            v-if="field.type === 'bool'"
            :value="Boolean(paramValue(field))"
            size="medium"
            @change="(value: boolean) => onParamChange(field.key, value)"
          />
          <t-select
            v-else-if="field.type === 'enum'"
            :value="String(paramValue(field) ?? '')"
            clearable
            @change="(value: string) => onParamChange(field.key, value)"
          >
            <t-option v-for="opt in field.options ?? []" :key="opt" :value="opt" :label="opt" />
          </t-select>
          <t-input
            v-else
            :value="String(paramValue(field) ?? '')"
            @change="(value: string) => onParamChange(field.key, value)"
          />
        </div>
      </div>

      <!-- 每条破损规则都列出而非概括：每行点名哪里错了，并经它指向的字段指出该把哪个
           控件打回去。位于面板底部，挨着用户必须按下才能摆脱它的那个按钮。 -->
      <ul v-if="invalid" class="image-pipeline-violations">
        <li
          v-for="violation in violations"
          :key="`${violation.message_key}:${violation.field ?? ''}`"
          class="image-pipeline-desc image-pipeline-desc--error"
        >
          {{ $t(violation.message_key) || violation.message_key }}
        </li>
      </ul>
      <p v-else-if="validationFailed" class="image-pipeline-desc">
        {{ $t('knowledgeEditor.advanced.multimodal.imagePipelineValidateError') }}
      </p>
    </template>
  </div>
</template>

<style scoped lang="less">
.image-pipeline-settings {
  display: flex;
  flex-direction: column;
  gap: 12px;

  // 上方的分区描述到此结束；更大的间隙标出控件从哪里开始，与其它设置行的节奏一致。
  margin-top: 4px;

  .image-pipeline-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
  }

  .image-pipeline-info {
    flex: 1 1 auto;
    min-width: 0;

    label {
      font-weight: 500;
    }

    .desc {
      margin: 4px 0 0;
      opacity: 0.75;
      font-size: var(--app-text-sm);
      line-height: 1.5;
    }
  }

  .image-pipeline-control {
    flex: 0 0 200px;
    text-align: right;
  }

  .image-pipeline-desc {
    margin: -4px 0 0;
    opacity: 0.75;
    font-size: var(--app-text-sm);
    line-height: 1.5;

    &--error {
      margin: 0;
      opacity: 1;
      color: var(--error-color, #d54941);
    }
  }

  .image-pipeline-hint {
    opacity: 0.75;
    font-size: var(--app-text-md);

    &--error {
      color: var(--error-color, #d54941);
    }
  }

  // 每条破损规则一行，保持紧凑，让多条仍读作按钮的脚注而非第二张表单。
  .image-pipeline-violations {
    margin: 0;
    padding-left: 1em;
    list-style: disc;
  }
}
</style>
