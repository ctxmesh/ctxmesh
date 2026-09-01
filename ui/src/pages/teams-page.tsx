import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Sparkles, Waypoints } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  StatusBadge,
  humanizeStatusReason,
  nextStepRank,
  resolveStatus,
  resourcePath,
  type Column,
  type DataTableError,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type AgentTeamSummary } from "@/lib/api";

// TeamsPage — the AgentTeam index (M151 §6.1 archetype A1, §4.4 "Teams index"
// budget). A team is a supervisor plus the roster of sub-agents it may summon,
// bounded by a spawn budget; this list says which teams exist, what shape each
// one declares, whether it resolved, and what to do about the ones that did not.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `GET /api/teams` (internal/bff/teams.go, `AgentTeamSummary`) returns exactly:
// name, namespace, registry, supervisor, roster[], members[], ready, reason and
// the resolved spawnBudget. There is NO delegation count, NO per-team running /
// queued / held total, and no traffic of any kind — those are per-RUN facts and
// the teams endpoint never touches the run store.
//
// So the §4.4 budget's "Running · queued · held" pressure-strip cell is ABSENT
// from this table rather than rendered empty. A strip is a picture of a
// distribution; with no numbers behind it, every width it could draw would be
// invented — the one thing this console must not do. One QuietNote above the
// table says so once (§7 A1's backend-cannot-answer column), and the team detail
// page shows the delegation tree of ONE run, which is the honest, run-scoped
// version of the same question.
//
// Everything the table DOES print is declared structure the endpoint really
// sends: the roster's size, the budget's three ceilings, the controller's Ready
// condition and its reason.
//
// ── SORT: WHAT IS BLOCKING, NOT ALPHABETICAL ────────────────────────────────
// Same contract as the fleet list: `nextStepRank` is the primary key so anything
// asking for a person sits above everything that is not, and the §6.1 attention
// order breaks the tie inside each half.
//
// data-testid contract:
//   teams-page          — root container
//   team-reason-<name>  — the inline not-ready reason
//   next-step-<name>    — the row's next step
// The table itself is found by its accessible name: role="table", "Agent teams".

/** The §6.1 attention order, as a comparator key. */
const ATTENTION: Record<StatusTone, number> = {
  failed: 1,
  waiting: 2,
  progressing: 3,
  ready: 4,
  draft: 5,
};

/**
 * The reasons the AgentTeam controller sets when the ROSTER itself is wrong —
 * `RegistryNotFound`, `MemberNotFound`, `NotARegistryMember` (they are named in
 * `api/v1beta1/agentteam_types.go` on AgentTeamStatus.Conditions). These get the
 * specific next step, because the fix is an edit to the team, not a wait.
 */
const ROSTER_FAULT = /(registrynotfound|membernotfound|notaregistrymember|notfound|notamember)/i;

/**
 * A condition reason as the controller writes it is often `Code: a whole
 * sentence of detail` ("MemberNotFound: escalation-agent is not an
 * AgentDeployment in default"). `humanizeStatusReason` is built for the CODE —
 * fed the whole string it sentence-cases the detail too, turning identifiers
 * into prose ("escalation agent is not an agent deployment"), which reads like
 * the console mis-transcribed the machine. Split first; the raw string still
 * rides along in `title` and, on the detail page, in a mono well.
 */
export function reasonCode(reason?: string): string {
  const head = (reason ?? "").split(":")[0]?.trim();
  return head || (reason ?? "");
}

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

export interface TriagedTeam {
  team: AgentTeamSummary;
  tone: StatusTone;
  /** Roster roles — the length of the CRD's `roster` list, as sent. */
  roles: number;
  /**
   * Distinct agents this team DECLARES: the supervisor plus every distinct
   * roster `agentRef`. Counted from the wire rather than guessed — two roster
   * entries may legitimately point at the same agent under different role
   * names, and counting entries would then overstate the fleet.
   */
  declaredAgents: number;
  next: NextStep;
  to: string;
}

/** The team's detail route. */
function teamPath(t: AgentTeamSummary): string {
  return resourcePath("team", t.namespace, t.name) ?? "/teams";
}

