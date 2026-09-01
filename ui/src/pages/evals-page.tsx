import * as React from "react";
import { Filter, RefreshCw, Sparkles, TestTube2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  CellEntity,
  ClosingNote,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  ErrorState,
  FilterChipRow,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  SkeletonText,
  StatusBadge,
  UNKNOWN,
  UnknownValue,
  Wizard,
  nextStepRank,
  useFocusTrap,
  useToast,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { cn } from "@/lib/utils";
import {
  api,
  ApiError,
  type EvalCondition,
  type GateResult,
  type EvalGatedMetricResponse,
  type EvalScore,
  type EvalSuiteDetail,
  type EvalSuiteResults,
  type EvalSuiteSummary,
} from "@/lib/api";
import { RES_EVALSUITES } from "@/lib/nav";

// EvalsPage — archetype A1 (index/table) with two extras the §6.2 mapping names:
// an in-page builder Wizard and a per-suite results panel (M151; the surface
// itself is m17.12). PageHeader → stat band → FilterChipRow → DataTable →
// ClosingNote, with the results panel in the row's drawer.
//
// ── THIS IS THE PAGE WHERE "NO COSMETIC AUTHORITY" IS LOAD-BEARING ──────────
// An eval suite exists to decide whether an agent may ship. A console that
// invents a number here does not merely look untidy: it green-lights a deploy
// on a figure nobody computed. So the three honesty rules are absolute:
//
//   1. CONDITIONS ALWAYS, SCORES ONLY WHEN `scoresAvailable` (§6.2). A score
//      the backend did not compute is not drawn — not as 0, not as "—" beside
//      a scorer name that implies one was attempted. The whole Scores section
//      collapses to a QuietNote carrying the backend's own reason.
//   2. A PENDING GATE SHOWS NO FIGURE. `GateResult.pending` means the agent
//      references this suite and no gate has run. It renders the `open` Tag
//      ("not scored yet"), never a 0.0000 that reads as a failing score.
//   3. A SCORER THAT RETURNED NEITHER A NUMBER NOR A STRING renders the honest
//      dash with its reason in `title` — the one place on this page where a
//      value is genuinely unknown rather than absent.
//
// ── SORTED BY WHAT IS BLOCKING (§6.1 A1) ────────────────────────────────────
// `nextStepRank` is the primary key, so anything that needs a person sits above
// everything that does not and "Nothing needed" sinks — identically to the
// other twenty list pages. Inside the needs-a-person half, a suite that can
// never produce a score (no scorers) outranks one that simply has not reported
// ready, because the first will never resolve on its own.
//
// ── WHAT THE LIST MAY NOT CLAIM ─────────────────────────────────────────────
// `EvalSuiteSummary` carries identity + readiness and nothing else: no scores,
// no last-run, no pass rate. Those live behind GET .../results and are fetched
// only when a row is opened, so no column here pretends to know them. The
// fleet-level "eval-gated deploys" ratio is a SEPARATE endpoint; when it
// answers 501 the band is replaced by a QuietNote rather than silently
// vanishing — an absent capability the reader cannot see is indistinguishable
// from a bug.
//
// data-testid contract:
//   evals-page                       — root container
//   eval-gated-stat / -percent / -sub / -target — the PRD §5 ratio band
//   eval-gated-unavailable           — the calm 501 replacement for that band
//   eval-suite-{name}                — each row's entity cell
//   next-step-{name}                 — the row's Next step cell
//   eval-delete-{name}               — the row's delete action (RBAC-gated)
//   eval-results-panel-{name}        — the drawer's results body
//   eval-results-loading|forbidden|error-{name}
//   eval-gate-unavailable-{name} / eval-gate-results-{name} / eval-gate-result-{agent}
//   eval-scores-unavailable-{name} / eval-score-{scorer}

// ---- discriminated state types -----------------------------------------------

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; suites: EvalSuiteSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

type ResultsState =
  | { kind: "loading" }
  | { kind: "ready"; results: EvalSuiteResults }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

type DeleteState =
  | { kind: "idle" }
  | { kind: "deleting" }
  | { kind: "error"; message: string };

// MetricState holds the eval-gated metric load state (PRD §5, ADR 0062
// governance #2). "unavailable" (501) is a calm degrade — the endpoint requires
// a wired cluster; "error" is a real failure. Neither is ever fabricated.
type MetricState =
  | { kind: "loading" }
  | { kind: "ready"; data: EvalGatedMetricResponse }
  | { kind: "unavailable" }
  | { kind: "error" };

// ---- wizard step indices ------------------------------------------------------

const STEP_DATASET = 0;
const STEP_SCORERS = 1;
const STEP_GATE = 2;
const STEP_REVIEW = 3;

type BuilderState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "done"; suite: EvalSuiteDetail }
  | { kind: "error"; message: string; forbidden?: boolean };

// ---- triage: the state model the page sorts and speaks from --------------------

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
}

interface Triaged {
  suite: EvalSuiteSummary;
  /** The suite's scorers, normalised: the API omits an empty list (Go omitempty). */
  scorers: string[];
  next: NextStep;
  /** Tie-break inside the needs-a-person half — lower is more urgent. */
  rank: number;
}

