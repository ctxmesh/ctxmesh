import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  ArrowRight,
  Boxes,
  KeyRound,
  Loader2,
  LogIn,
  TerminalSquare,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { login } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { api } from "@/lib/api";
import { startEndUserLogin, startLogin } from "@/lib/oidc";

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

// returnPath resolves the post-login destination from the location state the
// auth guard captured — but ONLY if it is a same-origin, in-app path. This is a
// SECURITY boundary: a hostile `from` (e.g. a crafted link that lands the user
// on /login carrying `from.pathname = "//evil.com/steal"` or an absolute
// "https://evil.com") must never become the navigate() target, or a valid login
// would bounce the user off-origin — a classic post-login open-redirect /
// phishing primitive on the trusted origin. (Especially important before the
// M18 OIDC redirect flows.)
//
// The whole composed target (pathname + search + hash) is validated, not just
// the pathname, so nothing slips in through search/hash. An in-app path must
// start with exactly one "/" and not a second (which would be protocol-relative
// "//host") — enforced by /^\/(?!\/)/. Anything else falls back to "/".
function returnPath(state: unknown): string {
  const from = (state as FromState | null)?.from;
  if (!from?.pathname) return "/";

  const target = `${from.pathname}${from.search ?? ""}${from.hash ?? ""}`;

  // Must be a single-slash-rooted in-app path: starts with "/" but not "//"
  // (protocol-relative) and is not an absolute URL. Reject backslashes too
  // (some UAs treat "\" as "/", so "/\evil.com" could be coerced off-origin).
  if (!/^\/(?!\/)/.test(target) || target.includes("\\")) {
    return "/";
  }
  return target;
}

export function LoginPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const session = useSession();
  const target = returnPath(location.state);

  const [token, setToken] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  // SSO availability (ADR 0020) — probed from /api/authconfig. When on, SSO is the
  // primary path and token login is the fallback. undefined = still probing.
  const [ssoEnabled, setSsoEnabled] = React.useState<boolean | undefined>(
    undefined,
  );
  const [ssoError, setSsoError] = React.useState<string | null>(null);
  const [ssoRedirecting, setSsoRedirecting] = React.useState(false);
  // End-user IdP availability (M137/EU1b) — probed from /api/end-user-auth-config, which is
  // host-derived: present ONLY at an agent origin whose tenant configured an end-user IdP,
  // null everywhere else. undefined = still probing.
  const [endUserAuth, setEndUserAuth] = React.useState<boolean | undefined>(
    undefined,
  );
  const [endUserError, setEndUserError] = React.useState<string | null>(null);
  const [endUserRedirecting, setEndUserRedirecting] = React.useState(false);

  // Already signed in (e.g. hit /login with a live session) → go to the console.
  React.useEffect(() => {
    if (session) navigate(target, { replace: true });
  }, [session, target, navigate]);

  // Probe SSO availability once. A failure keeps token login (the safe default).
  React.useEffect(() => {
    const ctrl = new AbortController();
    api
      .authConfig(ctrl.signal)
      .then((c) => setSsoEnabled(c.oidcEnabled))
      .catch(() => setSsoEnabled(false));
    return () => ctrl.abort();
  }, []);

  // Probe end-user IdP availability once (M137/EU1b). A 404 / failure ⇒ not available (the
  // safe default: this isn't an agent origin with an end-user IdP).
  React.useEffect(() => {
    const ctrl = new AbortController();
    api
      .endUserAuthConfig(ctrl.signal)
      .then((cfg) => setEndUserAuth(!!cfg))
      .catch(() => setEndUserAuth(false));
    return () => ctrl.abort();
  }, []);

  const onEndUserSso = async () => {
    if (endUserRedirecting) return;
    setEndUserError(null);
    setEndUserRedirecting(true);
    try {
      // Redirects the browser to the tenant's IdP; the callback route completes the login.
      await startEndUserLogin(target);
    } catch (err) {
      setEndUserRedirecting(false);
      setEndUserError(
        err instanceof Error
          ? err.message
          : "Could not start sign-in with your identity provider.",
      );
    }
  };

  const onSso = async () => {
    if (ssoRedirecting) return;
    setSsoError(null);
    setSsoRedirecting(true);
    try {
      // Redirects the browser to Dex; the callback route completes the login.
      await startLogin(target);
    } catch (err) {
      setSsoRedirecting(false);
      setSsoError(
        err instanceof Error ? err.message : "Could not start single sign-on.",
      );
    }
  };

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
            Paste a Kubernetes bearer token. It&apos;s held for this session
            only and sent as your identity on every request.
          </p>
        </div>

        <div className="space-y-4">
          {endUserAuth && (
            <div className="space-y-2">
              <Button
                type="button"
                className="w-full"
                onClick={onEndUserSso}
                disabled={endUserRedirecting}
                data-testid="end-user-login"
              >
                {endUserRedirecting ? (
                  <>
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Redirecting to your identity provider…
                  </>
                ) : (
                  <>
                    <LogIn className="h-4 w-4" />
                    Log in with your identity provider
                  </>
                )}
              </Button>
              {endUserError && (
                <p role="alert" className="text-xs text-destructive">
                  {endUserError}
                </p>
              )}
              <div className="flex items-center gap-2 pt-1 text-[10px] uppercase tracking-wide text-muted-foreground">
                <span className="h-px flex-1 bg-border" />
                or use a token
                <span className="h-px flex-1 bg-border" />
              </div>
            </div>
          )}
          {ssoEnabled && (
            <div className="space-y-2">
              <Button
                type="button"
                className="w-full"
                onClick={onSso}
                disabled={ssoRedirecting}
                data-testid="sso-login"
              >
                {ssoRedirecting ? (
                  <>
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Redirecting to your identity provider…
                  </>
                ) : (
                  <>
                    <LogIn className="h-4 w-4" />
                    Sign in with SSO
                  </>
                )}
              </Button>
              {ssoError && (
                <p role="alert" className="text-xs text-destructive">
                  {ssoError}
                </p>
              )}
              <div className="flex items-center gap-2 pt-1 text-[10px] uppercase tracking-wide text-muted-foreground">
                <span className="h-px flex-1 bg-border" />
                or use a token
                <span className="h-px flex-1 bg-border" />
              </div>
            </div>
          )}

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
              <p
                id="token-error"
                role="alert"
                className="text-xs text-destructive"
              >
                {error}
              </p>
            )}
          </div>

          <Button
            type="submit"
            className="w-full"
            disabled={submitting || !token.trim()}
          >
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
            . The token expires on its own — paste a fresh one when it does.
            {ssoEnabled
              ? " Or use Sign in with SSO above for your org identity."
              : " Single sign-on (OIDC) is available when your cluster enables it."}
          </div>
        </div>
      </form>
    </div>
  );
}
