import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ExternalLink, KeyRound, Lock, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { FieldError } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { FormField } from "@/components/config/form-field";
import {
  CredentialSourceBadge,
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuietNote,
  SectionHeader,
  Wizard,
  useToast,
  type KeyValueItem,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { api, ApiError, type AddMcpResponse, type DiscoveredTool } from "@/lib/api";

// AddMcpPage — the BYO-MCP wizard (spec §5, ADR 0016; M151 §6.1 archetype A4).
// A guided flow to add your own MCP server so its tools land in the catalog:
//
//   1. Server  — a name + a remote URL (or an image) + auth mode (key or OAuth).
//                The step's forward action SUBMITS: the BFF probes the server
//                (key-auth) or starts the OAuth 2.1 flow (OAuth).
//                Key-auth: runs tools/list discovery, stores the key as a Secret.
//                OAuth: returns 202 + { authorizationURL, state } — the SPA
//                REDIRECTS the browser to that URL (window.location.href). The
//                OAuth provider's consent page runs, the BFF handles the callback
//                server-side, and the user lands back with the MCP registered.
//                The SPA NEVER sees, stores, or displays an OAuth token.
//   2. Review  — the discovered tools (names + descriptions) + that they're now
//                in the merged catalog. On a hardened install they render as
//                pending-approval (an operator approves before binding).
//                For an OAuth add, the SPA redirects before this step renders —
//                the user lands back in the console via the callback.
//
// ── THE PAGE'S ONE IDEA: THREE WAYS A PROBE CAN END, AND THEY ARE NOT THE SAME
//
// Continuing off step 1 submits, and that is the moment this page is most likely
// to be wrong about something. A single "something went wrong" would collapse
// three different truths into one useless sentence, so `Failure` keeps them
// apart and each gets its own register:
//
//   • REFUSED   — the API reached the server and the handshake did not complete.
//                 Something about THIS server (its address, its auth) has to
//                 change before it will ever work. Crit: the source field is
//                 marked invalid, the server's own words are quoted verbatim in
//                 a mono well, and the copy names what to check.
//   • NEVER RAN — the request never came back at all (offline, DNS, the BFF is
//                 down). Nothing was sent to the server and nothing was
//                 registered; the server may be perfectly fine. Warn, and NO
//                 field is blamed — the same submit is worth retrying unchanged.
//   • NOT WIRED — the API answered 501: this install does not do MCP discovery.
//                 Also nothing tried, but retrying will never help and nothing
//                 is broken, so it is a QuietNote (§7.1), never an error.
//
// The difference that matters to a reader: after REFUSED, change something.
// After NEVER RAN, change nothing and try again. After NOT WIRED, ask an
// operator. Blaming the URL for a request that never left the browser is the lie
// this split exists to prevent.
//
// ── CREDENTIALS ──────────────────────────────────────────────────────────────
// THE BEARER KEY NEVER LIVES CLIENT-SIDE AFTER SUBMIT — same discipline as the
// provider connect: held only in the field until submit, snapshotted into the
// POST body, cleared on dispatch; never a store / localStorage / sessionStorage
// / URL, never echoed back, and never placed in a `title`. After add, the UI
// only shows the discovered tools, their approval state, and a
// CredentialSourceBadge — WHERE the credential came from, never what it is.
//
// OAUTH TOKENS NEVER REACH THE SPA — only the authorization URL + the opaque
// state handle. The full token exchange (auth-code → access-token → refresh)
// happens server-side in the BFF callback (GET /api/mcp/oauth/callback).
//
// Other honest failures (never swallowed):
//   • viewer (403)      → ForbiddenInline (the API is the real gate).
//   • kill-switch (404) → the "BYO-MCP is disabled" fallback state.

type SourceKind = "url" | "image";
type AuthMode = "none" | "key" | "oauth";

/** How a probe ended. See the header note — these are three different truths. */
type Failure =
  | { reach: "refused"; message: string; status: number }
  | { reach: "never-ran"; message: string }
  | { reach: "not-wired" };

type Submit =
  | { kind: "idle" }
  | { kind: "adding" }
  | { kind: "added"; res: AddMcpResponse }
  | { kind: "oauth-redirecting"; authorizationURL: string }
  | { kind: "failed"; failure: Failure }
  | { kind: "forbidden"; message: string }
  | { kind: "killed" };

/**
 * Sort a thrown error into the three outcomes. 403 (RBAC) and 404 (kill-switch)
 * are terminal page states and are peeled off by the caller before this runs.
 *
 * The load-bearing line is the last one: anything that is NOT an ApiError never
 * completed a request at all — fetch itself rejected — so the platform learned
 * nothing about the server and must not imply that it did.
 */
function classify(err: unknown): Failure {
  if (err instanceof ApiError) {
    if (err.isNotImplemented) return { reach: "not-wired" };
    return { reach: "refused", message: err.message, status: err.status };
  }
  return {
    reach: "never-ran",
    message: err instanceof Error ? err.message : "the request never completed",
  };
}

// isValidHttpUrl guards the OAuth redirect (m18.7) AND the URL field: only an
// absolute http(s) URL is a safe `window.location.href` target or a probeable
// MCP endpoint. Rejects "", relative paths, and non-http schemes (e.g.
// "javascript:") so a bad authorizationURL can never remount the SPA into the
// login bounce.
function isValidHttpUrl(u: string | undefined): boolean {
  if (!u) return false;
  try {
    const parsed = new URL(u);
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

/**
 * Where the credential for these tools comes from — never what it is. `none`
 * returns undefined so `CredentialSourceBadge` renders nothing and the review
 * row states the absence in words instead of leaving a blank.
 */
function credentialSourceFor(mode: AuthMode): string | undefined {
  if (mode === "oauth") return "byo-oauth";
  if (mode === "key") return "shared";
  return undefined;
}

const STEP_SERVER = 0;
const STEP_REVIEW = 1;

export function AddMcpPage() {
  const navigate = useNavigate();
  const [current, setCurrent] = React.useState(STEP_SERVER);
  const [name, setName] = React.useState("");
  const [sourceKind, setSourceKind] = React.useState<SourceKind>("url");
  const [authMode, setAuthMode] = React.useState<AuthMode>("key");
  const [url, setUrl] = React.useState("");
  const [image, setImage] = React.useState("");
  // ── The bearer key lives ONLY here — a local field value, cleared on submit.
  const [apiKey, setApiKey] = React.useState("");
  const [submit, setSubmit] = React.useState<Submit>({ kind: "idle" });

  const { toast } = useToast();
  // RBAC-aware chrome (§3): registering an MCP server creates a ToolRegistry
  // entry (+ a Secret when a key is given) — a write. A viewer sees the gated
  // entry in the shell; if they reach this page directly the API 403 is the
  // real gate. DISPLAY-ONLY (ADR 0011).
  const { can, reprobe } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");

  // Editing any part of the server description retires the last verdict: a probe
  // result describes the values that were SUBMITTED, not the ones now in the
  // fields. Leaving it up would let a stale "refused" sit under a URL that was
  // never tried.
  function clearVerdict() {
    setSubmit((s) => (s.kind === "failed" ? { kind: "idle" } : s));
  }

  // connectViaOAuth runs the zero-config OAuth 2.1 connect (m24.7, ADR 0028): POST
  // the NESTED auth block the BFF routes on (req.auth.type == "oauth") with
  // autoDiscover so the BFF discovers the endpoints + registers a client (DCR) — the
  // user enters NO endpoints or client id. The BFF returns 202 + an authorization
  // URL; the ENTIRE token exchange is server-side (we never hold a token). Reused by
  // the OAuth-mode submit AND the "Continue with OAuth" offer on a key-probe 422.
  async function connectViaOAuth() {
    setSubmit({ kind: "adding" });
    const req = {
      name: name.trim(),
      ...(sourceKind === "url" ? { url: url.trim() } : { image: image.trim() }),
      authType: "oauth" as const,
      auth: {
        type: "oauth" as const,
        autoDiscover: true,
        // redirectUri is this console's OAuth callback (same origin as the BFF that
        // serves us); DCR registers it so the auth server accepts it.
        redirectUri: `${window.location.origin}/api/mcp/oauth/callback`,
      },
    };
    try {
      const oauthRes = await api.addMcpServerOAuth(req);
      // GUARD (m18.7): only a well-formed absolute http(s) URL is a safe redirect
      // target. A missing/malformed authorizationURL would otherwise navigate the
      // SPA to a bad target, remount it, and bounce the user to /login. This IS a
      // "refused" — the server answered and its answer was unusable — so it names
      // the server as the thing that has to change.
      if (!isValidHttpUrl(oauthRes.authorizationURL)) {
        setSubmit({
          kind: "failed",
          failure: {
            reach: "refused",
            message:
              "The MCP server did not return a valid OAuth authorization URL — it may not support OAuth 2.1. Nothing was changed.",
            status: 502,
          },
        });
        return;
      }
      setSubmit({ kind: "oauth-redirecting", authorizationURL: oauthRes.authorizationURL });
      // A correct OAuth redirect (a different origin) — NOT a client-side navigation.
      window.location.href = oauthRes.authorizationURL;
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isNotFound) { setSubmit({ kind: "killed" }); return; }
        if (err.isForbidden) {
          reprobe();
          setSubmit({ kind: "forbidden", message: err.message });
          return;
        }
      }
      setSubmit({ kind: "failed", failure: classify(err) });
    }
  }

  async function onAdd() {
    setSubmit({ kind: "adding" });

    if (authMode === "oauth") {
      await connectViaOAuth();
      return;
    }

    // Key-auth (or no-auth) flow: probe immediately, discover tools.
    // Snapshot the key into the request body, then WIPE it from state — it never
    // survives the submit in any client-side store.
    const req = {
      name: name.trim(),
      ...(sourceKind === "url" ? { url: url.trim() } : { image: image.trim() }),
      // Only attach a key in "key" mode — "none" (open server) probes with no auth.
      ...(authMode === "key" && apiKey ? { apiKey } : {}),
    };
    setApiKey("");
    try {
      const res = await api.addMcpServer(req);
      setSubmit({ kind: "added", res });
      setCurrent(STEP_REVIEW);
      toast({
        variant: "success",
        title: `Added ${res.name}`,
        description: `${res.tools.length} tool${res.tools.length === 1 ? "" : "s"} discovered`,
      });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isNotFound) {
          setSubmit({ kind: "killed" });
          return;
        }
        if (err.isForbidden) {
          reprobe();
          setSubmit({ kind: "forbidden", message: err.message });
          return;
        }
      }
      setSubmit({ kind: "failed", failure: classify(err) });
    }
  }

  // Leaving the SERVER step (0 → 1) SUBMITS; the review step is entered only on a
  // successful add (onAdd sets current). Backward moves pass through.
  function onStepChange(next: number) {
    if (current === STEP_SERVER && next > STEP_SERVER) {
      void onAdd();
      return;
    }
    setCurrent(next);
  }

  const adding = submit.kind === "adding";
  const source = sourceKind === "url" ? url : image;
  const failure = submit.kind === "failed" ? submit.failure : null;
  // A URL that is not an absolute http(s) address can never be probed, so the
  // field says so before a submit spends a round trip proving it. Empty is not
  // an error — it is simply not filled in yet.
  const urlSyntaxError =
    sourceKind === "url" && url.trim() !== "" && !isValidHttpUrl(url.trim())
      ? "Needs an absolute http:// or https:// address."
      : undefined;
  // A refused probe IS about this server: as described here, it did not complete
  // the MCP handshake. A probe that never ran says nothing about the server, so
  // in that case the field is left alone.
  const sourceRefused = failure?.reach === "refused";
  const sourceInvalid = !!urlSyntaxError || sourceRefused;
  const serverReady =
    name.trim().length > 0 && source.trim().length > 0 && !urlSyntaxError;

  if (submit.kind === "killed") return <KillSwitchFallback />;

  if (submit.kind === "forbidden") {
    return (
      <PageFrame>
        <ForbiddenInline
          title="Not allowed to add an MCP server"
          description="Adding an MCP server creates a ToolRegistry entry (and a Secret for its key) — your account can't create those in this cluster. Ask an operator to add it for you."
          resource="MCP servers"
          permission="create"
        />
      </PageFrame>
    );
  }

  // OAuth redirecting: the SPA has the authorization URL and is about to redirect.
  // Render a brief "redirecting to consent…" banner while window.location is set.
  // The SPA NEVER holds a token here — only the URL we received and are navigating to.
  if (submit.kind === "oauth-redirecting") {
    return (
      <PageFrame>
        <div
          className="rounded-lg border border-border bg-card p-6"
          data-testid="oauth-redirecting"
        >
          <div className="flex items-start gap-3">
            <ExternalLink className="mt-0.5 h-5 w-5 shrink-0 text-faint" />
            <div className="min-w-0">
              <p className="font-serif text-lg font-medium">
                Handing you to the consent page…
              </p>
              <p className="mt-1 max-w-[64ch] text-sm text-secondary-foreground">
                The provider is about to ask whether this console may act on your
                behalf. Once you agree you land back here with the server
                registered — the token is exchanged server-side and never reaches
                this browser.
              </p>
            </div>
          </div>
          <div className="mt-4">
            <Button
              variant="outline"
              size="sm"
              onClick={() => { window.location.href = submit.authorizationURL; }}
              data-testid="oauth-redirect-button"
            >
              <ExternalLink className="h-3.5 w-3.5" />
              Open consent page
            </Button>
          </div>
        </div>
      </PageFrame>
    );
  }

  const steps: WizardStep[] = [
    {
      id: "server",
      title: "Server",
      description: "URL or image + auth",
      content: (
        <div className="space-y-5">
          <SectionHeader
            title="The server"
            lede="Where it lives, and how the platform authenticates to it. Continuing probes the server — nothing is registered unless that probe succeeds."
          />
          <div className="space-y-4">
            <FormField id="mcp-name" label="Name">
              <Input
                id="mcp-name"
                value={name}
                onChange={(e) => { clearVerdict(); setName(e.target.value); }}
                placeholder="acme-support"
              />
            </FormField>
            <FormField id="mcp-source-kind" label="Source">
              <Select
                id="mcp-source-kind"
                value={sourceKind}
                onChange={(e) => { clearVerdict(); setSourceKind(e.target.value as SourceKind); }}
              >
                <option value="url">Remote URL</option>
                <option value="image">Sidecar image</option>
              </Select>
            </FormField>
            {sourceKind === "url" ? (
              <FormField id="mcp-url" label="MCP server URL">
                <Input
                  id="mcp-url"
                  value={url}
                  onChange={(e) => { clearVerdict(); setUrl(e.target.value); }}
                  placeholder="https://mcp.acme.dev/sse"
                  className="font-mono text-xs"
                  aria-invalid={sourceInvalid || undefined}
                  aria-describedby={sourceInvalid ? "mcp-url-error" : undefined}
                />
                <FieldError id="mcp-url-error">
                  {urlSyntaxError ??
                    (sourceRefused
                      ? "This server didn’t complete the MCP handshake."
                      : undefined)}
                </FieldError>
              </FormField>
            ) : (
              <FormField id="mcp-image" label="Image">
                <Input
                  id="mcp-image"
                  value={image}
                  onChange={(e) => { clearVerdict(); setImage(e.target.value); }}
                  placeholder="ghcr.io/acme/mcp-support:v1"
                  className="font-mono text-xs"
                  aria-invalid={sourceRefused || undefined}
                  aria-describedby={sourceRefused ? "mcp-image-error" : undefined}
                />
                <FieldError id="mcp-image-error">
                  {sourceRefused
                    ? "This server didn’t complete the MCP handshake."
                    : undefined}
                </FieldError>
              </FormField>
            )}
            <FormField id="mcp-auth-mode" label="Authentication">
              <Select
                id="mcp-auth-mode"
                value={authMode}
                onChange={(e) => { clearVerdict(); setAuthMode(e.target.value as AuthMode); }}
                data-testid="mcp-auth-mode"
              >
                <option value="none">None (open server)</option>
                <option value="key">Bearer key</option>
                <option value="oauth">OAuth 2.1 (redirect to consent)</option>
              </Select>
            </FormField>
            {authMode === "key" && (
              <FormField id="mcp-key" label="Bearer key (optional)">
                <div className="relative">
                  <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-faint" />
                  <Input
                    id="mcp-key"
                    type="password"
                    autoComplete="off"
                    value={apiKey}
                    onChange={(e) => { clearVerdict(); setApiKey(e.target.value); }}
                    placeholder="If the server requires auth…"
                    className="pl-9 font-mono text-xs"
                  />
                </div>
                {/* Said out loud because the field really does empty itself on
                    submit — a user who is not told reads that as a bug. */}
                <p className="text-xs text-faint">
                  The key is sent with the probe and then dropped from this page.
                  If you submit again, paste it again.
                </p>
              </FormField>
            )}
          </div>

          {/* An aside about how the platform handles this, in the calm note
              register (role="note") — parenthetical, never an alarm. */}
          {authMode === "oauth" ? (
            <QuietNote title="No key to paste, and no token in this browser.">
              Continuing sends the server details to the console’s API, which
              discovers the OAuth endpoints and registers a client for you. Your
              browser visits the provider’s consent page; the token exchange
              happens entirely server-side.
            </QuietNote>
          ) : (
            <QuietNote title="What the probe does.">
              The API contacts the server and runs{" "}
              <span className="font-mono text-xs">tools/list</span> discovery.{" "}
              {authMode === "none"
                ? "An open server is contacted with no credentials."
                : "A bearer key, if given, is stored as a Secret and attached at the egress hop — never by the browser, never inside the agent container."}{" "}
              Egress opens per approved server only.
            </QuietNote>
          )}

          {failure?.reach === "refused" && (
            <div className="space-y-3">
              <div
                className="rounded-lg border border-destructive/40 bg-destructive-surface/40 px-4 py-3"
                role="alert"
                data-testid="probe-error"
              >
                <p className="font-serif text-md font-medium text-destructive">
                  {authMode === "oauth"
                    ? "The OAuth handshake didn’t start."
                    : "The probe ran, and the server refused it."}
                </p>
                {/* The server's own words, verbatim, in the machine register.
                    Paraphrasing them is how a console throws away the one clue
                    a reader can actually act on. */}
                <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
                  {failure.message} ({failure.status})
                </pre>
                <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
                  {authMode === "oauth"
                    ? "Nothing was registered. The server may not speak OAuth 2.1 — try a bearer key instead, or check the address."
                    : "Nothing was registered. Check the URL is a reachable MCP endpoint (and the bearer key if it needs auth), then try again."}
                </p>
              </div>
              {/* One-click OAuth (m24.7): an auth-required probe (422) on a URL
                  server is very likely an OAuth server. Offer to connect via OAuth
                  right here — the BFF discovers the endpoints + registers a client
                  (DCR), so the user needs no key and no OAuth config. */}
              {failure.status === 422 && sourceKind === "url" && authMode !== "oauth" && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  data-testid="continue-with-oauth"
                  onClick={() => {
                    setAuthMode("oauth");
                    void connectViaOAuth();
                  }}
                >
                  Continue with OAuth
                </Button>
              )}
            </div>
          )}

          {failure?.reach === "never-ran" && (
            <div
              className="rounded-lg border border-warning/40 bg-warning-surface/40 px-4 py-3"
              role="alert"
              data-testid="probe-unreachable"
            >
              <p className="font-serif text-md font-medium text-warning">
                The probe never ran.
              </p>
              <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
                {failure.message}
              </pre>
              <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
                The request didn’t reach the console’s API, so nothing was sent to
                the server and nothing was registered. That says nothing about the
                server — it may be perfectly fine. Check your connection, then
                submit the same details again.
              </p>
            </div>
          )}

          {failure?.reach === "not-wired" && (
            <div data-testid="probe-unsupported">
              <QuietNote title="Adding MCP servers isn’t wired up on this install.">
                The console asked its API to contact the server, and the API
                answered that it does not do that here. Nothing was tried and
                nothing was registered — this is not a problem with the address or
                the key, and submitting again will not change it. An operator
                turns MCP discovery on in the BFF.
              </QuietNote>
            </div>
          )}
        </div>
      ),
    },
    {
      id: "tools",
      title: "Tools",
      review: true,
      content:
        submit.kind === "added" ? (
          <ReviewStep res={submit.res} authMode={authMode} />
        ) : (
          <SectionHeader
            title="Tools"
            lede="Nothing is discovered until the server answers. Submit the server on the previous step and whatever it advertises lands here."
          />
        ),
    },
  ];

  // The review step is terminal — the server is already registered by the time
  // it renders — so its footer action must carry the reader ONWARD rather than
  // sit there disabled saying "Create". Which onward depends on what the API
  // said: tools queued for an operator cannot be bound yet, so sending someone
  // to the agent builder would be sending them to a dead end.
  const added = submit.kind === "added" ? submit.res : null;
  const queuedForOperator = added?.approvalStatus === "pending";

  const canProceed =
    current === STEP_SERVER ? serverReady && canAdd : added !== null;

  return (
    <PageFrame>
      {!canAdd && <ReadOnlyNote />}
      <Wizard
        steps={steps}
        current={current}
        onStepChange={onStepChange}
        canProceed={canProceed}
        busy={adding}
        nextLabel={
          current === STEP_SERVER
            ? authMode === "oauth"
              ? "Connect via OAuth"
              : "Probe + discover"
            : "Continue"
        }
        finishLabel={
          queuedForOperator ? "See the approval queue" : "Use these in an agent"
        }
        onFinish={() =>
          navigate(queuedForOperator ? "/tools/approvals" : "/agents/new")
        }
        // Cancel belongs in the footer (§6.1 A4) while there is still something
        // to cancel. Once the server IS registered there is nothing to abandon.
        onCancel={
          submit.kind === "added" ? undefined : () => navigate("/tools/mcp-servers")
        }
        dirty={name.trim() !== "" || source.trim() !== ""}
      />
    </PageFrame>
  );
}