export function triageTeam(t: AgentTeamSummary): TriagedTeam {
  const { tone } = resolveStatus(t.ready, undefined, t.reason);
  const to = teamPath(t);
  const refs = new Set<string>();
  if (t.supervisor) refs.add(t.supervisor);
  for (const r of t.roster) if (r.agentRef) refs.add(r.agentRef);

  let next: NextStep;
  if (!t.ready && ROSTER_FAULT.test(t.reason ?? "")) {
    // §7.2's own vocabulary for exactly this case.
    next = { label: "Fix the roster", tone: "crit", to };
  } else if (!t.ready) {
    next = { label: "Open the team", tone: "default", to };
  } else {
    next = { tone: "none" };
  }

  return { team: t, tone, roles: t.roster.length, declaredAgents: refs.size, next, to };
}

/** The §5.18 closing line: the honest ratio, restating what the table showed. */
export function teamsClosingLine(rows: TriagedTeam[]): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const needs = rows.filter((r) => nextStepRank(r.next.tone) === 0).length;
  const quiet = total - needs;
  if (needs === 0) {
    return `All ${total} team${total === 1 ? "" : "s"} resolved: every supervisor, roster member and registry reference is in place. Nothing here needs a person.`;
  }
  if (quiet === 0) {
    return `Every one of the ${total} teams has something unresolved. None of them can summon its roster until that is fixed.`;
  }
  if (quiet === 1) {
    return `${needs} of the ${total} teams need${needs === 1 ? "s" : ""} a person. The other one resolved cleanly and can summon its roster.`;
  }
  return `${needs} of the ${total} teams need${needs === 1 ? "s" : ""} a person. The other ${quiet} resolved cleanly and can summon their rosters.`;
}

