interface PortalDesktopBootstrap {
  execToken: string;
}

interface PortalDesktopBindings {
  portalBootstrap(): Promise<PortalDesktopBootstrap>;
}

declare const bindings: PortalDesktopBindings;
