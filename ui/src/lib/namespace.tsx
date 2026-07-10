import * as React from "react";

import { api, ApiError, type NamespaceSummary } from "@/lib/api";

// namespace.tsx — the shell's namespace scope (ui-foundation §3/§5). One
// selected namespace scopes BOTH the agents list AND the capability probe; the
// empty string means "all namespaces the caller can see" (cluster-wide), which
// is the honest default before the user narrows.
//
// The available namespaces come from GET /api/namespaces. Honest failure
// (baked-in reviewer carry-forward): a 403 is "you can't list namespaces", NOT
// an empty list — the picker says so and still lets the user work in "all".
// Fetched once per session (namespaces rarely change within a tab); the picker
// re-fetch is a manual affordance, not on every render.

// NamespaceListState is the load state of the picker's options. `forbidden` is a
// FIRST-CLASS outcome, distinct from an authentically empty `[]`.
export type NamespaceListState =
  | { kind: "loading" }
  | { kind: "ready"; namespaces: NamespaceSummary[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

export interface NamespaceContextValue {
  /** The selected namespace ("" = all namespaces the caller can see). */
  namespace: string;
  /** Select a namespace (or "" for all). Re-scopes the list + capabilities. */
  setNamespace: (ns: string) => void;
  /** The picker's options + their honest load state. */
  list: NamespaceListState;
  /** Re-fetch the namespace list (the picker's retry affordance). */
  reload: () => void;
}

const NamespaceContext = React.createContext<NamespaceContextValue | null>(null);

export function NamespaceProvider({ children }: { children: React.ReactNode }) {
  const [namespace, setNamespace] = React.useState("");
  const [list, setList] = React.useState<NamespaceListState>({ kind: "loading" });

  const reload = React.useCallback((signal?: AbortSignal) => {
    setList({ kind: "loading" });
    api
      .namespaces(signal)
      .then((res) => {
        if (signal?.aborted) return;
        setList({ kind: "ready", namespaces: res.namespaces });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          // Honest 403: can't LIST namespaces ≠ no namespaces exist. The user can
          // still operate in "all namespaces" (their RBAC scopes each list).
          setList({ kind: "forbidden", message: err.message });
          return;
        }
        setList({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    reload(controller.signal);
    return () => controller.abort();
  }, [reload]);

  const value = React.useMemo<NamespaceContextValue>(
    () => ({ namespace, setNamespace, list, reload: () => reload() }),
    [namespace, list, reload],
  );

  return (
    <NamespaceContext.Provider value={value}>
      {children}
    </NamespaceContext.Provider>
  );
}

// useNamespace reads the shell's namespace scope. Outside a provider it falls
// back to a stable "all namespaces" scope so a bare surface (e.g. a test that
// renders a page directly) still works.
export function useNamespace(): NamespaceContextValue {
  const ctx = React.useContext(NamespaceContext);
  if (ctx) return ctx;
  return FALLBACK;
}

const FALLBACK: NamespaceContextValue = {
  namespace: "",
  setNamespace: () => {},
  list: { kind: "ready", namespaces: [] },
  reload: () => {},
};
