import { useCallback, useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { MessageSquare } from "lucide-react";

import { ChatPanel } from "@/components/agent-chat";
import { api, ApiError, type AgentDetailResponse } from "@/lib/api";

// AgentChatboxPage — a standalone, CHROME-LESS chatbox for ONE agent (m37). Reached two ways with
// the SAME component: (1) a console-origin path /chat/:ns/:name (the agent comes from the URL); (2)
// the agent's OWN hostname (m37.3), where the BFF injects the agent via a <meta> tag and the app
// passes it as props. Either way it reuses the console's ChatPanel — the SAME memory-threaded invoke
// + inline MCP "Connect" consent flow — WITHOUT the console shell/nav. Auth is the same login (behind
// RequireAuth); the agent is pinned (URL or host), so there's no agent picker.

type Load =
  | { kind: "loading" }
  | { kind: "ready"; ready: boolean; memoryBound: boolean }
  | { kind: "error"; message: string };

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
    <div className="flex h-dvh flex-col bg-background">
      <header className="flex items-center gap-2 border-b px-4 py-3">
        <span className="flex h-8 w-8 items-center justify-center rounded-full bg-primary/10 text-primary">
          <MessageSquare className="h-4 w-4" />
        </span>
        <div className="leading-tight">
          <p className="text-sm font-semibold tracking-tight" data-testid="chatbox-agent">
            {name || "agent"}
          </p>
          <p className="text-[11px] text-muted-foreground">{ns}</p>
        </div>
      </header>
      <main className="mx-auto flex w-full max-w-3xl flex-1 flex-col overflow-hidden p-3 sm:p-4">
        {load.kind === "loading" ? (
          <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
            Loading…
          </div>
        ) : load.kind === "error" ? (
          <div
            className="flex flex-1 items-center justify-center text-sm text-destructive"
            role="alert"
          >
            {load.message}
          </div>
        ) : (
          <ChatPanel
            ns={ns}
            name={name}
            ready={load.ready}
            memoryBound={load.memoryBound}
            onTraced={onTraced}
          />
        )}
      </main>
    </div>
  );
}
