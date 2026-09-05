import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { BUCKETS, type Bucket, bucketOf } from "@/lib/lifecycle";
import { mapLimit, NAMESPACE_SCAN_CONCURRENCY } from "@/lib/map-limit";
import { CheckCircle2, Circle, RefreshCw, Sparkles } from "lucide-react";

import {
  ClosingNote,
  ErrorState,
  ForbiddenInline,
  Meter,
  NextStepLink,
  PageHeader,
  QuietNote,
  SectionHeader,
  Skeleton,
  SkeletonCard,
  UNKNOWN,
  isKnown,
  lifecycleFactNumber,
  meterState,
  resourcePath,
  type LifecycleStage,
  type LifecycleStageCell,
  type NextStepTone,
  type Quantity,
  type StopScopeKind,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, PanelHeader } from "@/components/ui/card";
import {
  api,
  ApiError,
  type ActiveStop,
  type AgentSummary,
  type CensusResponse,
  type AlertSummary,
  type ApprovalQueueItem,
  type TenantSummary,
  type TenantUsageItem,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { formatRelativeTime, formatUSD } from "@/lib/format";
import { FIRST_RUN_CHECKLIST, RES_AGENTS } from "@/lib/nav";

// Home — the first screen an operator sees (M151, spec §6.1 archetype A11). The
// route `/` and this file's export name are unchanged; what the page IS changed.
//
// ── THE PAGE ASKS ONE QUESTION: WHAT NEEDS ME? ──────────────────────────────
// Everything on it is ordered by how loudly the answer is "you", and nothing is
// here for any other reason:
//
//   1. StopNotice     — work is halted right now. Nothing outranks that.
//   2. LifecycleStrip — the fleet census: where every agent sits, Build → Improve.
//   3. Waiting on a person — the decisions blocked ON A HUMAN.
//   4. Spending       — the bounds, and how close each is to refusing work.
//   5. Needs looking at · Alerts — what is broken or drifting but not blocking.
//
// It is deliberately NOT the old dashboard. Live topology and the recent-runs
// table were a picture of the system rather than a list of what it needs; §6.1
// A11 carries neither, and the nav reaches /topology and /runs directly.
//
// ── NO COSMETIC AUTHORITY (§7.1), AND THIS IS THE PAGE IT IS HARDEST ON ─────
// A dashboard is where fabricated numbers live: a zero is easier to draw than
// an absence, and it looks better. So:
//
//   • Every count is COUNTED FROM ROWS IN HAND, never inferred.
//   • `/api/agents` is cursor-paged, so a count of the loaded window is a fact
//     ONLY when nothing follows it (`nextCursor === ""` on the first page).
//     Otherwise the lifecycle facts are absent — LifecycleStrip renders its own
//     "not yet known" — and the fleet sentence drops the clause.
//   • A 501 (not part of this install) renders a QuietNote saying what is
//     missing and why. Never an empty chart, never a zero.
//   • A 403 collapses ONE panel to a ForbiddenInline; the page never fully 403s
//     (§7 A11, panel isolation). An error takes one panel, not the page.
//   • Cost is TENANT-SCOPED (ADR 0077) and `/api/cost` 400s without `?tenant=`,
//     so this page never calls it. Spending reads live gateway usage
//     (`/api/tenants/usage`) against the caps tenants declare (`/api/tenants`) —
//     two endpoints that need no tenant context — and links to /cost, where a
//     tenant is chosen.
//
// ── STOPS ARE READ CLUSTER-WIDE, AND THE PAGE SAYS SO ───────────────────────
// The sidebar's Stops count is filtered to the selected workspace
// (`stopsReaching` in app-shell). Home is not, for the same reason stops-page
// is not: during an incident the fleet stop holding another team's work is
// exactly what you must see.
//
// data-testid contract:
//   home-page             — root
//   home-refresh          — the manual refresh
//   home-stop-<scope>     — one active stop's notice
//   home-waiting          — the "Waiting on a person" panel
//   home-spending         — the "Spending" panel
//   home-attention        — the "Needs looking at" panel
//   home-alerts           — the "Alerts" panel
//   first-run-checklist / first-run-step-<i> / first-run-cta — the guided path

/**
 * One page of agents censuses most installs. Beyond it the window is not the
 * fleet, and this page says so rather than counting what it happens to see.
 */
const FLEET_WINDOW = 200;

/** Rows a Home panel shows before it defers to its own full surface. */
const PANEL_ROWS = 3;


/**
 * The fraction of a cap at which this console calls a bound "near" and draws
 * the Meter's tick. It is the same number tenants-page uses, and it is a
 * CONSOLE CONVENTION rather than a backend-configured alert level — so the
 * panel says so in words instead of implying the platform was set up with it.
 */
const NEAR_CAP_RATIO = 0.8;

// ── Load states: five different truths, never collapsed into two ────────────

type Load<T> =
  | { kind: "loading" }
  | { kind: "ready"; data: T }
  /** 501 — this install has no backend for it. Calm, never an error. */
  | { kind: "unavailable" }
  /** 403 — the caller may not read it. Never a fake empty. */
  | { kind: "forbidden" }
  | { kind: "error"; message: string };

type Failure = Extract<
  Load<never>,
  { kind: "unavailable" | "forbidden" | "error" }
>;

function toFailure(err: unknown): Failure {
  if (err instanceof ApiError) {
    if (err.status === 501) return { kind: "unavailable" };
    if (err.isForbidden) return { kind: "forbidden" };
  }
  return {
    kind: "error",
    message: err instanceof Error ? err.message : "request failed",
  };
}

function isReady<T>(l: Load<T>): l is { kind: "ready"; data: T } {
  return l.kind === "ready";
}

// ── The greeting ────────────────────────────────────────────────────────────


// ── The fleet census ────────────────────────────────────────────────────────




export interface Census {
  total: number;
  halted: number;
  failing: number;
  held: number;
  draft: number;
  serving: number;
  comingUp: number;
  /**
   * The loaded window IS the whole fleet — the one condition under which these
   * numbers are facts rather than one page's worth of guess.
   */
  complete: boolean;
}

export function census(items: AgentSummary[], complete: boolean): Census {
  const c: Census = {
    total: items.length,
    halted: 0,
    failing: 0,
    held: 0,
    draft: 0,
    serving: 0,
    comingUp: 0,
    complete,
  };
  for (const a of items) {
    switch (bucketOf(a)) {
      case "halted":
        c.halted += 1;
        break;
      case "failing":
        c.failing += 1;
        break;
      case "held":
        c.held += 1;
        break;
      case "draft":
        c.draft += 1;
        break;
      case "serving":
        c.serving += 1;
        break;
      default:
        c.comingUp += 1;
    }
  }
  return c;
}

// ── "Needs you": the one queue everything blocked on a person lands in ───────
//
// Home used to carry four surfaces for this — a red stop banner, a waiting queue,
// a "needs looking at" panel and an alerts panel — which is four places to look
// before knowing whether anything wants you. They are one kind of thing: work the
// platform cannot move without a human. Ranked in one list, the top row is the
// answer.
//
// Warning-level alerts deliberately do NOT get rows. Activity owns alerts; Home
// borrows only the critical ones, and the rest are counted in a single quiet line.

/** How many rows the queue shows before it defers to the page that owns them. */
const NEEDS_ROWS = 7;

type NeedKind = "stop" | "approval" | "failing" | "alert";

interface Need {
  key: string;
  kind: NeedKind;
  /** The tag word. Uppercased by the Badge recipe, so written sentence-case. */
  word: string;
  variant: "crit" | "hold" | "warn";
  /** The resource this is about — machine-owned, so rendered mono. */
  subject: string;
  why: string;
  /** The scope line, machine-owned: who stopped it, or which workspace it is in. */
  where: string;
  /** RFC3339, when known. Absent is absent: no row invents an age. */
  since?: string;
  action: { label: string; to: string; tone: NextStepTone; testId?: string };
  rank: number;
}

/**
 * The ranking, in the order a person can act on it: what is stopped, then who is
 * waiting (oldest first — age is the SLA risk), then what is broken, then what is
 * alarming. Every input is optional; a source that did not answer contributes
 * nothing rather than a zero, so the queue renders whatever subset succeeded.
 */
export function needsYouRows(input: {
  stops: ActiveStop[];
  approvals: ApprovalQueueItem[];
  attention: Attention[];
  firing: AlertSummary[];
}): Need[] {
  const rows: Need[] = [];

  for (const s of input.stops) {
    rows.push({
      key: `stop:${s.scope}`,
      kind: "stop",
      word: "Stop",
      variant: "crit",
      subject: stopName(s),
      why: s.reason,
      where: `by ${s.principal}`,
      action: { label: "Review the stop", to: "/stops", tone: "crit" },
      rank: 0,
    });
  }

  for (const a of input.approvals) {
    rows.push({
      key: `approval:${a.namespace}/${a.runId}`,
      kind: "approval",
      word: "Approval",
      variant: "hold",
      subject: a.agent,
      why: a.message?.trim() || "a decision is waiting on a person",
      where: a.namespace,
      since: a.waitingSince,
      action: {
        label: "Review",
        to: `/runs/${a.runId}`,
        tone: "default",
        testId: `home-review-${a.runId}`,
      },
      rank: 1,
    });
  }

  // Only the broken ones. A drifting or never-called agent is worth knowing about
  // but is not blocking anyone, so it stays in the quiet remainder.
  for (const a of input.attention) {
    if (a.variant !== "crit") continue;
    rows.push({
      key: `agent:${a.key}`,
      kind: "failing",
      word: a.word,
      variant: "crit",
      subject: `${a.namespace}/${a.name}`,
      why: a.why,
      where: a.namespace,
      action: { label: "Inspect", to: a.to ?? "/agents", tone: a.tone },
      rank: 2,
    });
  }

  for (const a of input.firing) {
    if (alertVariant(a) !== "crit") continue;
    rows.push({
      key: `alert:${a.id}`,
      kind: "alert",
      word: "Alert",
      variant: "crit",
      subject: a.agent || a.namespace,
      why: a.message?.trim() || alertWord(a),
      where: a.namespace,
      since: a.firedAt,
      action: { label: "View alert", to: "/alerts", tone: "default" },
      rank: 3,
    });
  }

  return rows.sort((x, y) => {
    if (x.rank !== y.rank) return x.rank - y.rank;
    // Within approvals, the oldest is the most urgent. Rows with no age sort last
    // rather than pretending to be new.
    if (x.since && y.since) return x.since.localeCompare(y.since);
    if (x.since) return -1;
    if (y.since) return 1;
    return x.subject.localeCompare(y.subject);
  });
}

/**
 * The status line — scope, size, and the page's whole point, in one line.
 *
 * Composed only from the clauses that answered, the same rule the old greeting
 * followed: a console refused the agent list says nothing about agent counts
 * rather than reassuring anyone with a zero.
 */
export function statusLineParts(input: {
  namespace: string;
  total?: number;
  serving?: number;
  totalIsBound?: boolean;
  needs?: number;
}): { scope: string; clauses: string[]; needs?: number } {
  const clauses: string[] = [];
  if (input.total !== undefined) {
    // Not plural(): the bound has to sit between the number and the noun.
    const word = input.total === 1 ? "agent" : "agents";
    clauses.push(`${input.total}${input.totalIsBound ? "+" : ""} ${word}`);
  }
  if (input.serving !== undefined) clauses.push(`${input.serving} serving`);
  return {
    scope: input.namespace || "all workspaces",
    clauses,
    needs: input.needs,
  };
}

// ── "Needs looking at": the fleet's own attention rows ──────────────────────

type AttentionVariant = "crit" | "warn" | "hold" | "open";

interface Attention {
  key: string;
  name: string;
  namespace: string;
  /** The tag word. Uppercased by the Badge recipe, so written sentence-case. */
  word: string;
  variant: AttentionVariant;
  /** Why, in the operator's words. One line, always truncated with a title. */
  why: string;
  label: string;
  tone: NextStepTone;
  to?: string;
  rank: number;
}

/**
 * The agents a person should look at, in §6.1 attention order. "Needs looking
 * at" is deliberately NOT "needs deciding": a run held for a human belongs in
 * the queue panel above; this panel is what is broken, drifting, or unfinished.
 */
export function attentionRows(items: AgentSummary[]): Attention[] {
  const rows: Attention[] = [];
  for (const a of items) {
    const to = resourcePath("agent", a.namespace, a.name) ?? undefined;
    const base = {
      key: `${a.namespace}/${a.name}`,
      name: a.name,
      namespace: a.namespace,
      to,
    };
    const bucket = bucketOf(a);
    if (bucket === "halted") {
      rows.push({
        ...base,
        word: "halted",
        variant: "crit",
        why: a.reason || a.phase || "stopped",
        label: "Review the stop",
        tone: "crit",
        rank: 0,
      });
    } else if (bucket === "failing") {
      rows.push({
        ...base,
        word: "failing",
        variant: "crit",
        why: a.message || a.reason || a.phase || "not converging",
        label: "Open the failure",
        tone: "crit",
        rank: 1,
      });
    } else if (bucket === "held") {
      rows.push({
        ...base,
        word: "held",
        variant: "hold",
        why: a.reason || a.phase || "waiting on a decision",
        label: "Review the hold",
        tone: "default",
        rank: 2,
      });
    } else if (a.drift) {
      rows.push({
        ...base,
        word: "drift",
        variant: "warn",
        why: "the live spec has diverged from the console config",
        label: "Review the drift",
        tone: "default",
        rank: 3,
      });
    } else if (bucket === "draft") {
      rows.push({
        ...base,
        word: "unfinished",
        variant: "open",
        why: "drafted, never deployed",
        label: "Finish setup",
        tone: "default",
        to: to && `${to}?edit=1`,
        rank: 4,
      });
    }
  }
  rows.sort((x, y) => x.rank - y.rank || x.key.localeCompare(y.key));
  return rows;
}

// ── The approval queue (workspace-scoped, so Home fans out) ─────────────────

interface Queue {
  items: ApprovalQueueItem[];
  /** Workspaces that refused to answer — counted, never folded into "none". */
  unreadable: number;
}

/**
 * The queue's outcomes. `unscoped` is its own truth: the endpoint reads ONE
 * workspace at a time and this caller cannot list the workspaces, so the page
 * knows neither that something is waiting nor that nothing is.
 */
type QueueLoad = Load<Queue> | { kind: "unscoped" };

/** Oldest first: the longest-blocked ask is the one a person owes an answer to. */
function byAge(a: ApprovalQueueItem, b: ApprovalQueueItem): number {
  const x = a.waitingSince ? Date.parse(a.waitingSince) : NaN;
  const y = b.waitingSince ? Date.parse(b.waitingSince) : NaN;
  if (!Number.isNaN(x) && !Number.isNaN(y)) {
    return x - y || a.runId.localeCompare(b.runId);
  }
  // A row with no recorded age sorts last — it cannot claim to be the oldest.
  if (!Number.isNaN(x)) return -1;
  if (!Number.isNaN(y)) return 1;
  return a.runId.localeCompare(b.runId);
}

/** The kind word. A taxonomy, never a state — approvals-page's own ruling. */
const KIND_WORD: Record<ApprovalQueueItem["kind"], string> = {
  plan_approval: "Plan gate",
  approval: "Step approval",
};

// ── Spending: live gateway usage against the caps tenants declare ───────────

interface Bound {
  tenant: string;
  used: Quantity;
  cap: Quantity;
}

/** A cap the tenant actually declared, or an explicit absence. Never 0-as-none. */
function capOf(budgetUSD?: string): Quantity {
  if (!budgetUSD) return UNKNOWN;
  const parsed = Number.parseFloat(budgetUSD);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : UNKNOWN;
}

/**
 * Tenants as bounds, closest-to-the-cap first. A tenant with real spend and NO
 * declared cap sorts above the comfortable ones rather than below them: spend
 * with no ceiling is a governance gap, not a quiet row.
 *
 * `usage === null` means the usage backend did not answer at all. Every row is
 * then UNKNOWN, and the Meter draws the real cap with an empty track and says
 * why — the honest shape of "we know the bound, not the spend".
 */
export function bounds(
  tenants: TenantSummary[],
  usage: TenantUsageItem[] | null,
): Bound[] {
  const spend = new Map(usage?.map((u) => [u.name, u.spendUSD]) ?? []);
  const rows: Bound[] = tenants.map((t) => ({
    tenant: t.name,
    cap: capOf(t.model?.budgetUSD),
    used: usage === null ? UNKNOWN : (spend.get(t.name) ?? UNKNOWN),
  }));
  const pressure = (b: Bound): number => {
    if (isKnown(b.used) && isKnown(b.cap) && b.cap > 0) return b.used / b.cap;
    if (isKnown(b.used) && b.used > 0) return -1; // spending, no ceiling
    return -2;
  };
  rows.sort((a, b) => pressure(b) - pressure(a) || a.tenant.localeCompare(b.tenant));
  return rows;
}


// ── The fleet sentence (§6.1 A11: the lede IS the answer) ───────────────────

export interface Clauses {
  /** Undefined = the backend did not answer, so the clause is omitted. */
  waiting?: number;
  stopped?: number;
  near?: number;
  attention?: number;
  serving?: number;
}


function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}


