<template>
  <div class="sandbox-desktop">
    <!-- Toolbar -->
    <div v-if="active" class="sandbox-desktop-toolbar">
      <div class="toolbar-left">
        <span class="desktop-status" :class="{ 'is-connected': isConnected }">
          <span class="status-dot" />
          {{ isConnecting ? t('chat.desktop.connecting') : isConnected ? t('chat.desktop.connected') : t('chat.desktop.disconnected') }}
        </span>
      </div>
      <div class="toolbar-right">
        <t-button
          v-if="!isConnected && !isConnecting"
          variant="text"
          size="small"
          @click="handleConnect"
        >
          <template #icon><t-icon name="play" size="14px" /></template>
          {{ t('chat.desktop.connect') }}
        </t-button>
        <t-button
          v-if="isConnected"
          variant="text"
          size="small"
          @click="handleDisconnect"
        >
          <template #icon><t-icon name="close" size="14px" /></template>
          {{ t('chat.desktop.disconnect') }}
        </t-button>
        <t-tooltip :content="t('chat.desktop.ctrlAltDel')">
          <t-button
            variant="text"
            size="small"
            :disabled="!isConnected"
            @click="sendCtrlAltDel"
          >
            <template #icon><t-icon name="keyboard" size="14px" /></template>
          </t-button>
        </t-tooltip>
      </div>
    </div>

    <!-- Desktop display area -->
    <div class="sandbox-desktop-view" ref="viewRef">
      <!-- Loading state -->
      <div v-if="isConnecting" class="desktop-loading">
        <t-loading size="medium" />
        <span class="loading-text">{{ t('chat.desktop.connecting') }}</span>
      </div>

      <!-- Error state -->
      <div v-else-if="errorMessage && !isConnected" class="desktop-error">
        <t-icon name="error-circle" size="32px" />
        <span class="error-text">{{ errorMessage }}</span>
        <t-button size="small" theme="primary" @click="handleConnect">
          {{ t('chat.desktop.retry') }}
        </t-button>
      </div>

      <!-- Empty / inactive state -->
      <div v-else-if="!active" class="desktop-empty">
        <t-icon name="desktop" size="40px" />
        <span>{{ t('chat.desktop.tip') }}</span>
      </div>

      <!-- VNC display (rendered by noVNC into this div) -->
      <div
        v-show="isConnected"
        ref="novncContainer"
        class="novnc-container"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useSandboxDesktop } from '@/composables/useSandboxDesktop';

const props = withDefaults(
  defineProps<{
    sessionId: string;
    active?: boolean;
  }>(),
  {
    active: false,
  },
);

const { t } = useI18n();

const viewRef = ref<HTMLElement | null>(null);
const novncContainer = ref<HTMLElement | null>(null);

const sessionIdRef = ref(props.sessionId);
const activeRef = ref(props.active);

watch(
  () => props.sessionId,
  (val) => { sessionIdRef.value = val; },
);

watch(
  () => props.active,
  (val) => { activeRef.value = val; },
  { immediate: true },
);

const {
  isConnecting,
  isConnected,
  errorMessage,
  connect,
  disconnect,
  sendCtrlAltDel,
} = useSandboxDesktop({
  sessionId: sessionIdRef,
  active: activeRef,
  containerRef: novncContainer,
});

function handleConnect() {
  void connect();
}

function handleDisconnect() {
  disconnect();
}

onMounted(() => {
  // noVNC renders into novncContainer ref
});
</script>

<style scoped lang="less">
.sandbox-desktop {
  height: 100%;
  display: flex;
  flex-direction: column;
  background: #1a1a1a;
  overflow: hidden;
}

.sandbox-desktop-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 12px;
  background: #252525;
  border-bottom: 1px solid #333;
  flex-shrink: 0;
}

.toolbar-left,
.toolbar-right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.desktop-status {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #888;

  &.is-connected {
    color: #07c05f;
  }
}

.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: currentColor;
}

.sandbox-desktop-view {
  flex: 1;
  min-height: 0;
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
}

.novnc-container {
  width: 100%;
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;

  :deep(canvas) {
    max-width: 100%;
    max-height: 100%;
  }
}

.desktop-loading,
.desktop-error,
.desktop-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  color: #888;
  font-size: 13px;
  padding: 24px;
  text-align: center;
}

.loading-text {
  font-size: 13px;
}

.desktop-error {
  .error-text {
    color: #ff4d6a;
    font-size: 12px;
    max-width: 280px;
  }
}

.desktop-empty {
  color: #555;
}
</style>
