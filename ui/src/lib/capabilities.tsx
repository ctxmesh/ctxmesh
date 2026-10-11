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
  | {
      kind: "ready";
      allowed: Record<string, Record<string, boolean>>;
      /** Server-computed flow → completable (see internal/bff/flows.go). */
      flows: Record<string, boolean>;
    }
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
  /**
   * canFlow(name) — may the caller complete this whole console FLOW? Answered by the
   * server from the handler that performs the writes, so the UI never assembles a
   * resource×verb conjunction again. It got that wrong twice: omitting the ModelRoute the
   * connect path writes, and gating rotation on the binding rather than the Secret.
   *
   * UNLIKE can(), this fails CLOSED while loading or after a probe failure. Optimism is
   * right for chrome and wrong for a write — offering a flow we did not verify is how a
   * partial write leaves a live credential behind with no route to use it.
   */
  canFlow: (name: string) => boolean;
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
  /**
   * canIn(namespace, resource, verb) — can() for a namespace other than the working one: the
   * namespace a resource lives in, which a detail route or an "All workspaces" row may name.
   * Same optimistic-on-unknown rule; "" means the working namespace. ensure() starts the probe.
   */
  canIn: (namespace: string, resource: string, verb: string) => boolean;
  /** ensure(namespace) — probe that namespace once (cached, deduplicated). */
  ensure: (namespace: string) => void;
  /** reprobeIn(namespace) — drop that namespace's cached answer and probe it again. */
  reprobeIn: (namespace: string) => void;
}

const CapabilitiesContext =
  React.createContext<CapabilitiesContextValue | null>(null);