/**
 * One suite → (scorers, next step, urgency). Everything the page sorts and
 * renders is decided here so the order can never disagree with the link it
 * ordered by.
 *
 * Two states ask something of a person, and neither is a failure, so neither
 * takes the crit tone (§7.2 — crit only when the target is a failure or a stop):
 *
 *   • NO SCORERS — the suite runs nothing, so it can never produce a result to
 *     gate on. It will not fix itself; it is the most urgent row on the page.
 *   • NOT READY — the controller has not reported this suite ready. Worth a
 *     look, but the suite is well-formed.
 *
 * Everything else needs nothing FROM THIS LIST. Whether a gate is currently
 * passing lives behind the results endpoint, which the list does not call —
 * inventing "3 agents blocked" from a list response is exactly the claim this
 * page must not make.
 */
function triage(s: EvalSuiteSummary): Triaged {
  const scorers = s.scorers ?? [];
  if (scorers.length === 0) {
    return { suite: s, scorers, next: { label: "Add a scorer", tone: "default" }, rank: 0 };
  }
  if (!s.ready) {
    return { suite: s, scorers, next: { label: "Review the suite", tone: "default" }, rank: 1 };
  }
  return { suite: s, scorers, next: { tone: "none" }, rank: 2 };
}

/** The gate, as one string so it never renders as a bare threshold number. */
function gateLabel(s: EvalSuiteSummary): string | null {
  if (!s.gate) return null;
  return s.threshold !== undefined ? `${s.gate} ≥ ${s.threshold}` : s.gate;
}

// ---- the chip views (§5.28): one question, one answer at a time ---------------

const EVAL_VIEWS = ["all", "needs-you", "gating"] as const;
type EvalView = (typeof EVAL_VIEWS)[number];

const EVAL_VIEW_LABEL: Record<EvalView, string> = {
  all: "Everything",
  "needs-you": "Needs you",
  gating: "Gating a deploy",
};

// The chips are BUILT from this union below, so a chip whose id is not a view
// stops compiling instead of silently filtering to nothing.
const EVAL_VIEW_MATCH: Record<EvalView, (t: Triaged) => boolean> = {
  all: () => true,
  "needs-you": (t) => t.next.tone !== "none",
  gating: (t) => Boolean(t.suite.gate),
};

const EVAL_VIEW_EMPTY: Record<
  Exclude<EvalView, "all">,
  { title: string; description: string }
> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "Every suite in view has its scorers and has reported ready. Show everything to see them all.",
  },
  gating: {
    title: "No suite gates a deploy",
    description:
      "No suite in view declares a gate, so none of them can block a promotion — they only report. Show everything to see them all.",
  },
};

/**
 * The §5.18 closing line: the honest ratio in words, restating what the table
 * already showed. Every number is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are not the whole list.
 */
