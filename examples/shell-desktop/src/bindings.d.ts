interface PortalDesktopBootstrap {
  execToken: string;
}

interface PortalDesktopBindings {
  portalBootstrap(): Promise<PortalDesktopBootstrap>;
}

// The desktop runtime injects `bindings` only in packaged mode, where
// `createWindow` registers the `portalBootstrap` channel. Under the framework
// dev server there is no window bind, so the global is absent and the renderer
// falls back to the loopback dev-exec-token endpoint.
declare const bindings: PortalDesktopBindings | undefined;
