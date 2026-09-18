// noVNC 1.7 ships no type declarations, and its package exports a single entry
// ("./core/rfb.js") so subpath imports are not possible. We use a handful of
// members, so a narrow local declaration beats a dependency on DefinitelyTyped.
declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    constructor(
      target: Element,
      url: string,
      options?: {
        shared?: boolean;
        credentials?: { password?: string };
      },
    );
    scaleViewport: boolean;
    resizeSession: boolean;
    background: string;
    disconnect(): void;
    focus(): void;
    sendCtrlAltDel(): void;
    sendKey(keysym: number, code: string | null, down?: boolean): void;
  }
}
