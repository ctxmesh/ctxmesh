import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/kit";
import { api } from "@/lib/api";
import { startEndUserLogin, startLogin } from "@/lib/oidc";
import { login } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { cn } from "@/lib/utils";

// LoginPage — the auth gate (ADR 0012 token login, ADR 0020 console SSO,
// M137/EU1b end-user SSO). M151 §6.1 archetype A7, §6.2 row `login-page.tsx`.
//
// ── THIS PAGE HAS TWO READERS, AND THEY ARE NOT THE SAME PERSON ─────────────
//
// The console origin is read by an OPERATOR. They know what a namespace is,
// they have `kubectl`, and the honest thing to tell them is that the console
// acts as their own Kubernetes account and nothing more.
//
// An AGENT'S OWN ORIGIN is read by someone who came to talk to an assistant.
// Namespaces, service accounts, RBAC verbs and bearer tokens are not their
// vocabulary and never appear in front of them. What they need to know is four
// things, and the approved mock says all four: they are signing in to talk to
// *this assistant*, not to the platform that runs it; what it can see; what it
// cannot; and that every answer shows where it came from — and when it does not
// know, it says so.
//
// The register is chosen by `GET /api/end-user-auth-config`, which is
// HOST-DERIVED on the server: it answers only at an agent origin whose tenant
// configured an end-user IdP, and 404s everywhere else. So a `true` there is
// exactly "this is an assistant's own front door", which is the fact the copy
// turns on. The page waits for that probe before committing to a register (§7
// A7 sanctions button-shaped skeletons here) rather than flashing the operator
// card — kubectl copy in front of an end user, even for 200ms, is the failure
// this page exists to avoid.
//
// The agent's NAME comes from the `agent-pin` meta the BFF injects at an agent
// hostname (m37.3, the same tag App.tsx reads to mount the chatbox-only app).
// When it is absent the card says "Sign in to continue" and shows no avatar: an
// avatar with an invented initial is decoration pretending to be identity.
//
// ── THREE FAILURES, THREE FACTS ─────────────────────────────────────────────
//
// A login that lies about why it failed sends people to fix the wrong thing.
// These three are different facts and get three different messages:
//
//   1. REJECTED   — the cluster answered and would not accept the credential
//                   (whoami non-2xx). The fix is a fresh token.
//   2. UNREACHABLE— nothing answered, so the credential was never checked
//                   (`kind: "unknown"` — fetch itself threw). The fix is the
//                   network; the token may be perfectly good.
//   3. NO SSO     — this install has no OIDC configured, so there is no
//                   provider button at all (§7 A7: absent, never disabled). It
//                   is not a failure and is not drawn as one — it is a stated
//                   fact in the fine print, so nobody hunts for a button that
//                   was never there.
//
// A fourth exists and is deliberately kept apart from all three: SSO is
// advertised but would not START (issuer unreachable, discovery incomplete).
// Nothing was sent to a provider, and saying so is the difference between "try
// again" and "my password is wrong".
//
// ── NOTHING RENDERS A CREDENTIAL ────────────────────────────────────────────
//
// The field is `type="password"`, carries no `title`, and is cleared the moment
// `login()` returns — on success AND on failure — so a pasted token does not
// sit in the DOM of a page someone walks away from. No error message ever
// interpolates the token.
//
// data-testid contract:
//   sso-login       — the console SSO button (present only when OIDC is on)
//   end-user-login  — the tenant IdP button on an assistant's own front door
//   token-login     — the token submit
//   auth-probing    — the pre-register skeleton

interface FromState {
  from?: { pathname?: string; search?: string; hash?: string };
}

// returnPath resolves the post-login destination from the location state the
// auth guard captured — but ONLY if it is a same-origin, in-app path. This is a
// SECURITY boundary: a hostile `from` (e.g. a crafted link that lands the user
// on /login carrying `from.pathname = "//evil.com/steal"` or an absolute
// "https://evil.com") must never become the navigate() target, or a valid login
// would bounce the user off-origin — a classic post-login open-redirect /
// phishing primitive on the trusted origin.
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

