import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Filter, GitFork, Play } from "lucide-react";
import { useNavigate } from "react-router-dom";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  StatusBadge,
  humanizeStatusReason,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type WorkflowSummary } from "@/lib/api";

// WorkflowsPage — the Workflow CR list (m67.9/m67.15, ADR 0060), rebuilt on
// archetype A1 (M151 spec §6.1: PageHeader → FilterChipRow → DataTable →
// ClosingNote) with the §4.4 "resource list" column budget.
//
// Read-only (caller-scoped, ADR 0011): a Workflow is authored as YAML and the
// console surfaces it for visibility, operator awareness, and one action —
// invoking it. A 403 surfaces as an honest forbidden state, never a fake empty
// list.
//
// ── THE PAGE'S ONE IDEA: SORT BY WHAT IS BLOCKING, NOT BY NAME ───────────────
// A workflow matters to an operator for exactly one reason: it either passes
// validation and can be run, or it cannot. So the list is ordered by what needs
// a person — `nextStepRank` is the PRIMARY key (identically on all 43 pages,
// which is the point of the shared helper), the §6.1 attention order breaks the
// tie, and the name breaks that. The last column then says what to DO about it,
// verb-first and in the user's voice (§7.2).
//
// ── WHY THE STATE CELL NO LONGER PRINTS THE WHOLE REASON (§4.5) ─────────────
// The controller's reason is a sentence — `StepAgentNotFound: step "settle"
// references agent ledger-writer`. It used to be passed as the badge's phase,
// so a 62-character uppercase mono tag with `whitespace-nowrap` set the width
// of the table. The tag now carries the STATE and a subordinate faint line
// carries the reason CODE, with the controller's full sentence in `title` — the
// same shape guardrail-policies uses, so the two pages read alike.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `WorkflowSummary` carries what a workflow DECLARES (its steps, its registry)
// and the controller's verdict on it. It carries no run history at all — no
// last run, no success rate, no spend — so the page renders no such column and
// says why in one QuietNote. An absent backend is never a zero.
//
// ── COUNTS (kit FilterChipRow contract) ─────────────────────────────────────
// `GET /api/workflows` returns `{ items }` with no cursor, so what arrived is
// what one response happened to carry and the console cannot prove it is the
// whole set. The chips therefore carry NO numbers; the ClosingNote states the
// ratio in words, counted from the rows actually in hand.
//
// data-testid contract:
//   workflows-page          — root container
//   next-step-{name}        — the row's Next step cell
//   workflow-state-{name}   — the row's State cell
//   namespace-{name}        — the entity cell's namespace line
//   row-actions-{name}      — the row's trailing action group
//   invoke-btn-{name}       — the row's Run action
//   invoke-panel            — the invoke card (when a workflow is selected)
//   invoke-input            — the JSON input textarea
//   invoke-submit           — the submit button
//   invoke-error            — error text on failure
//   invoke-cancel           — dismiss the invoke panel

/** The §6.1 attention order, as a comparator key. */
const ATTENTION: Record<StatusTone, number> = {
  failed: 1,
  waiting: 2,
  progressing: 3,
  ready: 4,
  draft: 5,
};

/** ≤26 chars renders whole; deeper namespaces middle-truncate (§4.5). */
const NS_WHOLE_MAX = 26;

/**
 * Middle-truncate a deep namespace: first segment + `…` + the last two (§4.5).
 * An end-ellipsis would throw away the tail, which is the half that
 * disambiguates `…-team-d-shared-ingest` from `…-team-c-shared-ingest`. The
 * full value always rides along in `title`.
 */
function shortNamespace(ns: string): string {
  if (ns.length <= NS_WHOLE_MAX) return ns;
  const sep = ns.includes("/") ? "/" : "-";
  const parts = ns.split(sep).filter(Boolean);
  if (parts.length < 4) return ns; // nothing to elide without losing a whole word
  return `${parts[0]}…${parts.slice(-2).join(sep)}`;
}

/**
 * The reason CODE, humanised, with the controller's full sentence kept for the
 * `title`. The BFF sends `StepAgentNotFound: step "settle" references agent
 * ledger-writer`; the leading token is the part that fits a table cell, and the
 * whole string stays recoverable on hover.
 */
function reasonCode(reason?: string): string {
  const head = (reason ?? "").split(":")[0]?.trim();
  return head ? humanizeStatusReason(head) : "";
}

/** The step count spoken with its noun — the plural a bare digit cannot carry. */
function stepPhrase(n: number): string {
  return `${n} ${n === 1 ? "step" : "steps"}`;
}

