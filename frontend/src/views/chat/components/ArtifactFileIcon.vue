<template>
  <span class="artifact-file-icon" :class="`is-${type}`">
    <t-icon :name="iconName" :size="size" />
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import {
  classifyArtifactFile,
  artifactFileIcon,
  type ArtifactFileType,
} from '@/utils/artifactFileIcon';

const props = withDefaults(
  defineProps<{
    fileName: string;
    size?: string;
  }>(),
  { size: '20px' },
);

const type = computed<ArtifactFileType>(() => classifyArtifactFile(props.fileName));
const iconName = computed(() => artifactFileIcon(type.value));
</script>

<style scoped lang="less">
.artifact-file-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  flex-shrink: 0;
}

.is-document { color: #2b6ef5; background: rgba(43, 110, 245, 0.1); }
.is-spreadsheet { color: #07c05f; background: rgba(7, 192, 95, 0.1); }
.is-presentation { color: #ff7d00; background: rgba(255, 125, 0, 0.1); }
.is-image { color: #c139ff; background: rgba(193, 57, 255, 0.1); }
.is-audio { color: #ff4d6a; background: rgba(255, 77, 106, 0.1); }
.is-video { color: #ff2b2b; background: rgba(255, 43, 43, 0.1); }
.is-archive { color: #ff8f1f; background: rgba(255, 143, 31, 0.1); }
.is-code { color: #36cfc9; background: rgba(54, 207, 201, 0.1); }
.is-data { color: #864cff; background: rgba(134, 76, 255, 0.1); }
</style>
