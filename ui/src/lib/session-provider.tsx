import * as React from "react";
import { Navigate, useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";

import { setSessionExpiredHandler, setTokenProvider } from "@/lib/api";
import { getToken, forceClear, restore } from "@/lib/session";
import { useSession } from "@/lib/use-session";

// SessionProvider wires the session store (lib/session.ts) into api.ts and boots
// the app: on mount it (1) registers the token provider so every /api/* request
// carries the caller's bearer token, (2) registers the mid-session-401 handler
// so an expired live token clears the session, and (3) calls restore() to
// re-validate any token persisted in sessionStorage (refresh survival, ADR
// 0012). Until restore() resolves it renders a boot splash so the router never
// flashes login → console.
//
// The 401 handler here does NOT navigate directly (it can fire outside React's
// render, from any pending fetch). It only clears the session; the auth guards
// (RequireAuth) then redirect on the next render — the single, declarative
// redirect path, which also preserves the return location.

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [booted, setBooted] = React.useState(false);

  React.useEffect(() => {
    // Register the api.ts seams FIRST so restore()'s own whoami call (and any
    // request racing boot) carries the token / is handled correctly.
    setTokenProvider(getToken);
    // A mid-session 401 clears the session without a network round-trip (the
    // token is already known-dead). restore()/login() pass {login:true} so their
    // OWN validation 401s never reach this handler.
    setSessionExpiredHandler(forceClear);

    let alive = true;
    restore().finally(() => {
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
  return <>{children}</>;
}

// RequireAuth guards the console: an unauthenticated visit to any console route
// redirects to /login, preserving the attempted location in router state so the
// login page can return the user there after a successful login (ADR 0012 —
// "preserving the return path"). `replace` so login isn't a back-button trap.
export function RequireAuth({ children }: { children: React.ReactNode }) {
  const session = useSession();
  const location = useLocation();
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
