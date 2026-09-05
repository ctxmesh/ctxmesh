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
  /** The selected namespace ("" = all namespaces the caller can see). A READ FILTER only. */
  namespace: string;
  /**
   * workingNamespace — the CONCRETE namespace a write would land in, and the only namespace
   * a capability probe may use. Never "".
   *
   * `namespace` overloads one string with two questions: "what should the list show?" and
   * "where do I write?". Feeding "" to a SelfSubjectAccessReview asks "may I do this in EVERY
   * namespace at once", which only a ClusterRoleBinding can satisfy — so a caller granted
   * per-namespace (the binding shape ADR 0136 mandates) was told they could do nothing, and
   * the console hid controls they in fact had.
   *
   * Empty only when the caller has no namespace at all, which is a first-class state the
   * shell must render rather than silently treat as "denied everywhere".
   */
  workingNamespace: string;
  /** Select a namespace (or "" for all). Re-scopes the list + capabilities. */
  setNamespace: (ns: string) => void;
  /** The picker's options + their honest load state. */
  list: NamespaceListState;
  /** Re-fetch the namespace list (the picker's retry affordance). */
  reload: () => void;
}

const NamespaceContext = React.createContext<NamespaceContextValue | null>(null);

const NS_KEY = "ctxmesh.namespace";

export function NamespaceProvider({ children }: { children: React.ReactNode }) {
  // Persisted, because on a cluster where the caller cannot list namespaces the value is typed
  // by hand — losing it on every reload would make the console unusable for exactly the
  // callers the per-namespace binding model produces.
  const [namespace, setNamespaceState] = React.useState(
    () => localStorage.getItem(NS_KEY) ?? "",
  );
  const setNamespace = React.useCallback((ns: string) => {
    setNamespaceState(ns);
    if (ns) localStorage.setItem(NS_KEY, ns);
    else localStorage.removeItem(NS_KEY);
  }, []);
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

  // The concrete namespace writes land in. An explicit selection wins; otherwise the first
  // namespace the caller can actually use. Resolving to a hardcoded "default" would be wrong
  // in the same way the empty probe was — asserting a namespace the caller may not hold.
  const workingNamespace = React.useMemo(() => {
    if (namespace !== "") return namespace;
    if (list.kind === "ready" && list.namespaces.length > 0) return list.namespaces[0].name;
    return "";
  }, [namespace, list]);

  const value = React.useMemo<NamespaceContextValue>(
    () => ({ namespace, workingNamespace, setNamespace, list, reload: () => reload() }),
    [namespace, workingNamespace, list, reload],
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
  workingNamespace: "",
  setNamespace: () => {},
  list: { kind: "ready", namespaces: [] },
  reload: () => {},
};
