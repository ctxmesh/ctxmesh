import { useCallback, useEffect, useRef, useState } from "react";
import { Waypoints } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { api, ApiError, type AgentTeamSummary } from "@/lib/api";

// TeamsPage — the AgentTeam orchestration rosters (m64.11, ADR 0057).
//
// Read-only (caller-scoped, ADR 0011): each row is a team — its supervisor, the roster of summonable
// sub-agents, the resolved member readiness, and the spawn budget (fan-out / depth / total) that bounds
// its delegations. A team is authored via YAML/kubectl for now; the conversational "describe → team"
// builder is M71. A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// data-testid contract:
//   teams-page       — root container
//   teams-table      — the DataTable (aria-label="Agent teams")
//   team-row-{name}  — each row (via rowKey)

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; teams: AgentTeamSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function TeamsPage() {
  const [query, setQuery] = useState("");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listTeams(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", teams: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const all = loadState.kind === "ready" ? loadState.teams : [];
  const q = query.trim().toLowerCase();
  const teams = q ? all.filter((t) => t.name.toLowerCase().includes(q)) : all;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<AgentTeamSummary>[] = [
    {
      id: "name",
      header: "Team",
      cell: (t) => <span className="font-medium">{t.name}</span>,
    },
    {
      id: "supervisor",
      header: "Supervisor",
      cell: (t) => <span className="text-sm">{t.supervisor}</span>,
    },
    {
      id: "roster",
      header: "Roster",
      hideOnMobile: true,
      cell: (t) => (
        <span className="text-sm text-muted-foreground">
          {t.roster.length > 0 ? t.roster.map((r) => r.name).join(", ") : "—"}
        </span>
      ),
    },
    {
      id: "registry",
      header: "Registry",
      hideOnMobile: true,
      cell: (t) => <span className="text-sm text-muted-foreground">{t.registry}</span>,
    },
    {
      id: "budget",
      header: "Spawn budget",
      hideOnMobile: true,
      cell: (t) => (
        <span className="text-xs text-muted-foreground">
          fan-out {t.budget.maxFanOut} · depth {t.budget.maxSpawnDepth} · total{" "}
          {t.budget.maxTotalSpawns}
        </span>
      ),
    },
    {
      id: "ready",
      header: "Ready",
      cell: (t) =>
        t.ready ? (
          <Badge variant="success">ready</Badge>
        ) : (
          <Badge variant="warning" title={t.reason}>
            {t.reason || "not ready"}
          </Badge>
        ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="teams-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Agent Teams</h2>
        <p className="text-sm text-muted-foreground">
          Orchestration rosters — a supervisor summons the roster's sub-agents on demand (delegate_to),
          bounded by the spawn budget. Authored via YAML for now.
        </p>
      </div>

      <DataTable<AgentTeamSummary>
        columns={columns}
        rows={teams}
        rowKey={(t) => `${t.namespace}/${t.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter teams by name…"
        ariaLabel="Agent teams"
        empty={{
          icon: Waypoints,
          title: "No agent teams",
          description:
            "No AgentTeams defined yet. Apply an AgentTeam manifest (a supervisor + a roster of sub-agents) with kubectl to enable dynamic delegation; a describe-to-team builder arrives in a later milestone.",
        }}
      />
    </div>
  );
}