// ── Stops: the level vocabulary, and what Home may say about it ─────────────

/** Widest blast radius first — what holds the most, on top. */
const STOP_RANK: Record<ActiveStop["level"], number> = {
  fleet: 0,
  tenant: 1,
  namespace: 2,
  agent: 3,
};

/**
 * The wire level → the kit's scope kind.
 *
 * `tenant` maps to NOTHING on purpose. `StopNotice` prints the scope's reach
 * from this word, and the kit has no phrase for "every agent in every workspace
 * this tenant owns" — the nearest options either understate it ("this team") or
 * overstate it ("cluster-wide"), and both are claims the backend never made. A
 * tenant-level stop is therefore counted in the lede, named in a QuietNote, and
 * read in full on the Stops page, which owns that vocabulary.
 */
const SCOPE_KIND: Record<ActiveStop["level"], StopScopeKind | undefined> = {
  agent: "agent",
  namespace: "workspace",
  fleet: "fleet",
  tenant: undefined,
};

function stopName(s: ActiveStop): string {
  switch (s.level) {
    case "agent":
      return `${s.namespace ?? "?"}/${s.agent ?? "?"}`;
    case "namespace":
      return s.namespace || s.scope;
    case "tenant":
      return s.tenant || s.scope;
    default:
      return "everything";
  }
}


