import * as React from "react";
import { useNavigate } from "react-router-dom";
import { AlertTriangle, Boxes, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { completeLogin } from "@/lib/oidc";
import { login } from "@/lib/session";

// AuthCallbackPage — the OIDC redirect target (/auth/callback, ADR 0020). Dex sends
// the browser here with ?code&state after login. completeLogin verifies the state
// (anti-CSRF) and exchanges the code + PKCE verifier for an ID token; that token then
// flows through the SAME session seam as a pasted token (login() → whoami validation →
// session), so nothing downstream changes. On success we route to the returnTo the
// flow captured; on any failure we show an honest error with a way back to /login —
// never a silent dead end. Public route (no session yet), like /login.
export function AuthCallbackPage() {
  const navigate = useNavigate();
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const { idToken, returnTo } = await completeLogin(
          new URLSearchParams(window.location.search),
        );
        const result = await login(idToken);
        if (!alive) return;
        if (result.ok) {
          navigate(returnTo || "/", { replace: true });
        } else {
          setError(
            `Signed in with SSO, but the cluster rejected the token${
              result.status ? ` (${result.status})` : ""
            }. Your identity may not be bound to any RBAC role yet.`,
          );
        }
      } catch (err) {
        if (alive) {
          setError(
            err instanceof Error ? err.message : "Single sign-on failed.",
          );
        }
      }
    })();
    return () => {
      alive = false;
    };
  }, [navigate]);

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-8">
      <div className="mx-auto w-full max-w-md rounded-xl border bg-card p-8 text-center shadow-elevated">
        <div className="mb-3 flex justify-center">
          <div className="flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-sm">
            <Boxes className="h-6 w-6" />
          </div>
        </div>
        {error ? (
          <div className="space-y-4">
            <div className="flex flex-col items-center gap-2">
              <AlertTriangle className="h-5 w-5 text-destructive" />
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            </div>
            <Button
              className="w-full"
              onClick={() => navigate("/login", { replace: true })}
            >
              Back to sign in
            </Button>
          </div>
        ) : (
          <div
            role="status"
            aria-label="Completing sign-in"
            className="flex flex-col items-center gap-2 text-sm text-muted-foreground"
          >
            <Loader2 className="h-6 w-6 animate-spin" />
            Completing single sign-on…
          </div>
        )}
      </div>
    </div>
  );
}
