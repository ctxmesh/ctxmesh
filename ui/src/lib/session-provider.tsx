import * as React from "react";
import { Navigate, useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";

import { api, setSessionExpiredHandler, setTokenProvider } from "@/lib/api";
import { getToken, forceClear, restore } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { DevModeContext, useDevMode } from "@/lib/dev-mode";

// SessionProvider wires the session store (lib/session.ts) into api.ts and boots
// the app: on mount it (1) registers the token provider so every /api/* request
// carries the caller's bearer token, (2) registers the mid-session-401 handler
// so an expired live token clears the session, (3) calls restore() to re-validate
// any token persisted in sessionStorage (refresh survival, ADR 0012), and (4)
// probes GET /api/devmode (ADR 0021) so the dev-mode substrate is known before any
// guard runs. Until BOTH settle it renders a boot splash so the router never flashes
// login → console (and a dev session never bounces to /login on a transient false).
//
// The 401 handler here does NOT navigate directly (it can fire outside React's
// render, from any pending fetch). It only clears the session; the auth guards
// (RequireAuth) then redirect on the next render — the single, declarative
// redirect path, which also preserves the return location.

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [booted, setBooted] = React.useState(false);
  const [devMode, setDevMode] = React.useState(false);

  React.useEffect(() => {
    // Register the api.ts seams FIRST so restore()'s own whoami call (and any
    // request racing boot) carries the token / is handled correctly.
    setTokenProvider(getToken);
    // A mid-session 401 clears the session without a network round-trip (the
    // token is already known-dead). restore()/login() pass {login:true} so their
    // OWN validation 401s never reach this handler.
    setSessionExpiredHandler(forceClear);

    let alive = true;
    // Restore the session and probe dev mode IN PARALLEL, behind the one splash.
    // devMode failing keeps false (login wall on) — the safe default on a cluster.
    Promise.allSettled([
      restore(),
      api.devMode().then((r) => {
        if (alive) setDevMode(r.devMode);
      }),
    ]).finally(() => {
      if (alive) setBooted(true);
    });
    return () => {
      alive = false;
    };
  }, []);

  if (!booted) {
    return (
      <div
        role="status"
        aria-label="Loading session"
        className="flex min-h-screen items-center justify-center bg-background text-muted-foreground"
      >
        <Loader2 className="h-6 w-6 animate-spin" />
      </div>
    );
  }
  return (
    <DevModeContext.Provider value={devMode}>
      {children}
    </DevModeContext.Provider>
  );
}

// RequireAuth guards the console: an unauthenticated visit to any console route
// redirects to /login, preserving the attempted location in router state so the
// login page can return the user there after a successful login (ADR 0012 —
// "preserving the return path"). `replace` so login isn't a back-button trap.
export function RequireAuth({ children }: { children: React.ReactNode }) {
  const session = useSession();
  const devMode = useDevMode();
  const location = useLocation();
  // Dev mode (`agent-engine dev --ui`, ADR 0021) is a single local developer with no
  // cluster and no multi-tenant RBAC to enforce — there is no login wall. The BFF runs
  // AllowAll on loopback, so guarding here would only block a console that has no auth
  // to satisfy. Gate on the server-confirmed devMode flag, never a client toggle.
  if (devMode) {
    return <>{children}</>;
  }
  if (!session) {
    return (
      <Navigate
        to="/login"
        replace
        state={{
          from: {
            pathname: location.pathname,
            search: location.search,
            hash: location.hash,
          },
        }}
      />
    );
  }
  return <>{children}</>;
}