function workflowPath(w: WorkflowSummary): string {
  return `/workflows/${encodeURIComponent(w.namespace)}/${encodeURIComponent(w.name)}`;
}

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Triaged {
  workflow: WorkflowSummary;
  tone: StatusTone;
  next: NextStep;
}

/**
 * One workflow → (tone, next step). Everything the page sorts by and renders is
 * decided here once, so a cell can never disagree with the key the row was
 * ordered by.
 */
function triage(w: WorkflowSummary): Triaged {
  const { tone } = resolveStatus(w.validated, undefined, w.reason);
  const to = workflowPath(w);

  let next: NextStep;
  if (tone === "failed") {
    // It will not run until someone changes the YAML. Crit is the one action
    // hue allowed here (§2.3), because the target genuinely is a failure.
    next = { label: "Fix the workflow", tone: "crit", to };
  } else if (tone === "waiting") {
    next = { label: "Review the hold", tone: "default", to };
  } else if (w.validated && w.stepCount === 0) {
    // Valid and empty: it passes the check and does nothing. That is a setup
    // gap, not a failure, so it stays pine.
    next = { label: "Add a step", tone: "default", to };
  } else {
    // Valid with steps, or still being checked. Either way nothing is asked of
    // a person, and saying so is more useful than inventing an errand.
    next = { tone: "none" };
  }

  return { workflow: w, tone, next };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "not-valid" | "runnable" | "all";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (t) => t.next.tone !== "none" },
  { id: "not-valid", label: "Not valid", match: (t) => t.tone === "failed" },
  {
    id: "runnable",
    label: "Runnable",
    match: (t) => t.workflow.validated && t.next.tone === "none",
  },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "Every workflow in view is valid, or still being checked. Show everything to see them all.",
  },
  "not-valid": {
    title: "Nothing is failing validation",
    description:
      "Every workflow in view passed the controller's check. Show everything to see them all.",
  },
  runnable: {
    title: "Nothing is runnable yet",
    description:
      "No workflow in view has passed validation. Show everything to see what is still being checked.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number in it is counted from the rows in hand, and the
 * sentence is grammatical at n=1.
 */
export function closingLine(
  total: number,
  needs: number,
  broken: number,
): string | null {
  if (total === 0) return null;
  const quiet = total - needs;
  if (total === 1) {
    if (needs === 0) return "The one workflow here needs nothing from you.";
    return broken === 1
      ? "The one workflow here won’t run until someone fixes it."
      : "The one workflow here is waiting on a person.";
  }
  if (needs === 0) {
    return `None of the ${total} workflows needs a person. Every one of them is valid, or still being checked.`;
  }
  // The trailing clauses count in WORDS at one, so the sentence never reads
  // "The other 1 need nothing" — the exact ungrammaticality M151 fixed on the
  // guardrail page. The leading ratio stays in numerals: it is the fact the
  // note exists to state.
  const brokenClause =
    broken === 0
      ? ""
      : broken === 1
        ? " One of them won’t run until it is fixed."
        : ` ${broken} of them won’t run until they are fixed.`;
  if (quiet === 0) {
    return `Every one of the ${total} workflows needs a person.${brokenClause}`;
  }
  const otherClause =
    quiet === 1
      ? " The other one needs nothing from you."
      : ` The other ${quiet} need nothing from you.`;
  return `${needs} of the ${total} workflows need${needs === 1 ? "s" : ""} a person.${brokenClause}${otherClause}`;
}

/**
 * The row's one action: start a run. It lives in the DataTable's trailing
 * actions slot rather than in a column of its own — the §4.4 budget has no
 * column slot for a button, and a column with an empty header only earns an
 * `sr-only` head. Clicks do not propagate, so pressing Run never also opens the
 * workflow's detail page.
 */
function RowActions({
  workflow,
  onInvoke,
}: {
  workflow: WorkflowSummary;
  onInvoke: (w: WorkflowSummary) => void;
}) {
  return (
    <div
      className="flex items-center justify-end"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${workflow.name}`}
    >
      <Button
        size="sm"
        variant="outline"
        disabled={!workflow.validated}
        title={
          workflow.validated
            ? `Run ${workflow.name}`
            : "This workflow did not pass validation — it cannot be run."
        }
        data-testid={`invoke-btn-${workflow.name}`}
        onClick={(e) => {
          e.stopPropagation();
          onInvoke(workflow);
        }}
      >
        <Play className="h-3.5 w-3.5" />
        Run
      </Button>
    </div>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; workflows: WorkflowSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

type InvokeState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "error"; message: string };

export function WorkflowsPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [view, setView] = useState<ViewId>("all");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  // Invoke panel state: selected workflow + input + submission state.
  const [invokeTarget, setInvokeTarget] = useState<WorkflowSummary | null>(null);
  const [invokeInput, setInvokeInput] = useState("{}");
  const [invokeState, setInvokeState] = useState<InvokeState>({ kind: "idle" });

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listWorkflows(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", workflows: res.items });
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

  const all = useMemo(
    () => (loadState.kind === "ready" ? loadState.workflows : []),
    [loadState],
  );

  // Triage once, sort once. Attention-first (§6.1): nextStepRank is the primary
  // key so "Nothing needed" always sinks; the attention order breaks ties.
  const sorted = useMemo(() => {
    const rows = all.map(triage);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        ATTENTION[x.tone] - ATTENTION[y.tone] ||
        x.workflow.name.localeCompare(y.workflow.name),
    );
    return rows;
  }, [all]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const inView = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // The name filter is applied AFTER the sort, so the ordering a user sees never
  // changes shape while they type.
  const q = query.trim().toLowerCase();
  const visible = q
    ? inView.filter((t) => t.workflow.name.toLowerCase().includes(q))
    : inView;

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling. No counts: `/api/workflows` sends none, and a client-side count
  // of one response's rows would look like a total (kit FilterChipRow contract).
  const chips: FilterChip[] = VIEWS.map((v) => ({ id: v.id, label: v.label }));

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "workflows",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // openInvoke selects a workflow for the invoke panel.
  function openInvoke(wf: WorkflowSummary) {
    setInvokeTarget(wf);
    setInvokeInput("{}");
    setInvokeState({ kind: "idle" });
  }

  // closeInvoke dismisses the panel without invoking.
  function closeInvoke() {
    setInvokeTarget(null);
    setInvokeState({ kind: "idle" });
  }

  // onInvoke POSTs the workflow run and navigates to the created run's trace view on 202.
  async function onInvoke() {
    if (!invokeTarget) return;
    let parsedInput: unknown;
    try {
      parsedInput = invokeInput.trim() ? JSON.parse(invokeInput) : {};
    } catch {
      setInvokeState({ kind: "error", message: "Input must be valid JSON." });
      return;
    }
    setInvokeState({ kind: "submitting" });
    try {
      const result = await api.createWorkflowRun(invokeTarget.name, {
        input: parsedInput,
        namespace: invokeTarget.namespace,
      });
      // Navigate to the run's trace view — the run id is the trace entry point.
      navigate(`/traces/${encodeURIComponent(result.id)}`);
    } catch (err) {
      setInvokeState({
        kind: "error",
        message: err instanceof Error ? err.message : "invoke failed",
      });
    }
  }

  // The §4.4 "resource list" budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 1 never. Workflow,
  // State and Next step survive every width — the row's identity, its
  // condition, and what to do about it.
  const columns: Column<Triaged>[] = [
    {
      id: "workflow",
      header: "Workflow",
      priority: 1,
      cell: ({ workflow: w }) => (
        <CellEntity
          // The cap is what makes `truncate` bite: it clamps the cell's
          // max-content contribution, so a 63-character name can no longer set
          // the width of the table (and through it, of the document).
          className="max-w-[18rem]"
          name={w.name}
          title={w.name}
          namespace={
            <span title={w.namespace} data-testid={`namespace-${w.name}`}>
              {shortNamespace(w.namespace)}
            </span>
          }
        />
      ),
    },
    {
      id: "registry",
      header: "Registry",
      priority: 4,
      className: "max-w-[14rem]",
      cell: ({ workflow: w }) => (
        // The trust boundary the workflow's steps resolve inside — a machine-
        // owned reference, so mono, one line, full value in `title` (§4.5).
        <span
          className="block truncate font-mono text-xs text-secondary-foreground"
          title={w.registryRef}
        >
          {w.registryRef}
        </span>
      ),
    },
    {
      id: "steps",
      header: "Steps",
      priority: 3,
      numeric: true,
      // A known count is a real number in the mono tabular register (§4.5); the
      // noun lives in the header, and the plural phrase in `title` so the digit
      // stays speakable. A zero-step workflow renders `0`, never a dash — "it
      // declares no steps" and "we do not know" are different facts (§7.1).
      cell: ({ workflow: w }) => (
        <span title={stepPhrase(w.stepCount)}>
          <QuantityValue value={w.stepCount} />
        </span>
      ),
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[9rem]",
      cell: ({ workflow: w }) => {
        const code = w.validated ? "" : reasonCode(w.reason);
        return (
          <div className="min-w-0" data-testid={`workflow-state-${w.name}`}>
            <StatusBadge ready={w.validated} reason={w.validated ? undefined : w.reason} />
            {code && (
              // The cause, subordinate to the state (M144.1): readable faint,
              // never ghost — this is information you have to read. The
              // controller's full sentence stays in `title`.
              <div className="mt-1 truncate text-xs text-faint" title={w.reason}>
                {code}
              </div>
            )}
          </div>
        );
      },
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[11rem]",
      cell: (t) => (
        <NextStepLink
          label={t.next.label}
          to={t.next.to}
          tone={t.next.tone}
          ariaLabel={t.next.label ? `${t.next.label} — ${t.workflow.name}` : undefined}
          testId={`next-step-${t.workflow.name}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED list, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching an operator with eight workflows what a workflow is.
  const chipEmptied = all.length > 0 && inView.length === 0;
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
        totalCount: all.length,
        countNoun: "workflows",
      }
    : {
        icon: GitFork,
        title: "No workflows",
        description:
          "No workflows yet. A workflow is a declarative graph of agent invocations — conditional branching, map/loop control flow, and deterministic execution. They are authored as YAML and appear here once the controller has seen them.",
      };

  const needs = sorted.filter((t) => nextStepRank(t.next.tone) === 0).length;
  const broken = sorted.filter((t) => t.tone === "failed").length;
  const closing = closingLine(sorted.length, needs, broken);
  const showChips = loadState.kind === "ready" && all.length > 0;
  const metaLine =
    loadState.kind === "ready"
      ? `${all.length} workflow${all.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="workflows-page">
      <PageHeader
        title="Workflows"
        meta={metaLine}
        lede="Declarative graphs of agent invocations — conditional branching, map/loop control flow, and deterministic execution. Sorted by what is blocking: anything that will not run sits at the top."
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter workflows"
          className="min-w-0"
        />
      )}

      {/* The backend-cannot-answer state (§7 A1 / §7.1): a whole class of value
          a reader expects here — how often this workflow ran, and how it went —
          simply is not in this response. One calm note above the table, never a
          column of zeroes and never an error. */}
      {loadState.kind === "ready" && all.length > 0 && (
        <QuietNote title="Run history isn’t in the workflow list.">
          This list reads what each workflow <em>declares</em> — its steps, and the
          registry those steps resolve inside — plus the controller’s verdict on
          it. How often it has run, and how those runs went, come from the trace
          backend and are not projected here. Nothing is estimated — those figures
          are simply absent.
        </QuietNote>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={(t) => `${t.workflow.namespace}/${t.workflow.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter workflows by name…"
        ariaLabel="Workflows"
        onRowClick={(t) => navigate(workflowPath(t.workflow))}
        rowActions={(t) => <RowActions workflow={t.workflow} onInvoke={openInvoke} />}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {/* Invoke panel — shown when a row's Run action is pressed. */}
      {invokeTarget && (
        <Card data-testid="invoke-panel">
          <CardHeader>
            <CardTitle className="flex min-w-0 items-baseline gap-2 text-base">
              <span className="shrink-0">Run</span>
              {/* A 63-character name truncates on one line here too (§4.5) —
                  never `break-all`, and never a heading that sets the width. */}
              <span className="min-w-0 truncate font-mono" title={invokeTarget.name}>
                {invokeTarget.name}
              </span>
            </CardTitle>
            <CardDescription>
              Provide JSON input for the workflow invocation. An empty object{" "}
              <code className="font-mono text-xs">{"{}"}</code> is fine when the
              workflow accepts no required input.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="space-y-1">
              <label
                htmlFor="invoke-input"
                className="text-xs font-medium text-muted-foreground"
              >
                Input (JSON)
              </label>
              <Textarea
                id="invoke-input"
                data-testid="invoke-input"
                className="min-h-[6rem] font-mono text-xs"
                value={invokeInput}
                onChange={(e) => setInvokeInput(e.target.value)}
                placeholder="{}"
                disabled={invokeState.kind === "submitting"}
              />
            </div>
            {invokeState.kind === "error" && (
              <p className="text-sm text-destructive" role="alert" data-testid="invoke-error">
                {invokeState.message}
              </p>
            )}
            <div className="flex flex-wrap items-center gap-3">
              <Button
                data-testid="invoke-submit"
                onClick={() => void onInvoke()}
                disabled={invokeState.kind === "submitting"}
              >
                <Play className="h-4 w-4" />
                {invokeState.kind === "submitting" ? "Starting…" : "Start workflow run"}
              </Button>
              <Button
                variant="outline"
                data-testid="invoke-cancel"
                onClick={closeInvoke}
                disabled={invokeState.kind === "submitting"}
              >
                Cancel
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