type Load =
  | { kind: "loading" }
  | { kind: "ready"; teams: AgentTeamSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function TeamsPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [state, setState] = useState<Load>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listTeams(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", teams: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
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

  const items = state.kind === "ready" ? state.teams : [];

  // Triage once, sort once — attention first, then name.
  //
  // The middle key is the one worth naming: among the teams that need a person,
  // a broken ROSTER (crit — it will not delegate until someone edits it) sorts
  // above a team merely waiting on its supervisor to come up. Without it the tie
  // falls to the alphabet and the two urgencies interleave, which is exactly the
  // triage the reader came here to have done for them.
  const sorted = useMemo(() => {
    const rows = items.map(triageTeam);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        (x.next.tone === "crit" ? 0 : 1) - (y.next.tone === "crit" ? 0 : 1) ||
        ATTENTION[x.tone] - ATTENTION[y.tone] ||
        x.team.name.localeCompare(y.team.name),
    );
    return rows;
  }, [items]);

  // `listTeams` is NOT cursor-paged — the BFF returns the whole caller-visible
  // list in one body — so this filter narrows a complete list. That is why there
  // is no "more pages may match" caveat here: it would be false.
  const q = query.trim().toLowerCase();
  const visible = useMemo(
    () => (q ? sorted.filter((r) => r.team.name.toLowerCase().includes(q)) : sorted),
    [sorted, q],
  );

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          resource: "agent teams",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 "Teams index" budget, minus the pressure-strip cell the backend
  // cannot fill: Team(1) · Shape(3) · Agents(2) · State(1) · Next step(1).
  const columns: Column<TriagedTeam>[] = [
    {
      id: "team",
      header: "Team",
      priority: 1,
      cell: ({ team: t }) => (
        <CellEntity
          className="max-w-[18rem]"
          title={t.name}
          name={<span className="truncate">{t.name}</span>}
          namespace={<span title={t.namespace}>{t.namespace}</span>}
        />
      ),
    },
    {
      id: "shape",
      header: "Shape",
      priority: 3,
      className: "w-[15rem]",
      cell: ({ team: t, roles }) => (
        // The declared ladder. Every figure in it is on the wire: the roster's
        // length and the three RESOLVED spawn-budget ceilings (the BFF applies
        // the CRD defaults, so these are never blank). Written with "≤" on
        // purpose — they are bounds, not measurements.
        <div className="font-mono text-xs leading-tight">
          <div className="whitespace-nowrap">
            1 supervisor → {roles} role{roles === 1 ? "" : "s"}
          </div>
          <div className="mt-0.5 whitespace-nowrap text-2xs text-faint">
            depth ≤ {t.budget.maxSpawnDepth} · fan-out ≤ {t.budget.maxFanOut} ·{" "}
            {t.budget.maxTotalSpawns.toLocaleString()} spawns
          </div>
        </div>
      ),
    },
    {
      id: "agents",
      header: "Agents",
      priority: 2,
      numeric: true,
      cell: ({ declaredAgents }) => (
        <QuantityValue
          value={declaredAgents}
          title="Distinct agents this team declares: the supervisor plus every distinct roster agentRef."
        />
      ),
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[13rem]",
      cell: ({ team: t }) => (
        <div className="flex min-w-0 flex-col items-start gap-1">
          <StatusBadge ready={t.ready} reason={t.reason} />
          {!t.ready && t.reason && (
            <span
              className="max-w-[12rem] truncate text-xs text-faint"
              data-testid={`team-reason-${t.name}`}
              title={t.reason}
            >
              {humanizeStatusReason(reasonCode(t.reason))}
            </span>
          )}
        </div>
      ),
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      className: "w-[10rem]",
      cell: (r) => (
        <NextStepLink
          label={r.next.label}
          to={r.next.to}
          tone={r.next.tone}
          ariaLabel={r.next.label ? `${r.next.label} — ${r.team.name}` : undefined}
          testId={`next-step-${r.team.name}`}
        />
      ),
    },
  ];

  const filteredEmpty = items.length > 0 && visible.length === 0;
  const closing = state.kind === "ready" ? teamsClosingLine(visible) : null;
  const showNote = state.kind === "ready" && items.length > 0;
  const metaLine =
    state.kind === "ready"
      ? `${items.length} team${items.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="teams-page">
      <PageHeader
        title="Teams"
        meta={metaLine}
        lede="A team is one supervisor and the roster of agents it may summon. Sorted by what is blocking: a roster that did not resolve sits at the top, because it cannot delegate until it is fixed."
        actionsSlot={
          <Button asChild size="sm" className="text-sm" data-testid="new-team-button">
            <Link to="/teams/new">
              <Sparkles className="h-4 w-4" />
              New team
            </Link>
          </Button>
        }
      />

      {showNote && (
        <QuietNote title="Live delegation traffic isn’t in the team API.">
          This list reads each team’s <em>declared</em> shape — its supervisor,
          its roster, and the spawn budget that bounds it. How much a team is
          delegating right now, and how many of its agents are running, held or
          failing, are per-run facts the teams endpoint does not return. So
          there is no traffic strip here: a strip with no numbers behind it
          would be a picture of an estimate. Open a team to see the delegation
          tree of one of its runs.
        </QuietNote>
      )}

      <DataTable<TriagedTeam>
        columns={columns}
        rows={visible}
        rowKey={(r) => `${r.team.namespace}/${r.team.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter teams by name…"
        ariaLabel="Agent teams"
        tableClassName="min-w-[52rem]"
        onRowClick={(r) => navigate(r.to)}
        empty={
          filteredEmpty
            ? {
                intent: "filtered",
                icon: Waypoints,
                title: "No teams match that filter",
                description: "Clear the filter to see every team you can read.",
                action: {
                  label: "Clear filter",
                  variant: "outline",
                  onClick: () => setQuery(""),
                },
                totalCount: items.length,
                countNoun: "teams",
              }
            : {
                icon: Waypoints,
                title: "No agent teams",
                description:
                  "A team is one supervisor and the roster of agents it may summon on demand, bounded by a spawn budget. Create the first one to see it here.",
                action: {
                  label: "New team",
                  icon: Sparkles,
                  onClick: () => navigate("/teams/new"),
                },
              }
        }
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}
    </div>
  );
}