// ── The lifecycle facts ─────────────────────────────────────────────────────

function fig(value: number): ReactNode {
  return <span className={lifecycleFactNumber}>{value}</span>;
}

/**
 * Build → Govern → Ship → Improve, from the census.
 *
 * A stage whose fact is not a fact gets NO fact: `LifecycleStrip` renders its
 * own "not yet known" (§5.20), which is the whole reason the prop is optional.
 * Improve is answered by the alert store — the only backend on this page that
 * knows whether quality moved — and stays silent when that store is absent.
 *
 * `active` is a POSITION claim, so it is made only when the window is the whole
 * fleet, and Improve is never lit: nothing here places an agent there.
 */
export function stages(
  c: Census,
  regressions: number | undefined,
): LifecycleStageCell[] {
  const known = c.complete;
  const shipping = c.serving + c.comingUp + c.failing + c.halted;
  const sizes: [LifecycleStage, number][] = [
    ["Build", c.draft],
    ["Govern", c.held],
    ["Ship", shipping],
  ];
  const top =
    known && c.total > 0 ? sizes.reduce((a, b) => (b[1] > a[1] ? b : a))[0] : null;

  return [
    {
      name: "Build",
      active: top === "Build",
      fact: known
        ? c.draft === 0
          ? "no drafts"
          : <>{fig(c.draft)} drafts</>
        : undefined,
    },
    {
      name: "Govern",
      active: top === "Govern",
      fact: known
        ? c.held === 0
          ? "nothing held"
          : <>{fig(c.held)} held for a decision</>
        : undefined,
    },
    {
      name: "Ship",
      active: top === "Ship",
      fact: known ? (
        <>
          {fig(c.serving)} serving
          {c.failing + c.halted > 0 ? <> · {fig(c.failing + c.halted)} not</> : null}
        </>
      ) : undefined,
    },
    {
      name: "Improve",
      fact:
        regressions === undefined
          ? undefined
          : regressions === 0
            ? "no regressions flagged"
            : (
                <>
                  {fig(regressions)} regression{regressions === 1 ? "" : "s"}{" "}
                  flagged
                </>
              ),
    },
  ];
}

// ── Small shared pieces ─────────────────────────────────────────────────────

/** A bordered panel with the §5.4 header band. The page's only card shape. */
function Panel({
  title,
  meta,
  testId,
  children,
  foot,
}: {
  title: ReactNode;
  meta?: ReactNode;
  testId: string;
  children: ReactNode;
  foot?: ReactNode;
}) {
  return (
    <Card className="flex min-w-0 flex-col" data-testid={testId}>
      <PanelHeader title={title} meta={meta} />
      <div className="min-w-0 flex-1">{children}</div>
      {foot ? (
        <div className="border-t border-border px-5 py-3">{foot}</div>
      ) : null}
    </Card>
  );
}

/** One item row: tag · what it is · what to do (the §4.4 queue budget). */
function Item({
  word,
  variant,
  headline,
  sub,
  next,
  testId,
}: {
  word: string;
  variant: "crit" | "warn" | "hold" | "open" | "muted";
  headline: ReactNode;
  sub: string;
  next: ReactNode;
  testId?: string;
}) {
  return (
    <li
      className="flex min-w-0 items-start gap-3 border-b border-border-soft px-5 py-4 last:border-b-0"
      data-testid={testId}
    >
      <Badge variant={variant} className="mt-0.5 shrink-0">
        {word}
      </Badge>
      <div className="min-w-0 flex-1">
        <p className="line-clamp-2 break-words text-sm">{headline}</p>
        <p className="mt-0.5 truncate font-mono text-xs text-faint" title={sub}>
          {sub}
        </p>
      </div>
      <div className="shrink-0 pt-0.5">{next}</div>
    </li>
  );
}

/** The "here is the rest, and where it lives" line every capped panel ends on. */
function MoreLine({
  text,
  label,
  to,
}: {
  text: string;
  label: string;
  to: string;
}) {
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
      <span className="min-w-0 text-sm text-faint">{text}</span>
      <NextStepLink label={label} to={to} />
    </div>
  );
}

/**
 * One tenant's bound. The foot speaks only when the bound is actually near or
 * crossed — four identical "31% of the cap is left" lines is noise, and a
 * comfortable bound has nothing to say.
 */
function BoundMeter({ bound }: { bound: Bound }) {
  const { used, cap } = bound;
  let foot: ReactNode = null;
  if (isKnown(used) && isKnown(cap) && cap > 0) {
    const state = meterState(used, cap, cap * NEAR_CAP_RATIO);
    const left = Math.max(0, Math.round(((cap - used) / cap) * 100));
    if (state === "over") {
      foot = "Past the cap — the next call may be refused.";
    } else if (state === "warn") {
      foot = `Past the tick. ${left}% of the cap is left.`;
    }
  }
  return (
    <Meter
      label={`tenant/${bound.tenant}`}
      used={used}
      cap={cap}
      threshold={isKnown(cap) && cap > 0 ? cap * NEAR_CAP_RATIO : undefined}
      format={formatUSD}
      thing="tenant"
      foot={foot}
    />
  );
}

// ── The page ────────────────────────────────────────────────────────────────

