import * as React from "react";

import { api } from "@/lib/api";
import { useNamespace } from "@/lib/namespace";

// capabilities.tsx — the RBAC-aware chrome's brain (ui-foundation §3, ADR 0012).
// It probes GET /api/capabilities for the CURRENT namespace and exposes:
//   • useCapabilities() — the raw allow-map + load state + a reprobe() seam.
//   • <Can resource verb> — render children only when the caller is allowed.
//   • useCan(resource, verb) — the boolean form for gating buttons.
//
// DISPLAY-ONLY (ADR 0011): a client-side "can't" is NEVER security — the API
// server still enforces. So the honest-failure rules matter more than the happy
// path (all three are baked-in reviewer carry-forwards):
//
//   1. PROBE FAILURE (500/network) ≠ "denied". When the probe errors we hold an
//      UNKNOWN map and default `can()` to TRUE (affordances stay visible) and
//      raise a non-blocking banner — NOT a silently all-disabled console. The
//      user still gets an honest 403 from the API if they lack the right.
//   2. FLAT map: `allowed[resource][verb]`. A present `false` is a real deny; a
//      MISSING entry (probe never answered it) is unknown → treated as visible.
//   3. 403-DESPITE-A-"YES" surprise: a surface whose gated action nonetheless
//      412/403s calls reprobe() → we re-fetch the map + raise the banner (the
//      cached "yes" was stale). This re-probe path is NEW in m13.5.
//
// CACHING: the result is cached per (namespace) for the session — a lazy,
// per-namespace probe (switching namespaces fetches once, then serves cache).

// Cap is one namespace's probe outcome. `unknown` covers "not fetched yet" and
// "probe failed" — both mean: don't hide affordances, show the banner if failed.
export type CapState =
  | { kind: "loading" }
  | { kind: "ready"; allowed: Record<string, Record<string, boolean>> }
  | { kind: "error"; message: string };

export interface CapabilitiesContextValue {
  /** The current namespace's probe state. */
  state: CapState;
  /**
   * can(resource, verb) — is the caller allowed? A definite server `false` →
   * false; anything unknown (loading / probe-failed / unprobed cell) → TRUE
   * (fail-OPEN for DISPLAY, never fail-hidden: the API still enforces). Editors
   * see their affordances; a probe hiccup never blanks a working console.
   */
  can: (resource: string, verb: string) => boolean;
  /** True while the current namespace's probe is in flight. */
  loading: boolean;
  /**
   * True when the probe FAILED (500/network) — the chrome shows the honest
   * "couldn't determine your permissions — actions may be hidden or shown
   * optimistically" banner. Distinct from a successful all-false result.
   */
  probeError: string | null;
  /**
   * reprobe() — re-fetch the current namespace's capabilities (drops the cache
   * for it). Called on a 403-despite-a-"yes" surprise so the stale map is
   * corrected; also raises the banner until it resolves.
   */
  reprobe: () => void;
}

const CapabilitiesContext =
  React.createContext<CapabilitiesContextValue | null>(null);

export function CapabilitiesProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  const { namespace } = useNamespace();
  const [state, setState] = React.useState<CapState>({ kind: "loading" });
  // Per-namespace session cache of the last successful map. Keyed by namespace
  // ("" = all). A cache HIT serves instantly; a miss (or a forced reprobe)
  // fetches. Kept in a ref so it survives re-renders without re-triggering them.
  const cacheRef = React.useRef<Map<string, Record<string, Record<string, boolean>>>>(
    new Map(),
  );

  const fetchCaps = React.useCallback(
    (ns: string, force: boolean, signal?: AbortSignal) => {
      const cached = cacheRef.current.get(ns);
      if (cached && !force) {
        setState({ kind: "ready", allowed: cached });
        return;
      }
      setState({ kind: "loading" });
      api
        .capabilities(ns, signal)
        .then((res) => {
          if (signal?.aborted) return;
          cacheRef.current.set(ns, res.allowed);
          setState({ kind: "ready", allowed: res.allowed });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          // PROBE FAILURE — NOT "denied". Keep the console usable (can() stays
          // optimistic) and raise the honest banner via the error state.
          setState({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
          });
        });
    },
    [],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    fetchCaps(namespace, false, controller.signal);
    return () => controller.abort();
  }, [namespace, fetchCaps]);

  const reprobe = React.useCallback(() => {
    // Drop the cached (possibly stale) map for this namespace and re-fetch.
    cacheRef.current.delete(namespace);
    fetchCaps(namespace, true);
  }, [namespace, fetchCaps]);

  const value = React.useMemo<CapabilitiesContextValue>(() => {
    const can = (resource: string, verb: string): boolean => {
      if (state.kind !== "ready") return true; // loading / failed → optimistic.
      const verbs = state.allowed[resource];
      if (!verbs || !(verb in verbs)) return true; // unprobed cell → optimistic.
      return verbs[verb];
    };
    return {
      state,
      can,
      loading: state.kind === "loading",
      probeError: state.kind === "error" ? state.message : null,
      reprobe,
    };
  }, [state, reprobe]);

  return (
    <CapabilitiesContext.Provider value={value}>
      {children}
    </CapabilitiesContext.Provider>
  );
}

// useCapabilities reads the RBAC chrome context. Outside a provider it returns a
// permissive fallback (everything allowed, no probe) so a bare surface — e.g. a
// unit test rendering a page directly — is never accidentally locked down.
export function useCapabilities(): CapabilitiesContextValue {
  const ctx = React.useContext(CapabilitiesContext);
  return ctx ?? FALLBACK;
}

const FALLBACK: CapabilitiesContextValue = {
  state: { kind: "ready", allowed: {} },
  can: () => true,
  loading: false,
  probeError: null,
  reprobe: () => {},
};

// useCan is the boolean gate for a single (resource, verb) — the ergonomic form
// for disabling/hiding a button. Same optimistic-on-unknown semantics as can().
export function useCan(resource: string, verb: string): boolean {
  return useCapabilities().can(resource, verb);
}

// Can conditionally renders `children` when the caller is allowed to <verb> the
// <resource>. When denied it renders `fallback` (default: nothing) — the write
// affordance simply isn't there for a viewer. DISPLAY-ONLY: the API still
// enforces, so a Can-hidden action is UX, not a security boundary.
export function Can({
  resource,
  verb,
  children,
  fallback = null,
}: {
  resource: string;
  verb: string;
  children: React.ReactNode;
  fallback?: React.ReactNode;
}): React.ReactElement | null {
  const allowed = useCan(resource, verb);
  return <>{allowed ? children : fallback}</>;
}
