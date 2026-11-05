<template>
  <div class="browser-settings">
    <div class="settings-header">
      <h3 class="settings-title">{{ t('localBrowser.settings.title') }}</h3>
      <p class="settings-desc">{{ t('localBrowser.settings.description') }}</p>
    </div>

    <!-- Feature disabled -->
    <div v-if="!enabled && !loading" class="disabled-notice">
      <t-icon name="info-circle" size="20px" />
      <span>{{ t('localBrowser.settings.disabled') }}</span>
    </div>

    <template v-else>
      <!-- Pairing section -->
      <div class="pairing-section">
        <t-button
          theme="primary"
          :loading="pairingLoading"
          @click="handleGeneratePairing"
        >
          <template #icon><t-icon name="link" /></template>
          {{ t('localBrowser.settings.generatePairing') }}
        </t-button>

        <div v-if="pairingInfo" class="pairing-token-box">
          <div class="pairing-hint">{{ t('localBrowser.settings.pairingHint') }}</div>
          <div class="token-row">
            <span class="token-label">{{ t('localBrowser.settings.pairingToken') }}:</span>
            <code class="token-value">{{ pairingInfo.token }}</code>
            <t-button
              size="small"
              variant="outline"
              @click="handleCopyToken"
            >
              {{ copied ? t('localBrowser.settings.copied') : t('localBrowser.settings.copy') }}
            </t-button>
          </div>
          <div class="token-expiry">
            <t-icon name="time" size="14px" />
            <span>
              {{ t('localBrowser.settings.expiresIn', { minutes: pairingExpiryMinutes }) }}
            </span>
          </div>
        </div>
      </div>

      <!-- Connected devices -->
      <div class="devices-section">
        <h4 class="devices-title">{{ t('localBrowser.settings.connectedDevices') }}</h4>

        <t-loading v-if="loading" size="small">
          <div style="height: 80px" />
        </t-loading>

        <div v-else-if="devices.length === 0" class="no-devices">
          <t-icon name="desktop" size="32px" />
          <span>{{ t('localBrowser.settings.noDevices') }}</span>
        </div>

        <div v-else class="device-list">
          <div
            v-for="device in devices"
            :key="device.id"
            class="device-item"
          >
            <div class="device-icon">
              <t-icon name="laptop" size="24px" />
            </div>
            <div class="device-info">
              <div class="device-name">
                {{ device.device_name || t('localBrowser.settings.unknownDevice') }}
              </div>
              <div class="device-meta">
                <span v-if="device.browser_name">{{ device.browser_name }}</span>
                <span v-if="device.browser_version">{{ device.browser_version }}</span>
                <span v-if="device.os_name">· {{ device.os_name }}</span>
              </div>
              <div class="device-last-seen" v-if="device.last_seen_at">
                {{ formatLastSeenText(device.last_seen_at) }}
              </div>
            </div>
            <div class="device-actions">
              <t-popconfirm
                :content="t('localBrowser.settings.removeConfirmDesc')"
                :confirmBtn="t('localBrowser.settings.remove')"
                theme="danger"
                @confirm="handleRemoveDevice(device.id)"
              >
                <t-button size="small" variant="text" theme="danger">
                  {{ t('localBrowser.settings.remove') }}
                </t-button>
              </t-popconfirm>
            </div>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue';
import { useI18n } from 'vue-i18n';
import { MessagePlugin } from 'tdesign-vue-next';
import { useBrowserConnectionStore } from '@/stores/browserConnection';

const { t } = useI18n();
const store = useBrowserConnectionStore();

const devices = computed(() => store.devices);
const loading = computed(() => store.loading);
const enabled = computed(() => store.enabled);
const pairingInfo = computed(() => store.pairingInfo);
const pairingLoading = computed(() => store.pairingLoading);
const copied = ref(false);