// PageFrame — the §6.1 A4 shell: the page band, then a column wide enough for
// the Wizard's 15rem rail + its 2rem gap + the 46rem content column the
// archetype fixes. Capping the OUTER column is what sizes the inner one without
// reaching into the kit.
function PageFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Add an MCP server"
        lede="Point the console at an MCP server you run. It probes the server, and whatever tools the server advertises become bindable by your agents."
      />
      <div className="max-w-[63rem] space-y-5">{children}</div>
    </div>
  );
}

// ReadOnlyNote — the display-only RBAC gate above the wizard (ADR 0011). A
// permission boundary is routine, not a failure, so it reads in the calm lock
// register (M99 C1) rather than as an alarm; the API is still the real gate.
function ReadOnlyNote() {
  return (
    <div
      className="flex items-start gap-3 rounded-lg border border-border bg-surface-2/40 px-4 py-3"
      data-testid="add-mcp-readonly-note"
    >
      <Lock className="mt-0.5 h-4 w-4 shrink-0 text-faint" />
      <p className="max-w-[64ch] text-sm text-secondary-foreground">
        You have read-only access here. Adding an MCP server needs permission to
        create AgentRegistries in this cluster — ask an operator to add one for
        you, or to grant your account that role.
      </p>
    </div>
  );
}

