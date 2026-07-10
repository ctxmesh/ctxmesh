import { useSyncExternalStore } from "react";

import { getSession, subscribe, type Session } from "@/lib/session";

// useSession subscribes a component to the session store (lib/session.ts) so the
// who-am-I header, auth routing guards, and any RBAC-aware chrome re-render when
// the session changes (login / logout / expiry). It is a thin
// useSyncExternalStore binding — no new state library (ADR 0012 / task scope).
//
// getSession() returns a STABLE reference until the session actually changes
// (setSession swaps it), so useSyncExternalStore's snapshot check is safe (no
// infinite re-render). The server snapshot is null (SSR-safe, though the SPA is
// client-only).
export function useSession(): Session | null {
  return useSyncExternalStore(subscribe, getSession, () => null);
}
