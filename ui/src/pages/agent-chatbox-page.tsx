import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { ChatPanel } from "@/components/agent-chat";
import { ErrorState, Skeleton } from "@/components/kit";
import { api, ApiError, type AgentDetailResponse } from "@/lib/api";

// AgentChatboxPage — one agent, no console (m37; M151 §6.1 archetype A10, §6.2
// row `agent-chatbox-page.tsx`: "chrome-less; both origins").
//
// The SAME component answers two doors:
//
//   1. `/chat/:ns/:name` on the console origin — an operator opening one agent's
//      chat without the shell around it. The agent comes from the URL.
//   2. the agent's OWN hostname (m37.3) — where the BFF injects the agent as a
//      `<meta name="agent-pin">` and App.tsx mounts this page as the whole app.
//      The agent comes in as props.
//
// ── THE SECOND DOOR IS THE ONLY SURFACE OUTSIDE PEOPLE SEE ──────────────────
//
// So it does not get to inherit the console's vocabulary. A workspace is a
// Kubernetes namespace; it is a fact about where the platform put something and
// it means nothing to a person who came here to ask a question. It renders on
// the console door and is absent on the agent's own, and the same switch picks
// the copy inside the chat (ChatPanel's `audience`).
//
// There is no shell here, so this page is also its own masthead, its own error
// state and its own empty state at every width down to 360 — nothing above it
// will catch a layout that does not fit.
//
// data-testid contract:
//   chatbox-page     — root
//   chatbox-agent    — the assistant's name in the masthead
//   chatbox-loading  — the three message-shaped bars (§7 A10)
//   chatbox-error    — the agent-not-found state

type Load =
  | { kind: "loading" }
  | { kind: "ready"; ready: boolean; memoryBound: boolean }
  | { kind: "error"; message: string };

/**
 * True when this origin belongs to the agent itself (m37.3) — the BFF injects
 * the pin meta only at an agent hostname. It is the same signal App.tsx uses to
 * decide the console is unreachable here, and the same one the login card uses
 * to choose its register, so all three agree on who is reading.
 */
function isAgentOrigin(): boolean {
  const content =
    document
      .querySelector('meta[name="agent-pin"]')
      ?.getAttribute("content")
      ?.trim() ?? "";
  const slash = content.indexOf("/");
  return slash > 0 && slash < content.length - 1;
}

// Props override the URL params — set when the agent is pinned by the HOST (m37.3), not the path.
export function AgentChatboxPage({
  ns: nsProp,
  name: nameProp,
}: { ns?: string; name?: string } = {}) {
  const params = useParams();
  const ns = nsProp ?? params.ns ?? "default";
  const name = nameProp ?? params.name ?? "";
  const navigate = useNavigate();
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  const endUser = useMemo(isAgentOrigin, []);

  useEffect(() => {
    const controller = new AbortController();
    api
      .agentDetail(ns, name, controller.signal)
      .then((d: AgentDetailResponse) =>
        setLoad({
          kind: "ready",
          ready: d.ready,
          memoryBound: d.bindings.some((b) => b.kind === "memory"),
        }),
      )
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // A genuinely-missing agent is an honest error; any other lookup hiccup shouldn't
        // block chatting, so default to ready + no-memory and let the invoke be the real gate.
        if (err instanceof ApiError && err.isNotFound) {
          setLoad({ kind: "error", message: `Agent "${ns}/${name}" not found.` });
        } else {
          setLoad({ kind: "ready", ready: true, memoryBound: false });
        }
      });
    return () => controller.abort();
  }, [ns, name]);

  const onTraced = useCallback(
    (traceId: string) => navigate(`/traces/${encodeURIComponent(traceId)}`),
    [navigate],
  );

  return (
    <div className="flex h-dvh flex-col bg-background" data-testid="chatbox-page">
      {/* The masthead. It is the whole of this page's chrome, so it carries
          identity and nothing else — no nav, no search, no actions. */}
      <header className="border-b border-border bg-card">
        <div className="mx-auto flex w-full max-w-[46rem] items-center gap-3 px-4 py-4 sm:px-6">
          <div
            aria-hidden="true"
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-sm bg-primary font-serif text-lg font-medium text-primary-foreground"
          >
            {(name || "a").charAt(0).toUpperCase()}
          </div>
          <div className="min-w-0">
            <p
              className="truncate font-serif text-lg font-medium tracking-snug"
              data-testid="chatbox-agent"
              title={name}
            >
              {name || "agent"}
            </p>
            {/* The one line each reader actually needs. An operator needs to know
                WHICH copy of this agent they are talking to; the person outside
                needs the promise the sign-in card already made to them. */}
            {/* Withheld when the assistant did not resolve: promising that
                every answer is sourced, above a panel saying there is nothing
                here to answer, is a claim about something that does not exist. */}
            {load.kind !== "error" && (
              <p className="truncate text-xs text-faint">
                {endUser ? "Every answer shows where it came from." : ns}
              </p>
            )}
          </div>
        </div>
      </header>

      {load.kind === "loading" ? (
        // §7 A10: three message-shaped bars — the shape the transcript will
        // take, so nothing jumps when it arrives.
        <div
          className="mx-auto w-full max-w-[46rem] px-4 py-6 sm:px-6"
          role="status"
          aria-busy="true"
          aria-label="Loading the conversation"
          data-testid="chatbox-loading"
        >
          <Skeleton decorative className="ml-auto h-12 w-2/5 rounded-lg" />
          <Skeleton decorative className="mt-6 h-20 w-full rounded-lg" />
          <Skeleton decorative className="ml-auto mt-6 h-12 w-1/3 rounded-lg" />
        </div>
      ) : load.kind === "error" ? (
        <div className="mx-auto w-full max-w-[46rem] px-4 py-10 sm:px-6" data-testid="chatbox-error">
          <ErrorState
            title={
              endUser
                ? "This assistant isn't here"
                : `Agent "${ns}/${name}" not found.`
            }
            description={
              endUser
                ? "Nothing answers at this address. If you followed a link to it, whoever sent you the link is the person who can say where it went."
                : "Nothing on this cluster answers to that name in that workspace. It may have been deleted, or the link may name an agent from another cluster."
            }
            onRetry={() => window.location.reload()}
            retryLabel="Try again"
          />
        </div>
      ) : (
        <ChatPanel
          ns={ns}
          name={name}
          ready={load.ready}
          memoryBound={load.memoryBound}
          onTraced={onTraced}
          audience={endUser ? "end-user" : "operator"}
          frame="bare"
        />
      )}
    </div>
  );
}