/** Kept as `DashboardPage`: it is the name `App.tsx` mounts at `/`. */
export function DashboardPage() {
  const { namespace } = useNamespace();
  const { can, canFlow } = useCapabilities();
  const canCreate = can(RES_AGENTS, "create");
  // canFlow fails CLOSED: an unprobed or refused flow means the entry point is
  // ABSENT, never a button that 403s when pressed.
  const canConnectProvider = canFlow("connectProvider");

  const [stops, setStops] = useState<Load<ActiveStop[]>>({ kind: "loading" });
  const [fleet, setFleet] = useState<
    Load<{ items: AgentSummary[]; complete: boolean }>
  >({ kind: "loading" });
  const [queue, setQueue] = useState<QueueLoad>({ kind: "loading" });
  const [tenants, setTenants] = useState<Load<TenantSummary[]>>({ kind: "loading" });
  const [usage, setUsage] = useState<Load<TenantUsageItem[]>>({ kind: "loading" });
  const [alerts, setAlerts] = useState<Load<AlertSummary[]>>({ kind: "loading" });
  // True when the alert feed held more than we asked for, so every count derived
  // from it is a lower bound and must be rendered as one.
  const [alertsTruncated, setAlertsTruncated] = useState(false);
  // The whole-fleet census. listAgents can only ever see one page, and its window
  // IS the server's ceiling, so the counts on this page come from here instead.
  const [fleetCensus, setFleetCensus] = useState<Load<CensusResponse>>({
    kind: "loading",
  });
  // Providers + runs drive ONLY the first-run checklist. They are fetched for
  // that gate and nothing else — this page shows no provider list and no runs.
  const [providers, setProviders] = useState<Load<number>>({ kind: "loading" });
  const [runs, setRuns] = useState<Load<number>>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);

  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(
    (silent = false) => {
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      const live = () => !controller.signal.aborted;
      if (!silent) {
        setStops({ kind: "loading" });
        setFleet({ kind: "loading" });
        setFleetCensus({ kind: "loading" });
        setQueue({ kind: "loading" });
        setTenants({ kind: "loading" });
        setUsage({ kind: "loading" });
        setAlerts({ kind: "loading" });
        setProviders({ kind: "loading" });
        setRuns({ kind: "loading" });
      }

      // Every feed lands independently: one panel's 403 or 501 must never take
      // the page down with it (§7 A11, panel isolation).

      api
        .listStops(controller.signal)
        .then((data) => {
          if (live()) setStops({ kind: "ready", data });
        })
        .catch((err: unknown) => {
          if (live()) setStops(toFailure(err));
        });

      // The fleet, scoped by the shell's workspace picker. `complete` is the
      // whole authority story: one page, and no cursor after it.
      api
        .listAgents(
          {
            limit: FLEET_WINDOW,
            includeDrafts: true,
            namespace: namespace || undefined,
          },
          controller.signal,
        )
        .then((res) => {
          if (!live()) return;
          setFleet({
            kind: "ready",
            data: { items: res.items, complete: res.nextCursor === "" },
          });
        })
        .catch((err: unknown) => {
          if (live()) setFleet(toFailure(err));
        });

      api
        .agentCensus(namespace || undefined, controller.signal)
        .then((res) => {
          if (live()) setFleetCensus({ kind: "ready", data: res });
        })
        .catch((err: unknown) => {
          if (live()) setFleetCensus(toFailure(err));
        });

      loadQueue(controller, namespace, (q) => {
        if (live()) setQueue(q);
      });

      api
        .listTenants(controller.signal)
        .then((res) => {
          if (live()) setTenants({ kind: "ready", data: res.items });
        })
        .catch((err: unknown) => {
          if (live()) setTenants(toFailure(err));
        });

      api
        .listTenantUsage(controller.signal)
        .then((res) => {
          if (live()) setUsage({ kind: "ready", data: res.items });
        })
        .catch((err: unknown) => {
          if (live()) setUsage(toFailure(err));
        });

      api
        .listAlerts(
          { limit: 50, ...(namespace ? { namespace } : {}) },
          controller.signal,
        )
        .then((res) => {
          if (!live()) return;
          // null is the client's calm 501 sentinel: no alert store on this
          // install. That is neither an error nor "no alerts are firing".
          setAlertsTruncated(res?.truncated === true);
          setAlerts(
            res === null
              ? { kind: "unavailable" }
              : { kind: "ready", data: res.items ?? [] },
          );
        })
        .catch((err: unknown) => {
          if (live()) setAlerts(toFailure(err));
        });

      api
        .listProviders(controller.signal)
        .then((res) => {
          if (live()) setProviders({ kind: "ready", data: res.providers.length });
        })
        .catch((err: unknown) => {
          if (live()) setProviders(toFailure(err));
        });

      api
        .runs(controller.signal)
        .then((res) => {
          if (live()) setRuns({ kind: "ready", data: res.runs.length });
        })
        .catch((err: unknown) => {
          if (live()) setRuns(toFailure(err));
        });
    },
    [namespace],
  );

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  function refresh() {
    setRefreshing(true);
    load();
    // The spinner is feedback, not a lock — the feeds repaint as they land.
    window.setTimeout(() => setRefreshing(false), 600);
  }

  // ── Derived, once ────────────────────────────────────────────────────────

  const fleetData = isReady(fleet) ? fleet.data : null;
  const facts = useMemo(
    () => (fleetData ? census(fleetData.items, fleetData.complete) : null),
    [fleetData],
  );
  // The bar's counts: bucket each distinct status tuple once, then multiply by
  // how many agents carry it. Anything the server could not fit in its table is
  // surfaced as unclassified rather than quietly dropped from the total.
  const fleetBar = useMemo(() => {
    if (!isReady(fleetCensus)) return null;
    const c = fleetCensus.data;
    const counts = {} as Record<Bucket, number>;
    for (const b of BUCKETS) counts[b.id] = 0;
    let classified = 0;
    for (const g of c.groups) {
      counts[bucketOf(g)] += g.count;
      classified += g.count;
    }
    return {
      counts,
      total: c.total,
      complete: c.complete,
      unclassified: Math.max(0, c.total - classified),
    };
  }, [fleetCensus]);

  const attention = useMemo(
    () => (fleetData ? attentionRows(fleetData.items) : []),
    [fleetData],
  );

  const firing = useMemo(
    () => (isReady(alerts) ? alerts.data.filter((a) => a.firing) : []),
    [alerts],
  );

  const boundRows = useMemo(() => {
    if (!isReady(tenants)) return null;
    if (usage.kind === "loading") return null;
    return bounds(tenants.data, isReady(usage) ? usage.data : null);
  }, [tenants, usage]);

  const queueItems = useMemo(
    () => (queue.kind === "ready" ? [...queue.data.items].sort(byAge) : []),
    [queue],
  );

  const orderedStops = useMemo(() => {
    if (!isReady(stops)) return [];
    return [...stops.data].sort(
      (a, b) => STOP_RANK[a.level] - STOP_RANK[b.level] || a.scope.localeCompare(b.scope),
    );
  }, [stops]);
  // Every stop whose reach this page can phrase becomes a queue row; the queue
  // caps itself, so there is no second cap here. A stop whose scope cannot be
  // stated exactly is still counted and named below, never silently dropped.
  const shownStops = orderedStops.filter((s) => SCOPE_KIND[s.level] !== undefined);
  const unshownStops = orderedStops.length - shownStops.length;

  const needs = needsYouRows({
    stops: isReady(stops) ? shownStops : [],
    approvals: queueItems,
    attention,
    firing,
  });
  const needsTone = needs.some((n) => n.variant === "crit") ? "crit" : "hold";
  // "Nothing needs you" is a claim about four backends. It may only be made when
  // all four answered — a zero standing in for a refusal is the one thing this
  // line must never do. A non-zero count is safe either way: it is a floor.
  const everySourceAnswered =
    isReady(stops) &&
    queue.kind === "ready" &&
    fleet.kind === "ready" &&
    (isReady(alerts) || alerts.kind === "unavailable");
  const status = statusLineParts({
    namespace,
    total: fleetBar?.total,
    totalIsBound: fleetBar ? !fleetBar.complete : undefined,
    serving: fleetBar?.counts.serving,
    needs: needs.length > 0 || everySourceAnswered ? needs.length : undefined,
  });


  // ── The first-run gate (DX-4 semantics, carried forward unchanged) ───────
  //
  // Only a "ready" runs feed can confirm a run happened; loading / 501 / error
  // all mean unknown ⇒ treat as not-yet-run. A cluster with a provider and an
  // agent whose runs feed is UNAVAILABLE is treated as set up: we cannot verify
  // a run, so we must not nag about it forever.
  const hasProvider = isReady(providers) && providers.data > 0;
  const hasAgent = fleetData !== null && fleetData.items.length > 0;
  const hasRun = isReady(runs) && runs.data > 0;
  const gateReady = isReady(providers) && fleetData !== null;
  const showChecklist =
    gateReady && !(hasProvider && hasAgent && (hasRun || runs.kind === "unavailable"));
  // A genuinely empty install: there is provably nothing to queue, spend, or
  // look at, so Home IS the checklist rather than four empty frames.
  const firstRun = showChecklist && fleetData !== null && fleetData.items.length === 0;

  const scopeWord = namespace || "all workspaces";
  const meta = facts
    ? `${scopeWord} · ${
        facts.complete
          ? plural(facts.total, "agent")
          : `${facts.total} agents on this page`
      }`
    : scopeWord;

  const showAttentionRow =
    !firstRun && (attention.length > 0 || alerts.kind !== "ready" || firing.length > 0);

  return (
    <div className="min-w-0 space-y-6" data-testid="home-page">
      <PageHeader
        title="Home"
        meta={meta}
        lede={
          // §7 A11: while it loads the lede is a BAR, never placeholder prose.
          fleet.kind === "loading" && queue.kind === "loading" ? (
            <Skeleton className="mt-1 h-4 w-[28rem] max-w-full" />
          ) : (
            <StatusLine parts={status} tone={needsTone} />
          )
        }
        actionsSlot={
          <>
            <Button
              variant="outline"
              size="sm"
              className="text-sm"
              onClick={refresh}
              disabled={refreshing}
              data-testid="home-refresh"
            >
              <RefreshCw className="h-4 w-4" />
              Refresh
            </Button>
            {canCreate && (
              <Button asChild size="sm" className="text-sm" data-testid="new-agent-button">
                <Link to="/agents/new">
                  <Sparkles className="h-4 w-4" />
                  New agent
                </Link>
              </Button>
            )}
          </>
        }
      />

      {/* 1 ── Everything blocked on a person, in one ranked list. Stops are its
          first rows rather than a second red surface above it; lifting one is a
          destructive act and lives on the page that owns it. */}
      <NeedsYou rows={needs} />
      {queue.kind === "ready" && queue.data.unreadable > 0 && (
        <QuietNote title="Some workspaces did not answer.">
          {plural(queue.data.unreadable, "workspace")} refused the queue read, so
          anything waiting in {queue.data.unreadable === 1 ? "it" : "them"} is not
          counted above. This is what the account can see, not what exists.
        </QuietNote>
      )}
      {unshownStops > 0 && (
        <QuietNote
          title={`${plural(unshownStops, "further stop")} ${unshownStops === 1 ? "is" : "are"} in force.`}
        >
          Home states a stop&rsquo;s reach exactly or not at all, and{" "}
          {unshownStops === 1 ? "this one covers a scope" : "these cover scopes"}{" "}
          this banner cannot phrase without over- or understating it. The Stops
          page lists every stop in force, with what each one holds.{" "}
          <NextStepLink label="Review the stops" to="/stops" tone="crit" />
        </QuietNote>
      )}
      {/* Every way the stop list can fail to answer says so. Gating this on
          kind === "error" alone left 403 (forbidden) and 501 (unavailable)
          rendering NOTHING, and Home then read as an all-clear about stops it
          had never been shown (M151 hardening, B1). Silence about a halt is
          the one silence this page cannot afford. */}
      {stops.kind === "error" && (
        <QuietNote title="Whether anything is stopped could not be read.">
          The stop list did not answer ({stops.message}), so this page cannot say
          that nothing is halted — only that it does not know.{" "}
          <NextStepLink label="Open the stops" to="/stops" tone="crit" />
        </QuietNote>
      )}
      {stops.kind === "forbidden" && (
        <QuietNote title="Whether anything is stopped is not yours to read.">
          Your account cannot list emergency stops, so this page cannot say that
          nothing is halted. Work of yours may be stopped without it showing
          here — ask an administrator, or open the stops to see what you are
          allowed.{" "}
          <NextStepLink label="Open the stops" to="/stops" tone="crit" />
        </QuietNote>
      )}
      {stops.kind === "unavailable" && (
        <QuietNote title="This install cannot say whether anything is stopped.">
          The emergency stop switch is not configured here, so there is no stop
          list to read. Nothing on this page speaks to whether work has been
          halted by other means.
        </QuietNote>
      )}

      {/* 2 ── The fleet census. */}
      <section aria-labelledby="home-fleet">
        <SectionHeader
          id="home-fleet"
          title="Across the fleet"
          lede={
            namespace
              ? `Where every agent in ${namespace} sits in its lifecycle right now.`
              : "Where every agent sits in its lifecycle right now."
          }
        />
        {fleet.kind === "loading" && <SkeletonCard />}
        {fleet.kind === "forbidden" && (
          <ForbiddenInline resource="agents" />
        )}
        {fleet.kind === "unavailable" && (
          <QuietNote title="The agent registry isn’t readable on this install.">
            The census reads the agent registry. Without it there is no count to
            draw — nothing here is estimated.
          </QuietNote>
        )}
        {fleet.kind === "error" && (
          <ErrorState
            title="The fleet could not be read"
            description="The agent list did not answer, so the lifecycle census is absent rather than estimated."
            detail={fleet.message}
            onRetry={() => load()}
          />
        )}
        {fleetBar && (
          <>
            <FleetBar bar={fleetBar} namespace={namespace} />
            {!fleetBar.complete && (
              <QuietNote
                className="mt-3"
                title={`These counts are a floor, not a total.`}
              >
                The fleet is larger than one census will walk, so every number
                above is at least that many and possibly more. It is a bound, not
                a guess.{" "}
                <NextStepLink label="Open the fleet" to="/agents" />
              </QuietNote>
            )}
            {fleetBar.unclassified > 0 && (
              <QuietNote
                className="mt-3"
                title={`${plural(fleetBar.unclassified, "agent")} could not be classified.`}
              >
                Their status did not fit the census table, so they are counted in
                the fleet total but sit in no stage above.
              </QuietNote>
            )}
          </>
        )}
      </section>

      {/* The guided path, while the install is not set up yet (§7 A11). */}
      {showChecklist && (
        <FirstRun hasProvider={hasProvider} hasAgent={hasAgent} hasRun={hasRun} />
      )}

      {/* 3 + 4 ── What is waiting on a person · what it is spending. */}
      {!firstRun && (
        <div className="grid min-w-0 items-start gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          {queue.kind !== "ready" && (
            <WaitingPanel state={queue} items={queueItems} onRetry={() => load()} />
          )}
          <SpendingPanel
            tenants={tenants}
            usage={usage}
            rows={boundRows}
            onRetry={() => load()}
          />
        </div>
      )}

      {/* 5 ── The rest of the fleet's own signal. Anything BLOCKING a person is
          already a queue row above; these two carry what is merely worth knowing,
          plus the honest report when a source did not answer at all. */}
      {showAttentionRow && (
        <div className="grid min-w-0 items-start gap-5 lg:grid-cols-2">
          {attention.some((a) => a.variant !== "crit") && (
            <AttentionPanel
              rows={attention.filter((a) => a.variant !== "crit")}
              complete={facts?.complete ?? false}
            />
          )}
          {(alerts.kind !== "ready" || firing.some((a) => alertVariant(a) === "warn")) && (
            <AlertsPanel
              state={alerts}
              firing={firing.filter((a) => alertVariant(a) === "warn")}
              truncated={alertsTruncated}
              onRetry={() => load()}
            />
          )}
        </div>
      )}

      {!firstRun && (canCreate || canConnectProvider) && (
        <div
          className="flex flex-wrap gap-2 border-t border-border-soft pt-4"
          data-testid="home-start-rail"
        >
          {canCreate && (
            <StartLink to="/agents/new" label="New agent" />
          )}
          {canConnectProvider && (
            <StartLink to="/providers/connect" label="Connect a provider" />
          )}
          <StartLink to="/gallery" label="Browse the gallery" />
        </div>
      )}

      {!firstRun && (
        <Closing facts={facts} attention={attention.length} queue={queue} />
      )}
    </div>
  );
}

