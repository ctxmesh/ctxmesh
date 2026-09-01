import * as React from "react";
import { useNavigate } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { AuthCard } from "@/pages/login-page";
import { completeLogin } from "@/lib/oidc";
import { login } from "@/lib/session";

// AuthCallbackPage — the OIDC redirect target (/auth/callback, ADR 0020; M137
// end-user flow lands here too). M151 §6.1 archetype A7, §6.2 row
// `auth-callback-page.tsx`: "progress + honest error with a way back".
//
// The provider sends the browser here with ?code&state. `completeLogin` verifies
// the state (anti-CSRF) and exchanges the code + PKCE verifier for an ID token;
// that token then flows through the SAME session seam as a pasted token
// (login() → whoami → session), so nothing downstream changes.
//
// ── THIS PAGE IS A DOOR, NOT A DESTINATION ─────────────────────────────────
//
// It is the only screen in the product a person can arrive at with no session,
// no chrome and no navigation, having just been bounced through two origins.
// A dead end here is how somebody gets locked out of their own console: the
// back button re-submits a consumed authorization code, and there is no menu to
// escape into. So "Back to sign in" renders in EVERY state — including while
// the exchange is still running. A round trip that hangs must still have a way
// out, and a way out that only appears on failure is not one.
//
// ── THREE OUTCOMES, THREE FACTS ────────────────────────────────────────────
//
//   • The round trip itself failed (state mismatch, no code, the token endpoint
//     refused). The provider never signed you in. Retry the whole flow.
//   • The provider signed you in and the CLUSTER refused the identity (whoami
//     answered non-2xx). This is the one people misread as "wrong password":
//     the login worked and the authorization did not, and the fix is an RBAC
//     binding, not another attempt.
//   • The provider signed you in and nothing answered at all. Nothing was
//     decided about you either way.
//
// Saying "single sign-on failed" for all three sends two of the three people to
// fix the wrong thing.
//
// ── NOTHING RENDERS A CREDENTIAL ───────────────────────────────────────────
//
// The ID token is passed to login() and never held in state, never rendered,
// never put in a title. `window.location.search` — which carries the
// authorization code — is read once and never echoed into the DOM.
//
// data-testid contract:
//   auth-callback-working — the progress state
//   auth-callback-error   — the failed state

/** The round trip did not complete. Whatever the provider said, no session exists. */
const FLOW_FAILED =
  "Sign-in didn't complete, so you are still signed out. Nothing was changed about your account — start again from the sign-in page.";

/** The provider vouched for you; the cluster would not accept the identity. */
function clusterRefused(status?: number): string {
  return `Your provider signed you in, but the cluster would not accept that identity${
    status ? ` (${status})` : ""
  }. The sign-in itself worked — your account is not bound to a role that may use this console yet.`;
}

/** The provider vouched for you; nothing answered. Nothing was decided either way. */
const CLUSTER_SILENT =
  "Your provider signed you in, but the cluster never answered, so nothing was decided about your access. This is a connection problem, not a refusal.";

interface Failure {
  message: string;
  /** The machine's own words. Mono register, never prose. */
  detail?: string;
}

export function AuthCallbackPage() {
  const navigate = useNavigate();
  const [failure, setFailure] = React.useState<Failure | null>(null);

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
          return;
        }
        setFailure(
          result.kind === "bad-token"
            ? { message: clusterRefused(result.status), detail: result.message }
            : { message: CLUSTER_SILENT, detail: result.message },
        );
      } catch (err) {
        if (!alive) return;
        setFailure({
          message: FLOW_FAILED,
          detail: err instanceof Error ? err.message : undefined,
        });
      }
    })();
    return () => {
      alive = false;
    };
  }, [navigate]);

  // The exit. It is the same element in both states so it never moves under a
  // pointer that was already reaching for it.
  const backToSignIn = (
    <Button
      variant={failure ? "default" : "outline"}
      className="h-11 w-full"
      onClick={() => navigate("/login", { replace: true })}
    >
      Back to sign in
    </Button>
  );

  if (failure) {
    return (
      <AuthCard>
        <div data-testid="auth-callback-error">
          <h1 className="font-serif text-2xl font-medium tracking-snug">
            Sign-in didn&rsquo;t finish
          </h1>
          {/* A 2px crit rule, not three lines of crit prose: §2.2's hues are
              annotation and never fill an area. The rule says "this is the bad
              outcome"; the sentence stays readable ink. */}
          <div
            role="alert"
            className="mt-4 space-y-1.5 border-l-2 border-l-destructive pl-4"
          >
            <p className="text-md text-secondary-foreground">
              {failure.message}
            </p>
            {failure.detail && (
              <p className="break-words font-mono text-xs text-faint">
                {failure.detail}
              </p>
            )}
          </div>
          <div className="mt-6">{backToSignIn}</div>
        </div>
      </AuthCard>
    );
  }

  return (
    <AuthCard>
      <div data-testid="auth-callback-working">
        {/* The progressing tag, not a spinner: it says "the machine is
            converging on its own" (§2.5) and it says it identically under
            prefers-reduced-motion, which a spinning glyph does not. */}
        <Badge variant="progressing">Working</Badge>
        <h1 className="mt-3 font-serif text-2xl font-medium tracking-snug">
          Finishing sign-in
        </h1>
        <p role="status" className="mt-1.5 text-md text-secondary-foreground">
          Checking what your provider sent back, then asking the cluster who you
          are. This takes a moment and does not need anything from you.
        </p>
        {/* The way out, present BEFORE anything has gone wrong. */}
        <div className="mt-6">{backToSignIn}</div>
        <p className="mt-4 border-t border-border-soft pt-4 text-sm text-faint">
          If this sits here, leaving is safe: nothing has been signed in yet, and
          starting over from the sign-in page costs you nothing.
        </p>
      </div>
    </AuthCard>
  );
}
