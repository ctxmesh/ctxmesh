import * as React from "react";
import { Send } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { ChatMarkdown } from "@/components/chat-markdown";
import {
  ForbiddenInline,
  NextStepLink,
  QuietNote,
  Timeline,
  type TimelineStep,
} from "@/components/kit";
import { api, ApiError } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useDevMode } from "@/lib/dev-mode";
import { useDurableRun } from "@/lib/use-durable-run";
import { RES_AGENTS } from "@/lib/nav";
import { extractAgentOutput } from "@/lib/agent-output";
import { cn } from "@/lib/utils";
import {
  isValidHttpUrl,
  MCP_OAUTH_MESSAGE,
  type McpOAuthPopupMessage,
  readMcpOAuthReturn,
} from "@/lib/oauth-popup";

// mcpCallbackOrigin is the canonical console origin the BFF injects (ADR 0040): the MCP-consent
// callback runs THERE, so a chatbox served at an agent hostname must also trust its cross-origin
// "connected" relay message (not only same-origin). Empty on a single-origin console (same-origin only).
function mcpCallbackOrigin(): string {
  return (
    document.querySelector('meta[name="mcp-callback-origin"]')?.getAttribute("content")?.trim() ?? ""
  );
}

// ChatPanel — a turn-by-turn chat with a deployed agent (M151 §6.1 archetype A10).
//
// ── TWO FRAMES, TWO READERS ─────────────────────────────────────────────────
//
// This one component is BOTH the console's agent-detail chat tab and the
// chrome-less chatbox served at an agent's own hostname, and those are not the
// same surface for the same person:
//
//   frame="panel"      a Card among other Cards on a console page. It keeps its
//                      own header, because there it is one panel of many.
//   frame="bare"       it IS the page (agent-chatbox-page). No card, no header
//                      of its own — the page masthead carries identity — and the
//                      transcript owns the scroll while the composer is pinned.
//
//   audience="operator"  someone who runs the platform. RBAC verbs, workspaces
//                        and run ids are their vocabulary and links into the
//                        console resolve.
//   audience="end-user"  someone who came to talk to an assistant. NONE of that
//                        vocabulary appears, and no link leaves for a console
//                        route that does not exist at an agent origin.
//
// The default is operator/panel, so the console's existing embedding is
// unchanged by the props existing.
//
// ── THE GRAMMAR (§6.1 A10) ──────────────────────────────────────────────────
//
// A 46rem column. The person's messages are right-aligned pine-TINT bubbles —
// tint, not the solid pine they used to be: solid pine is the colour of a
// control you can press (§2.1), and a message is not one. The assistant's
// answers are full-width bordered blocks on card with a mono eyebrow, because
// an answer with citations is a document, not a speech bubble.
//
// Consent and holds are NOT bubbles. They are governance, and §5.26's ruling is
// that governance renders in the same spine as the work it interrupted — so
// they render as a one-step kit Timeline in the hold violet, the hue that means
// exactly one thing: work is paused because a PERSON must decide (ADR 0128).
// The consent prompt used to be amber, which now means "a bound is near or
// crossed" — a quota signal painted on a human gate.
//
// ── WHAT THIS SURFACE MAY NOT CLAIM (§7 A10) ────────────────────────────────
//
// An agent with no memory binding is not an agent whose memory is empty. The
// difference is stated once, in a QuietNote pinned above the composer, rather
// than being left for the reader to discover when the assistant forgets what
// they said. A failed turn renders IN the transcript with a way to try again —
// the transcript is never blanked, because the earlier answers are still true.
//
// Under the hood each turn is a DURABLE, observable run (ADR 0093): createRun →
// openRunStream → finalize (via getRun), exactly as the Playground does — so every chat
// turn appears in the Runs list, loads its native trace, is cost-attributed, and is
// shareable/approvable. Tokens stream LIVE into the turn. Session threading is preserved
// by forwarding the shared X-Conversation-Id on the create path (two chat sessions are two
// distinct runs sharing a conversation id). Each agent turn keeps the trace-id link and the
// inline per-user Connect banner (ADR 0031) — connecting RESUMES the same run.
//
// Dev-mode (`ctxmesh dev --ui`) has no cluster/run store — only /api/invoke works there —
// so the OLD synchronous invoke path is kept as a fallback, gated on the GET /api/devmode probe
// (ADR 0093 §2). On a real cluster the durable path is used.
//
// data-testid contract:
//   chat-panel · chat-thread · chat-empty · chat-input · chat-send · chat-new
//   chat-turn-user · chat-turn-agent · chat-pending · chat-turn-error
//   chat-consent · connect-<server> · connect-error · chat-hold
//   chat-memory-note · chat-not-ready-note · chat-readonly-note · open-trace
//   mcp-oauth-return

