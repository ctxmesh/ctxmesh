import * as React from "react";
import { AlertTriangle, MessageSquare, Send } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { ChatMarkdown } from "@/components/chat-markdown";
import { ForbiddenInline } from "@/components/kit";
import { api, ApiError } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useDevMode } from "@/lib/dev-mode";
import { useDurableRun } from "@/lib/use-durable-run";
import { RES_AGENTS } from "@/lib/nav";
import { extractAgentOutput } from "@/lib/agent-output";
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

// ChatPanel — a turn-by-turn chat with a deployed agent, threaded on the framework's OWN
// conversationId → the memory plane: one stable id per chat session lets a memory-bound
// agent hold context across turns (state-layer.md).
//
// Under the hood each turn is now a DURABLE, observable run (ADR 0093): createRun →
// openRunStream → finalize (via getRun), exactly as the Playground does — so every chat
// turn appears in the Runs list, loads its native trace, is cost-attributed, and is
// shareable/approvable. Tokens stream LIVE into the turn (a UX upgrade over the old
// wait-for-the-whole-response). Session threading is preserved by forwarding the shared
// X-Conversation-Id on the create path (the isolation story is literally true — two chat
// sessions are two distinct runs sharing a conversation id). Each agent turn keeps the
// trace-id link (opens the run inspector ON DEMAND) and the inline per-user Connect banner
// (ADR 0031) — connecting now RESUMES the same run rather than firing a second invisible call.
//
// Dev-mode (`ctxmesh dev --ui`) has no cluster/run store — only /api/invoke works there —
// so the OLD synchronous invoke path is kept as a fallback, gated on the GET /api/devmode probe
// (ADR 0093 §2). On a real cluster the durable path is used.
//
// Extracted from the agent-detail page (m37) so the SAME chat drives both the console's
// agent-detail chat tab AND the standalone per-agent chatbox surface.
type ChatTurn = {
  id: number;
  role: "user" | "agent";
  text: string;
  // agent turns carry the user input that produced them, so a post-connect resume
  // re-runs the SAME message without appending a duplicate user turn.
  sourceText?: string;
  // runId is the durable run backing this agent turn (ADR 0093) — the connect CTA RESUMES it
  // (same run) instead of creating a new one. Absent in the dev-mode /invoke fallback.
  runId?: string;
  traceId?: string;
  consentRequired?: string[];
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

export function ChatPanel({
  ns,
  name,
  ready,
  memoryBound,
  onTraced,
}: {
  ns: string;
  name: string;
  ready: boolean;
  memoryBound: boolean;
  onTraced: (traceId: string) => void;
}) {
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
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
  // finalized run onto the active turn (traceId, consent CTA, forbidden). Streamed tokens are
  // projected via the state-effect below. A forbidden run reprobes the RBAC-aware chrome.
  const run = useDurableRun({
    onForbidden: () => reprobe(),
    onFinalized: (detail) => {
      const turnId = activeTurnRef.current;
      if (turnId === null) return;
      const ra = detail.requiresAction;
      const consentRequired =
        ra?.kind === "consent_required" ? ra.servers ?? [] : undefined;
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
      ts.map((t) => (t.id === agentTurnId ? { ...t, pending: true, error: undefined } : t)),
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
    setTurns((ts) => [
      ...ts,
      { id: userId, role: "user", text },
      { id: agentId, role: "agent", text: "", pending: true, sourceText: text },
    ]);
    setDraft("");
    await runTurn(text, agentId);
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

  function renderTurn(t: ChatTurn) {
    if (t.role === "user") {
      return (
        <div key={t.id} className="flex flex-col items-end gap-1" data-testid="chat-turn-user">
          <span className="px-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            You
          </span>
          <div className="max-w-[80%] whitespace-pre-wrap break-words rounded-2xl rounded-br-sm bg-primary px-3.5 py-2 text-xs text-primary-foreground shadow-sm">
            {t.text}
          </div>
        </div>
      );
    }
    const traceId = t.traceId;
    return (
      <div key={t.id} className="flex flex-col items-start gap-1" data-testid="chat-turn-agent">
        <span className="flex items-center gap-1 px-1 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
          <MessageSquare className="h-3 w-3" />
          {name}
        </span>
        <div className="w-full max-w-[92%] space-y-2 rounded-2xl rounded-bl-sm border border-border bg-card px-3.5 py-2.5">
          {t.pending ? (
            <span
              className="inline-flex items-center gap-1 py-1"
              data-testid="chat-pending"
              aria-label="Agent is thinking"
            >
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40 [animation-delay:-0.25s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40 [animation-delay:-0.12s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground/40" />
            </span>
          ) : t.error ? (
            t.forbidden ? (
              <ForbiddenInline
                title="Not allowed to run this agent"
                description="Your account can't invoke agents in this cluster."
                detail={t.error}
              />
            ) : (
              <span className="text-xs text-destructive" role="alert" data-testid="chat-turn-error">
                {t.error}
              </span>
            )
          ) : (
            <>
              {t.consentRequired && t.consentRequired.length > 0 && (
                <div
                  className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-xs"
                  data-testid="chat-consent"
                >
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" />
                  <div className="space-y-2">
                    <p className="font-medium">Connect your account to continue</p>
                    <p className="text-muted-foreground">
                      This message needs your own credentials. Connect, and it continues
                      automatically.
                    </p>
                    <div className="flex flex-wrap gap-2">
                      {t.consentRequired.map((server) => (
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
                    </div>
                    {connectError && (
                      <p className="text-destructive" data-testid="connect-error">
                        {connectError}
                      </p>
                    )}
                  </div>
                </div>
              )}
              {t.text && <ChatMarkdown>{extractAgentOutput(t.text)}</ChatMarkdown>}
              {traceId && (
                <button
                  type="button"
                  onClick={() => onTraced(traceId)}
                  className="pt-0.5 font-mono text-[11px] text-muted-foreground hover:text-primary hover:underline"
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

  return (
    <div className="flex h-full flex-col rounded-lg border bg-card shadow-card" data-testid="chat-panel">
      <div className="flex items-center justify-between gap-2 border-b px-4 py-3">
        <div className="flex items-center gap-2">
          <span className="flex h-7 w-7 items-center justify-center rounded-full bg-primary/10 text-primary">
            <MessageSquare className="h-4 w-4" />
          </span>
          <div className="leading-tight">
            <p className="text-sm font-medium">Chat</p>
            <p className="text-[11px] text-muted-foreground" data-testid="chat-memory-hint">
              {!canRun
                ? "read-only access"
                : memoryBound
                  ? "keeps context across turns"
                  : "no memory — won't remember earlier turns"}
            </p>
          </div>
        </div>
        {turns.length > 0 && (
          <Button size="sm" variant="ghost" onClick={newChat} data-testid="chat-new">
            New chat
          </Button>
        )}
      </div>

      {oauthReturn && (
        <div
          data-testid="mcp-oauth-return"
          className={`mx-4 mt-3 rounded-md border px-3 py-2 text-xs ${
            oauthReturn.error
              ? "border-destructive/40 text-destructive"
              : "border-success/40 text-success"
          }`}
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
      )}

      {!canRun ? (
        <p
          className="m-4 rounded-md border border-dashed bg-card/40 px-3 py-2 text-xs text-muted-foreground"
          data-testid="chat-readonly-note"
        >
          You have read-only access — chatting with an agent requires create permission on
          AgentDeployments.
        </p>
      ) : (
        <>
          {/* A pre-flight warning only: once the user has sent a turn, the actual result (a reply or an
              error turn) is the real signal, so stop nagging. This also avoids a misleading "not Ready"
              over a WORKING answer when the ksvc Ready is a transient-false (a Knative revision-creation
              race leaves ConfigurationsReady=false while the latestReady revision already serves). */}
          {!ready && turns.length === 0 && (
            <p
              className="mx-4 mt-3 rounded-md border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning-foreground"
              data-testid="chat-not-ready-note"
            >
              The agent isn't Ready yet — a message may fail until it comes up.
            </p>
          )}

          <div
            ref={scrollRef}
            className="flex flex-1 flex-col gap-4 overflow-y-auto px-4 py-4"
            style={{ minHeight: "18rem" }}
            data-testid="chat-thread"
          >
            {turns.length === 0 ? (
              <div
                className="m-auto max-w-xs text-center text-xs text-muted-foreground"
                data-testid="chat-empty"
              >
                <MessageSquare className="mx-auto mb-2 h-6 w-6 opacity-40" />
                <p>Start a conversation with {name}.</p>
                <p className="mt-1 opacity-80">
                  {memoryBound
                    ? "It remembers earlier turns in this chat."
                    : "Each message is independent (no memory bound)."}
                </p>
              </div>
            ) : (
              turns.map(renderTurn)
            )}
          </div>

          <div className="border-t p-3">
            <div className="flex items-end gap-2">
              <Textarea
                aria-label="Chat message"
                rows={2}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={onKeyDown}
                placeholder="Message the agent…"
                className="resize-none text-xs"
                data-testid="chat-input"
              />
              <Button
                size="icon"
                onClick={() => void send()}
                disabled={busy || draft.trim() === ""}
                data-testid="chat-send"
                aria-label="Send message"
                className="h-9 w-9 shrink-0"
              >
                <Send className="h-4 w-4" />
              </Button>
            </div>
            <p className="mt-1.5 px-0.5 text-[10px] text-muted-foreground">
              Enter to send · Shift+Enter for a new line
            </p>
          </div>
        </>
      )}
    </div>
  );
}
