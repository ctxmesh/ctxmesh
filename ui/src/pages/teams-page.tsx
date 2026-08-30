import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Plus, Waypoints } from "lucide-react";

import { DataTable, StatusBadge, humanizeStatusReason, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api, ApiError, type AgentTeamSummary, type AgentTeamRoster } from "@/lib/api";

// TeamsPage — the AgentTeam orchestration rosters (m64.11, ADR 0057).
//
// Read-only (caller-scoped, ADR 0011): each row is a team — its supervisor, the roster of summonable
// sub-agents, the resolved member readiness, and the spawn budget (fan-out / depth / total) that bounds
// its delegations. A team is authored via YAML/kubectl for now; the conversational "describe → team"
// builder is M71. A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// Row-click opens an inline detail panel (I3 m76.6): per-member name → agentRef · description +
// team-level readiness + the not-ready reason. Mirrors the tenants-page pattern.
//
// data-testid contract:
//   teams-page    — root container
//   teams-table   — the DataTable (aria-label="Agent teams")
//   team-detail   — the inline detail panel (row-click)

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; teams: AgentTeamSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

// The inline detail panel for a selected team (I3 m76.6).
// Mirrors the tenants-page detail pattern: an inline card below the table,
// no secondary fetch needed (all data is in the list row).
function TeamDetailPanel({
  team,
  onClose,
}: {
  team: AgentTeamSummary;
  onClose: () => void;
}) {
  return (
    <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="team-detail">
      <div className="mb-3 flex items-center justify-between">
        <h3 className="text-sm font-medium">{team.name}</h3>
        <Button variant="ghost" size="sm" onClick={onClose} data-testid="team-detail-close">
          Close
        </Button>
      </div>

      {/* Team-level readiness */}
      <div className="mb-3 flex items-center gap-2">
        {team.ready ? (
          <Badge variant="success" data-testid="team-detail-ready-badge">Ready</Badge>
        ) : (
          // H5: the badge is a STATUS label; the reason detail lives in the span below — previously
          // both rendered the reason, so it appeared twice.
          <Badge variant="warning" data-testid="team-detail-notready-badge">
            Not ready
          </Badge>
        )}
        {!team.ready && team.reason && (
          <span className="text-xs text-muted-foreground" data-testid="team-detail-notready-reason">
            {team.reason}
          </span>
        )}
      </div>

      <dl className="space-y-3 text-sm">
        <div>
          <dt className="mb-1 text-xs text-muted-foreground">Supervisor</dt>
          <dd className="font-medium">{team.supervisor}</dd>
        </div>

        <div>
          <dt className="mb-1 text-xs text-muted-foreground">
            Roster ({team.roster.length} sub-agent{team.roster.length === 1 ? "" : "s"})
          </dt>
          <dd>
            {team.roster.length === 0 ? (
              <span className="text-muted-foreground">—</span>
            ) : (
              <ul className="space-y-2" data-testid="team-detail-roster">
                {team.roster.map((m: AgentTeamRoster) => (
                  <li
                    key={m.name}
                    className="rounded-md border bg-surface-2/40 px-3 py-2"
                    data-testid={`team-member-${m.name}`}
                  >
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-sm">{m.name}</span>
                      <span className="text-muted-foreground">→</span>
                      <span className="font-mono text-xs text-muted-foreground">{m.agentRef}</span>
                    </div>
                    {m.description && (
                      <p className="mt-0.5 text-xs text-muted-foreground">{m.description}</p>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </dd>
        </div>

        <div>
          <dt className="mb-1 text-xs text-muted-foreground">Spawn budget</dt>
          <dd className="text-xs text-muted-foreground">
            fan-out {team.budget.maxFanOut} · depth {team.budget.maxSpawnDepth} · total{" "}
            {team.budget.maxTotalSpawns}
          </dd>
        </div>
      </dl>
    </div>
  );
}

export function TeamsPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  // I3 m76.6: selected team for the inline detail panel (nil = closed).
  const [selectedTeam, setSelectedTeam] = useState<AgentTeamSummary | null>(null);
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
          resource: "agent teams",
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
      header: "Status",
      // Two-line status (M144.1): the badge is the STATE; the humanized reason is
      // the subordinate cause line — so a not-ready team says WHY, not just "Not ready".
      cell: (t) => (
        <div className="flex flex-col gap-0.5">
          <StatusBadge ready={t.ready} reason={t.reason} />
          {!t.ready && t.reason ? (
            <span className="text-xs text-muted-foreground">{humanizeStatusReason(t.reason)}</span>
          ) : null}
        </div>
      ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="teams-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Agent Teams</h2>
          <p className="text-sm text-muted-foreground">
            Orchestration rosters — a supervisor summons the roster's sub-agents on demand (delegate_to),
            bounded by the spawn budget.
          </p>
        </div>
        <Button onClick={() => navigate("/teams/new")}>
          <Plus className="h-4 w-4" />
          New team
        </Button>
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
        onRowClick={(t) =>
          setSelectedTeam((prev) =>
            // H5: compare namespace+name — two teams can share a name across namespaces (rowKey is
            // already ns/name); keying the toggle on name alone mis-selected the wrong row.
            prev?.name === t.name && prev?.namespace === t.namespace ? null : t,
          )
        }
        empty={{
          icon: Waypoints,
          title: "No agent teams",
          description:
            "No AgentTeams defined yet. Use the New team button to describe a team in a sentence — we compose a supervisor and roster from your registry's published agents.",
        }}
      />

      {/* I3 m76.6: inline detail panel — row-click reveals roster + budget + readiness. */}
      {selectedTeam && (
        <TeamDetailPanel team={selectedTeam} onClose={() => setSelectedTeam(null)} />
      )}
    </div>
  );
}