// ── Waiting on a person ─────────────────────────────────────────────────────

/**
 * The approval queue is read one workspace at a time (the endpoint 400s without
 * a namespace), so with "all workspaces" selected the page fans out over every
 * workspace the caller can list. A workspace that REFUSES is counted, never
 * swallowed: a 403 folded into `[]` renders as "nothing is waiting on you",
 * which is the one lie this panel cannot afford.
 */
function loadQueue(
  controller: AbortController,
  namespace: string,
  set: (q: QueueLoad) => void,
) {
  if (namespace) {
    api
      .listApprovals(namespace, controller.signal)
      .then((items) => set({ kind: "ready", data: { items, unreadable: 0 } }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        set(toFailure(err));
      });
    return;
  }

  api
    .namespaces(controller.signal)
    .then(async (resp) => {
      // Bounded: this runs on the landing page, and an install with sixty
      // workspaces used to open sixty concurrent requests on every load.
      const outcomes = await mapLimit(
        resp.namespaces,
        NAMESPACE_SCAN_CONCURRENCY,
        (ns) =>
          api
            .listApprovals(ns.name, controller.signal)
            .then((items) => ({ ok: true as const, items }))
            .catch((err: unknown) => ({ ok: false as const, failure: toFailure(err) })),
      );
      if (controller.signal.aborted) return;
      const answered = outcomes.filter(
        (o): o is { ok: true; items: ApprovalQueueItem[] } => o.ok,
      );
      if (outcomes.length > 0 && answered.length === 0) {
        // Nothing answered, and WHY nothing answered is the point: an install
        // without the queue and a caller without the grant are different truths.
        const first = outcomes.find((o) => !o.ok);
        set(
          first && !first.ok
            ? first.failure
            : { kind: "error", message: "no workspace answered" },
        );
        return;
      }
      const items: ApprovalQueueItem[] = [];
      const seen = new Set<string>();
      for (const o of answered) {
        for (const item of o.items) {
          if (seen.has(item.runId)) continue;
          seen.add(item.runId);
          items.push(item);
        }
      }
      set({
        kind: "ready",
        data: { items, unreadable: outcomes.length - answered.length },
      });
    })
    .catch((err: unknown) => {
      if (controller.signal.aborted) return;
      // Cannot even enumerate the workspaces. That is neither "nothing is
      // waiting" nor "you may not read approvals" — it is its own state.
      if (err instanceof ApiError && err.isForbidden) {
        set({ kind: "unscoped" });
        return;
      }
      set(toFailure(err));
    });
}

