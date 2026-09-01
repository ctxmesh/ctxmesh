import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Filter, MessagesSquare } from "lucide-react";

import {
  CellEntity,
  CellId,
  ClosingNote,
  DataTable,
  ErrorState,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  UnknownValue,
  isKnown,
  nextStepRank,
  truncateId,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, ApiError, type RunSummary, type RunsFilteredParams } from "@/lib/api";
import { formatDateTime, formatLatency, formatRelativeTime } from "@/lib/format";

// RunsPage — the global run feed, on the editorial ACTIVITY-FEED archetype
// (M151 §6.1 A1 composition, §4.4 activity-feed column budget:
// Time(1) · Agent(1) · What(1) · Duration(3) · Cost(3) · State(2) · Next step(1)).
//
// ── A FEED IS CHRONOLOGICAL, NOT TRIAGED ────────────────────────────────────
// The eleven resource lists sort by what is blocking. A feed may not: "what
// happened, most recent first" IS the thing a run feed is for, and re-ordering
// it by attention would make the same page mean two different things depending
// on the row's outcome. So the sort is `timestamp desc`, with `nextStepRank` as
// the tie-break (identical timestamps put "Nothing needed" last), and the
// attention view is reachable in one click through the "Needs you" chip. The
// Next step column still rides on every row that needs a person.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// A run's outcome reaches this list ONLY through the opt-in per-trace
// enrichment (ADR 0081). An unenriched row has no status at all, and that is
// NOT a pass: it renders the readable `—` with a reason, never a green chip and
// never a fabricated "ok". Same for a duration the trace store did not record.
//
// Backend: GET /api/runs?agent=&from=&to=&q=&limit=&cursor=&enrich=1
//   • agent (ns/name), from, to: SERVER-SIDE filters (the bar above the chips)
//   • q: page-windowed CLIENT-SIDE substring filter (the BFF filters the loaded
//     page window, NOT the whole cluster — K8s has no server-side substring
//     search). Because q is page-windowed, a page can return SHORT while
//     nextCursor is present, so "more pages" keys off the CURSOR, never off
//     runs.length. The DataTable renders that honest state for free.
//   • NO status filter — rejected server-side in m16.3. Do NOT add one.
//
// 501-calm / 502-error discipline (mirrors agentRuns + feedback):
//   • 501 (Langfuse not configured): runsFiltered() returns null → a calm
//     QuietNote saying the capability is absent. NEVER an error state, never a
//     zero, never a toast.
//   • 200 + notice (the trace store is transiently down): a real, retryable
//     ErrorState — distinct from "no runs", so a reader knows to retry.
//   • 502 (Langfuse configured, upstream fetch FAILED): ApiError → error state.
//
// RBAC: READ-only (ADR 0011 — the API is the real gate). No write affordances.
//
// data-testid contract:
//   runs-page             — root container
//   runs-filter-bar       — the server-side filter inputs
//   runs-unavailable      — the 501 "not configured" QuietNote
//   runs-degraded         — the transient trace-store failure
//   run-agent-link-{id}   — the row's link to its originating agent
//   next-step-{id}        — the row's Next step cell

const PAGE_LIMIT = 50;

// ── The honest-unknown vocabulary (§7.1) ────────────────────────────────────

const OUTCOME_UNKNOWN_TITLE =
  "This run's outcome was not recorded — unknown, not a pass. The trace list carries no per-trace status; only the enriched path can fill it.";

const DURATION_UNKNOWN_TITLE =
  "No duration was recorded for this run — unknown, not zero.";

const AGENT_UNKNOWN_TITLE =
  "This run carries no agent tag, so the agent that launched it is unknown — it was not launched by a named agent, or the tag was never written.";

const WHEN_UNKNOWN_TITLE = "This run carries no timestamp — unknown, not never.";

/**
 * Money, §4.5: `< $1` renders 4 decimals, `≥ $1` renders 2, and the column is
 * NEVER truncated, wrapped, or elided (the `numeric` column register keeps it
 * `whitespace-nowrap`). One precision rule down the whole column, so `$0.0003`
 * and `$0.0421` line up on their decimal instead of one collapsing to `$0.00`.
 */
