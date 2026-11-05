<template>
  <div class="browser-tool-result">
    <!-- URL / Title header -->
    <div v-if="url || title" class="browser-header">
      <div class="browser-title" v-if="title">{{ title }}</div>
      <div class="browser-url" v-if="url">
        <t-icon name=" globe" size="12px" />
        <span class="url-text">{{ url }}</span>
      </div>
    </div>

    <!-- Screenshot -->
    <div v-if="screenshot" class="browser-screenshot">
      <img :src="screenshot" alt="browser screenshot" class="screenshot-img" />
    </div>

    <!-- Status badge -->
    <div class="browser-status">
      <t-tag :theme="statusTheme" size="small">
        {{ statusText }}
      </t-tag>
      <span v-if="humanHelp" class="human-help-note">
        <t-icon name="user" size="12px" />
        {{ t('localBrowser.tool.humanHelp') }}
      </span>
    </div>

    <!-- Content preview -->
    <div v-if="content" class="browser-content">
      <div class="content-label">{{ t('localBrowser.tool.pageContent') }}</div>
      <div class="content-text">{{ truncatedContent }}</div>
      <t-button
        v-if="content.length > 500"
        size="small"
        variant="text"
        @click="expanded = !expanded"
      >
        {{ expanded ? t('localBrowser.tool.collapse') : t('localBrowser.tool.expand') }}
      </t-button>
    </div>

    <!-- Error -->
    <div v-if="error" class="browser-error">
      <t-icon name="error-circle" size="14px" />
      <span>{{ error }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue';
import { useI18n } from 'vue-i18n';

const props = defineProps<{
  url?: string;
  title?: string;
  screenshot?: string;
  content?: string;
  status?: string;
  error?: string;
  humanHelp?: boolean;
}>();

const { t } = useI18n();
const expanded = ref(false);

const statusTheme = computed(() => {
  switch (props.status) {
    case 'completed':
      return 'success';
    case 'failed':
      return 'danger';
    case 'paused':
      return 'warning';
    case 'running':
      return 'primary';
    default:
      return 'default';
  }
});

const statusText = computed(() => {
  switch (props.status) {
    case 'completed':
      return t('localBrowser.tool.succeeded');
    case 'failed':
      return t('localBrowser.tool.failed');
    case 'paused':
      return t('localBrowser.tool.paused');
    case 'running':
      return t('localBrowser.tool.running');
    default:
      return props.status || '';
  }
});

const truncatedContent = computed(() => {
  if (!props.content) return '';
  if (expanded.value) return props.content;
  return props.content.slice(0, 500) + (props.content.length > 500 ? '...' : '');
});
</script>

<style scoped lang="less">
.browser-tool-result {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 8px 0;
}

.browser-header {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.browser-title {
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  line-height: 1.4;
}

.browser-url {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--td-brand-color);
}

.url-text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 100%;
}

.browser-screenshot {
  border-radius: 6px;
  overflow: hidden;
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-component);
}

.screenshot-img {
  width: 100%;
  height: auto;
  display: block;
  max-height: 300px;
  object-fit: contain;
}

.browser-status {
  display: flex;
  align-items: center;
  gap: 8px;
}

.human-help-note {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--td-warning-color);
}

.browser-content {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.content-label {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  font-weight: 500;
}

.content-text {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 200px;
  overflow-y: auto;
}

.browser-error {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  padding: 8px 12px;
  background: var(--td-error-color-1);
  border-radius: 4px;
  font-size: 12px;
  color: var(--td-error-color-7);
}
</style>