export function closingLine(rows: Triaged[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const needs = rows.filter((t) => nextStepRank(t.next.tone) === 0).length;
  const quiet = total - needs;
  if (total === 1) {
    return needs === 1
      ? "The one suite here needs a person before it can gate anything."
      : "The one suite here is ready to gate and needs nothing from you.";
  }
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More suites follow.";
  if (needs === 0) {
    return `All ${total} suites${where} are ready to gate. None of them needs a person.${more}`;
  }
  if (quiet === 0) {
    return `Every one of the ${total} suites${where} needs a person before it can gate.${more}`;
  }
  return `${needs} of the ${total} suites${where} need${needs === 1 ? "s" : ""} a person. The other ${quiet} ${quiet === 1 ? "is" : "are"} ready to gate.${more}`;
}

// ---- the PRD §5 eval-gated ratio band -----------------------------------------

// TARGET_PERCENT is the PRD §5 quality-discipline target: > 50% of production
// deploys must be gated by an EvalSuite.
const TARGET_PERCENT = 50;

/** One decimal, always — `75` and `75.0` must not read as different measurements. */
function formatPercent(n: number): string {
  return `${n.toFixed(1)}%`;
}

/**
 * EvalGatedStatBand — the PRD §5 ">50% of production deploys gated by an
 * EvalSuite" metric, as the A1 stat-band variant above the table.
 *
 * The figure goes through `QuantityValue`, so an unread metric CANNOT render as
 * a number: the honest dash is a compile-time consequence of handing it
 * `UNKNOWN` rather than a discipline the next editor has to remember.
 */
function EvalGatedStatBand({ metric }: { metric: MetricState }) {
  const data = metric.kind === "ready" ? metric.data : undefined;
  const meetsTarget = data !== undefined && data.percent > TARGET_PERCENT;

  return (
    <div
      className="flex min-w-0 flex-wrap items-baseline gap-x-8 gap-y-2 rounded-lg border bg-card px-5 py-4"
      data-testid="eval-gated-stat"
    >
      <div className="min-w-0">
        <p className="font-mono text-2xs font-medium uppercase tracking-wide text-faint">
          Eval-gated deploys
        </p>
        <p className="mt-1 text-2xl" data-testid="eval-gated-percent">
          {metric.kind === "loading" ? (
            <span className="font-mono tabular-nums text-ghost">…</span>
          ) : (
            <QuantityValue
              value={data === undefined ? UNKNOWN : data.percent}
              format={formatPercent}
              title="The eval-gated ratio could not be read — unknown, not zero."
            />
          )}
        </p>
        <p className="mt-0.5 h-4 text-xs text-faint" data-testid="eval-gated-sub">
          {metric.kind === "loading"
            ? ""
            : data !== undefined
              ? `${data.gated}/${data.total} deployments`
              : "Couldn't load metric"}
        </p>
      </div>
      {data !== undefined && (
        // Above the bar is a verified condition, so it takes the ok hue; below
        // it is not a warning — it is simply the target, restated (§2.2).
        <p
          className={cn("text-sm", meetsTarget ? "text-success" : "text-faint")}
          data-testid="eval-gated-target"
        >
          {meetsTarget
            ? `Above ${TARGET_PERCENT}% target`
            : `Target: >${TARGET_PERCENT}% of deploys eval-gated`}
        </p>
      )}
    </div>
  );
}

// ---- main page ----------------------------------------------------------------

export function EvalsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [showBuilder, setShowBuilder] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [view, setView] = React.useState<EvalView>("all");
  const [open, setOpen] = React.useState<EvalSuiteSummary | null>(null);
  const [resultsMap, setResultsMap] = React.useState<Map<string, ResultsState>>(
    new Map(),
  );
  const [deleteTarget, setDeleteTarget] = React.useState<EvalSuiteSummary | null>(
    null,
  );
  const [deleteState, setDeleteState] = React.useState<DeleteState>({ kind: "idle" });
  // PRD §5 eval-gated metric — live snapshot of how many deploys are eval-gated.
  const [metric, setMetric] = React.useState<MetricState>({ kind: "loading" });

  const { can } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();

  const canCreate = can(RES_EVALSUITES, "create");
  const canDelete = can(RES_EVALSUITES, "delete");

  const load = React.useCallback(
    (signal?: AbortSignal) => {
      setPage({ kind: "loading" });
      setMetric({ kind: "loading" });

      // Load EvalSuite list.
      api
        .listEvalSuites({ namespace: shellNs || undefined }, signal)
        .then((res) => {
          if (signal?.aborted) return;
          setPage({ kind: "ready", suites: res.items, nextCursor: res.nextCursor });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          setPage({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
            forbidden: err instanceof ApiError && err.isForbidden,
          });
        });

      // Load eval-gated metric (PRD §5, ADR 0062 governance #2).
      api
        .evalGatedMetric({ namespace: shellNs || undefined, signal })
        .then((data) => {
          if (signal?.aborted) return;
          setMetric({ kind: "ready", data });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          // 501 = the endpoint is not wired on this install — calm, not an error.
          if (err instanceof ApiError && err.status === 501) {
            setMetric({ kind: "unavailable" });
            return;
          }
          setMetric({ kind: "error" });
        });
    },
    [shellNs],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const loadResults = React.useCallback((suite: EvalSuiteSummary) => {
    const key = `${suite.namespace}/${suite.name}`;
    setResultsMap((m) => new Map(m).set(key, { kind: "loading" }));
    api
      .evalSuiteResults(suite.namespace, suite.name)
      .then((results) => {
        setResultsMap((m) => new Map(m).set(key, { kind: "ready", results }));
      })
      .catch((err: unknown) => {
        if (err instanceof ApiError && err.isForbidden) {
          setResultsMap((m) =>
            new Map(m).set(key, { kind: "forbidden", message: err.message }),
          );
          return;
        }
        setResultsMap((m) =>
          new Map(m).set(key, {
            kind: "error",
            message: err instanceof Error ? err.message : "failed to load results",
          }),
        );
      });
  }, []);

  // Opening a row is what fetches its results — the list endpoint carries none,
  // and a per-row prefetch would be N requests for data most rows never show.
  const openSuite = React.useCallback(
    (suite: EvalSuiteSummary) => {
      setOpen(suite);
      const existing = resultsMap.get(`${suite.namespace}/${suite.name}`);
      if (!existing || existing.kind === "error") loadResults(suite);
    },
    [resultsMap, loadResults],
  );

  async function handleDelete() {
    if (!deleteTarget) return;
    setDeleteState({ kind: "deleting" });
    try {
      await api.removeEvalSuite(deleteTarget.namespace, deleteTarget.name);
      setDeleteState({ kind: "idle" });
      setDeleteTarget(null);
      toast({ variant: "success", title: `Deleted ${deleteTarget.name}` });
      load();
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? err.message
          : err instanceof Error
            ? err.message
            : "delete failed";
      setDeleteState({ kind: "error", message: msg });
    }
  }

  const all = React.useMemo(
    () => (page.kind === "ready" ? page.suites : []),
    [page],
  );
  // The loaded window IS the whole list only when no cursor follows it. That is
  // the one condition under which counting the rows in hand is a FACT rather
  // than a windowed guess (kit FilterChipRow contract).
  const complete = page.kind === "ready" && page.nextCursor === "";

  // Triage once, sort once. Attention-first (§6.1 A1).
  const sorted = React.useMemo(() => {
    const rows = all.map(triage);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        x.rank - y.rank ||
        x.suite.name.localeCompare(y.suite.name),
    );
    return rows;
  }, [all]);

  const q = query.trim().toLowerCase();
  const visible = React.useMemo(() => {
    const inView = sorted.filter(EVAL_VIEW_MATCH[view]);
    return q
      ? inView.filter(
          (t) =>
            t.suite.name.toLowerCase().includes(q) ||
            t.suite.datasetRef.toLowerCase().includes(q),
        )
      : inView;
  }, [sorted, view, q]);

  const chips: FilterChip[] = EVAL_VIEWS.map((id) => ({
    id,
    label: EVAL_VIEW_LABEL[id],
    // No number unless the loaded window provably IS the whole list — a count
    // that describes one page while looking like a total hides work.
    count: complete ? sorted.filter(EVAL_VIEW_MATCH[id]).length : undefined,
  }));

  const error: DataTableError | null =
    page.kind === "error"
      ? {
          message: page.message,
          forbidden: page.forbidden,
          resource: "eval suites",
          onRetry: page.forbidden ? undefined : () => load(),
        }
      : null;

  // §4.4 resource-list budget, in visual order. Suite, State and Next step are
  // priority 1 and survive every width — the row's identity, its condition, and
  // what to do about it.
  //
  // The caps are chosen so the six columns PLUS the delete action fit inside the
  // frame at 1440 (256+208+144+160+112+160+action ≈ 1130 of ~1136). A cap that
  // is merely generous is not free: the table then exceeds its frame and the
  // last column is clipped at the very width the budget promises renders whole.
  const columns: Column<Triaged>[] = [
    {
      id: "suite",
      header: "Suite",
      priority: 1,
      className: "max-w-[16rem]",
      cell: (t) => (
        <div data-testid={`eval-suite-${t.suite.name}`}>
          <CellEntity name={t.suite.name} namespace={t.suite.namespace} />
        </div>
      ),
    },
    {
      id: "dataset",
      header: "Dataset",
      priority: 4,
      className: "max-w-[13rem]",
      cell: (t) => (
        <span
          className="block truncate font-mono text-xs text-secondary-foreground"
          title={t.suite.datasetRef}
        >
          {t.suite.datasetRef}
        </span>
      ),
    },
    {
      id: "scorers",
      header: "Scorers",
      priority: 3,
      className: "max-w-[9rem]",
      cell: (t) =>
        // An empty list is a REAL answer (the API omits it when empty), not a
        // missing measurement — so it takes the `open` Tag, never a dash.
        t.scorers.length === 0 ? (
          <Badge variant="open">no scorers</Badge>
        ) : t.scorers.length === 1 ? (
          <span className="block truncate font-mono text-xs" title={t.scorers[0]}>
            {t.scorers[0]}
          </span>
        ) : (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums"
            title={t.scorers.join(", ")}
          >
            {`${t.scorers.length} scorers`}
          </span>
        ),
    },
    {
      id: "gate",
      header: "Gate",
      priority: 3,
      className: "max-w-[10rem]",
      cell: (t) => {
        const gate = gateLabel(t.suite);
        // The gate and its threshold render as ONE string. Split apart, the
        // bare number reads as a score the suite achieved rather than the bar
        // it must clear.
        return gate ? (
          <span className="block truncate font-mono text-xs" title={gate}>
            {gate}
          </span>
        ) : (
          <Badge variant="open">no gate</Badge>
        );
      },
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[7rem]",
      cell: (t) => <StatusBadge ready={t.suite.ready} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[10rem]",
      cell: (t) => (
        <NextStepLink
          label={t.next.label}
          tone={t.next.tone}
          onClick={t.next.tone === "none" ? undefined : () => openSuite(t.suite)}
          ariaLabel={
            t.next.label ? `${t.next.label} — ${t.suite.name}` : undefined
          }
          testId={`next-step-${t.suite.name}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7) — it offers a way back out rather than teaching
  // a user who already has suites how to make their first.
  const chipEmptied = all.length > 0 && visible.length === 0 && view !== "all";
  const empty: EmptyStateProps = chipEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: EVAL_VIEW_EMPTY[view as Exclude<EvalView, "all">].title,
        description: EVAL_VIEW_EMPTY[view as Exclude<EvalView, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: complete ? all.length : undefined,
        countNoun: "suites",
      }
    : {
        icon: TestTube2,
        title: "No eval suites yet",
        description:
          "An eval suite is a dataset, the scorers to run over it, and the bar an agent must clear before it ships. Create the first one to see it here.",
        action: canCreate
          ? {
              label: "New eval suite",
              icon: Sparkles,
              onClick: () => setShowBuilder(true),
            }
          : undefined,
      };

  const closing = page.kind === "ready" ? closingLine(sorted, complete) : null;
  const metaLine =
    page.kind === "ready"
      ? complete
        ? `${all.length} suite${all.length === 1 ? "" : "s"}`
        : `${all.length} on this page`
      : undefined;
  const openKey = open ? `${open.namespace}/${open.name}` : "";
  const openResults = openKey ? resultsMap.get(openKey) : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="evals-page">
      <PageHeader
        title="Evals"
        meta={metaLine}
        loading={page.kind === "loading"}
        lede="Sorted by what is blocking. A suite is a dataset, its scorers, and the bar an agent must clear — open one to see the gate outcome the controller actually recorded."
        // Both controls go through `actionsSlot` rather than the structured
        // `actions` list: `PageHeaderAction` carries no `testId`, and the viewer
        // suite asserts `evals-new-btn` is ABSENT — an assertion that would pass
        // forever if the id quietly disappeared.
        actionsSlot={
          <>
            <Button
              variant="ghost"
              size="icon"
              onClick={() => load()}
              aria-label="Refresh evals"
              data-testid="evals-refresh"
            >
              <RefreshCw className="h-4 w-4" />
            </Button>
            {canCreate && (
              <Button
                size="sm"
                className="text-sm"
                onClick={() => setShowBuilder(true)}
                data-testid="evals-new-btn"
              >
                <Sparkles className="h-4 w-4" />
                New eval suite
              </Button>
            )}
          </>
        }
      />

      {/* The PRD §5 ratio. A 501 replaces the band with the §7.1 note rather
          than removing it silently — a capability the reader cannot see is
          indistinguishable from a page that forgot to render. */}
      {metric.kind === "unavailable" ? (
        <div data-testid="eval-gated-unavailable">
          <QuietNote title="The eval-gated ratio isn’t available on this install.">
            How many production deploys are gated by an eval suite is computed by
            the platform, and this install doesn’t answer for it. The suites
            below are live and unaffected. Nothing here is estimated — the ratio
            is simply absent.
          </QuietNote>
        </div>
      ) : (
        <EvalGatedStatBand metric={metric} />
      )}

      {all.length > 0 && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as EvalView)}
          label="Filter eval suites"
          className="min-w-0"
        />
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={(t) => `${t.suite.namespace}/${t.suite.name}`}
        loading={page.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter suites by name or dataset…"
        ariaLabel="Eval suites"
        tableClassName="min-w-[52rem]"
        onRowClick={(t) => openSuite(t.suite)}
        rowActions={
          canDelete
            ? (t) => (
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-faint hover:text-destructive"
                  onClick={() => setDeleteTarget(t.suite)}
                  data-testid={`eval-delete-${t.suite.name}`}
                >
                  Delete
                </Button>
              )
            : undefined
        }
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {/* Dropped ≠ lost (§4.4): every field the budget hides renders here, plus
          the results the list endpoint never carries. */}
      <DetailDrawer
        open={open !== null}
        onClose={() => setOpen(null)}
        title={open?.name ?? ""}
        subtitle={open?.namespace}
        size="lg"
        status={open ? <StatusBadge ready={open.ready} /> : undefined}
      >
        {open && (
          <div className="space-y-6">
            <KeyValueList
              items={[
                { key: "Dataset", value: open.datasetRef, absent: "not set" },
                {
                  key: "Scorers",
                  value: (open.scorers ?? []).join(", "),
                  absent: "none — this suite scores nothing",
                  mono: false,
                },
                {
                  key: "Gate",
                  value: gateLabel(open) ?? "",
                  absent: "no gate — it reports, it does not block",
                  mono: false,
                },
              ]}
            />
            <ResultsPanel
              results={openResults ?? { kind: "loading" }}
              suite={open}
              onRetry={() => loadResults(open)}
            />
          </div>
        )}
      </DetailDrawer>

      {showBuilder && (
        <EvalBuilderWizard
          onClose={() => setShowBuilder(false)}
          onCreated={() => {
            setShowBuilder(false);
            load();
          }}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          open
          destructive
          title={`Delete ${deleteTarget.name}?`}
          description={`This will delete the eval suite "${deleteTarget.name}" in namespace "${deleteTarget.namespace}". This cannot be undone.`}
          confirmLabel={deleteState.kind === "deleting" ? "Deleting…" : "Delete"}
          busy={deleteState.kind === "deleting"}
          onConfirm={handleDelete}
          onCancel={() => {
            setDeleteTarget(null);
            setDeleteState({ kind: "idle" });
          }}
          impact={
            deleteState.kind === "error" ? (
              <p className="text-sm text-destructive" role="alert">
                {deleteState.message}
              </p>
            ) : undefined
          }
        />
      )}
    </div>
  );
}

// ---- ResultsPanel ------------------------------------------------------------
// The honest rendering of GET .../results:
//   • conditions + gate results → ALWAYS (or the backend's own reason for their
//     absence, as a QuietNote — never a silently empty panel)
//   • scores → ONLY when scoresAvailable=true
//   • scoresAvailable=false → the reason, calmly. NEVER a fabricated figure.

interface ResultsPanelProps {
  results: ResultsState;
  suite: EvalSuiteSummary;
  onRetry: () => void;
}

function ResultsPanel({ results, suite, onRetry }: ResultsPanelProps) {
  if (results.kind === "loading") {
    return (
      <div data-testid={`eval-results-loading-${suite.name}`}>
        <SectionHeader as="h3" title="Gate outcome" />
        <SkeletonText lines={4} />
      </div>
    );
  }

  if (results.kind === "forbidden") {
    return (
      <div data-testid={`eval-results-forbidden-${suite.name}`}>
        <ForbiddenInline
          resource="eval results"
          title="Not allowed to view results for this suite"
        />
      </div>
    );
  }

  if (results.kind === "error") {
    return (
      <div data-testid={`eval-results-error-${suite.name}`}>
        <ErrorState
          title="Couldn’t load results"
          description="The results endpoint did not answer. Nothing below is estimated in its place."
          detail={results.message}
          onRetry={onRetry}
        />
      </div>
    );
  }

  const {
    conditions,
    gateResults,
    gateResultsAvailable,
    gateResultsUnavailableReason,
    scoresAvailable,
    scores,
    scoresUnavailableReason,
  } = results.results;

  return (
    <div className="space-y-6" data-testid={`eval-results-panel-${suite.name}`}>
      {/* Gate outcome — the real per-agent offline gate result (ADR 0094),
          projected from the AgentDeployments that gate on this suite. */}
      <section>
        <SectionHeader
          as="h3"
          title="Gate outcome"
          lede="What the offline gate decided for each agent that gates on this suite."
        />
        {!gateResultsAvailable ? (
          // An RBAC / list degrade. Calm, and it says WHY — never an empty panel
          // that reads as "no agents gate on this".
          <div data-testid={`eval-gate-unavailable-${suite.name}`}>
            <QuietNote title="The gate outcome can’t be read from here.">
              <p className="font-mono text-xs">
                {gateResultsUnavailableReason ??
                  "The backend gave no reason for the absence."}
              </p>
              <p className="mt-2">
                Nothing here is estimated — the outcome is simply absent, not
                empty.
              </p>
            </QuietNote>
          </div>
        ) : gateResults.length === 0 ? (
          // A real, backend-confirmed zero: the list was readable and held
          // nothing. Stated in words, never as a dash.
          <p className="text-sm text-faint">No agent gates on this suite yet.</p>
        ) : (
          <div data-testid={`eval-gate-results-${suite.name}`}>
            {gateResults.map((g: GateResult, i: number) => (
              <GateResultRow key={`${g.agent}-${i}`} result={g} />
            ))}
          </div>
        )}
        {/* CRD status.conditions (empty today — no EvalSuite reconciler); shown
            only if a reconciler ever writes them, never fabricated. */}
        {conditions.length > 0 && (
          <div className="mt-3">
            {conditions.map((c: EvalCondition, i: number) => (
              <ConditionRow key={`${c.type}-${i}`} condition={c} />
            ))}
          </div>
        )}
      </section>

      {/* Scores — ONLY when scoresAvailable=true (§6.2). */}
      <section>
        <SectionHeader
          as="h3"
          title="Scores"
          lede="What each scorer returned on the suite’s dataset."
        />
        {scoresAvailable ? (
          scores && scores.length > 0 ? (
            <div>
              {scores.map((s: EvalScore, i: number) => (
                <ScoreRow key={`${s.scorer}-${i}`} score={s} />
              ))}
            </div>
          ) : (
            <p className="text-sm text-faint">
              The scorers ran and returned no scores.
            </p>
          )
        ) : (
          // scoresAvailable=false — the backend's own reason, calmly. This is
          // the branch the whole page is built around: no zero, no estimate, no
          // half-drawn score row.
          <div data-testid={`eval-scores-unavailable-${suite.name}`}>
            <QuietNote title="Scores aren’t available for this suite.">
              <p className="font-mono text-xs">
                {scoresUnavailableReason ?? "Scores unavailable."}
              </p>
              <p className="mt-2">
                The gate outcome above is unaffected — it comes from the
                controller, not the scorer store. Nothing here is estimated: the
                scores are simply absent, not zero.
              </p>
            </QuietNote>
          </div>
        )}
      </section>
    </div>
  );
}

/**
 * One AgentDeployment's offline eval-gate outcome (ADR 0094).
 *
 * `pending` means the agent references the suite and no gate has run yet, so it
 * renders the `open` Tag and NO figure. A 0.0000 in that slot would read as a
 * catastrophic score rather than an absent one — the single most damaging
 * fabrication this page could make.
 */
function GateResultRow({ result }: { result: GateResult }) {
  const blocked = result.decision === "blocked" || result.decision === "fail";
  return (
    <div
      className="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-border-soft py-2 last:border-0"
      data-testid={`eval-gate-result-${result.agent}`}
    >
      <span
        className="min-w-0 flex-1 truncate font-mono text-xs font-medium"
        title={result.agent}
      >
        {result.agent}
      </span>
      {result.pending ? (
        <Badge variant="open">not scored yet</Badge>
      ) : (
        <>
          {result.score && (
            <span className="whitespace-nowrap font-mono text-xs tabular-nums">
              score {result.score}
            </span>
          )}
          {result.threshold && (
            <span className="whitespace-nowrap font-mono text-xs tabular-nums text-faint">
              / threshold {result.threshold}
            </span>
          )}
          {result.decision && (
            <Badge variant={blocked ? "crit" : "ok"}>{result.decision}</Badge>
          )}
        </>
      )}
      {result.reason && !result.pending && (
        <p className="w-full text-xs text-faint">{result.reason}</p>
      )}
      {result.scoredRevision && !result.pending && (
        <p className="w-full font-mono text-2xs text-faint">
          scored revision {result.scoredRevision}
        </p>
      )}
    </div>
  );
}

function ConditionRow({ condition }: { condition: EvalCondition }) {
  const variant =
    condition.status === "True"
      ? "ok"
      : condition.status === "False"
        ? "crit"
        : "progressing";
  return (
    <div className="border-b border-border-soft py-2 last:border-0">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="font-mono text-xs font-medium">{condition.type}</span>
        {condition.reason && (
          <span className="text-xs text-faint">({condition.reason})</span>
        )}
        <Badge variant={variant}>{condition.status}</Badge>
      </div>
      {condition.message && (
        <p className="mt-1 text-xs text-faint">{condition.message}</p>
      )}
    </div>
  );
}

/**
 * One scorer's result. A scorer that returned NEITHER a number nor a string is
 * the one genuinely unknown value in this panel: it renders the honest dash
 * with its reason, never a zero.
 */
function ScoreRow({ score }: { score: EvalScore }) {
  const shown =
    score.value !== undefined
      ? String(score.value)
      : score.stringValue !== undefined
        ? score.stringValue
        : undefined;
  return (
    <div
      className="flex items-baseline justify-between gap-3 border-b border-border-soft py-2 last:border-0"
      data-testid={`eval-score-${score.scorer}`}
    >
      <span
        className="min-w-0 truncate font-mono text-xs text-faint"
        title={score.scorer}
      >
        {score.scorer}
      </span>
      {shown !== undefined ? (
        <span className="whitespace-nowrap font-mono text-xs tabular-nums">
          {shown}
        </span>
      ) : (
        <UnknownValue title="This scorer returned neither a numeric nor a categorical value — unknown, not zero." />
      )}
    </div>
  );
}

// ---- EvalBuilderWizard -------------------------------------------------------
// A 4-step wizard: dataset ref → scorers → gate/threshold → review + create.
// The wizard calls createEvalSuite on finish. A 403 surfaces honestly, inline
// in the review step (§7 A4) — never as a toast.

interface EvalBuilderWizardProps {
  onClose: () => void;
  onCreated: () => void;
}

function EvalBuilderWizard({ onClose, onCreated }: EvalBuilderWizardProps) {
  const [step, setStep] = React.useState(STEP_DATASET);
  const [datasetRef, setDatasetRef] = React.useState("");
  const [scorersInput, setScorersInput] = React.useState("");
  const [gate, setGate] = React.useState("");
  const [threshold, setThreshold] = React.useState("");
  const [name, setName] = React.useState("");
  const [namespace, setNamespace] = React.useState("");
  const [builderState, setBuilderState] = React.useState<BuilderState>({
    kind: "idle",
  });

  const { can, reprobe } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  React.useEffect(() => {
    if (shellNs) setNamespace(shellNs);
  }, [shellNs]);

  const scorers = React.useMemo(
    () =>
      scorersInput
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean),
    [scorersInput],
  );

  const canProceed = React.useMemo(() => {
    if (step === STEP_DATASET) return datasetRef.trim().length > 0;
    if (step === STEP_SCORERS) return scorers.length > 0;
    if (step === STEP_GATE) return true; // gate is optional
    if (step === STEP_REVIEW) return false; // handled by finish
    return true;
  }, [step, datasetRef, scorers]);

  async function handleCreate() {
    setBuilderState({ kind: "submitting" });
    try {
      const thresholdNum = threshold.trim()
        ? parseFloat(threshold.trim())
        : undefined;
      const created = await api.createEvalSuite({
        name: name.trim() || undefined,
        namespace: namespace.trim() || undefined,
        datasetRef: datasetRef.trim(),
        scorers,
        gate: gate.trim() || undefined,
        threshold: thresholdNum,
      });
      setBuilderState({ kind: "done", suite: created });
      toast({
        variant: "success",
        title: `Created ${created.name}`,
        description: "Eval suite is created and will be reconciled.",
      });
      onCreated();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setBuilderState({
            kind: "error",
            message: `Not allowed: ${err.message}`,
            forbidden: true,
          });
          return;
        }
        setBuilderState({ kind: "error", message: err.message });
        return;
      }
      setBuilderState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
      });
    }
  }

  const busy = builderState.kind === "submitting";
  const canBuild = can(RES_EVALSUITES, "create");

  const steps: WizardStep[] = [
    {
      id: "dataset",
      title: "Dataset",
      description: "Specify the evaluation dataset reference",
      content: (
        <div className="space-y-4" data-testid="eval-builder-dataset-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-dataset-ref">
              Dataset ref <span className="text-destructive">*</span>
            </Label>
            <Input
              id="eval-dataset-ref"
              value={datasetRef}
              onChange={(e) => setDatasetRef(e.target.value)}
              placeholder="my-dataset or gs://bucket/eval.jsonl"
              data-testid="eval-dataset-ref-input"
            />
            <p className="text-xs text-faint">
              A name or URI pointing to the evaluation dataset.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-name">
              Suite name <span className="text-faint">(auto-generated)</span>
            </Label>
            <Input
              id="eval-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-eval-suite"
              data-testid="eval-name-input"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-namespace">Namespace</Label>
            <Input
              id="eval-namespace"
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
              placeholder="default"
              data-testid="eval-namespace-input"
            />
          </div>
        </div>
      ),
    },
    {
      id: "scorers",
      title: "Scorers",
      description: "Choose the scorers to run",
      content: (
        <div className="space-y-4" data-testid="eval-builder-scorers-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-scorers">
              Scorers <span className="text-destructive">*</span>
            </Label>
            <Input
              id="eval-scorers"
              value={scorersInput}
              onChange={(e) => setScorersInput(e.target.value)}
              placeholder="exact-match, bleu, rouge"
              data-testid="eval-scorers-input"
            />
            <p className="text-xs text-faint">
              Comma-separated scorer names. A suite with no scorer can never
              produce a result to gate on.
            </p>
          </div>
          {scorers.length > 0 && (
            <div className="flex flex-wrap gap-1">
              {scorers.map((s) => (
                <Badge key={s} variant="muted">
                  {s}
                </Badge>
              ))}
            </div>
          )}
        </div>
      ),
    },
    {
      id: "gate",
      title: "Gate",
      description: "Optional pass/fail gate and threshold",
      content: (
        <div className="space-y-4" data-testid="eval-builder-gate-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-gate">
              Gate condition <span className="text-faint">(optional)</span>
            </Label>
            <Input
              id="eval-gate"
              value={gate}
              onChange={(e) => setGate(e.target.value)}
              placeholder="exact-match"
              data-testid="eval-gate-input"
            />
            <p className="text-xs text-faint">
              The scorer whose value is compared against the threshold. Leave it
              empty and the suite reports without blocking a promotion.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-threshold">
              Threshold <span className="text-faint">(optional, 0–1)</span>
            </Label>
            <Input
              id="eval-threshold"
              type="number"
              min="0"
              max="1"
              step="0.01"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
              placeholder="0.8"
              data-testid="eval-threshold-input"
            />
          </div>
        </div>
      ),
    },
    {
      id: "review",
      title: "Review",
      description: "Confirm the eval suite definition",
      review: true,
      content: (
        <div className="space-y-4" data-testid="eval-builder-review-step">
          <div className="rounded-lg border bg-surface-2 p-4">
            <KeyValueList
              items={[
                { key: "Name", value: name, absent: "auto-generated" },
                { key: "Namespace", value: namespace, absent: "default" },
                { key: "Dataset", value: datasetRef, absent: "not set" },
                {
                  key: "Scorers",
                  value: scorers.join(", "),
                  absent: "none — this suite would score nothing",
                  mono: false,
                },
                {
                  key: "Gate",
                  value: gate ? `${gate}${threshold ? ` ≥ ${threshold}` : ""}` : "",
                  absent: "no gate — it reports, it does not block",
                  mono: false,
                },
              ]}
            />
          </div>
          {builderState.kind === "error" && (
            <p
              className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
              role="alert"
              data-testid="eval-builder-error"
            >
              {builderState.message}
            </p>
          )}
          {!canBuild && (
            <p className="text-sm text-faint">
              You don’t have permission to create eval suites. Ask an admin for a
              role that can create eval suites.
            </p>
          )}
        </div>
      ),
    },
  ];

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label="New eval suite"
      data-testid="eval-builder"
    >
      {/* The overlay tint is the kit's dialog treatment (a token, not a raw
          palette colour) so every floating layer dims the page identically. */}
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative flex max-h-[85vh] w-full max-w-2xl flex-col overflow-y-auto rounded-lg border bg-card shadow-overlay outline-none"
      >
        <div className="p-6">
          <h3 className="font-serif text-lg font-medium tracking-snug">
            New eval suite
          </h3>
          <p className="mt-1 text-sm text-faint">
            A dataset, the scorers to run over it, and the bar an agent must
            clear before it ships.
          </p>
        </div>
        <div className="px-6 pb-6">
          <Wizard
            steps={steps}
            current={step}
            onStepChange={setStep}
            canProceed={step === STEP_REVIEW ? canBuild && !busy : canProceed}
            busy={busy}
            onFinish={handleCreate}
            finishLabel={busy ? "Creating…" : "Create"}
            nextLabel="Next"
            onCancel={onClose}
          />
        </div>
      </div>
    </div>
  );
}
