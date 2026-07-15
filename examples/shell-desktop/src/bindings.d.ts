interface PortalDesktopBootstrap {
  execToken: string;
}

interface PortalDesktopBindings {
  portalBootstrap(): Promise<PortalDesktopBootstrap>;
}

// The desktop webview injects the `bindings` bridge in BOTH desktop modes as
// callable proxies — presence does not imply a live server-side handler. Under
// `deno desktop --hmr` the server has no window bind, so `portalBootstrap()`
// rejects with "No binding for 'portalBootstrap'"; the renderer asks the
// loopback dev-exec-token endpoint first and awaits this binding only after a
// packaged-mode 404 (see acquireExecToken in routes/shell.tsx).
declare const bindings: PortalDesktopBindings | undefined;
