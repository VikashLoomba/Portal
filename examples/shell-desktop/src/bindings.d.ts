interface PortalDesktopBootstrap {
  execToken: string;
}

interface PortalDesktopBindings {
  portalBootstrap(): Promise<PortalDesktopBootstrap>;
}

// The desktop webview injects the `bindings` bridge in BOTH desktop modes as
// callable proxies — presence does not imply a live server-side handler. Under
// `deno desktop --hmr` the server has no window bind, so `portalBootstrap()`
// never resolves; the renderer must deadline-race it and fall back to the
// loopback dev-exec-token endpoint (see acquireExecToken in routes/shell.tsx).
declare const bindings: PortalDesktopBindings | undefined;