/** Who is reading. Chooses vocabulary and whether console links resolve. */
export type ChatAudience = "operator" | "end-user";

/** Whether the chat is one panel on a page, or the page. */
export type ChatFrame = "panel" | "bare";

type ChatTurn = {
  id: number;
  role: "user" | "agent";
  text: string;
  /** When the turn appeared in this tab. A real, local fact — not a server clock. */
  at: number;
  // agent turns carry the user input that produced them, so a post-connect resume
  // re-runs the SAME message without appending a duplicate user turn.
  sourceText?: string;
  // runId is the durable run backing this agent turn (ADR 0093) — the connect CTA RESUMES it
  // (same run) instead of creating a new one. Absent in the dev-mode /invoke fallback.
  runId?: string;
  traceId?: string;
  consentRequired?: string[];
  /** The run paused on a person's DECISION (approval / plan approval), not on consent. */
  heldFor?: "approval" | "plan_approval";
  pending?: boolean;
  error?: string;
  // a forbidden run (viewer without invoke rights) gets the ForbiddenInline treatment —
  // the API is the real gate, not the SPA's create capability.
  forbidden?: boolean;
};

// newConversationId mints a key-safe thread id (no ':' '/' ' ' — the launcher's
// validateConversationID rules). randomUUID is hex+hyphen only; the fallback keeps jsdom
// (no crypto.randomUUID) working in tests.
function newConversationId(): string {
  const rand =
    typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
  return `chat-${rand}`;
}