/**
 * The assistant this origin belongs to, from the `agent-pin` meta the BFF
 * injects at an agent hostname (m37.3). Empty on the console origin.
 */
function readAgentName(): string {
  const content =
    document
      .querySelector('meta[name="agent-pin"]')
      ?.getAttribute("content")
      ?.trim() ?? "";
  const slash = content.indexOf("/");
  if (slash <= 0 || slash === content.length - 1) return "";
  return content.slice(slash + 1);
}

/** The avatar glyph: the assistant's first letter. Never invented. */
function initialOf(name: string): string {
  const ch = name.trim().charAt(0);
  return ch ? ch.toUpperCase() : "";
}

// ── The three failures, in words ────────────────────────────────────────────

/** 1. The cluster answered and refused the credential. */
function rejectedMessage(status?: number): string {
  return `That token was rejected${status ? ` (${status})` : ""}. The cluster answered — it just would not accept this credential. It may have expired, or belong to a different cluster. Paste a fresh one.`;
}

/** 2. Nothing answered, so the credential was never checked. */
const UNREACHABLE_MESSAGE =
  "The server never answered, so your token was not checked. This is a connection problem, not a rejected credential — nothing about your sign-in has changed.";

/** 3. This install has no OIDC. Not a failure; a fact, stated where the button isn't. */
const NO_SSO_MESSAGE =
  "Single sign-on isn't configured on this install, so there is no provider button to press. When the cluster enables OIDC one appears here on its own.";

/** The fourth: SSO is advertised but would not start. Nothing left the browser. */
const SSO_DID_NOT_START =
  "Single sign-on didn't start, so nothing was sent to a provider and you are still signed out.";

const END_USER_DID_NOT_START =
  "Sign-in didn't start, so nothing was sent to your organisation and you are still signed out.";

/** Placeholder only — deliberately too short to read as a real credential. */
const TOKEN_PLACEHOLDER = "eyJhbGciOi…";

interface Failure {
  /** The sentence. Never contains the credential. */
  message: string;
  /** The raw reason, for whoever is debugging. Mono, secondary. */
  detail?: string;
}

// ── The A7 frame ────────────────────────────────────────────────────────────

/**
 * The auth card (§6.1 A7): chrome-less, one centred card on a bare ground.
 *
 * `ground` is the whole difference between the two front doors. Paper is the
 * console's own plane. Pine-dark is the assistant's: the mock puts the card on
 * `--accent-dk` so the page reads as the assistant's property rather than as a
 * page of the platform — which is the same sentence the fine print makes.
 *
 * Exported because `auth-callback-page.tsx` is the other half of the same
 * archetype and the two must not drift into two cards.
 */
export function AuthCard({
  ground = "paper",
  children,
  className,
}: {
  ground?: "paper" | "pine";
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex min-h-dvh items-center justify-center px-4 py-10 sm:px-6",
        // Deviation from §1.3's "dark swaps token values, not utilities",
        // recorded: §6.1 A7 names `bg-brand-2` as the end-user ground, and
        // `--brand-2` is defined as "the direction pine moves under
        // interaction" — which in dark is a BRIGHTENING, not a deepening.
        // Painting a whole page in the brightest brand colour on screen is the
        // opposite of the deep-pine ground the mock specifies, and no single
        // token carries "the assistant's own ground" in both themes. In dark
        // `--accent` does (the pine-cast deep plane); in light it is the pale
        // pine tint and does not. The variant is the honest way to say one
        // thing in two palettes without inventing a token.
        ground === "pine" ? "bg-brand-2 dark:bg-accent" : "bg-background",
      )}
    >
      <div
        className={cn(
          // 420px per §6.1 A7. Below that it is the viewport minus the page
          // gutter — at 360 the card is 328px wide and nothing inside it has a
          // fixed width, so there is nothing to overflow.
          "w-full max-w-[26.25rem] rounded-lg border border-border bg-card p-6 text-card-foreground sm:p-9",
          className,
        )}
      >
        {children}
      </div>
    </div>
  );
}

