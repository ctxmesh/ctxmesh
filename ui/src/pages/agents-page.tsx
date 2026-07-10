import { useCallback, useEffect, useState } from "react";
import { RefreshCw } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { api, type AgentSummary } from "@/lib/api";

// AgentsPage — the M12.4 FOUNDATION PROOF page: it calls the Go BFF's
// GET /api/agents (client-go → AgentDeployments, RBAC-scoped, creds server-side)
// and renders the result on-theme. "No agents" is a valid, expected state. This
// proves the full seam: Vite build → static assets → Go BFF → client-go. The
// rich dashboard/topology is m12.5; this page is the wiring proof only.
type LoadState =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ready"; agents: AgentSummary[] };

export function AgentsPage() {
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  const load = useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });
    api
      .listAgents(signal)
      .then((res) => setState({ kind: "ready", agents: res.agents }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        const message = err instanceof Error ? err.message : "request failed";
        setState({ kind: "error", message });
      });
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Agents</h2>
          <p className="text-sm text-muted-foreground">
            AgentDeployments listed via the BFF (client-go, RBAC-scoped).
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => load()}
          disabled={state.kind === "loading"}
        >
          <RefreshCw className="h-4 w-4" />
          Refresh
        </Button>
      </div>

      {state.kind === "loading" && (
        <Card>
          <CardContent className="py-10 text-center text-sm text-muted-foreground">
            Loading agents…
          </CardContent>
        </Card>
      )}

      {state.kind === "error" && (
        <Card>
          <CardContent className="py-10 text-center text-sm text-destructive">
            Failed to load agents: {state.message}
          </CardContent>
        </Card>
      )}

      {state.kind === "ready" && state.agents.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center">
            <p className="text-sm font-medium">No agents yet</p>
            <p className="mt-1 text-sm text-muted-foreground">
              The BFF returned an empty list — the client-go seam works.
            </p>
          </CardContent>
        </Card>
      )}

      {state.kind === "ready" && state.agents.length > 0 && (
        <div className="grid gap-4">
          {state.agents.map((agent) => (
            <Card key={`${agent.namespace}/${agent.name}`}>
              <CardHeader className="flex-row items-center justify-between space-y-0">
                <div>
                  <CardTitle className="text-base">{agent.name}</CardTitle>
                  <CardDescription>{agent.namespace}</CardDescription>
                </div>
                <Badge variant={agent.ready ? "success" : "warning"}>
                  {agent.phase || (agent.ready ? "Ready" : "Pending")}
                </Badge>
              </CardHeader>
              <CardContent>
                <p className="font-mono text-xs text-muted-foreground">
                  {agent.image || "—"}
                </p>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
