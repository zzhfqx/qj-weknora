import { defineStore } from 'pinia';
import { ref, computed } from 'vue';
import {
  listBrowserDevices,
  createBrowserPairing,
  deleteBrowserDevice,
  type BrowserDevice,
  type BrowserPairingInfo,
} from '@/api/browser';

export const useBrowserConnectionStore = defineStore('browser-connection', () => {
  // --- state ---
  const devices = ref<BrowserDevice[]>([]);
  const loading = ref(false);
  const enabled = ref(false);
  const pairingInfo = ref<BrowserPairingInfo | null>(null);
  const pairingLoading = ref(false);

  // --- getters ---
  const hasActiveDevice = computed(() => devices.value.length > 0);

  const deviceCount = computed(() => devices.value.length);

  // --- actions ---

  /**
   * Fetch the list of paired devices and feature availability.
   */
  async function fetchDevices() {
    loading.value = true;
    try {
      const resp = await listBrowserDevices();
      devices.value = resp.data || [];
      enabled.value = resp.enabled || false;
    } catch (err) {
      console.error('[browser] Failed to fetch devices:', err);
      devices.value = [];
      enabled.value = false;
    } finally {
      loading.value = false;
    }
  }

  /**
   * Generate a new pairing token.
   */
  async function generatePairingToken(): Promise<BrowserPairingInfo | null> {
    pairingLoading.value = true;
    try {
      const resp = await createBrowserPairing();
      pairingInfo.value = resp.data;
      return resp.data;
    } catch (err) {
      console.error('[browser] Failed to create pairing:', err);
      pairingInfo.value = null;
      return null;
    } finally {
      pairingLoading.value = false;
    }
  }

  /**
   * Remove a paired device.
   */
  async function removeDevice(deviceId: string): Promise<boolean> {
    try {
      await deleteBrowserDevice(deviceId);
      // Remove from local list
      devices.value = devices.value.filter((d) => d.id !== deviceId);
      return true;
    } catch (err) {
      console.error('[browser] Failed to remove device:', err);
      return false;
    }
  }

  /**
   * Clear the pairing info (e.g. after it expires or is consumed).
   */
  function clearPairing() {
    pairingInfo.value = null;
  }

  /**
   * Format a last_seen_at timestamp into a human-readable string.
   */
  function formatLastSeen(lastSeenAt?: number): string {
    if (!lastSeenAt) return '';
    const now = Math.floor(Date.now() / 1000);
    const diff = now - lastSeenAt;
    if (diff < 60) return 'justNow';
    if (diff < 3600) return Math.floor(diff / 60) + 'min';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h';
    return Math.floor(diff / 86400) + 'd';
  }

  return {
    // state
    devices,
    loading,
    enabled,
    pairingInfo,
    pairingLoading,
    // getters
    hasActiveDevice,
    deviceCount,
    // actions
    fetchDevices,
    generatePairingToken,
    removeDevice,
    clearPairing,
    formatLastSeen,
  };
});
