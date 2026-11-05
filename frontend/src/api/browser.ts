import axios from 'axios';

// --- Types ---

export interface BrowserDevice {
  id: string;
  tenant_id: number;
  user_id: string;
  device_name: string;
  browser_name: string;
  browser_version: string;
  os_name: string;
  last_seen_at?: number;
  created_at: number;
}

export interface BrowserPairingInfo {
  token: string;
  expires_at: number;
}

export interface BrowserListResponse {
  success: boolean;
  data: BrowserDevice[];
  enabled: boolean;
}

export interface BrowserPairingResponse {
  success: boolean;
  data: BrowserPairingInfo;
}

// --- API methods ---

/**
 * List all paired browser devices for the current user.
 */
export async function listBrowserDevices(): Promise<BrowserListResponse> {
  const resp = await axios.get('/api/v1/browser/devices');
  return resp.data;
}

/**
 * Generate a new browser pairing token.
 */
export async function createBrowserPairing(): Promise<BrowserPairingResponse> {
  const resp = await axios.post('/api/v1/browser/pairing');
  return resp.data;
}

/**
 * Remove (un-pair) a browser device.
 */
export async function deleteBrowserDevice(deviceId: string): Promise<void> {
  await axios.delete(`/api/v1/browser/devices/${deviceId}`);
}
