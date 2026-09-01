import * as React from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Users } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import {
  CellId,
  ClosingNote,
  ErrorState,
  ForbiddenInline,
  Meter,
  NextStepLink,
  PageHeader,
  QuietNote,
  SectionHeader,
  StatusBadge,
  TreeTable,
  UNKNOWN,
  UnknownValue,
  humanizeStatusReason,
  resolveStatus,
  resourcePath,
  type NextStepTone,
  type Quantity,
  type StatusTone,
  type TreeColumn,
  type TreeNameTone,
  type TreeRow,
} from "@/components/kit";
import {
  api,
  ApiError,
  type AgentSummary,
  type AgentTeamSummary,
  type RunTree,
  type RunTreeNode,
} from "@/lib/api";
import { formatDateTime, formatRelativeTime } from "@/lib/format";
// The console has ONE run-status vocabulary (M144.1) and it lives on the run
// reader. Importing it is deliberate: a second copy of the status→hue map here
// is exactly how "running" ends up violet on one page and green on another.
import { fmtStatus, statusVariant } from "@/pages/run-detail-page";
// The index owns the reason-code split; both team surfaces must say the same
// words about the same condition.
import { reasonCode } from "@/pages/teams-page";

// TeamDetailPage — the team as an OUTLINE (M151 §6.1 archetype A3, §5.22).
// Route: /teams/:ns/:name
//
// The outline was chosen over an org chart and a flow diagram for one reason:
// it is the only one of the three that is size-blind. This page has to read the
// same when the team is two agents and when a run of it is a thousand, so both
// trees here go through the kit TreeTable — arbitrary depth, a drawn gutter,
// viewport windowing above 60 rows, a real treegrid keyboard.
//
// ══ WHAT IS REAL ON THIS PAGE, AND WHERE EACH FIGURE COMES FROM ═════════════
//
// `GET /api/teams` (internal/bff/teams.go, AgentTeamSummary) returns DECLARED
// STRUCTURE ONLY: name, namespace, registry, supervisor, roster[], members[],
// ready, reason, and the resolved spawn budget (maxFanOut / maxSpawnDepth /
// maxTotalSpawns). There is no team-scoped run index, no delegation count, no
// running/held/failed total, and no per-route traffic ANYWHERE in the team API.
// So:
//
//   • The ROSTER OUTLINE is the supervisor and the roster entries, exactly as
//     declared, with each member's live readiness joined from
//     `GET /api/agents?namespace=<the team's>` — the agent's own Ready
//     condition, not a guess.
//   • The BOUNDS are the three spawn-budget ceilings. Real, and the governing
//     facts about a team.
//   • DELEGATION TRAFFIC IS UNKNOWN. The column reads the §7.1 dash and one
//     QuietNote says why. A per-team total would have to be invented, and an
//     invented number is the one thing this console must never print.
//   • The RUN TREE is where the 1,024 case is real. `GET /api/runs/{id}/tree`
//     returns every run in one orchestration — arbitrary depth, arbitrary
//     width — and it is drawn through the same TreeTable.
//
// ── How this page finds "a recent run" (a seam worth stating) ───────────────
// There is no `/api/teams/{ns}/{name}/runs`. What exists is
// `GET /api/runs?agent=<ns>/<name>` (Langfuse-backed, server-side agent filter)
// and `GET /api/runs/{id}/tree`. Two facts make the join legal:
//
//   1. `authorizeRunAccess` (internal/bff/shares.go) resolves the {id} path
//      value by run.ID *and falls back to GetByTraceID* — so the traceId the
//      runs list carries IS a valid identifier for the tree endpoint. (run.ID
//      and TraceID are genuinely distinct columns; the fallback is what makes
//      this work, and it is why the page passes a traceId without lying.)
//   2. A team's delegation trees are rooted at its SUPERVISOR's runs.
//
// So the page reads the supervisor's most recent recorded run and draws that
// tree. What it may NOT claim is that this is "the team's traffic": the same
// agent can supervise for more than one team, and one run is not an aggregate.
// The section says so in its lede rather than implying a total.
//
// ── A broken roster member is DRAWN, never dropped ──────────────────────────
// A roster entry whose `agentRef` does not resolve to an AgentDeployment in the
// team's namespace renders as a crit row with its own next step (§7 A3). A
// roster that hides its gaps is one you find out about in production. The page
// keeps three states apart and never collapses them: resolved (we read the
// agent), missing (the namespace list was exhausted and it is not there), and
// unknown (we could not read the agents list at all — which is NOT evidence of
// absence).
//
// data-testid contract:
//   team-detail-page        — root container
//   team-detail             — the roster section (also the demo-film anchor)
//   team-supervisor         — the supervisor row's name cell
//   team-member-<role>      — one roster row, keyed by the roster-local name
//   team-bounds             — the spawn-budget strip
//   team-run-tree           — the delegation-tree section
//   team-not-found          — the 404 state

// ── Constants ────────────────────────────────────────────────────────────────

/**
 * Pages of `GET /api/agents` we will walk to resolve roster members. The BFF
 * caps a page at 200 (`maxListLimit`), so this reads at most 1,000 agents in
 * the team's namespace. If the walk stops before the list is exhausted, an
 * unfound member is `unknown`, NEVER `missing` — "we did not look at all of it"
 * and "it is not there" are different claims.
 */