export function CapabilitiesProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  // workingNamespace, NOT namespace. "" is a read filter; feeding it to an SSAR asks "may I
  // do this in every namespace at once", which only a ClusterRoleBinding satisfies — so the
  // per-namespace RoleBinding the platform mandates produced an all-denied map.
  const { workingNamespace } = useNamespace();
  const namespace = workingNamespace;
  const [state, setState] = React.useState<CapState>({ kind: "loading" });
  // Per-namespace session cache of the last successful map. Keyed by namespace
  // ("" = all). A cache HIT serves instantly; a miss (or a forced reprobe)
  // fetches. Kept in a ref so it survives re-renders without re-triggering them.
  const cacheRef = React.useRef<
    Map<
      string,
      {
        allowed: Record<string, Record<string, boolean>>;
        flows: Record<string, boolean>;
      }
    >
  >(new Map());

  const fetchCaps = React.useCallback(
    (ns: string, force: boolean, signal?: AbortSignal) => {
      const cached = cacheRef.current.get(ns);
      if (cached && !force) {
        setState({ kind: "ready", allowed: cached.allowed, flows: cached.flows });
        return;
      }
      setState({ kind: "loading" });
      api
        .capabilities(ns, signal)
        .then((res) => {
          if (signal?.aborted) return;
          cacheRef.current.set(ns, { allowed: res.allowed, flows: res.flows ?? {} });
          setState({ kind: "ready", allowed: res.allowed, flows: res.flows ?? {} });
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
    // NEVER probe with no namespace. The SSAR then asks "may I do this in EVERY namespace at
    // once", which only a ClusterRoleBinding satisfies, and the answer comes back a definite
    // all-false at HTTP 200 — measured 0/58 verbs against 57/58 for the same caller one line
    // later with ?namespace=default. Rule 2 above consumes a present `false` as a real deny, so
    // that reply was read as a verdict about the user: a caller holding full rights in their own
    // namespace was shown "You have read-only access" before the namespace list had even
    // resolved (M177). Staying in `loading` keeps can() optimistic, which is the honest state
    // for a question we have not asked yet.
    if (namespace === "") return;
    const controller = new AbortController();
    fetchCaps(namespace, false, controller.signal);
    return () => controller.abort();
  }, [namespace, fetchCaps]);

  const reprobe = React.useCallback(() => {
    // Drop the cached (possibly stale) map for this namespace and re-fetch.
    cacheRef.current.delete(namespace);
    fetchCaps(namespace, true);
  }, [namespace, fetchCaps]);

  // Other namespaces' answers, for a resource that lives outside the working namespace. Kept
  // apart from `state` so probing an item's namespace never disturbs the shell's own map; the
  // shared cacheRef means switching the shell there later is instant.
  const [others, setOthers] = React.useState<Record<string, CapState>>({});
  const inFlight = React.useRef<Set<string>>(new Set());
  const probeOther = React.useCallback((ns: string, force: boolean) => {
    if (ns === "" || inFlight.current.has(ns)) return;
    const cached = cacheRef.current.get(ns);
    if (cached && !force) {
      setOthers((m) => (m[ns]?.kind === "ready" ? m : { ...m, [ns]: { kind: "ready", ...cached } }));
      return;
    }
    inFlight.current.add(ns);
    setOthers((m) => ({ ...m, [ns]: { kind: "loading" } }));
    api
      .capabilities(ns)
      .then((res) => {
        const entry = { allowed: res.allowed, flows: res.flows ?? {} };
        cacheRef.current.set(ns, entry);
        setOthers((m) => ({ ...m, [ns]: { kind: "ready", ...entry } }));
      })
      .catch((err: unknown) => {
        setOthers((m) => ({
          ...m,
          [ns]: { kind: "error", message: err instanceof Error ? err.message : "request failed" },
        }));
      })
      .finally(() => inFlight.current.delete(ns));
  }, []);
  const ensure = React.useCallback(
    (ns: string) => {
      if (ns !== namespace) probeOther(ns, false);
    },
    [namespace, probeOther],
  );
  const reprobeIn = React.useCallback(
    (ns: string) => {
      if (ns === "" || ns === namespace) {
        reprobe();
        return;
      }
      cacheRef.current.delete(ns);
      probeOther(ns, true);
    },
    [namespace, reprobe, probeOther],
  );

  const value = React.useMemo<CapabilitiesContextValue>(() => {
    const can = (resource: string, verb: string): boolean => allows(state, resource, verb);
    const canIn = (ns: string, resource: string, verb: string): boolean =>
      ns === "" || ns === namespace
        ? can(resource, verb)
        : allows(others[ns] ?? { kind: "loading" }, resource, verb);
    // Fails CLOSED, deliberately the opposite of can(). See canFlow's doc above.
    const canFlow = (name: string): boolean =>
      state.kind === "ready" && state.flows[name] === true;
    return {
      state,
      can,
      canFlow,
      loading: state.kind === "loading",
      probeError: state.kind === "error" ? state.message : null,
      reprobe,
      canIn,
      ensure,
      reprobeIn,
    };
  }, [state, reprobe, namespace, others, ensure, reprobeIn]);

  return (
    <CapabilitiesContext.Provider value={value}>
      {children}
    </CapabilitiesContext.Provider>
  );
}

// allows reads one cell of a probe outcome: a definite false denies; anything unknown (loading,
// failed, unprobed) is optimistic, because display gating is never the security boundary.
function allows(state: CapState, resource: string, verb: string): boolean {
  if (state.kind !== "ready") return true;
  const verbs = state.allowed[resource];
  if (!verbs || !(verb in verbs)) return true;
  return verbs[verb];
}

// useCapabilities reads the RBAC chrome context. Outside a provider it returns a
// permissive fallback (everything allowed, no probe) so a bare surface — e.g. a
// unit test rendering a page directly — is never accidentally locked down.
export function useCapabilities(): CapabilitiesContextValue {
  const ctx = React.useContext(CapabilitiesContext);
  return ctx ?? FALLBACK;
}

const FALLBACK: CapabilitiesContextValue = {
  state: { kind: "ready", allowed: {}, flows: {} },
  can: () => true,
  // No provider ⇒ no flow was verified. can() stays optimistic here (it only reveals chrome),
  // but a WRITE must not be offered on the strength of a missing provider.
  canFlow: () => false,
  loading: false,
  probeError: null,
  reprobe: () => {},
  canIn: () => true,
  ensure: () => {},
  reprobeIn: () => {},
};

/**
 * useCapabilitiesIn(namespace) — can() and reprobe() for the namespace a resource lives in. A
 * write gate on an item asks about the item's namespace, not the shell's working namespace: under
 * "All workspaces", or on a detail route, the two differ. "" means the working namespace.
 */
export function useCapabilitiesIn(namespace: string): {
  can: (resource: string, verb: string) => boolean;
  reprobe: () => void;
} {
  const ctx = useCapabilities();
  const { ensure, canIn, reprobeIn } = ctx;
  React.useEffect(() => {
    ensure(namespace);
  }, [ensure, namespace]);
  return React.useMemo(
    () => ({
      can: (resource: string, verb: string) => canIn(namespace, resource, verb),
      reprobe: () => reprobeIn(namespace),
    }),
    [canIn, reprobeIn, namespace],
  );
}

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

// CanIn is Can for a resource in another namespace: a control on an "All workspaces" row or an
// item opened from one, which acts where the item lives, not where the shell is.
export function CanIn({
  namespace,
  resource,
  verb,
  children,
  fallback = null,
}: {
  namespace: string;
  resource: string;
  verb: string;
  children: React.ReactNode;
  fallback?: React.ReactNode;
}): React.ReactElement | null {
  const allowed = useCapabilitiesIn(namespace).can(resource, verb);
  return <>{allowed ? children : fallback}</>;
}
