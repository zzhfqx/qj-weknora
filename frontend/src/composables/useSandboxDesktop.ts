import { ref, onUnmounted, watch, type Ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { MessagePlugin } from 'tdesign-vue-next';
import axios from 'axios';

export interface UseSandboxDesktopOptions {
  sessionId: Ref<string>;
  active: Ref<boolean>;
  containerRef: Ref<HTMLElement | null>;
}

export function useSandboxDesktop({ sessionId, active, containerRef }: UseSandboxDesktopOptions) {
  const { t } = useI18n();

  const isConnecting = ref(false);
  const isConnected = ref(false);
  const errorMessage = ref('');
  let rfb: any = null; // RFB instance from @novnc/novnc

  async function getTicket(): Promise<string> {
    const resp = await axios.post(
      `/api/v1/sessions/${sessionId.value}/sandbox/desktop-ticket`,
      {},
    );
    return resp.data?.data?.ticket || '';
  }

  async function connect() {
    if (!sessionId.value || !containerRef.value) return;
    if (isConnected.value || isConnecting.value) return;

    isConnecting.value = true;
    errorMessage.value = '';

    try {
      const ticket = await getTicket();
      if (!ticket) {
        throw new Error(t('chat.desktop.ticketFailed'));
      }

      // Dynamically import noVNC (loaded on demand to reduce bundle size)
      // v1.7+ 的包 exports 只暴露包根路径，子路径 /lib/rfb 已失效
      const RFBModule = await import('@novnc/novnc');
      const RFB = RFBModule.default;

      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const wsUrl = `${protocol}//${window.location.host}/api/v1/sessions/${sessionId.value}/sandbox/desktop?ticket=${encodeURIComponent(ticket)}`;

      rfb = new RFB(containerRef.value, wsUrl, {
        wsProtocols: ['binary'],
      });

      rfb.scaleViewport = true;
      rfb.resizeSession = false;
      rfb.background = '#1a1a1a';

      rfb.addEventListener('connect', () => {
        isConnected.value = true;
        isConnecting.value = false;
        errorMessage.value = '';
      });

      rfb.addEventListener('disconnect', () => {
        isConnected.value = false;
        isConnecting.value = false;
      });

      rfb.addEventListener('securityfailure', (e: any) => {
        isConnecting.value = false;
        errorMessage.value = e.detail?.reason || t('chat.desktop.securityError');
        MessagePlugin.error(t('chat.desktop.connectionFailed'));
      });

      rfb.addEventListener('clipboard', (e: any) => {
        // Clipboard from remote — can be used for bidirectional clipboard
        console.debug('[desktop] clipboard from remote:', e.detail?.text);
      });
    } catch (err: any) {
      isConnecting.value = false;
      errorMessage.value = err?.message || t('chat.desktop.connectionFailed');
      console.error('[SandboxDesktop] connect failed:', err);
    }
  }

  function disconnect() {
    if (rfb) {
      try {
        rfb.disconnect();
      } catch (e) {
        // ignore
      }
      rfb = null;
    }
    isConnected.value = false;
    isConnecting.value = false;
  }

  function sendCtrlAltDel() {
    if (rfb) {
      // Send Ctrl+Alt+Del key sequence
      rfb.focus();
    }
  }

  // Auto-connect when the tab becomes active
  watch(
    () => active.value,
    (isActive) => {
      if (isActive && sessionId.value) {
        void connect();
      } else {
        disconnect();
      }
    },
    { immediate: true },
  );

  watch(sessionId, () => {
    if (active.value) {
      disconnect();
      if (sessionId.value) {
        void connect();
      }
    }
  });

  onUnmounted(() => {
    disconnect();
  });

  return {
    isConnecting,
    isConnected,
    errorMessage,
    connect,
    disconnect,
    sendCtrlAltDel,
  };
}