const AGENT_PAGE_LIMIT = 200;
const AGENT_MAX_PAGES = 5;

/**
 * How many collapsed ancestors the run tree may auto-open to reveal what needs
 * a person. Above this the tree opens at the root only: auto-expanding 500
 * branches is not "showing you the problem", it is rendering everything.
 */
const AUTO_EXPAND_MAX = 40;

/**
 * At or below this many runs the tree opens COMPLETELY. This is the §6.1 A3
 * small-team acceptance stated as code: "at 2 agents every member renders
 * expanded with nothing behind a chevron". Collapsing a six-run tree to protect
 * against a thousand-run one is the failure mode where a design meant to be
 * size-blind becomes size-hostile at the small end.
 */
const WHOLE_TREE_MAX = 40;

/** Per expanded parent: how many needing-a-person children render before the summary. */
const NEEDING_CAP = 50;
/** …and the minimum rows shown, so a healthy branch is not a single summary line. */
const MIN_VISIBLE = 5;

const DELEGATIONS_UNKNOWN_TITLE =
  "The teams API reports no delegation counts. Unknown — not zero.";

// ── Next steps (§5.19 / §7.2) ────────────────────────────────────────────────

interface NextStep {
  label?: string;
  tone: NextStepTone;
  to?: string;
}

const NOTHING: NextStep = { tone: "none" };

// ── The roster model ─────────────────────────────────────────────────────────

type Resolution = "resolved" | "missing" | "unknown";

interface RosterRow {
  key: string;
  /** The AgentDeployment this entry names. */
  agentRef: string;
  /** The roster-local name the supervisor summons it by. Absent on the supervisor. */
  role?: string;
  description?: string;
  isSupervisor: boolean;
  resolution: Resolution;
  agent?: AgentSummary;
  tone?: StatusTone;
  next: NextStep;
}

function agentNextStep(
  ns: string,
  ref: string,
  resolution: Resolution,
  tone?: StatusTone,
): NextStep {
  const to = resourcePath("agent", ns, ref) ?? undefined;
  if (resolution === "missing") {
    // §7 A3's example copy is "Fix the roster →", but this console has no
    // team-edit surface for that link to land on, and NextStepLink is explicit
    // that a next step with no destination is a page bug. The action that
    // actually clears a MemberNotFound is creating the agent the roster names,
    // so that is the verb, and it goes somewhere real.
    return { label: "Create the agent", tone: "crit", to: "/agents/new" };
  }
  if (resolution === "unknown") {
    return { label: "Open the agent", tone: "default", to };
  }
  if (tone === "failed") return { label: "Open the failure", tone: "crit", to };
  if (tone === "waiting") return { label: "Review the hold", tone: "default", to };
  return NOTHING;
}

function buildRoster(
  team: AgentTeamSummary,
  agents: Map<string, AgentSummary> | null,
  exhausted: boolean,
): RosterRow[] {
  const resolve = (ref: string): { resolution: Resolution; agent?: AgentSummary } => {
    if (!agents) return { resolution: "unknown" };
    const agent = agents.get(ref);
    if (agent) return { resolution: "resolved", agent };
    // Absent from a list we did not finish reading proves nothing.
    return { resolution: exhausted ? "missing" : "unknown" };
  };

  const rows: RosterRow[] = [];

  const sup = resolve(team.supervisor);
  const supTone = sup.agent
    ? resolveStatus(sup.agent.ready, sup.agent.phase, sup.agent.reason).tone
    : undefined;
  rows.push({
    key: `supervisor:${team.supervisor}`,
    agentRef: team.supervisor,
    isSupervisor: true,
    resolution: sup.resolution,
    agent: sup.agent,
    tone: supTone,
    next: agentNextStep(team.namespace, team.supervisor, sup.resolution, supTone),
  });

  for (const entry of team.roster) {
    const r = resolve(entry.agentRef);
    const tone = r.agent
      ? resolveStatus(r.agent.ready, r.agent.phase, r.agent.reason).tone
      : undefined;
    rows.push({
      key: `member:${entry.name}`,
      agentRef: entry.agentRef,
      role: entry.name,
      description: entry.description,
      isSupervisor: false,
      resolution: r.resolution,
      agent: r.agent,
      tone,
      next: agentNextStep(team.namespace, entry.agentRef, r.resolution, tone),
    });
  }
  return rows;
}

/** Flatten the roster into TreeTable rows: supervisor at depth 0, members under it. */
function rosterTreeRows(rows: RosterRow[], expanded: boolean): TreeRow<RosterRow>[] {
  if (rows.length === 0) return [];
  const [supervisor, ...members] = rows;
  const out: TreeRow<RosterRow>[] = [
    {
      row: supervisor,
      depth: 0,
      kind: "root",
      // Not expandable when there is nothing under it — TreeTable reads
      // `expanded === undefined` as "no chevron", which is the honest signal.
      expanded: members.length > 0 ? expanded : undefined,
      childCount: members.length,
    },
  ];
  if (!expanded) return out;
  for (const m of members) out.push({ row: m, depth: 1, kind: "leaf" });
  return out;
}

// ── The run tree model ───────────────────────────────────────────────────────