/**
 * A fine-print block (§6.1 A7: "fine print `text-sm text-faint` separated by
 * `border-border-soft` rules"). Consecutive blocks rule off from each other.
 */
function FinePrint({ children }: { children: React.ReactNode }) {
  return (
    <p className="mt-4 border-t border-border-soft pt-4 text-sm text-faint">
      {children}
    </p>
  );
}

/** Emphasis inside the fine print — one step up the ink ramp, never a hue. */
function Strong({ children }: { children: React.ReactNode }) {
  return (
    <span className="font-medium text-secondary-foreground">{children}</span>
  );
}

/** The full-width 44px control the archetype specifies for a provider button. */
const PROVIDER_BUTTON = "h-11 w-full";

export function LoginPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const session = useSession();
  const target = returnPath(location.state);

  const [token, setToken] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const [failure, setFailure] = React.useState<Failure | null>(null);
  // Console SSO availability (ADR 0020), from /api/authconfig. `undefined` =
  // still probing, and while it is undefined the page states NOTHING about SSO
  // — neither a button nor the "isn't configured" line, because both would be
  // claims we cannot yet make.
  const [ssoEnabled, setSsoEnabled] = React.useState<boolean | undefined>(
    undefined,
  );
  const [ssoFailure, setSsoFailure] = React.useState<Failure | null>(null);
  const [ssoRedirecting, setSsoRedirecting] = React.useState(false);
  // End-user IdP availability (M137/EU1b), from /api/end-user-auth-config —
  // host-derived, so `true` means "this is an assistant's own front door". This
  // is the probe the whole register hangs on.
  const [endUserAuth, setEndUserAuth] = React.useState<boolean | undefined>(
    undefined,
  );
  const [endUserFailure, setEndUserFailure] = React.useState<Failure | null>(
    null,
  );
  const [endUserRedirecting, setEndUserRedirecting] = React.useState(false);

  // The assistant whose door this is. Read once — a meta tag does not change.
  const agentName = React.useMemo(readAgentName, []);

  // Already signed in (e.g. hit /login with a live session) → go to the console.
  React.useEffect(() => {
    if (session) navigate(target, { replace: true });
  }, [session, target, navigate]);

  // Probe console SSO once. A failure keeps token login (the safe default).
  React.useEffect(() => {
    const ctrl = new AbortController();
    api
      .authConfig(ctrl.signal)
      // Coerced to a real boolean: `undefined` here would leave the page
      // permanently "still probing" and silently unable to say either fact.
      .then((c) => setSsoEnabled(c.oidcEnabled === true))
      .catch(() => setSsoEnabled(false));
    return () => ctrl.abort();
  }, []);

  // Probe the end-user IdP once (M137/EU1b). A 404 / failure ⇒ not an agent
  // front door: the console register, which is the safe default.
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
    setEndUserFailure(null);
    setEndUserRedirecting(true);
    try {
      // Redirects the browser to the tenant's IdP; the callback route completes
      // the login. If this resolves, the page is already gone.
      await startEndUserLogin(target);
    } catch (err) {
      setEndUserRedirecting(false);
      setEndUserFailure({
        message: END_USER_DID_NOT_START,
        detail: err instanceof Error ? err.message : undefined,
      });
    }
  };

  const onSso = async () => {
    if (ssoRedirecting) return;
    setSsoFailure(null);
    setSsoRedirecting(true);
    try {
      await startLogin(target);
    } catch (err) {
      setSsoRedirecting(false);
      setSsoFailure({
        message: SSO_DID_NOT_START,
        detail: err instanceof Error ? err.message : undefined,
      });
    }
  };

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    setFailure(null);
    setSubmitting(true);
    const result = await login(token);
    // The credential leaves the DOM the moment it has been used, whatever the
    // outcome — a rejected token is still a token, and a login screen someone
    // walks away from must not be holding one.
    setToken("");
    setSubmitting(false);
    if (result.ok) {
      // The useSession effect above also covers this; an explicit navigate
      // makes the happy path deterministic.
      navigate(target, { replace: true });
      return;
    }
    // `detail` is the machine's own words and is drawn in the mono register;
    // the prose fix lives in `message`, never the other way round.
    setFailure(
      result.kind === "bad-token"
        ? { message: rejectedMessage(result.status), detail: result.message }
        : { message: UNREACHABLE_MESSAGE, detail: result.message },
    );
  };

  // ── The token form. Shared by both registers; only its framing differs. ────
  const tokenForm = (
    <form onSubmit={onSubmit} aria-label="Sign in with a token">
      <div className="space-y-1.5">
        <Label htmlFor="token">Bearer token</Label>
        <Input
          id="token"
          name="token"
          type="password"
          autoComplete="off"
          spellCheck={false}
          autoFocus={!endUserAuth}
          placeholder={TOKEN_PLACEHOLDER}
          value={token}
          onChange={(e) => setToken(e.target.value)}
          // 44px, so the field and the button it sits above are one stack
          // rather than two control languages (§6.1 A7).
          className="h-11 font-mono text-xs"
          aria-invalid={failure != null}
          aria-describedby={failure ? "token-error" : undefined}
          disabled={submitting}
        />
        {failure && (
          <div id="token-error" role="alert" className="space-y-1 pt-0.5">
            <p className="text-sm text-destructive">{failure.message}</p>
            {failure.detail && (
              <p className="break-words font-mono text-xs text-faint">
                {failure.detail}
              </p>
            )}
          </div>
        )}
      </div>
      <Button
        type="submit"
        className={cn(PROVIDER_BUTTON, "mt-4")}
        data-testid="token-login"
        disabled={submitting || !token.trim()}
      >
        {submitting ? "Checking…" : "Continue"}
      </Button>
    </form>
  );

  // ── Still probing: no register has been earned yet (§7 A7) ────────────────
  if (endUserAuth === undefined) {
    return (
      <AuthCard>
        <div role="status" aria-busy="true" aria-label="Loading sign-in" data-testid="auth-probing">
          <Skeleton decorative className="h-7 w-40" />
          <Skeleton decorative className="mt-3 h-4 w-full" />
          <Skeleton decorative className="mt-6 h-11 w-full" />
          <Skeleton decorative className="mt-3 h-11 w-full" />
        </div>
      </AuthCard>
    );
  }

  // ── The assistant's own front door ────────────────────────────────────────
  //
  // Everything an operator knows is absent here on purpose. There is no
  // namespace, no cluster, no token, no verb — and the two grafs below are the
  // approved copy, because they are the only four things this reader needs.
  if (endUserAuth) {
    const initial = initialOf(agentName);
    return (
      <AuthCard ground="pine">
        {initial && (
          <div
            aria-hidden="true"
            className="flex h-11 w-11 items-center justify-center rounded-sm bg-primary font-serif text-xl font-medium text-primary-foreground"
          >
            {initial}
          </div>
        )}
        <h1
          className={cn(
            "font-serif text-2xl font-medium tracking-snug",
            initial && "mt-4",
          )}
        >
          {agentName || "Sign in to continue"}
        </h1>
        <p className="mt-1.5 text-md text-secondary-foreground">
          Sign in to start a conversation. Your sign-in identifies you to this
          assistant, and to nothing else.
        </p>

        <div className="mt-6">
          <Button
            type="button"
            className={PROVIDER_BUTTON}
            onClick={onEndUserSso}
            disabled={endUserRedirecting}
            data-testid="end-user-login"
          >
            {endUserRedirecting
              ? "Taking you to your organisation…"
              : "Continue with your organisation"}
          </Button>
          {endUserFailure && (
            <div role="alert" className="mt-2 space-y-1">
              <p className="text-sm text-destructive">
                {endUserFailure.message}
              </p>
              {endUserFailure.detail && (
                <p className="break-words font-mono text-xs text-faint">
                  {endUserFailure.detail}
                </p>
              )}
            </div>
          )}
        </div>

        <FinePrint>
          You&rsquo;re signing in to talk to this assistant — <Strong>not</Strong>{" "}
          to the platform that runs it. It can read the material its owner
          attached to it and the conversation you have with it. It cannot see
          anyone else&rsquo;s.
        </FinePrint>
        <FinePrint>
          <Strong>Every answer shows where it came from.</Strong> When it
          doesn&rsquo;t know, it says so instead of guessing.
        </FinePrint>

        {/* The other reader. Whoever RUNS this assistant reaches its origin too,
            and an install whose end users sign in through an IdP still has an
            operator who signs in with a platform credential. Folding that path
            into a closed disclosure keeps the page in front of the end user free
            of it while leaving nobody locked out of a door they own. */}
        <details className="mt-4 border-t border-border-soft pt-4">
          <summary className="cursor-pointer text-sm text-faint marker:text-ghost hover:text-secondary-foreground">
            Signing in to run this assistant?
          </summary>
          <div className="mt-4">{tokenForm}</div>
        </details>
      </AuthCard>
    );
  }

  // ── The console's own front door ──────────────────────────────────────────
  return (
    <AuthCard>
      <p className="font-serif text-xl tracking-snug">
        ctx<span className="italic text-primary">mesh</span>
      </p>
      <h1 className="mt-3 font-serif text-2xl font-medium tracking-snug">
        Sign in
      </h1>
      <p className="mt-1.5 text-md text-secondary-foreground">
        The console acts as you. What it shows you, and what it lets you change,
        is exactly what your own Kubernetes account is allowed to see and do —
        nothing more.
      </p>

      {ssoEnabled && (
        <div className="mt-6">
          <Button
            type="button"
            className={PROVIDER_BUTTON}
            onClick={onSso}
            disabled={ssoRedirecting}
            data-testid="sso-login"
          >
            {ssoRedirecting
              ? "Taking you to your provider…"
              : "Sign in with SSO"}
          </Button>
          {ssoFailure && (
            <div role="alert" className="mt-2 space-y-1">
              <p className="text-sm text-destructive">{ssoFailure.message}</p>
              {ssoFailure.detail && (
                <p className="break-words font-mono text-xs text-faint">
                  {ssoFailure.detail}
                </p>
              )}
            </div>
          )}
          <div className="mt-5 flex items-center gap-3 font-mono text-2xs uppercase tracking-wide text-faint">
            <span aria-hidden="true" className="h-px flex-1 bg-border" />
            or use a token
            <span aria-hidden="true" className="h-px flex-1 bg-border" />
          </div>
        </div>
      )}

      <div className="mt-5">{tokenForm}</div>

      <FinePrint>
        <Strong>First time?</Strong> Get a short-lived token with{" "}
        <span className="font-mono text-xs">
          kubectl create token &lt;sa&gt; -n &lt;ns&gt;
        </span>
        . It expires on its own — paste a fresh one when it does.
      </FinePrint>
      {/* §7 A7: an install without OIDC does not advertise it, so there is no
          disabled button to explain. The absence is stated instead, once, so
          nobody hunts for a control that was never rendered. */}
      {ssoEnabled === false && <FinePrint>{NO_SSO_MESSAGE}</FinePrint>}
      <FinePrint>
        Your token is kept for this browser tab only — never in a cookie, and
        gone when the tab closes.
      </FinePrint>
    </AuthCard>
  );
}