/** The eyebrow clock. Local, short, and only ever a moment this tab witnessed. */
function clock(ms: number): string {
  return new Date(ms).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** The reading column (§6.1 A10). Everything the transcript owns sits inside it. */
const COLUMN = "mx-auto w-full max-w-[46rem]";

export function ChatPanel({
  ns,
  name,
  ready,
  memoryBound,
  onTraced,
  audience = "operator",
  frame = "panel",
}: {
  ns: string;
  name: string;
  ready: boolean;
  memoryBound: boolean;
  onTraced: (traceId: string) => void;
  audience?: ChatAudience;
  frame?: ChatFrame;
}) {
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
  const endUser = audience === "end-user";
  const bare = frame === "bare";
  // Dev-mode (ADR 0021) has no cluster run store — only /api/invoke is served there; a real
  // cluster gets the durable run path (ADR 0093 §2). Gate on the server-confirmed probe.
  const devMode = useDevMode();
  const [conversationId, setConversationId] = React.useState(newConversationId);
  const [turns, setTurns] = React.useState<ChatTurn[]>([]);
  const [draft, setDraft] = React.useState("");
  // Inline consent (ADR 0031, m26.2) — a message whose tool call needs the user's own
  // credential surfaces a "Connect <server>" CTA in that agent turn.
  const [connecting, setConnecting] = React.useState<string | null>(null);
  const [connectError, setConnectError] = React.useState<string | null>(null);
  // A same-tab (popup-blocked) MCP OAuth return the boot handler stashed (DX-6) — surfaced so
  // consent-on-a-blocked-popup doesn't end in silence on the chat surface either.
  const [oauthReturn, setOauthReturn] = React.useState<McpOAuthPopupMessage | null>(null);
  const idRef = React.useRef(0);
  const scrollRef = React.useRef<HTMLDivElement>(null);
  // Teardown for an active inline-consent wait (OTH-2): the `message` listener + popup-close
  // poll leak past this component if the user closes the chat with the consent popup still open.
  const connectCleanupRef = React.useRef<(() => void) | null>(null);
  // The agent turn the durable run currently projects into — the newest pending turn. The hook
  // holds ONE run's state; this ref maps it onto the right turn as tokens stream / it finalizes.
  const activeTurnRef = React.useRef<number | null>(null);

  // The durable run engine (ADR 0093) — shared with the Playground. Its callbacks project the
  // finalized run onto the active turn (traceId, consent CTA, hold, forbidden). Streamed tokens
  // are projected via the state-effect below. A forbidden run reprobes the RBAC-aware chrome.
  const run = useDurableRun({
    onForbidden: () => reprobe(),
    onFinalized: (detail) => {
      const turnId = activeTurnRef.current;
      if (turnId === null) return;
      const ra = detail.requiresAction;
      const consentRequired =
        ra?.kind === "consent_required" ? ra.servers ?? [] : undefined;
      // A run that paused on a DECISION is held, not failed and not finished. It is a
      // different gate from consent (which the reader can clear themselves) and it says so.
      const heldFor =
        ra?.kind === "approval" || ra?.kind === "plan_approval" ? ra.kind : undefined;
      const lastMessage = detail.messages?.length
        ? detail.messages[detail.messages.length - 1].content
        : "";
      const failed = detail.status === "failed";
      setTurns((ts) =>
        ts.map((t) =>
          t.id === turnId
            ? {
                ...t,
                pending: false,
                runId: detail.id,
                // Keep the streamed text if the finalized message is empty (e.g. a
                // consent pause carries no assistant content yet).
                text: lastMessage || t.text,
                traceId: detail.traceId,
                consentRequired: consentRequired && consentRequired.length > 0 ? consentRequired : undefined,
                heldFor,
                error: failed ? detail.error || "The run failed." : undefined,
                forbidden: false,
              }
            : t,
        ),
      );
    },
    onFinalizeError: (err) => projectRunError(activeTurnRef.current, err),
  });

  // Project the durable run's LIVE stream (tokens + pre-stream failures) onto the active turn.
  // This is the streaming UX upgrade: tokens land in the turn's `text` as they arrive (the
  // pending dots stay until the first token). A pre-stream 403/error is projected too.
  React.useEffect(() => {
    const turnId = activeTurnRef.current;
    if (turnId === null) return;
    if (run.status === "streaming") {
      setTurns((ts) =>
        ts.map((t) =>
          t.id === turnId
            ? {
                ...t,
                // Keep the pending dots until the first token, then show the live text.
                pending: run.responseText === "",
                text: run.responseText,
                error: undefined,
                forbidden: false,
              }
            : t,
        ),
      );
    } else if (run.status === "forbidden" || run.status === "error") {
      setTurns((ts) =>
        ts.map((t) =>
          t.id === turnId
            ? {
                ...t,
                pending: false,
                error: run.error ?? "run failed",
                forbidden: run.status === "forbidden",
              }
            : t,
        ),
      );
    }
    // Only react to the durable-run stream/failure transitions.
  }, [run.status, run.responseText, run.error]);

  const busy = connecting !== null || turns.some((t) => t.pending);

  React.useEffect(() => {
    // Keep the newest turn in view as the thread grows.
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [turns]);

  // On mount: surface a same-tab (popup-blocked) MCP OAuth outcome the boot handler stashed
  // (DX-6). Unmount cleanup (OTH-2): clear any in-flight consent wait so its listener + interval
  // don't outlive the chat.
  React.useEffect(() => {
    setOauthReturn(readMcpOAuthReturn());
    return () => connectCleanupRef.current?.();
  }, []);

  function nextId(): number {
    idRef.current += 1;
    return idRef.current;
  }

  // projectRunError writes a thrown error onto the given turn (used by the dev-mode /invoke
  // fallback and by a finalize-time failure). A forbidden error reprobes the chrome.
  function projectRunError(turnId: number | null, err: unknown) {
    if (turnId === null) return;
    const forbidden = err instanceof ApiError && err.isForbidden;
    if (forbidden) reprobe();
    setTurns((ts) =>
      ts.map((t) =>
        t.id === turnId
          ? {
              ...t,
              pending: false,
              error: err instanceof Error ? err.message : "run failed",
              forbidden,
            }
          : t,
      ),
    );
  }

  // runTurn drives ONE turn's invocation, threading the conversationId. On a real cluster this
  // is a DURABLE run (ADR 0093): createRun → stream → finalize, so the turn becomes an
  // observable run. In dev-mode (no run store) it falls back to the synchronous /invoke. The
  // active-turn ref lets the durable engine's stream/finalize callbacks target THIS turn.
  async function runTurn(text: string, agentTurnId: number) {
    activeTurnRef.current = agentTurnId;
    setTurns((ts) =>
      ts.map((t) =>
        t.id === agentTurnId
          ? { ...t, pending: true, error: undefined, heldFor: undefined }
          : t,
      ),
    );
    if (devMode) {
      // Dev-mode fallback (ADR 0093 §2): the old synchronous /invoke — unchanged.
      try {
        const res = await api.invoke({
          agent: name, // TODO(ADR 0093): handoff-pointer — later turns could omit agent so handoffs stick
          namespace: ns,
          input: { input: text },
          conversationId,
        });
        setTurns((ts) =>
          ts.map((t) =>
            t.id === agentTurnId
              ? {
                  ...t,
                  pending: false,
                  text: res.response,
                  traceId: res.traceId,
                  consentRequired: res.consentRequired,
                }
              : t,
          ),
        );
      } catch (err) {
        projectRunError(agentTurnId, err);
      }
      return;
    }
    // Cluster: a durable, observable run (ADR 0093). Pin the agent on EVERY turn (current
    // behavior — keep it). TODO(ADR 0093): handoff-pointer — later turns could omit agent so
    // a mid-chat handoff sticks (deliberately NOT done here — it risks the single-agent chatbox).
    await run.start({
      agent: name,
      namespace: ns,
      input: { input: text },
      conversationId,
    });
  }

  async function send() {
    const text = draft.trim();
    if (!text || busy) return;
    const userId = nextId();
    const agentId = nextId();
    const at = Date.now();
    setTurns((ts) => [
      ...ts,
      { id: userId, role: "user", text, at },
      { id: agentId, role: "agent", text: "", at, pending: true, sourceText: text },
    ]);
    setDraft("");
    await runTurn(text, agentId);
  }

  // retryTurn re-sends the message that produced a failed turn, IN PLACE (§7 A10: a failed
  // send is a row in the transcript with a way to try again — the transcript is never blanked,
  // because everything above the failure is still true).
  async function retryTurn(t: ChatTurn) {
    if (busy || !t.sourceText) return;
    await runTurn(t.sourceText, t.id);
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    // Enter sends; Shift+Enter inserts a newline (the chat convention).
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  // resumeTurn continues the turn's SAME durable run after the user connects their account
  // (ADR 0093): RESUME the run (POST /api/runs/{id}/resume) + re-open the stream — NOT a new
  // createRun, so the connect-and-continue stays ONE run. In dev-mode there is no run to
  // resume, so we re-invoke the same message in place (the old behavior).
  async function resumeTurn(text: string, agentTurnId: number, runId?: string) {
    activeTurnRef.current = agentTurnId;
    setTurns((ts) =>
      ts.map((t) => (t.id === agentTurnId ? { ...t, pending: true, error: undefined } : t)),
    );
    if (devMode || !runId) {
      // Dev-mode (or a turn with no durable run) → re-invoke the same message in place.
      try {
        const res = await api.invoke({
          agent: name,
          namespace: ns,
          input: { input: text },
          conversationId,
        });
        setTurns((ts) =>
          ts.map((t) =>
            t.id === agentTurnId
              ? {
                  ...t,
                  pending: false,
                  text: res.response,
                  traceId: res.traceId,
                  consentRequired: res.consentRequired,
                }
              : t,
          ),
        );
      } catch (err) {
        projectRunError(agentTurnId, err);
      }
      return;
    }
    // Cluster: resume the SAME run (consent connect-and-continue → no decision).
    await run.resume();
  }

  // onConnect runs the INLINE per-user consent (ADR 0031): begin the OAuth grant for the
  // named server, open the provider consent in a POPUP so the chat stays on screen, then
  // RESUME the same run (ADR 0093) when it completes so the fresh credential is injected. The
  // token never touches the SPA (server-side exchange). Popup blocked → redirect fallback.
  async function onConnect(server: string, text: string, agentTurnId: number, runId?: string) {
    setConnectError(null);
    setConnecting(server);
    let authorizationURL: string;
    try {
      const res = await api.beginMcpGrant({
        server,
        namespace: ns,
        // Scope the grant to THIS agent's trust boundary (its registry, or itself) — the
        // consent empowers this agent's team, not every agent the user owns (ADR 0033).
        agent: name,
        redirectUri: `${window.location.origin}/api/mcp/oauth/callback`,
      });
      authorizationURL = res.authorizationURL;
    } catch (err) {
      setConnecting(null);
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setConnectError(err instanceof Error ? err.message : "Couldn't start the connect flow.");
      return;
    }

    const popup = window.open(
      authorizationURL,
      "ctxmesh-oauth-connect",
      "width=520,height=680,menubar=no,toolbar=no",
    );
    if (!popup) {
      // Popup blocked → same-tab redirect, but validate the URL first (DX-6) so a bad
      // authorizationURL can't hijack the tab.
      if (isValidHttpUrl(authorizationURL)) window.location.href = authorizationURL;
      return;
    }

    let done = false;
    let poll = 0;
    function teardown() {
      window.removeEventListener("message", onMessage);
      window.clearInterval(poll);
      connectCleanupRef.current = null;
    }
    function finish() {
      if (done) return;
      done = true;
      teardown();
      setConnecting(null);
      void resumeTurn(text, agentTurnId, runId); // continue the SAME run with the fresh credential
    }
    function onMessage(e: MessageEvent) {
      // Accept the relay from our own origin OR the canonical console origin (ADR 0040) — the popup's
      // callback runs at the console origin, which is a different origin when the chatbox is served at
      // an agent hostname. Any other origin is ignored.
      const callbackOrigin = mcpCallbackOrigin();
      if (e.origin !== window.location.origin && !(callbackOrigin && e.origin === callbackOrigin)) return;
      const data = e.data as { type?: string } | null;
      if (data?.type === MCP_OAUTH_MESSAGE) finish();
    }
    window.addEventListener("message", onMessage);
    poll = window.setInterval(() => {
      if (popup.closed) finish();
    }, 700);
    connectCleanupRef.current = teardown; // let the unmount effect clear this wait (OTH-2)
  }

  function newChat() {
    run.reset();
    activeTurnRef.current = null;
    setTurns([]);
    setConversationId(newConversationId());
    setConnectError(null);
    setDraft("");
  }

  // ── Governance inside a turn (§5.26): one Timeline step, never a bubble ────

  /** The consent gate: the reader can clear this one themselves, right here. */
  function consentStep(t: ChatTurn): TimelineStep {
    return {
      id: `consent-${t.id}`,
      tone: "hold",
      // Wrapped so the step's title is its own node: the tone prefix a screen
      // reader hears must not become part of the visible sentence.
      title: (
        <span className="font-medium">Connect your account to continue</span>
      ),
      detail: (
        <>
          <span className="block">
            This message needs your own credentials — the assistant is not
            allowed to borrow anyone else&rsquo;s. Connect, and it carries on
            from where it stopped. Nothing has been lost while it waits.
          </span>
          <span className="mt-2.5 flex flex-wrap gap-2">
            {(t.consentRequired ?? []).map((server) => (
              <Button
                key={server}
                size="sm"
                variant="outline"
                disabled={connecting !== null}
                onClick={() => void onConnect(server, t.sourceText ?? "", t.id, t.runId)}
                data-testid={`connect-${server}`}
              >
                {connecting === server ? "Connecting…" : `Connect ${server}`}
              </Button>
            ))}
          </span>
          {connectError && (
            <span
              className="mt-2 block text-destructive"
              data-testid="connect-error"
            >
              {connectError}
            </span>
          )}
        </>
      ),
    };
  }

  /**
   * The decision gate: somebody ELSE has to say yes. The reader cannot clear it
   * and must not be handed a control that pretends otherwise — so the operator
   * gets a link into the run and the end user gets the truth and nothing to press.
   */
  function holdStep(t: ChatTurn): TimelineStep {
    const plan = t.heldFor === "plan_approval";
    return {
      id: `hold-${t.id}`,
      tone: "hold",
      title: (
        <span className="font-medium">
          {plan
            ? "A person has to approve the plan before this goes on"
            : "A person has to approve this before it goes on"}
        </span>
      ),
      detail: (
        <>
          <span className="block">
            It is held, not failed: nothing has been done yet and the answer
            keeps its place in the queue until someone decides.
          </span>
          {!endUser && t.runId && (
            <span className="mt-2.5 block">
              <NextStepLink
                label="Review the hold"
                to={`/runs/${encodeURIComponent(t.runId)}`}
                ariaLabel={`Review the hold on run ${t.runId}`}
              />
            </span>
          )}
        </>
      ),
    };
  }

  function renderTurn(t: ChatTurn) {
    if (t.role === "user") {
      return (
        <div key={t.id} className="flex flex-col items-end gap-1" data-testid="chat-turn-user">
          <span className="px-1 font-mono text-2xs uppercase tracking-wide text-faint">
            You
          </span>
          {/* Pine TINT, not solid pine: solid pine is the colour of something
              you can press (§2.1/§2.3), and a message is not a control. */}
          <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-lg bg-accent px-4 py-2.5 text-md text-accent-foreground">
            {t.text}
          </div>
        </div>
      );
    }
    const traceId = t.traceId;
    const held = t.heldFor !== undefined;
    const consenting = !!t.consentRequired && t.consentRequired.length > 0;
    return (
      <div key={t.id} className="flex flex-col gap-1.5" data-testid="chat-turn-agent">
        <div className="flex items-center gap-2 px-1 font-mono text-2xs uppercase tracking-wide text-faint">
          <span className="truncate">{name}</span>
          <span aria-hidden="true" className="text-ghost">
            ·
          </span>
          <span className="tabular-nums">{clock(t.at)}</span>
          {held && (
            <Badge variant="hold" className="ml-1">
              Held
            </Badge>
          )}
        </div>
        <div className="min-w-0 space-y-3 rounded-lg border border-border bg-card px-4 py-3.5">
          {t.pending ? (
            <span
              className="inline-flex items-center gap-1 py-1"
              data-testid="chat-pending"
              aria-label={`${name} is thinking`}
            >
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost [animation-delay:-0.25s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost [animation-delay:-0.12s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost" />
            </span>
          ) : t.error ? (
            t.forbidden ? (
              <ForbiddenInline
                // The turn block is already a frame; a second bordered box
                // inside it is two frames saying one thing.
                className="border-0 bg-transparent px-0 py-2"
                title={
                  endUser
                    ? "This assistant isn't yours to talk to"
                    : "Not allowed to run this agent"
                }
                description={
                  endUser
                    ? "Ask whoever runs it for access. Nothing you have already asked is affected."
                    : "Your account can't invoke agents in this cluster."
                }
                detail={t.error}
              />
            ) : (
              // §7 A10: a failed send is a row IN the transcript, with a way to
              // try again. Everything above it still stands.
              <div className="space-y-2" data-testid="chat-turn-error">
                <p className="text-sm text-destructive" role="alert">
                  {endUser
                    ? "That message didn't get an answer. Nothing above has changed."
                    : "That turn didn't complete."}
                </p>
                {/* The server's own words help whoever runs this and are noise
                    — sometimes leaked internals — to whoever came to ask a
                    question. The reader outside gets the sentence and the way
                    forward; the operator also gets the reason. */}
                {!endUser && (
                  <p className="break-words font-mono text-xs text-faint">{t.error}</p>
                )}
                {t.sourceText && (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void retryTurn(t)}
                    data-testid={`chat-retry-${t.id}`}
                  >
                    Try again
                  </Button>
                )}
              </div>
            )
          ) : (
            <>
              {(consenting || held) && (
                <div data-testid={consenting ? "chat-consent" : "chat-hold"}>
                  <Timeline
                    label={`What this answer is waiting on, from ${name}`}
                    steps={[consenting ? consentStep(t) : holdStep(t)]}
                  />
                </div>
              )}
              {t.text && (
                // ChatMarkdown sets its body at the 11px mono-meta step, which is
                // the register for a table cell and not for something a person
                // reads a paragraph of. Lifted here, at the one call site that is
                // a reading surface, rather than by editing the shared renderer.
                <div className="[&_li]:text-md [&_p]:text-md">
                  <ChatMarkdown>{extractAgentOutput(t.text)}</ChatMarkdown>
                </div>
              )}
              {traceId && !endUser && (
                // Operator vocabulary and an operator destination. At an agent
                // origin the console route does not exist, so the link is not
                // offered rather than being offered and broken.
                <button
                  type="button"
                  onClick={() => onTraced(traceId)}
                  className="block truncate font-mono text-xs text-faint hover:text-primary hover:underline"
                  data-testid="open-trace"
                >
                  trace {traceId} →
                </button>
              )}
            </>
          )}
        </div>
      </div>
    );
  }

  // ── The notes that sit above the composer ─────────────────────────────────

  const notReadyNote = !ready && turns.length === 0 && (
    // A pre-flight heads-up only: once a turn has been sent, the reply (or the
    // error turn) is the real signal. Converging is not a crossed bound, so this
    // wears the progressing tag rather than the amber it used to (§2.2/§2.5).
    <div
      className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border border-border bg-surface-2 px-4 py-3 text-sm text-secondary-foreground"
      data-testid="chat-not-ready-note"
    >
      <Badge variant="progressing">Starting up</Badge>
      <span className="min-w-0">
        {endUser
          ? "This assistant is still starting up — a message may not get through until it is."
          : "The agent isn't Ready yet — a message may fail until it comes up."}
      </span>
    </div>
  );

  const memoryNote = !memoryBound && (
    // §7 A10's backend-cannot-answer state: an agent with no memory binding is
    // not an agent whose memory is empty, and the reader finds out here rather
    // than by being forgotten mid-conversation.
    <div data-testid="chat-memory-note">
      <QuietNote>
        {endUser
          ? "This assistant has no memory. Each message is answered on its own — it won't remember what you said earlier in this chat."
          : "No memory is bound to this agent, so each message is answered on its own — nothing from an earlier turn is carried forward."}
      </QuietNote>
    </div>
  );

  // ── The composer ──────────────────────────────────────────────────────────

  const composer = (
    <div className={cn("border-t border-border", bare ? "bg-card" : undefined)}>
      <div className={cn(bare ? cn(COLUMN, "px-4 py-4 sm:px-6") : "p-4")}>
        {(notReadyNote || memoryNote) && (
          <div className="mb-3 space-y-2">
            {notReadyNote}
            {memoryNote}
          </div>
        )}
        <div className="flex items-end gap-2">
          <Textarea
            aria-label="Chat message"
            rows={2}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder={`Message ${name || "the agent"}…`}
            className="resize-none text-md"
            data-testid="chat-input"
          />
          <Button
            onClick={() => void send()}
            disabled={busy || draft.trim() === ""}
            data-testid="chat-send"
            className="h-11 shrink-0"
          >
            <Send className="h-4 w-4" />
            Send
          </Button>
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
          <p className="text-xs text-faint">
            Enter to send · Shift+Enter for a new line
          </p>
          {bare && turns.length > 0 && (
            <button
              type="button"
              onClick={newChat}
              className="ml-auto text-xs text-primary hover:underline"
              data-testid="chat-new"
            >
              Start a new chat
            </button>
          )}
        </div>
      </div>
    </div>
  );

  // ── The transcript ────────────────────────────────────────────────────────

  const transcript = (
    <div
      ref={scrollRef}
      className={cn(
        "min-h-0 flex-1 overflow-y-auto",
        bare ? "px-4 py-6 sm:px-6" : "px-4 py-4",
      )}
      style={bare ? undefined : { minHeight: "18rem" }}
      data-testid="chat-thread"
    >
      <div className={cn("flex min-h-full flex-col gap-6", bare && COLUMN)}>
        {turns.length === 0 ? (
          // §7 A10: the empty state teaches. The agent carries no greeting in
          // its payload today, so this is the spec's stated fallback — not an
          // invented one attributed to the assistant.
          <div
            className="m-auto max-w-sm text-center"
            data-testid="chat-empty"
          >
            <p className="font-serif text-lg font-medium">
              Say something to {name || "the agent"} to start.
            </p>
            <p className="mt-1.5 text-sm text-faint">
              {memoryBound
                ? "It remembers what you say earlier in this chat."
                : "Each message is answered on its own — it keeps no memory of this chat."}
            </p>
          </div>
        ) : (
          turns.map(renderTurn)
        )}
      </div>
    </div>
  );

  // ── The MCP-consent same-tab return (DX-6) ────────────────────────────────

  const oauthBanner = oauthReturn && (
    <div
      data-testid="mcp-oauth-return"
      className={cn(
        "border px-4 py-2.5 text-sm",
        oauthReturn.error
          ? "border-destructive/40 bg-destructive-surface text-destructive"
          : "border-success/40 bg-success-surface text-success",
      )}
    >
      {oauthReturn.error
        ? `Couldn't connect ${oauthReturn.server || "the server"}: ${oauthReturn.error}`
        : `Connected ${oauthReturn.server || "your account"} — send your message again to continue.`}
      <button
        type="button"
        className="ml-2 underline"
        onClick={() => setOauthReturn(null)}
      >
        dismiss
      </button>
    </div>
  );

  // ── The read-only gate ────────────────────────────────────────────────────

  const readOnly = (
    <div
      className={cn(bare ? cn(COLUMN, "px-4 py-8 sm:px-6") : "p-4")}
      data-testid="chat-readonly-note"
    >
      <QuietNote
        title={
          endUser
            ? "This assistant isn't open to you for chatting."
            : "You have read-only access."
        }
      >
        {endUser
          ? "Your sign-in worked — it just doesn't carry permission to send this assistant a message. Ask whoever runs it for access."
          : "Chatting with an agent needs create permission on AgentDeployments. Ask an admin for a role that can create agentdeployments."}
      </QuietNote>
    </div>
  );

  const body = canRun ? (
    <>
      {oauthBanner}
      {transcript}
      {composer}
    </>
  ) : (
    <>
      {oauthBanner}
      {readOnly}
    </>
  );

  if (bare) {
    return (
      <div className="flex min-h-0 flex-1 flex-col" data-testid="chat-panel">
        {body}
      </div>
    );
  }

  return (
    <div
      className="flex h-full min-h-0 flex-col rounded-lg border border-border bg-card"
      data-testid="chat-panel"
    >
      <div className="flex items-center gap-3 border-b border-border px-5 py-4">
        <div
          aria-hidden="true"
          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-sm bg-primary font-serif text-base font-medium text-primary-foreground"
        >
          {(name || "a").charAt(0).toUpperCase()}
        </div>
        <div className="min-w-0">
          <p className="font-serif text-lg font-medium tracking-snug">Chat</p>
          <p className="truncate font-mono text-2xs uppercase tracking-wide text-faint">
            {name}
          </p>
        </div>
        {turns.length > 0 && (
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            onClick={newChat}
            data-testid="chat-new"
          >
            New chat
          </Button>
        )}
      </div>
      {body}
    </div>
  );
}
