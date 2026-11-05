// Type declarations for @novnc/novnc library.
// The actual library is loaded dynamically so we declare the minimal
// surface we use from it.

declare module '@novnc/novnc/lib/rfb' {
  export default class RFB {
    constructor(target: HTMLElement, url: string, options?: RFBOptions);

    // Connection control
    connect(url: string): void;
    disconnect(): void;

    // State
    readonly connected: boolean;
    readonly desktopName: string;
    readonly viewportScale: number;

    // Settings
    scaleViewport: boolean;
    resizeSession: boolean;
    showDotCursor: boolean;
    background: string;
    qualityLevel: number;
    compressionLevel: number;

    // Events
    addEventListener(type: string, listener: (event: any) => void): void;
    removeEventListener(type: string, listener: (event: any) => void): void;

    // Clipboard
    clipboardPasteFrom(text: string): void;

    // Keyboard / mouse control
    focus(): void;
    blur(): void;
  }

  export interface RFBOptions {
    shared?: boolean;
    credentials?: { username?: string; password?: string };
    repeaterID?: string;
    wsProtocols?: string[];
  }
}