export function formatMoney(usd: number): string {
  return usd >= 1 ? `$${usd.toFixed(2)}` : `$${usd.toFixed(4)}`;
}

// ── Triage: what the page renders and sorts by, decided once ────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Row {
  run: RunSummary;
  next: NextStep;
}

/**
 * One run → its next step. Only a FAILED run asks anything of a person: a run
 * that succeeded is finished, and a run whose outcome was never recorded is not
 * a failure — inventing an errand for either would make the column noise.
 */
function triage(r: RunSummary): Row {
  const to = `/traces/${encodeURIComponent(r.traceId)}`;
  const next: NextStep =
    r.status === "error"
      ? // Crit is the one action hue allowed here (§2.3): the target genuinely
        // is a failure.
        { label: "Open the failure", tone: "crit", to }
      : { tone: "none" };
  return { run: r, next };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "succeeded" | "all";

const VIEWS: { id: ViewId; label: string; match: (r: Row) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (r) => r.next.tone !== "none" },
  { id: "succeeded", label: "Succeeded", match: (r) => r.run.status === "ok" },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "No run in this page window came back failed. Show everything to read the feed.",
  },
  succeeded: {
    title: "Nothing succeeded here",
    description:
      "No run in this page window carries a recorded success. Show everything — an unenriched run has no outcome at all, which is not the same as a failure.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number is counted from the rows in hand, the sentence
 * says so whenever the rows in hand are not the whole feed, and it never claims
 * an unrecorded outcome was a clean one.
 */
export function closingLine(rows: Row[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const failed = rows.filter((r) => r.run.status === "error").length;
  const unrecorded = rows.filter((r) => !r.run.status).length;
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More pages follow.";

  if (total === 1) {
    const one =
      failed === 1
        ? "The one run here failed, and opening it is the only thing waiting on you."
        : unrecorded === 1
          ? "The one run here came back with no recorded outcome — unknown, not clean."
          : "The one run here finished without an error.";
    return `${one}${more}`;
  }

  const failedClause =
    failed === 0
      ? `None of the ${total} runs${where} failed.`
      : failed === total
        ? `Every one of the ${total} runs${where} failed.`
        : `${failed} of the ${total} runs${where} failed.`;

  // Counted in WORDS at one, so the sentence never reads "1 came back".
  const unrecordedClause =
    unrecorded === 0
      ? ""
      : unrecorded === 1
        ? " One came back with no recorded outcome — unknown, not clean."
        : ` ${unrecorded} came back with no recorded outcome — unknown, not clean.`;

  return `${failedClause}${unrecordedClause}${more}`;
}

// ── Cells ───────────────────────────────────────────────────────────────────

/**
 * The run's outcome as a tag. Uppercase mono on a tint, never interactive
 * (§2.1's form rule) — the action lives next door in the Next step link. An
 * ABSENT status is not a state: it renders the readable dash with its reason,
 * because "we did not record it" and "it passed" are different facts (§7.1).
 */
function StateTag({ status }: { status?: string }) {
  if (status === "ok") return <Badge variant="ok">OK</Badge>;
  if (status === "error") return <Badge variant="crit">Error</Badge>;
  return <UnknownValue title={OUTCOME_UNKNOWN_TITLE} />;
}

// RunsFilterBar holds the three server-side filter inputs: agent (ns/name),
// date-from, date-to. The q (free-text) filter is owned by the DataTable
// itself (via onQueryChange). NO status filter — m16.3 explicitly rejected it.
function RunsFilterBar({
  agent,
  from,
  to,
  onAgent,
  onFrom,
  onTo,
}: {
  agent: string;
  from: string;
  to: string;
  onAgent: (v: string) => void;
  onFrom: (v: string) => void;
  onTo: (v: string) => void;
}) {
  return (
    <div className="flex min-w-0 flex-wrap items-end gap-3" data-testid="runs-filter-bar">
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-agent"
          className="font-mono text-2xs font-medium uppercase tracking-wide text-faint"
        >
          Agent (ns/name)
        </label>
        <Input
          id="runs-filter-agent"
          value={agent}
          onChange={(e) => onAgent(e.target.value)}
          placeholder="namespace/agent-name"
          className="h-8 w-52 text-sm"
          aria-label="Filter by agent"
        />
      </div>
      <div className="flex flex-col gap-1">
        <span className="font-mono text-2xs font-medium uppercase tracking-wide text-faint">
          Range
        </span>
        <div className="flex gap-1" data-testid="runs-range-presets">
          {RANGE_PRESETS.map((p) => (
            <Button
              key={p.label}
              type="button"
              variant="outline"
              size="sm"
              className="h-8 px-2 text-xs"
              onClick={() => {
                onFrom(datetimeLocalAgo(p.ms));
                onTo("");
              }}
              data-testid={`runs-range-${p.label}`}
            >
              {p.label}
            </Button>
          ))}
        </div>
      </div>
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-from"
          className="font-mono text-2xs font-medium uppercase tracking-wide text-faint"
        >
          From
        </label>
        <Input
          id="runs-filter-from"
          type="datetime-local"
          value={from}
          onChange={(e) => onFrom(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter from date"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label
          htmlFor="runs-filter-to"
          className="font-mono text-2xs font-medium uppercase tracking-wide text-faint"
        >
          To
        </label>
        <Input
          id="runs-filter-to"
          type="datetime-local"
          value={to}
          onChange={(e) => onTo(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter to date"
        />
      </div>
    </div>
  );
}

// runNameDistinctFromAgent returns the run's own name ONLY when it adds information beyond the Agent
// column — i.e. it is NOT just the agent-identity display ("ns/name" or bare "name") the launcher
// stamps as the default trace name (M100 UI99-runstable, the NAME≈AGENT dedup). When the name is
// merely the agent identity (the common case), it returns "" and the What cell falls back to the
// run's id, so the eye never reads the same text twice in one row.
export function runNameDistinctFromAgent(r: RunSummary): string {
  const name = (r.name ?? "").trim();
  if (!name) return "";
  if (!r.agentName) return name; // ambient/untagged run — the name is all we have
  const identity = r.agentNs ? `${r.agentNs}/${r.agentName}` : r.agentName;
  return name === identity ? "" : name;
}

// RANGE_PRESETS are the quick time-range shortcuts every observability console offers (M99 B3) — a
// one-click "last N" that beats hand-entering two datetimes. Each sets `from = now − ms` and clears `to`
// (open-ended = up to now).
const RANGE_PRESETS: { label: string; ms: number }[] = [
  { label: "15m", ms: 15 * 60_000 },
  { label: "1h", ms: 60 * 60_000 },
  { label: "24h", ms: 24 * 60 * 60_000 },
  { label: "7d", ms: 7 * 24 * 60 * 60_000 },
];

// datetimeLocalAgo formats (now − ms) as a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time), the shape the From/To inputs + toRFC3339 already expect.
function datetimeLocalAgo(ms: number): string {
  const d = new Date(Date.now() - ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// toRFC3339 converts a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time, no seconds/timezone) into a UTC RFC3339 string the BFF's runs
// filter accepts. Returns "" for empty/unparseable input (so the caller omits the
// param rather than sending a malformed one — the m24 fix for the Runs 400).
function toRFC3339(dtLocal: string): string {
  if (!dtLocal) return "";
  const d = new Date(dtLocal);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString();
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; runs: RunSummary[]; nextCursor: string }
  | { kind: "unavailable" } // 501 — the trace backend is not configured
  | { kind: "degraded"; message: string } // 200 + notice — trace store transiently down
  | { kind: "error"; message: string; forbidden: boolean };

const LEDE =
  "Every run across your agents, newest first. A run that failed carries what to do about it; everything else is history.";

export function RunsPage() {
  const navigate = useNavigate();

  // Server-side filter state
  const [agentFilter, setAgentFilter] = useState("");
  const [fromFilter, setFromFilter] = useState("");
  const [toFilter, setToFilter] = useState("");

  // Client-side q filter (page-windowed substring)
  const [query, setQuery] = useState("");

  // The chip row is a set of VIEWS over the loaded window — one question with
  // one answer at a time (§5.28), never an AND of checkboxes.
  const [view, setView] = useState<ViewId>("all");

  // Cursor pagination: stack of cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    // The <input type="datetime-local"> value is "YYYY-MM-DDTHH:MM" in LOCAL time
    // (no seconds, no timezone) — the BFF's runs filter requires RFC3339, so
    // sending it raw 400s ("from must be RFC3339"). Convert local -> UTC RFC3339.
    const fromRfc = toRFC3339(fromFilter);
    const toRfc = toRFC3339(toFilter);

    const params: RunsFilteredParams = {
      limit: PAGE_LIMIT,
      // The Runs browser opts into per-trace enrichment (ADR 0081) so each row shows REAL
      // tokens + an ok/error status the list API can't carry. The dashboard's recent-runs
      // peek deliberately does NOT (it only needs cost/latency — the cheap path).
      enrich: true,
      ...(cursor ? { cursor } : {}),
      ...(agentFilter ? { agent: agentFilter } : {}),
      ...(fromRfc ? { from: fromRfc } : {}),
      ...(toRfc ? { to: toRfc } : {}),
      ...(query ? { q: query } : {}),
    };

    api
      .runsFiltered(params, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (trace backend not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        // A `notice` means the trace store is transiently down (200 + empty, m23.6):
        // show "temporarily unavailable — retry", NOT the misleading "No runs yet".
        if (res.notice) {
          setLoadState({ kind: "degraded", message: res.notice });
          return;
        }
        setLoadState({
          kind: "ready",
          runs: res.runs,
          nextCursor: res.nextCursor ?? "",
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 502 and other non-501 errors surface as a real error (retryable).
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [cursor, agentFilter, fromFilter, toFilter, query]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Reset to page 0 whenever any server-side filter changes or q changes.
  const resetPaging = useCallback(() => setPageStack([""]), []);

  function onAgentChange(v: string) {
    setAgentFilter(v);
    resetPaging();
  }
  function onFromChange(v: string) {
    setFromFilter(v);
    resetPaging();
  }
  function onToChange(v: string) {
    setToFilter(v);
    resetPaging();
  }
  function onQueryChange(q: string) {
    setQuery(q);
    resetPaging();
  }
  function clearServerFilters() {
    setAgentFilter("");
    setFromFilter("");
    setToFilter("");
    resetPaging();
  }

  const runs = useMemo(
    () => (loadState.kind === "ready" ? loadState.runs : []),
    [loadState],
  );
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR (BFF), NEVER off runs.length — a page-windowed
  // q filter can empty the current page while later pages still have matches.
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;
  // The loaded window IS the whole feed only when no cursor precedes or follows
  // it — the one condition under which counting the rows in hand is a FACT
  // rather than a windowed guess (kit FilterChipRow contract).
  const feedComplete = loadState.kind === "ready" && !hasNext && !hasPrev;

  // Newest first — a feed is chronological. `nextStepRank` breaks a timestamp
  // tie so "Nothing needed" still sinks below a row that needs a person, which
  // is the same column contract every other list obeys; the trace id breaks
  // that, so the order is stable across refetches.
  const sorted = useMemo(() => {
    const rows = runs.map(triage);
    rows.sort(
      (a, b) =>
        b.run.timestamp.localeCompare(a.run.timestamp) ||
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        a.run.traceId.localeCompare(b.run.traceId),
    );
    return rows;
  }, [runs]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling. Counts appear only when the loaded window provably IS the whole
  // feed — a count that describes one page while looking like a total is the
  // failure mode that hides work.
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: feedComplete ? sorted.filter(v.match).length : undefined,
  }));

  function onNext() {
    if (!hasNext) return;
    setPageStack((s) => [...s, nextCursor]);
  }
  function onPrev() {
    if (!hasPrev) return;
    setPageStack((s) => s.slice(0, -1));
  }

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "runs",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 ACTIVITY-FEED budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 2 below 768, 1 never.
  // Time, Agent, What and Next step survive every width — when, who, what, and
  // what to do about it.
  const columns: Column<Row>[] = [
    {
      id: "when",
      header: "When",
      priority: 1,
      className: "w-[6.5rem]",
      cell: ({ run: r }) =>
        r.timestamp ? (
          // Relative time is sanctioned in feeds (§4.5) — always with the
          // absolute in `title`, never instead of it.
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums"
            title={formatDateTime(r.timestamp)}
          >
            {formatRelativeTime(r.timestamp)}
          </span>
        ) : (
          <UnknownValue title={WHEN_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "agent",
      header: "Agent",
      priority: 1,
      // The cap is what makes `truncate` bite, and it is the column's FLOOR as
      // well as its ceiling: a `white-space: nowrap` cell contributes its whole
      // text as min-content, clamped by this max-width — so the cap is exactly
      // how wide this column will be. It steps with the viewport so the four
      // columns that may never be dropped (§4.4) all stay on screen at 768,
      // 1024, 1280 and 1440 without the frame having to scroll to reach them.
      className:
        "max-w-[9rem] lg:max-w-[9.5rem] xl:max-w-[11rem] min-[1440px]:max-w-[15rem]",
      cell: ({ run: r }) =>
        r.agentNs && r.agentName ? (
          <CellEntity
            title={`${r.agentNs}/${r.agentName}`}
            name={
              // Per-row link straight to the originating agent (m54.2, M49 UX
              // review A1) — no trace→agent hop. stopPropagation so the link
              // doesn't also trigger the row-click's navigate-to-trace.
              <Link
                to={`/agents/${encodeURIComponent(r.agentNs)}/${encodeURIComponent(r.agentName)}`}
                data-testid={`run-agent-link-${r.traceId}`}
                onClick={(e) => e.stopPropagation()}
                className="border-b border-accent text-primary hover:border-primary"
              >
                {r.agentName}
              </Link>
            }
            namespace={r.agentNs}
          />
        ) : (
          <UnknownValue title={AGENT_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "what",
      header: "What",
      priority: 1,
      // Steps with the viewport for the same reason the Agent cap does.
      className:
        "max-w-[13.5rem] lg:max-w-[15.5rem] xl:max-w-[18rem] min-[1440px]:max-w-[24rem]",
      cell: ({ run: r }) => {
        const subject = runNameDistinctFromAgent(r);
        return (
          <div className="min-w-0">
            {subject ? (
              <>
                {/* One line, end-ellipsis, full value in `title` — never
                    `break-all`, which turns a subject line into a paragraph. */}
                <div className="truncate text-sm font-semibold" title={subject}>
                  {subject}
                </div>
                {/* Ids middle-truncate: the tail is what disambiguates two ids
                    that share a prefix (§4.5). */}
                <div
                  className="truncate font-mono text-xs text-faint"
                  title={r.traceId}
                >
                  {truncateId(r.traceId)}
                </div>
              </>
            ) : (
              // The run's name was only the agent identity, which the Agent
              // column already renders — so the id IS the row's subject here.
              <CellId id={r.traceId} className="text-sm" />
            )}
            {/* §4.4: below 768 the State column folds into the What line as a
                tag. Exactly one of the two copies is ever displayed, so the
                accessibility tree never carries the state twice. */}
            <div className="mt-1 md:hidden">
              <StateTag status={r.status} />
            </div>
          </div>
        );
      },
    },
    {
      id: "duration",
      header: "Duration",
      priority: 3,
      numeric: true,
      cell: ({ run: r }) =>
        isKnown(r.latencyMs) && r.latencyMs > 0 ? (
          <QuantityValue value={r.latencyMs} format={formatLatency} />
        ) : (
          // `text-faint`, not the QuantityValue ghost: an unrecorded duration is
          // meta a reader has to READ, and 0ms is not a duration a run can have.
          <UnknownValue title={DURATION_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "cost",
      header: "Cost",
      priority: 3,
      numeric: true,
      // Money is never truncated, wrapped, or elided (§4.5); the `numeric`
      // register supplies mono tabular right-aligned + whitespace-nowrap.
      cell: ({ run: r }) => <QuantityValue value={r.costUSD} format={formatMoney} />,
    },
    {
      id: "state",
      header: "State",
      priority: 2,
      className: "w-[6rem]",
      cell: ({ run: r }) => <StateTag status={r.status} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[10.5rem]",
      cell: (row) => (
        <NextStepLink
          label={row.next.label}
          to={row.next.to}
          tone={row.next.tone}
          ariaLabel={
            row.next.label ? `${row.next.label} — run ${truncateId(row.run.traceId)}` : undefined
          }
          testId={`next-step-${row.run.traceId}`}
        />
      ),
    },
  ];

  const serverFiltered = !!(agentFilter || fromFilter || toFilter);
  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching a user with 50 runs how to make their first.
  const chipEmptied = sorted.length > 0 && visible.length === 0;
  const empty: EmptyStateProps = chipEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: feedComplete ? sorted.length : undefined,
        countNoun: "runs",
      }
    : serverFiltered
      ? {
          intent: "filtered",
          icon: Filter,
          title: "No runs match these filters",
          description: agentFilter
            ? `Nothing ran under "${agentFilter}" in the selected range. Widen the range, or clear the filters to read the whole feed.`
            : "Nothing ran in the selected range. Widen it, or clear the filters to read the whole feed.",
          action: {
            label: "Clear filters",
            variant: "outline",
            onClick: clearServerFilters,
          },
        }
      : {
          icon: MessagesSquare,
          title: "No runs yet",
          description:
            "A run is one traced conversation with an agent — what it was asked, what it did, what it cost. Send an agent something from the Playground and it appears here.",
        };

  // 501 — the trace backend is not configured. This is the calm
  // backend-cannot-answer state (§7.1), NOT an error: nothing broke, the
  // platform is simply not wired to answer. No table, no zeroes, no retry.
  if (loadState.kind === "unavailable") {
    return (
      <div className="min-w-0 space-y-6" data-testid="runs-page">
        <PageHeader title="Runs" lede={LEDE} />
        <div data-testid="runs-unavailable">
          <QuietNote title="Run history isn’t configured on this install.">
            Runs are read from the trace backend, and this install has none wired
            up — so there is no feed to show, and no per-run duration or cost to
            report either. Configuring it needs a tracing backend the control
            plane can read. Nothing here is estimated; the history is simply
            absent.
          </QuietNote>
        </div>
      </div>
    );
  }

  // 200 + notice — the trace store answered, but is transiently unavailable
  // (slow / circuit-broken). That IS a failure, and an honest one: distinct
  // from "No runs yet" so the reader knows to retry rather than concluding
  // there was no activity (m24 — the notice was previously dropped).
  if (loadState.kind === "degraded") {
    return (
      <div className="min-w-0 space-y-6" data-testid="runs-page">
        <PageHeader title="Runs" lede={LEDE} />
        <div data-testid="runs-degraded">
          <ErrorState
            title="The trace store didn’t answer."
            description="The feed is temporarily unreadable. Nothing has been lost — the runs are still recorded."
            detail={loadState.message}
            onRetry={load}
          />
        </div>
      </div>
    );
  }

  const closing = closingLine(sorted, feedComplete);
  const showChips = loadState.kind !== "error";
  const metaLine =
    loadState.kind === "ready"
      ? feedComplete
        ? `${runs.length} run${runs.length === 1 ? "" : "s"}`
        : `${runs.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="runs-page">
      <PageHeader title="Runs" meta={metaLine} lede={LEDE} />

      {/* The server-side filters sit ABOVE the views: they narrow what the
          backend sends, the chips narrow what you are looking at within it.
          Two different questions, so two different rows. */}
      <RunsFilterBar
        agent={agentFilter}
        from={fromFilter}
        to={toFilter}
        onAgent={onAgentChange}
        onFrom={onFromChange}
        onTo={onToChange}
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter runs"
          className="min-w-0"
        />
      )}

      <DataTable<Row>
        columns={columns}
        rows={visible}
        rowKey={(row) => row.run.traceId}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter runs on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Runs"
        // Row-click → the native trace page (m16.7): the run's full timeline,
        // where every column this table drops at a narrow width still renders.
        onRowClick={(row) => navigate(`/traces/${encodeURIComponent(row.run.traceId)}`)}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}
    </div>
  );
}