interface RunNodeMeta {
  node: RunTreeNode;
  children: RunTreeNode[];
  /** This run itself is waiting on a person or has failed. */
  needsSelf: boolean;
  /** …or anything below it is. Drives the summary row's `needsPerson`. */
  needsBelow: boolean;
  depth: number;
}

interface RunIndex {
  root?: RunTreeNode;
  meta: Map<string, RunNodeMeta>;
  /** Every node reachable from the root, in the order the tree walks them. */
  reachable: number;
  maxDepth: number;
  /** Widest sibling group anywhere in the tree. */
  widestFanOut: number;
}

/**
 * A run tree's `agent` is the FLATTENED key `namespace/name`. Rendering it whole
 * in the tree cell is what §4.5 forbids — "namespaces never share the name's
 * line" — and it is not a small offence here: with a 45-character namespace,
 * every row of a thousand-run tree spends its width on the same repeated prefix
 * and ellipsises the one word that differs. So the tree cell shows the NAME, the
 * full key rides in `title`, and the namespace is spoken only when it is not the
 * team's own (delegation is registry-scoped, so that is the exception).
 */
function splitAgentKey(key: string): { ns: string; name: string } {
  const i = key.indexOf("/");
  return i < 0 ? { ns: "", name: key } : { ns: key.slice(0, i), name: key.slice(i + 1) };
}

/** First segment + … + last segment, so a foreign namespace stays one short chip. */
function shortNs(ns: string): string {
  if (ns.length <= 20) return ns;
  const parts = ns.split("-").filter(Boolean);
  if (parts.length < 3) return `${ns.slice(0, 17)}…`;
  return `${parts[0]}…${parts[parts.length - 1]}`;
}

/** A run in one of the two states that want a person (§2.4 hold, §2.2 crit). */
function runNeedsPerson(status: string): boolean {
  const v = statusVariant(status);
  return v === "hold" || v === "crit";
}

function runNextStep(node: RunTreeNode): NextStep {
  const to = `/runs/${encodeURIComponent(node.id)}`;
  const v = statusVariant(node.status);
  if (v === "crit") return { label: "Open the failure", tone: "crit", to };
  if (v === "hold") return { label: "Review the hold", tone: "default", to };
  return NOTHING;
}

/**
 * Index the flat `nodes[]` into a tree. Defensive by construction: a node whose
 * parent is absent (or is itself) becomes a root rather than vanishing, and the
 * walk carries a visited set so a malformed parent cycle cannot hang the page.
 * A run tree comes from a database, not from a schema that forbids cycles.
 */
function indexRunTree(tree: RunTree): RunIndex {
  const meta = new Map<string, RunNodeMeta>();
  for (const n of tree.nodes) {
    if (!meta.has(n.id)) {
      meta.set(n.id, { node: n, children: [], needsSelf: runNeedsPerson(n.status), needsBelow: false, depth: 0 });
    }
  }
  const roots: RunTreeNode[] = [];
  // Iterate the MAP, not the raw list: a duplicated id on the wire would
  // otherwise be linked twice and render its subtree twice.
  for (const m of meta.values()) {
    const pid = m.node.parentRunId;
    const parent = pid && pid !== m.node.id ? meta.get(pid) : undefined;
    if (parent) parent.children.push(m.node);
    else roots.push(m.node);
  }

  const declared = tree.rootId ? meta.get(tree.rootId)?.node : undefined;
  const root = declared ?? roots[0];

  let reachable = 0;
  let maxDepth = 0;
  let widestFanOut = 0;
  if (root) {
    const seen = new Set<string>();
    const stack: Array<{ id: string; depth: number }> = [{ id: root.id, depth: 0 }];
    // Post-order needsBelow needs the children resolved first, so collect the
    // walk order and fold it back.
    const order: string[] = [];
    while (stack.length > 0) {
      const cur = stack.pop();
      if (!cur) break;
      if (seen.has(cur.id)) continue;
      seen.add(cur.id);
      const m = meta.get(cur.id);
      if (!m) continue;
      m.depth = cur.depth;
      order.push(cur.id);
      reachable += 1;
      if (cur.depth > maxDepth) maxDepth = cur.depth;
      if (m.children.length > widestFanOut) widestFanOut = m.children.length;
      for (const c of m.children) {
        if (!seen.has(c.id)) stack.push({ id: c.id, depth: cur.depth + 1 });
      }
    }
    for (let i = order.length - 1; i >= 0; i--) {
      const m = meta.get(order[i]);
      if (!m) continue;
      m.needsBelow = m.needsSelf || m.children.some((c) => meta.get(c.id)?.needsBelow === true);
    }
  }

  return { root, meta, reachable, maxDepth, widestFanOut };
}

/**
 * The tree's opening posture.
 *
 *   • A SMALL tree opens whole — nothing behind a chevron (§6.1 A3).
 *   • A LARGE one opens the root and the path to everything that needs a
 *     person, and nothing else. Auto-expanding every branch that happens to
 *     contain a held run is not "showing you the problem", it is rendering the
 *     whole tree with extra steps — so past AUTO_EXPAND_MAX branches it opens
 *     the root only and lets the reader steer.
 */
