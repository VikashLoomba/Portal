import type { AnyRoute } from "@tanstack/react-router";

declare module "@tanstack/router-core" {
  interface UpdatableRouteOptionsExtensions {
    id?: string;
    path?: string;
    getParentRoute?: () => AnyRoute;
  }
}