// ReviewStep renders the discovered tools + their approval state. On a hardened
// install (approvalStatus === "pending") the tools queue for operator approval
// before any agent can bind them; otherwise they're immediately in the catalog.
//
// It says WHERE the credential came from (CredentialSourceBadge) and never what
// it is — the key left this page at submit and does not come back.
function ReviewStep({ res, authMode }: { res: AddMcpResponse; authMode: AuthMode }) {
  const pending = res.approvalStatus === "pending";
  const count = res.tools.length;
  const facts: KeyValueItem[] = [
    { key: "Server", value: res.name },
    { key: "Tools found", value: count },
    {
      key: "Credential",
      value: (
        <CredentialSourceBadge
          credentialSource={credentialSourceFor(authMode)}
          name={res.name}
        />
      ),
      absent: "none — open server",
      title: "This server was probed with no credentials.",
      mono: false,
    },
    {
      key: "In the catalog",
      // Awaiting an operator is a person-gate, not a warning (§2.2 hold).
      value: pending ? (
        <Badge variant="hold">awaiting an operator</Badge>
      ) : (
        <Badge variant="ok">bindable now</Badge>
      ),
      mono: false,
    },
  ];

  return (
    <div className="space-y-5">
      <SectionHeader
        title={`${res.name} answered`}
        lede={
          count === 0
            ? "The handshake completed, but the server advertised no tools. There is nothing to bind yet."
            : `It advertised ${count} tool${count === 1 ? "" : "s"}. ${
                pending
                  ? "They are queued for an operator before any agent can bind them."
                  : "They are in the merged catalog now."
              }`
        }
      />
      <KeyValueList items={facts} />
      <div className="space-y-2" data-testid="tool-list">
        {res.tools.map((t: DiscoveredTool) => {
          const toolPending = (t.approvalStatus ?? res.approvalStatus) === "pending";
          return (
            <div
              key={t.name}
              className="flex items-center gap-3 rounded-md border border-border bg-surface-2/40 px-3 py-2"
            >
              <Wrench className="h-4 w-4 shrink-0 text-faint" />
              <div className="min-w-0 flex-1">
                <p className="font-mono text-sm">{t.name}</p>
                {t.description && (
                  <p className="text-xs text-faint">{t.description}</p>
                )}
              </div>
              {toolPending && <Badge variant="hold">pending approval</Badge>}
            </div>
          );
        })}
      </div>
      {pending ? (
        <div data-testid="pending-approval-note">
          <QuietNote title="This is a hardened install.">
            The discovered tools are queued for operator approval into the
            ToolRegistry. Until one is approved no agent can bind it — the list
            above is a proposal, not yet a capability.
          </QuietNote>
        </div>
      ) : (
        <QuietNote title="What happens next.">
          These tools are in the merged catalog and can be bound to an agent.
          Egress opens to this server only for the agents that use them.
        </QuietNote>
      )}
    </div>
  );
}

// KillSwitchFallback — the hardened-install state: self-serve MCP registration
// is turned off (the endpoint 404s). Nothing is broken, so it reads as a calm
// note (§7.1) rather than an error, and it names the operator-driven route
// instead of dead-ending.
function KillSwitchFallback() {
  return (
    <PageFrame>
      <div data-testid="kill-switch-fallback">
        <QuietNote title="Adding MCP servers is turned off on this cluster.">
          <p>
            This is a hardened install — self-serve MCP registration is disabled.
            Tools are curated into the{" "}
            <span className="font-mono text-xs text-foreground">ToolRegistry</span>{" "}
            by an operator.
          </p>
          <p className="mt-2">
            Ask an operator to add the MCP server you need; its tools will then
            appear in the catalog for your agents.
          </p>
        </QuietNote>
      </div>
    </PageFrame>
  );
}