function defaultRunExpansion(idx: RunIndex): Set<string> {
  const open = new Set<string>();
  if (!idx.root) return open;
  open.add(idx.root.id);
  if (idx.reachable <= WHOLE_TREE_MAX) {
    for (const m of idx.meta.values()) {
      if (m.children.length > 0) open.add(m.node.id);
    }
    return open;
  }
  const wanted = new Set<string>();
  for (const m of idx.meta.values()) {
    if (!m.needsSelf) continue;
    let pid = m.node.parentRunId;
    let guard = 0;
    while (pid && guard < 128) {
      guard += 1;
      if (wanted.has(pid)) break;
      wanted.add(pid);
      pid = idx.meta.get(pid)?.node.parentRunId;
    }
  }
  if (wanted.size <= AUTO_EXPAND_MAX) for (const id of wanted) open.add(id);
  return open;
}

interface RunRowData {
  key: string;
  node?: RunTreeNode;
  /** Set on a summary row: the parent whose remainder it stands for. */
  parentId?: string;
  next: NextStep;
}

/**
 * Flatten the indexed tree for TreeTable.
 *
 * The size-blind contract, implemented on the parent side exactly as §5.22
 * describes it: an expanded parent renders the children that need a person,
 * tops up to a readable minimum, and then emits ONE summary row for the
 * remainder. Every number on that summary row is counted from the parent's own
 * complete child list — this page holds the whole tree, so the remainder is a
 * FACT here, not the client-side subtraction the component refuses to do on
 * data it cannot see.
 */
function flattenRunTree(
  idx: RunIndex,
  expanded: Set<string>,
  showAll: Set<string>,
): TreeRow<RunRowData>[] {
  const out: TreeRow<RunRowData>[] = [];
  if (!idx.root) return out;

  const seen = new Set<string>();
  const walk = (node: RunTreeNode, depth: number, kind: "root" | "group" | "leaf") => {
    if (seen.has(node.id)) return;
    seen.add(node.id);
    const m = idx.meta.get(node.id);
    const children = m?.children ?? [];
    const isOpen = expanded.has(node.id);
    out.push({
      row: { key: node.id, node, next: runNextStep(node) },
      depth,
      kind,
      expanded: children.length > 0 ? isOpen : undefined,
      childCount: children.length > 0 ? children.length : undefined,
    });
    if (children.length === 0 || !isOpen) return;

    const all = showAll.has(node.id);
    const needing = children.filter((c) => idx.meta.get(c.id)?.needsBelow === true);
    const quiet = children.filter((c) => idx.meta.get(c.id)?.needsBelow !== true);
    const shown = all
      ? children
      : [
          ...needing.slice(0, NEEDING_CAP),
          ...quiet.slice(0, Math.max(0, MIN_VISIBLE - Math.min(needing.length, NEEDING_CAP))),
        ];
    const shownIds = new Set(shown.map((c) => c.id));
    for (const c of shown) {
      const cm = idx.meta.get(c.id);
      walk(c, depth + 1, (cm?.children.length ?? 0) > 0 ? "group" : "leaf");
    }
    const hidden = children.length - shown.length;
    if (hidden > 0) {
      out.push({
        row: { key: `${node.id}:more`, parentId: node.id, next: NOTHING },
        depth: depth + 1,
        kind: "summary",
        childCount: hidden,
        needsPerson: children.filter(
          (c) => !shownIds.has(c.id) && idx.meta.get(c.id)?.needsBelow === true,
        ).length,
      });
    }
  };

  walk(idx.root, 0, "root");
  return out;
}

// ── Load states ──────────────────────────────────────────────────────────────

type TeamLoad =
  | { kind: "loading" }
  | { kind: "ready"; team: AgentTeamSummary }
  | { kind: "notfound" }
  | { kind: "error"; message: string; forbidden: boolean };

type AgentsLoad =
  | { kind: "loading" }
  | { kind: "ready"; agents: Map<string, AgentSummary>; exhausted: boolean }
  | { kind: "unavailable"; forbidden: boolean };

type RunLoad =
  | { kind: "loading" }
  | { kind: "ready"; tree: RunTree; index: RunIndex; traceId: string }
  | { kind: "none" }
  | { kind: "unconfigured" }
  | { kind: "error"; message: string; forbidden: boolean };

// ── The page ─────────────────────────────────────────────────────────────────

