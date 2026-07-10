import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { ArrowRight, Boxes, KeyRound, Loader2, TerminalSquare } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { login } from "@/lib/session";
import { useSession } from "@/lib/use-session";

// LoginPage — the token login (ADR 0012), realized from the approved
// auth-shell wireframe (design/wireframes/auth-shell.tsx `LoginCard`). The user
// pastes a Kubernetes bearer token; login() validates it via GET /api/whoami
// before a session exists. A rejected token (whoami 401/non-200) renders the
// honest inline error under the field — it is a LOGIN error, not a session
// expiry, so it never triggers the 401 redirect (session.login passes
// {login:true} through api.ts).
//
// On success we route to the return path the auth guard preserved (location
// state `from`), defaulting to the console root. An already-authenticated visit
// to /login bounces straight to the console (handled here + by RedirectIfAuthed).

interface FromState {
  from?: { pathname?: string; search?: string; hash?: string };
}

function returnPath(state: unknown): string {
  const from = (state as FromState | null)?.from;
  if (from?.pathname) {
    return `${from.pathname}${from.search ?? ""}${from.hash ?? ""}`;
  }
  return "/";
}

export function LoginPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const session = useSession();
  const target = returnPath(location.state);

  const [token, setToken] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // Already signed in (e.g. hit /login with a live session) → go to the console.
  React.useEffect(() => {
    if (session) navigate(target, { replace: true });
  }, [session, target, navigate]);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    setError(null);
    setSubmitting(true);
    const result = await login(token);
    setSubmitting(false);
    if (result.ok) {
      // Success — navigate; the useSession effect above also covers this, but an
      // explicit navigate makes the happy path deterministic in tests.
      navigate(target, { replace: true });
      return;
    }
    // Honest inline error. A rejected token (bad-token) names the 401 + the fix;
    // an unknown/transport failure says so plainly.
    setError(
      result.kind === "bad-token"
        ? `That token was rejected${result.status ? ` (${result.status})` : ""}. It may be expired or for another cluster. Run kubectl create token and paste a fresh one.`
        : `Could not validate the token: ${result.message}`,
    );
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-8">
      <form
        onSubmit={onSubmit}
        className="mx-auto w-full max-w-md rounded-xl border bg-card p-8 shadow-elevated"
        aria-label="Sign in"
      >
        <div className="mb-6 flex flex-col items-center text-center">
          <div className="mb-3 flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-sm">
            <Boxes className="h-6 w-6" />
          </div>
          <h1 className="text-xl font-semibold tracking-snug">
            Sign in to agent-engine
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Paste a Kubernetes bearer token. It&apos;s held for this session only
            and sent as your identity on every request.
          </p>
        </div>

        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="token">Bearer token</Label>
            <div className="relative">
              <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                id="token"
                name="token"
                type="password"
                autoComplete="off"
                autoFocus
                placeholder="eyJhbGciOiJSUzI1NiIsImtpZC…"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                className="pl-9 font-mono text-xs"
                aria-invalid={error != null}
                aria-describedby={error ? "token-error" : undefined}
                disabled={submitting}
              />
            </div>
            {error && (
              <p id="token-error" role="alert" className="text-xs text-destructive">
                {error}
              </p>
            )}
          </div>

          <Button type="submit" className="w-full" disabled={submitting || !token.trim()}>
            {submitting ? (
              <>
                <Loader2 className="h-4 w-4 animate-spin" />
                Validating…
              </>
            ) : (
              <>
                Continue
                <ArrowRight className="h-4 w-4" />
              </>
            )}
          </Button>

          <div className="rounded-md border bg-surface-2/50 p-3 text-xs text-muted-foreground">
            <p className="mb-1 flex items-center gap-1.5 font-medium text-foreground">
              <TerminalSquare className="h-3.5 w-3.5" /> First time?
            </p>
            Get a short-lived token with{" "}
            <span className="font-mono">
              kubectl create token &lt;sa&gt; -n &lt;ns&gt;
            </span>
            . The token expires on its own — paste a fresh one when it does. OIDC
            single-sign-on arrives in M18.
          </div>
        </div>
      </form>
    </div>
  );
}