/**
 * The queue everything blocked on a person lands in. Empty is a SUCCESS state and
 * is styled as one — an empty box would read as "not loaded".
 */
/**
 * The fleet, as one bar whose every stage opens the list behind it.
 *
 * This replaces the BUILD / GOVERN / SHIP / IMPROVE strip, which looked like
 * navigation and was not — teaching people that things on this page do not
 * respond, which then applied to everything below it. The lifecycle that is
 * actually worth a spine is the AGENT's, and it is real data.
 *
 * Colour says state, form says clickable (design system §2.3): the pip carries
 * the semantic hue and the label carries the pine link treatment, so a status
 * tint is never mistaken for something you can press.
 */
/** One creation entry point. Present only when the caller can actually complete it. */
function StartLink({ to, label }: { to: string; label: string }) {
  return (
    <Link
      to={to}
      className="rounded-sm border border-border-strong px-3 py-1.5 text-sm hover:bg-surface-2"
    >
      <span className="mr-2 font-mono text-primary">+</span>
      {label}
    </Link>
  );
}

function FleetBar({
  bar,
  namespace,
}: {
  bar: {
    counts: Record<Bucket, number>;
    total: number;
    complete: boolean;
    unclassified: number;
  };
  namespace: string;
}) {
  const scope = namespace ? `?namespace=${encodeURIComponent(namespace)}&` : "?";
  return (
    <div data-testid="home-fleet-bar">
      <div
        className="flex h-2 gap-px overflow-hidden rounded-sm bg-surface-2"
        role="presentation"
      >
        {BUCKETS.filter((b) => bar.counts[b.id] > 0).map((b) => (
          <span
            key={b.id}
            className={b.tint}
            style={{ width: `${(bar.counts[b.id] / Math.max(1, bar.total)) * 100}%` }}
          />
        ))}
      </div>
      <ul className="mt-3 flex flex-wrap gap-x-5 gap-y-1.5 text-sm">
        {BUCKETS.map((b) => {
          const n = bar.counts[b.id];
          return (
            <li key={b.id} className="flex items-center gap-2">
              <span className={`h-2 w-2 shrink-0 rounded-sm ${b.tint}`} aria-hidden />
              {n > 0 ? (
                <Link
                  to={`/agents${scope}view=${b.view}`}
                  className="border-b-[1.5px] border-accent text-primary hover:border-primary"
                  data-testid={`home-fleet-${b.id}`}
                >
                  {b.label}
                </Link>
              ) : (
                <span className="text-faint">{b.label}</span>
              )}
              <span
                className={`font-mono ${n > 0 ? "text-foreground" : "text-faint"}`}
              >
                {n}
              </span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function NeedsYou({ rows }: { rows: Need[] }) {
  const shown = rows.slice(0, NEEDS_ROWS);
  const more = rows.length - shown.length;

  return (
    <Panel
      title="Needs you"
      testId="home-needs-you"
      meta={
        rows.length > 0 ? (
          <span className="font-mono">{rows.length} waiting</span>
        ) : undefined
      }
      foot={
        rows.length > 0 ? (
          <MoreLine
            text={
              more > 0
                ? `${plural(more, "more decision")} ${more === 1 ? "is" : "are"} waiting.`
                : "Everything waiting on a person is listed above."
            }
            label="Open the queue"
            to="/approvals"
          />
        ) : undefined
      }
    >
      {rows.length === 0 ? (
        <p className="px-5 py-4 font-serif text-md italic text-muted-foreground">
          Nothing is waiting on a person.
        </p>
      ) : (
        <ul>
          {shown.map((n) => (
            <Item
              key={n.key}
              word={n.word}
              variant={n.variant}
              headline={
                <>
                  <span className="font-mono">{n.subject}</span> — {n.why}
                </>
              }
              sub={n.since ? `${n.where} · ${formatRelativeTime(n.since)}` : n.where}
              next={
                <NextStepLink
                  label={n.action.label}
                  to={n.action.to}
                  tone={n.action.tone}
                  testId={n.action.testId}
                />
              }
              testId={`home-need-${n.kind}`}
            />
          ))}
        </ul>
      )}
    </Panel>
  );
}

/** Scope, size, and the page's whole point, in one line. */
function StatusLine({
  parts,
  tone,
}: {
  parts: ReturnType<typeof statusLineParts>;
  tone: "crit" | "hold";
}) {
  return (
    <span
      className="flex flex-wrap items-baseline gap-x-2 gap-y-1"
      data-testid="home-status-line"
    >
      <span className="font-mono font-medium text-foreground">{parts.scope}</span>
      {parts.clauses.map((c) => (
        <span key={c} className="flex items-baseline gap-2">
          <span className="text-ghost">·</span>
          <span className="font-mono text-foreground">{c}</span>
        </span>
      ))}
      {parts.needs !== undefined && (
        <span className="flex items-baseline gap-2">
          <span className="text-ghost">·</span>
          {parts.needs > 0 ? (
            <span
              className={
                tone === "crit"
                  ? "font-semibold text-destructive"
                  : "font-semibold text-info"
              }
            >
              {parts.needs} {parts.needs === 1 ? "needs" : "need"} you
            </span>
          ) : (
            <span className="font-semibold text-success">nothing needs you</span>
          )}
        </span>
      )}
    </span>
  );
}

function WaitingPanel({
  state,
  items,
  onRetry,
}: {
  state: QueueLoad;
  items: ApprovalQueueItem[];
  onRetry: () => void;
}) {
  const shown = items.slice(0, PANEL_ROWS);
  const oldest = items[0]?.waitingSince;
  const meta =
    state.kind === "ready" && items.length > 0 ? (
      <>
        {/* The one place the hold violet is spent on this panel: the count.
            Repeating it on every row would say the same thing three times in
            the hue that means "act now", which is how a hue stops being read. */}
        <span className="font-semibold text-hold">{items.length}</span> awaiting a
        person{oldest ? ` · oldest ${formatRelativeTime(oldest)}` : ""}
      </>
    ) : undefined;

  return (
    <Panel
      title="Waiting on a person"
      meta={meta}
      testId="home-waiting"
      foot={
        state.kind === "ready" && items.length > 0 ? (
          <MoreLine
            text={
              items.length > shown.length
                ? `${plural(items.length - shown.length, "more decision")} ${
                    items.length - shown.length === 1 ? "is" : "are"
                  } waiting.`
                : "The queue shows every ask in full, oldest first."
            }
            label="Open the queue"
            to="/approvals"
          />
        ) : undefined
      }
    >
      {state.kind === "loading" && (
        <div className="p-5">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="mt-3 h-4 w-2/3" />
          <Skeleton className="mt-3 h-4 w-1/2" />
        </div>
      )}

      {state.kind === "forbidden" && (
        <div className="p-5">
          <ForbiddenInline resource="approvals" />
        </div>
      )}

      {state.kind === "unavailable" && (
        <div className="p-5">
          <QuietNote title="The approval queue isn’t part of this install.">
            Runs that pause for a person are recorded by the approval queue, and
            none is wired here. Nothing is estimated — the queue is absent, which
            is not the same as empty.
          </QuietNote>
        </div>
      )}

      {state.kind === "unscoped" && (
        <div className="p-5">
          <QuietNote title="The queue is read one workspace at a time.">
            This account cannot list the workspaces, so the queue cannot be
            gathered across all of them. Pick a workspace from the menu above and
            this panel will show what is waiting in it.
          </QuietNote>
        </div>
      )}

      {state.kind === "error" && (
        <div className="p-5">
          <ErrorState
            title="The queue could not be read"
            description="An empty panel here would read as “nothing is waiting”, which is not what happened."
            detail={state.message}
            onRetry={onRetry}
          />
        </div>
      )}

      {state.kind === "ready" && items.length === 0 && (
        <p className="px-5 py-6 font-serif text-md italic text-faint">
          Nothing is waiting on a person. Every run that started is either
          finished or still the machine&rsquo;s own work.
        </p>
      )}

      {state.kind === "ready" && items.length > 0 && (
        <ul className="min-w-0">
          {shown.map((item) => (
            <Item
              key={item.runId}
              testId={`home-waiting-${item.runId}`}
              // A taxonomy word, not a state: every row here is in the SAME
              // condition, so the kind tag is the neutral register.
              word={KIND_WORD[item.kind]}
              variant="muted"
              headline={
                item.message?.trim() || `Run ${item.runId} is waiting for a decision`
              }
              sub={`${item.agent} · ${item.namespace}${
                item.waitingSince
                  ? ` · ${formatRelativeTime(item.waitingSince)}`
                  : " · age not recorded"
              }`}
              next={
                // Deviation from §6.1 A11's "or inline Approve/Deny", with the
                // reason approvals-page already wrote down: a row shows a
                // PRÉCIS of the ask, and Home's is shorter still. Deciding from
                // it would be authority the row has not earned, so every row
                // leads to the surface that shows the whole thing.
                <NextStepLink
                  label={item.kind === "plan_approval" ? "Read the plan" : "Review the ask"}
                  to={`/runs/${encodeURIComponent(item.runId)}`}
                  ariaLabel={`Review the decision waiting on run ${item.runId}`}
                  testId={`home-review-${item.runId}`}
                />
              }
            />
          ))}
        </ul>
      )}

    </Panel>
  );
}

// ── Spending ────────────────────────────────────────────────────────────────

function SpendingPanel({
  tenants,
  usage,
  rows,
  onRetry,
}: {
  tenants: Load<TenantSummary[]>;
  usage: Load<TenantUsageItem[]>;
  rows: Bound[] | null;
  onRetry: () => void;
}) {
  const shown = rows?.slice(0, 4) ?? [];
  const more = (rows?.length ?? 0) - shown.length;

  return (
    <Panel
      title="Spending"
      meta={rows && rows.length > 0 ? plural(rows.length, "tenant") : undefined}
      testId="home-spending"
      foot={
        rows && rows.length > 0 ? (
          <MoreLine
            text={
              more > 0
                ? `${plural(more, "further tenant")} ${more === 1 ? "is" : "are"} not shown here.`
                : "Per-tenant totals and month-end forecasts live on the Cost page."
            }
            label="Open the cost page"
            to="/cost"
          />
        ) : undefined
      }
    >
      {tenants.kind === "loading" && (
        <div className="space-y-4 p-5">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-full" />
        </div>
      )}

      {tenants.kind === "forbidden" && (
        <div className="p-5">
          <ForbiddenInline resource="tenants" />
        </div>
      )}

      {tenants.kind === "unavailable" && (
        <div className="p-5">
          <QuietNote title="Tenants aren’t configured on this install.">
            Model budgets are declared per tenant, and this install declares
            none. There is no bound to draw spend against — nothing here is
            estimated.
          </QuietNote>
        </div>
      )}

      {tenants.kind === "error" && (
        <div className="p-5">
          <ErrorState
            title="The budgets could not be read"
            description="Spend is drawn against the caps tenants declare, and the tenant list did not answer."
            detail={tenants.message}
            onRetry={onRetry}
          />
        </div>
      )}

      {rows !== null && rows.length === 0 && (
        <p className="px-5 py-6 font-serif text-md italic text-faint">
          No tenant is defined, so nothing on this cluster has a spending bound
          to be measured against.
        </p>
      )}

      {shown.length > 0 && (
        <div className="space-y-5 p-5">
          {shown.map((b) => (
            <BoundMeter key={b.tenant} bound={b} />
          ))}
          {usage.kind === "unavailable" ? (
            <QuietNote title="Live spend isn’t recorded on this install.">
              The caps above are real — each tenant declares its own. What has
              been spent against them comes from the gateway&rsquo;s usage store,
              which is not wired here, so every bar reads{" "}
              <span className="font-mono">&mdash;</span> rather than a zero.
            </QuietNote>
          ) : usage.kind === "forbidden" ? (
            <QuietNote title="This account cannot read live spend.">
              The caps above are readable; the usage behind them is not. The bars
              are drawn empty rather than at zero, because zero would be a claim
              this account was never given.
            </QuietNote>
          ) : usage.kind === "error" ? (
            <QuietNote title="Live spend did not answer.">
              The caps above are real. What has been spent against them could not
              be read ({usage.message}), so the bars are empty rather than at
              zero.
            </QuietNote>
          ) : (
            <p className="text-sm text-faint">
              The tick on every bar is {Math.round(NEAR_CAP_RATIO * 100)}% of the
              cap — where this console calls a tenant near its budget. It is the
              console&rsquo;s line, not one the platform was configured with.
            </p>
          )}
        </div>
      )}
    </Panel>
  );
}

// ── Needs looking at ────────────────────────────────────────────────────────

function AttentionPanel({
  rows,
  complete,
}: {
  rows: Attention[];
  complete: boolean;
}) {
  const shown = rows.slice(0, PANEL_ROWS);
  const more = rows.length - shown.length;
  return (
    <Panel
      title="Needs looking at"
      meta={
        complete ? plural(rows.length, "agent") : `${rows.length} on this page`
      }
      testId="home-attention"
      foot={
        <MoreLine
          text={
            more > 0
              ? `${plural(more, "further agent")} ${more === 1 ? "needs" : "need"} looking at${
                  complete ? "" : " on this page"
                }.`
              : "The fleet list sorts every agent by what is blocking."
          }
          label="Open the fleet"
          to="/agents"
        />
      }
    >
      <ul className="min-w-0">
        {shown.map((r) => (
          <Item
            key={r.key}
            testId={`home-attention-${r.name}`}
            word={r.word}
            variant={r.variant}
            headline={
              <>
                <span className="font-mono text-sm font-semibold">{r.name}</span>
                <span className="text-secondary-foreground"> — {r.why}</span>
              </>
            }
            sub={r.namespace}
            next={
              <NextStepLink
                label={r.label}
                tone={r.tone}
                to={r.to}
                ariaLabel={`${r.label} — ${r.name}`}
                testId={`home-next-${r.name}`}
              />
            }
          />
        ))}
      </ul>
    </Panel>
  );
}

// ── Alerts ──────────────────────────────────────────────────────────────────

/**
 * Which hue an alert wears. A crossed bound or a degraded objective is warn; a
 * hard refusal or a quality regression will not proceed without a change, which
 * is what crit means (§2.2).
 */
function alertVariant(a: AlertSummary): "crit" | "warn" {
  return a.type === "budgetHard" ||
    a.type === "errorRate" ||
    a.type === "regressionDetected"
    ? "crit"
    : "warn";
}

/** The tag word, from the alert's own condition. Never invented. */
function alertWord(a: AlertSummary): string {
  return a.type.replace(/([a-z])([A-Z])/g, "$1 $2").toLowerCase();
}

function AlertsPanel({
  state,
  firing,
  truncated,
  onRetry,
}: {
  state: Load<AlertSummary[]>;
  firing: AlertSummary[];
  truncated: boolean;
  onRetry: () => void;
}) {
  const shown = firing.slice(0, PANEL_ROWS);
  const more = firing.length - shown.length;
  // The feed is limit-capped. When it came back full, every count derived from it
  // describes the page and not the fleet — so say "50+", never "50".
  const bound = truncated ? "+" : "";

  return (
    <Panel
      title="Alerts"
      meta={
        state.kind === "ready" ? `${firing.length}${bound} firing` : undefined
      }
      testId="home-alerts"
      foot={
        state.kind === "ready" && firing.length > 0 ? (
          <MoreLine
            text={
              more > 0
                ? `${more}${bound} further alert${more === 1 && !bound ? " is" : "s are"} firing.`
                : "Resolved alerts and their history live on the Alerts page."
            }
            label="Open the alerts"
            to="/alerts"
          />
        ) : undefined
      }
    >
      {state.kind === "loading" && (
        <div className="p-5">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="mt-3 h-4 w-2/3" />
        </div>
      )}

      {state.kind === "unavailable" && (
        <div className="p-5">
          {/* The canonical §7.1 "not configured" note — the pattern the mock's
              "Cost by trace" panel established. Not an error, not a zero: this
              install simply has no alert store to ask. */}
          <QuietNote title="Alerts aren’t configured.">
            Budgets, latency objectives and regression watches are evaluated by
            the alert store, and none is wired here. The bounds in the Spending
            panel are still exact — they come from the tenants themselves.
            Nothing is estimated; the alert history is simply absent.
          </QuietNote>
        </div>
      )}

      {state.kind === "forbidden" && (
        <div className="p-5">
          <ForbiddenInline resource="alerts" />
        </div>
      )}

      {state.kind === "error" && (
        <div className="p-5">
          <ErrorState
            title="Alerts could not be read"
            description="An empty panel here would read as “nothing is firing”, which is not what happened."
            detail={state.message}
            onRetry={onRetry}
          />
        </div>
      )}

      {state.kind === "ready" && firing.length === 0 && (
        <p className="px-5 py-6 font-serif text-md italic text-faint">
          No alert is firing. Every budget, latency objective and regression
          watch on this cluster is inside its bound.
        </p>
      )}

      {state.kind === "ready" && firing.length > 0 && (
        <ul className="min-w-0">
          {shown.map((a) => {
            // `agent` arrives as "namespace/name" when the alert names one, so
            // the row can lead to the agent itself instead of a second list.
            const slash = a.agent?.indexOf("/") ?? -1;
            const agentPath =
              a.agent && slash > 0
                ? resourcePath("agent", a.agent.slice(0, slash), a.agent.slice(slash + 1))
                : null;
            return (
              <Item
                key={a.id}
                testId={`home-alert-${a.id}`}
                word={alertWord(a)}
                variant={alertVariant(a)}
                headline={a.message || `${a.policy} fired`}
                sub={`${a.policy} · ${a.namespace}${a.value ? ` · ${a.value}` : ""} · ${formatRelativeTime(a.firedAt)}`}
                next={
                  <NextStepLink
                    label={agentPath ? "Open the agent" : "Open the alert"}
                    to={agentPath ?? "/alerts"}
                    ariaLabel={`Open ${a.policy}`}
                    testId={`home-alert-next-${a.id}`}
                  />
                }
              />
            );
          })}
        </ul>
      )}
    </Panel>
  );
}

// ── The first-run path ──────────────────────────────────────────────────────

/**
 * The guided three steps (`FIRST_RUN_CHECKLIST` in nav.ts — the IA and the
 * checklist are one source of truth). ORDERED: a step is done only once every
 * earlier step is, so the list cannot show "Create an agent" done while
 * "Connect a provider" is still open.
 */
function FirstRun({
  hasProvider,
  hasAgent,
  hasRun,
}: {
  hasProvider: boolean;
  hasAgent: boolean;
  hasRun: boolean;
}) {
  const done: Record<string, boolean> = {
    provider: hasProvider,
    agent: hasProvider && hasAgent,
    run: hasProvider && hasAgent && hasRun,
  };
  const steps = FIRST_RUN_CHECKLIST.map((s) => ({
    label: s.label,
    to: s.to,
    done: done[s.doneKey],
  }));
  const next = steps.find((s) => !s.done);

  return (
    <section aria-labelledby="home-first-run" data-testid="first-run-checklist">
      <SectionHeader
        id="home-first-run"
        title="Get started"
        lede="Three steps to your first running agent — paste a key once, describe an agent, run it. No YAML, no kubectl."
      />
      <Card className="min-w-0 p-5">
        <ol className="space-y-2">
          {steps.map((s, i) => (
            <li
              key={s.label}
              className="flex items-center gap-2 text-sm"
              data-testid={`first-run-step-${i}`}
            >
              {s.done ? (
                <CheckCircle2 className="h-4 w-4 shrink-0 text-success" />
              ) : (
                <Circle className="h-4 w-4 shrink-0 text-faint" />
              )}
              <span className={s.done ? "text-faint line-through" : undefined}>
                {s.label}
              </span>
            </li>
          ))}
        </ol>
        {next && (
          <Button asChild size="sm" className="mt-4 text-sm" data-testid="first-run-cta">
            <Link to={next.to}>{next.label}</Link>
          </Button>
        )}
      </Card>
    </section>
  );
}

// ── The closing line ────────────────────────────────────────────────────────

/**
 * The §5.18 closing note: the ratio the page already showed, in words. It is a
 * sighted flourish and RESTATES — never the only place a fact appears — so it
 * renders nothing at all when the counts behind it are not facts.
 */
function Closing({
  facts,
  attention,
  queue,
}: {
  facts: Census | null;
  attention: number;
  queue: QueueLoad;
}) {
  if (!facts || !facts.complete || facts.total === 0) return null;
  const waiting = queue.kind === "ready" ? queue.data.items.length : null;
  const quiet = facts.total - attention;
  if (attention === 0 && waiting === 0) {
    return (
      <ClosingNote>
        Nothing needs a person. All {plural(facts.total, "agent")}{" "}
        {facts.total === 1 ? "is running itself" : "are running themselves"}.
      </ClosingNote>
    );
  }
  if (attention === 0) return null;
  return (
    <ClosingNote>
      {plural(attention, "agent")} of {facts.total} need
      {attention === 1 ? "s" : ""} looking at. The other {quiet}{" "}
      {quiet === 1 ? "needs" : "need"} nothing from you.
    </ClosingNote>
  );
}