const pairingExpiryMinutes = computed(() => {
  if (!pairingInfo.value) return 0;
  const now = Math.floor(Date.now() / 1000);
  return Math.max(0, Math.ceil((pairingInfo.value.expires_at - now) / 60));
});

onMounted(() => {
  void store.fetchDevices();
});

async function handleGeneratePairing() {
  const result = await store.generatePairingToken();
  if (!result) {
    MessagePlugin.error(t('localBrowser.settings.pairingFailed'));
  }
}

async function handleCopyToken() {
  if (!pairingInfo.value) return;
  try {
    await navigator.clipboard.writeText(pairingInfo.value.token);
    copied.value = true;
    setTimeout(() => { copied.value = false; }, 2000);
  } catch {
    MessagePlugin.error(t('localBrowser.settings.copyFailed'));
  }
}

async function handleRemoveDevice(deviceId: string) {
  const success = await store.removeDevice(deviceId);
  if (success) {
    MessagePlugin.success(t('localBrowser.settings.removeSuccess'));
  } else {
    MessagePlugin.error(t('localBrowser.settings.removeFailed'));
  }
}

function formatLastSeenText(timestamp: number): string {
  const now = Math.floor(Date.now() / 1000);
  const diff = now - timestamp;
  if (diff < 60) {
    return t('localBrowser.settings.justNow');
  }
  if (diff < 3600) {
    const minutes = Math.floor(diff / 60);
    return t('localBrowser.settings.minutesAgo', { count: minutes });
  }
  if (diff < 86400) {
    const hours = Math.floor(diff / 3600);
    return t('localBrowser.settings.hoursAgo', { count: hours });
  }
  const days = Math.floor(diff / 86400);
  return t('localBrowser.settings.daysAgo', { count: days });
}
</script>

<style scoped lang="less">
.browser-settings {
  padding: 24px;
}

.settings-header {
  margin-bottom: 24px;
}

.settings-title {
  font-size: 18px;
  font-weight: 600;
  margin: 0 0 8px 0;
  color: var(--td-text-color-primary);
}

.settings-desc {
  font-size: 13px;
  color: var(--td-text-color-secondary);
  margin: 0;
}

.disabled-notice {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 16px;
  background: var(--td-bg-color-component-disabled);
  border-radius: 6px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.pairing-section {
  margin-bottom: 32px;
}

.pairing-token-box {
  margin-top: 16px;
  padding: 16px;
  background: var(--td-bg-color-secondarycontainer);
  border-radius: 8px;
  border: 1px solid var(--td-component-border);
}

.pairing-hint {
  font-size: 13px;
  color: var(--td-text-color-secondary);
  margin-bottom: 12px;
}

.token-row {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 8px;
}

.token-label {
  font-size: 13px;
  color: var(--td-text-color-primary);
  font-weight: 500;
}

.token-value {
  flex: 1;
  padding: 6px 12px;
  background: var(--td-bg-color-container);
  border-radius: 4px;
  font-family: monospace;
  font-size: 13px;
  color: var(--td-brand-color);
  word-break: break-all;
}

.token-expiry {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.devices-section {
  margin-top: 24px;
}

.devices-title {
  font-size: 15px;
  font-weight: 600;
  margin: 0 0 16px 0;
  color: var(--td-text-color-primary);
}

.no-devices {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 40px 20px;
  gap: 12px;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  background: var(--td-bg-color-secondarycontainer);
  border-radius: 8px;
}

.device-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.device-item {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 14px 16px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-border);
  border-radius: 8px;
  transition: border-color 0.2s;

  &:hover {
    border-color: var(--td-brand-color-light);
  }
}

.device-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  background: var(--td-brand-color-light);
  color: var(--td-brand-color);
  border-radius: 8px;
  flex-shrink: 0;
}

.device-info {
  flex: 1;
  min-width: 0;
}

.device-name {
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  margin-bottom: 4px;
}

.device-meta {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  margin-bottom: 2px;
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.device-last-seen {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.device-actions {
  flex-shrink: 0;
}
</style>