export function TeamDetailPage() {
  const navigate = useNavigate();
  const params = useParams<{ ns: string; name: string }>();
  const ns = params.ns ?? "";
  const name = params.name ?? "";

  const [teamState, setTeamState] = React.useState<TeamLoad>({ kind: "loading" });
  const [agentsState, setAgentsState] = React.useState<AgentsLoad>({ kind: "loading" });
  const [runState, setRunState] = React.useState<RunLoad>({ kind: "loading" });
  const [rosterOpen, setRosterOpen] = React.useState(true);
  const [runExpanded, setRunExpanded] = React.useState<Set<string>>(new Set());
  const [runShowAll, setRunShowAll] = React.useState<Set<string>>(new Set());

  // ── The team itself. There is no GET /api/teams/{ns}/{name}; the list is the
  // only read, so the page narrows it. A name that is not in the caller's list
  // is a 404 for this page, not an empty detail.
  const loadTeam = React.useCallback(() => {
    const controller = new AbortController();
    setTeamState({ kind: "loading" });
    api
      .listTeams(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const found = res.items.find((t) => t.namespace === ns && t.name === name);
        setTeamState(found ? { kind: "ready", team: found } : { kind: "notfound" });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setTeamState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  React.useEffect(() => loadTeam(), [loadTeam]);

  // ── Member readiness. Walks the namespace's agent pages so a roster member
  // that lives past page 1 is not reported as a gap.
  React.useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    setAgentsState({ kind: "loading" });
    (async () => {
      const map = new Map<string, AgentSummary>();
      let cursor = "";
      let exhausted = false;
      try {
        for (let page = 0; page < AGENT_MAX_PAGES; page++) {
          const res = await api.listAgents(
            {
              namespace: ns || undefined,
              limit: AGENT_PAGE_LIMIT,
              cursor: cursor || undefined,
              // A roster may legitimately name a draft agent; hiding drafts here
              // would render a real agent as a missing one.
              includeDrafts: true,
            },
            controller.signal,
          );
          for (const a of res.items) map.set(a.name, a);
          cursor = res.nextCursor ?? "";
          if (!cursor) {
            exhausted = true;
            break;
          }
        }
      } catch (err: unknown) {
        if (cancelled || controller.signal.aborted) return;
        setAgentsState({
          kind: "unavailable",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
        return;
      }
      if (cancelled || controller.signal.aborted) return;
      setAgentsState({ kind: "ready", agents: map, exhausted });
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [ns]);

  // ── The delegation tree of the supervisor's most recent recorded run.
  const supervisor = teamState.kind === "ready" ? teamState.team.supervisor : "";
  React.useEffect(() => {
    if (!supervisor) return;
    const controller = new AbortController();
    let cancelled = false;
    setRunState({ kind: "loading" });
    (async () => {
      try {
        const list = await api.runsFiltered(
          { agent: `${ns}/${supervisor}`, limit: 1 },
          controller.signal,
        );
        if (cancelled || controller.signal.aborted) return;
        // null = 501: the trace backend is not wired. Calm, not an error.
        if (list === null) {
          setRunState({ kind: "unconfigured" });
          return;
        }
        const first = list.runs[0];
        if (!first) {
          setRunState({ kind: "none" });
          return;
        }
        const tree = await api.getRunTree(first.traceId, controller.signal);
        if (cancelled || controller.signal.aborted) return;
        setRunState({ kind: "ready", tree, index: indexRunTree(tree), traceId: first.traceId });
      } catch (err: unknown) {
        if (cancelled || controller.signal.aborted) return;
        setRunState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      }
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [ns, supervisor]);

  // The tree opens on what needs a person; everything else stays behind a chevron.
  React.useEffect(() => {
    if (runState.kind !== "ready") return;
    setRunExpanded(defaultRunExpansion(runState.index));
    setRunShowAll(new Set());
  }, [runState]);

  // ── Derived ───────────────────────────────────────────────────────────────

  const team = teamState.kind === "ready" ? teamState.team : null;

  const roster = React.useMemo(() => {
    if (!team) return [];
    return buildRoster(
      team,
      agentsState.kind === "ready" ? agentsState.agents : null,
      agentsState.kind === "ready" ? agentsState.exhausted : false,
    );
  }, [team, agentsState]);

  const rosterRows = React.useMemo(
    () => rosterTreeRows(roster, rosterOpen),
    [roster, rosterOpen],
  );

  const runRows = React.useMemo(() => {
    if (runState.kind !== "ready") return [];
    return flattenRunTree(runState.index, runExpanded, runShowAll);
  }, [runState, runExpanded, runShowAll]);

  // The three bounds. Depth and total spawns are MEASURED against the ceiling
  // they are the ceiling for; fan-out is not, and says so — see the lede.
  const measured = runState.kind === "ready" ? runState.index : null;
  const usedDepth: Quantity = measured ? measured.maxDepth : UNKNOWN;
  const usedSpawns: Quantity = measured ? Math.max(0, measured.reachable - 1) : UNKNOWN;

  const gaps = roster.filter((r) => r.resolution === "missing").length;
  const unknowns = roster.filter((r) => r.resolution === "unknown").length;

  // ── Degraded whole-page states (§7 A3) ────────────────────────────────────

  if (teamState.kind === "error" && teamState.forbidden) {
    return (
      <div className="min-w-0 space-y-6" data-testid="team-detail-page">
        <PageHeader breadcrumb={[{ label: "Teams", to: "/teams" }, { label: name }]} title={name} titleMono />
        <ForbiddenInline resource="agent teams" />
      </div>
    );
  }

  if (teamState.kind === "error") {
    return (
      <div className="min-w-0 space-y-6" data-testid="team-detail-page">
        <PageHeader breadcrumb={[{ label: "Teams", to: "/teams" }, { label: name }]} title={name} titleMono />
        <ErrorState description={teamState.message} onRetry={loadTeam} />
      </div>
    );
  }

  if (teamState.kind === "notfound") {
    return (
      <div className="min-w-0 space-y-6" data-testid="team-detail-page">
        <PageHeader breadcrumb={[{ label: "Teams", to: "/teams" }, { label: name }]} title={name} titleMono />
        <div data-testid="team-not-found">
          <ErrorState
            title="No such team"
            description={`No AgentTeam named ${name} in ${ns || "this namespace"} — or your account cannot read it. Check the name, or open the teams list to see what you can reach.`}
            action={{ label: "Back to teams", onClick: () => navigate("/teams") }}
          />
        </div>
      </div>
    );
  }

  const loading = teamState.kind === "loading";
  const budget = team?.budget;

  return (
    <div className="min-w-0 space-y-8" data-testid="team-detail-page">
      <PageHeader
        loading={loading}
        breadcrumb={[{ label: "Teams", to: "/teams" }, { label: name }]}
        title={team?.name ?? name}
        titleMono
        status={team ? <StatusBadge ready={team.ready} reason={team.reason} /> : undefined}
        meta={
          team
            ? `${team.namespace} · registry ${team.registry} · ${team.roster.length} role${team.roster.length === 1 ? "" : "s"}`
            : undefined
        }
        lede={
          team
            ? "One supervisor and the roster it may summon. Everything below is what the team declares, plus the live readiness of each agent it names."
            : undefined
        }
      />

      {team && !team.ready && team.reason && (
        <QuietNote title={`This team is not ready: ${humanizeStatusReason(reasonCode(team.reason))}.`}>
          The controller reported{" "}
          <span className="font-mono text-xs">{team.reason}</span>. Until it
          resolves, the supervisor cannot summon the roster below.
        </QuietNote>
      )}

      {/* ── Bounds ─────────────────────────────────────────────────────────── */}
      {budget && (
        <section data-testid="team-bounds">
          <SectionHeader
            title="Bounds"
            lede="The spawn budget the platform enforces on every run of this team. Depth and total spawns are measured against the run below; how many sub-runs a single supervisor step started is not recorded, so the fan-out ceiling has nothing to draw against."
          />
          {/* Two up at tablet, three only when there is room for the third to keep
              its label, its figures and its foot on their own lines. */}
          <div className="grid gap-6 rounded-lg border bg-card p-5 sm:grid-cols-2 lg:grid-cols-3">
            <Meter
              label="spawn depth"
              used={usedDepth}
              cap={budget.maxSpawnDepth}
              foot={
                measured
                  ? "The deepest supervisor → sub-agent chain in the run below."
                  : undefined
              }
            />
            <Meter label="fan-out per step" used={UNKNOWN} cap={budget.maxFanOut} />
            <Meter
              label="spawns in one run"
              used={usedSpawns}
              cap={budget.maxTotalSpawns}
              foot={
                measured
                  ? "Every sub-run in the tree below, against the ceiling for one root run."
                  : undefined
              }
            />
          </div>
        </section>
      )}

      {/* ── The roster outline ─────────────────────────────────────────────── */}
      <section data-testid="team-detail">
        <SectionHeader
          title="Roster"
          lede="The supervisor, and every agent it is allowed to summon. Readiness is each agent's own Ready condition."
        />

        <QuietNote
          className="mb-3"
          title="Delegation traffic isn’t recorded per team."
        >
          The teams API returns a team’s declared shape and nothing about its
          traffic — there is no per-team delegation total anywhere in the
          platform. So no delegation figure appears on this page: where the
          column has room it reads <span className="font-mono">—</span>, never a
          number, because a number here would be invented. What the console{" "}
          <em>can</em> show is the delegation tree of <em>one run</em>, and that
          is the section below.
        </QuietNote>

        {agentsState.kind === "unavailable" && (
          <QuietNote className="mb-3" title="Member readiness isn’t readable here.">
            {agentsState.forbidden
              ? "Your account may not list agents in this namespace, so each member’s state reads “readiness unknown”. The roster itself is still exact — it comes from the team."
              : "The agents list could not be read, so each member’s state reads “readiness unknown”. Nothing below is a claim that a member is missing."}
          </QuietNote>
        )}

        <TreeTable<RosterRow>
          rows={rosterRows}
          columns={rosterColumns}
          rowKey={(r) => r.row.key}
          treeHeader="Roster"
          ariaLabel="Team roster"
          // Sized for the columns that SURVIVE the narrow width, not for the
          // full set. DataTable keeps its never-dropped last column readable at
          // any width by pinning it sticky; TreeTable does not, so a min-width
          // wide enough to force a scroll at 768 would push "Next step" — the
          // one column §4.4 says is never dropped — off the frame at rest.
          treeColumnClassName="min-w-[13rem] max-w-[20rem]"
          tableClassName="min-w-[28rem]"
          loading={loading || agentsState.kind === "loading"}
          name={(r) => (
            <span
              data-testid={
                r.row.isSupervisor ? "team-supervisor" : `team-member-${r.row.role}`
              }
            >
              {r.row.agentRef}
            </span>
          )}
          nameTitle={(r) =>
            r.row.description ? `${r.row.agentRef} — ${r.row.description}` : r.row.agentRef
          }
          suffix={(r) => (r.row.isSupervisor ? "· supervisor" : `· ${r.row.role}`)}
          nameTone={(r) => rosterTone(r.row)}
          onToggle={(_, next) => setRosterOpen(next)}
          onActivate={(r) => {
            if (r.row.next.to) navigate(r.row.next.to);
          }}
          empty={{
            icon: Users,
            title: "This team has no members yet",
            description:
              "A team's roster is the set of agents its supervisor may summon. Add agents to the roster to see them here.",
          }}
        />

        {roster.length > 0 && (
          <ClosingNote>{rosterClosingLine(roster, gaps, unknowns)}</ClosingNote>
        )}
      </section>

      {/* ── The delegation tree of one run ─────────────────────────────────── */}
      <section data-testid="team-run-tree">
        <SectionHeader
          title="Delegation tree of a recent run"
          lede={
            <>
              The most recent recorded run of this team’s supervisor, with every
              sub-run it opened. This is <em>one run</em>, not a total — the same
              agent can supervise for more than one team, and the platform keeps
              no per-team aggregate.
            </>
          }
          actions={
            runState.kind === "ready" ? (
              <NextStepLink
                label="Open the run"
                to={`/runs/${encodeURIComponent(runState.traceId)}`}
                testId="team-open-run"
              />
            ) : undefined
          }
        />

        {runState.kind === "ready" && (
          <p className="mb-3 font-mono text-xs text-faint">
            {runState.index.reachable.toLocaleString()} runs · depth{" "}
            {runState.index.maxDepth} · widest sibling group{" "}
            {runState.index.widestFanOut.toLocaleString()}
          </p>
        )}

        {runState.kind === "unconfigured" && (
          <QuietNote title="No trace backend is configured.">
            Runs are recorded in the trace backend, and this install has none
            wired up, so there is no run to open a tree for. The roster above is
            unaffected — it is read from the team itself. Nothing here is
            estimated; the tree is simply absent.
          </QuietNote>
        )}

        {runState.kind === "none" && (
          <QuietNote title="This supervisor has no recorded runs yet.">
            Nothing has been asked of{" "}
            <span className="font-mono text-xs">{supervisor}</span> that the
            trace backend recorded, so there is no delegation tree to draw. Send
            it something and this fills in.
          </QuietNote>
        )}

        {runState.kind === "error" && runState.forbidden && (
          <ForbiddenInline resource="runs" />
        )}
        {runState.kind === "error" && !runState.forbidden && (
          <ErrorState description={runState.message} />
        )}

        {(runState.kind === "loading" || runState.kind === "ready") && (
          <TreeTable<RunRowData>
            rows={runRows}
            columns={runColumns}
            rowKey={(r) => r.row.key}
            treeHeader="Run"
            ariaLabel="Delegation tree"
            // See the roster table above: narrow enough that the three columns
            // that survive 768 fit without scrolling the frame.
            treeColumnClassName="min-w-[14rem] max-w-[22rem]"
            tableClassName="min-w-[36rem]"
            loading={runState.kind === "loading"}
            name={(r) => splitAgentKey(r.row.node?.agent ?? "").name}
            nameTitle={(r) => r.row.node?.agent}
            suffix={(r) => {
              const key = splitAgentKey(r.row.node?.agent ?? "");
              if (!key.ns || key.ns === ns) return undefined;
              return `· ${shortNs(key.ns)}`;
            }}
            nameTone={(r) => runTone(r.row.node?.status)}
            onActivate={(r) => {
              if (r.row.next.to) navigate(r.row.next.to);
            }}
            onToggle={(r, next) => {
              const id = r.row.node?.id;
              if (!id) return;
              setRunExpanded((prev) => {
                const s = new Set(prev);
                if (next) s.add(id);
                else s.delete(id);
                return s;
              });
            }}
            onShowAll={(r) => {
              const pid = r.row.parentId;
              if (!pid) return;
              setRunShowAll((prev) => new Set(prev).add(pid));
            }}
            empty={{
              icon: Users,
              title: "This run delegated to nobody",
              description:
                "The supervisor answered without summoning any of its roster, so the tree is one run deep.",
            }}
          />
        )}
      </section>

      {team && (
        <ClosingNote>
          {closingLine(team, roster.length, gaps, runState.kind === "ready" ? runState.index.reachable : null)}
        </ClosingNote>
      )}
    </div>
  );
}

// ── Cells and copy ───────────────────────────────────────────────────────────

function rosterTone(r: RosterRow): TreeNameTone | undefined {
  if (r.resolution === "missing") return "failed";
  if (r.tone === "failed") return "failed";
  if (r.tone === "waiting") return "hold";
  return undefined;
}

function runTone(status?: string): TreeNameTone | undefined {
  if (!status) return undefined;
  const v = statusVariant(status);
  if (v === "crit") return "failed";
  if (v === "hold") return "hold";
  return undefined;
}

function RosterState({ row }: { row: RosterRow }) {
  if (row.resolution === "missing") {
    return (
      <Badge
        variant="crit"
        title={`No AgentDeployment named ${row.agentRef} exists in this namespace.`}
      >
        no such agent
      </Badge>
    );
  }
  if (row.resolution === "unknown" || !row.agent) {
    return (
      <Badge variant="muted" title="The agents list could not be read — unknown, not absent.">
        readiness unknown
      </Badge>
    );
  }
  return (
    <StatusBadge
      ready={row.agent.ready}
      phase={row.agent.phase}
      reason={row.agent.reason}
    />
  );
}

/**
 * The §4.4 tree-outline budget, trimmed to what the backend can answer.
 *
 * The full budget is Name-tree(1) · Kind(4) · State(1) · Delegations(2) ·
 * Held(2) · Failed(3) · Median(4) · Next step(1). Held, Failed and Median have
 * NO source in the team API — they are per-run facts — so rendering them would
 * be three more columns of dashes beside the one that already says it. §7 A3
 * asks for the dash plus a note, and one stated absence teaches where four is
 * noise, so Delegations carries the absence for all of them and the QuietNote
 * above the table explains it once.
 */
const rosterColumns: TreeColumn<RosterRow>[] = [
  {
    id: "kind",
    header: "Kind",
    priority: 4,
    className: "w-[7rem]",
    cell: (r) => (
      <span className="font-mono text-xs text-faint">
        {r.row.isSupervisor ? "supervisor" : "member"}
      </span>
    ),
  },
  {
    id: "state",
    header: "State",
    priority: 1,
    // Narrow enough that State and Next step — the two the design never drops —
    // both fit inside the frame at 768 without the table scrolling. Measured:
    // at 10rem the pinned Next step pushed State 59px past the visible edge.
    className: "w-[8.5rem]",
    cell: (r) => <RosterState row={r.row} />,
  },
  {
    id: "delegations",
    header: "Delegations",
    // 3, not 2: this column is a permanent stated absence, and the QuietNote
    // above the table already says why. When space is short it is the first
    // thing that should go — a dash nobody can act on must not crowd out the
    // State and Next step columns, which the design says are never dropped.
    priority: 3,
    numeric: true,
    cell: () => <UnknownValue title={DELEGATIONS_UNKNOWN_TITLE} />,
  },
  {
    id: "next",
    header: "Next step",
    priority: 1,
    className: "w-[9.5rem]",
    cell: (r) => (
      <NextStepLink
        label={r.row.next.label}
        to={r.row.next.to}
        tone={r.row.next.tone}
        ariaLabel={
          r.row.next.label ? `${r.row.next.label} — ${r.row.agentRef}` : undefined
        }
        testId={`roster-next-${r.row.role ?? "supervisor"}`}
      />
    ),
  },
];

const runColumns: TreeColumn<RunRowData>[] = [
  {
    id: "run",
    header: "Run",
    priority: 3,
    className: "w-[9rem]",
    cell: (r) => (r.row.node ? <CellId id={r.row.node.id} /> : null),
  },
  {
    id: "state",
    header: "State",
    priority: 1,
    className: "w-[10rem]",
    cell: (r) =>
      r.row.node ? (
        <Badge variant={statusVariant(r.row.node.status)}>
          {fmtStatus(r.row.node.status)}
        </Badge>
      ) : null,
  },
  {
    id: "task",
    header: "Task",
    priority: 4,
    cell: (r) =>
      r.row.node?.input ? (
        // Capped so a 400-character sub-task cannot set the table's width
        // (§4.5: prose truncates in a cell, with the full text in `title`).
        <span
          className="block max-w-[18rem] truncate text-sm text-secondary-foreground"
          title={r.row.node.input}
        >
          {r.row.node.input}
        </span>
      ) : null,
  },
  {
    id: "started",
    header: "Started",
    priority: 4,
    className: "w-[8rem]",
    cell: (r) =>
      r.row.node?.createdAt ? (
        <span
          className="whitespace-nowrap font-mono text-xs text-faint"
          title={formatDateTime(r.row.node.createdAt)}
        >
          {formatRelativeTime(r.row.node.createdAt)}
        </span>
      ) : null,
  },
  {
    id: "next",
    header: "Next step",
    priority: 1,
    className: "w-[9.5rem]",
    cell: (r) => (
      <NextStepLink
        label={r.row.next.label}
        to={r.row.next.to}
        tone={r.row.next.tone}
        testId={r.row.node ? `run-next-${r.row.node.id}` : undefined}
      />
    ),
  },
];

/** The §5.18 line under the roster — restates what the outline showed. */
export function rosterClosingLine(
  roster: RosterRow[],
  gaps: number,
  unknowns: number,
): string {
  const members = roster.length - 1;
  const head = `One supervisor and ${members} roster ${members === 1 ? "member" : "members"}.`;
  if (gaps > 0) {
    return `${head} ${gaps} of them ${gaps === 1 ? "names an agent that does not exist" : "name agents that do not exist"} — the roster cannot be summoned until that is fixed.`;
  }
  if (unknowns > 0) {
    return `${head} ${unknowns} could not be checked, so ${unknowns === 1 ? "its" : "their"} state is unknown rather than assumed good.`;
  }
  return `${head} Every one of them resolves to a real agent.`;
}

export function closingLine(
  team: AgentTeamSummary,
  rosterRows: number,
  gaps: number,
  runNodes: number | null,
): string {
  const bound = `Its budget lets one run reach depth ${team.budget.maxSpawnDepth} and ${team.budget.maxTotalSpawns.toLocaleString()} sub-runs.`;
  const roles = rosterRows - 1;
  const shape = `${team.name} declares ${roles} ${roles === 1 ? "role" : "roles"}${
    gaps > 0 ? `, ${gaps} of which ${gaps === 1 ? "does" : "do"} not resolve` : ""
  }.`;
  if (runNodes === null) return `${shape} ${bound}`;
  return `${shape} ${bound} The run above used ${(runNodes - 1).toLocaleString()} of them.`;
}
