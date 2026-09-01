import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { ArrowRight, GitBranch, Repeat, Rows3, Shield } from "lucide-react";

import {
  ClosingNote,
  ErrorState,
  ForbiddenInline,
  PageHeader,
  Skeleton,
  StatusBadge,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import {
  api,
  ApiError,
  type WorkflowDetailResponse,
  type WorkflowGraphEdge,
  type WorkflowGraphNode,
} from "@/lib/api";
import { cn } from "@/lib/utils";

// WorkflowDetailPage — the DECLARED workflow DAG on the shared canvas
// (M144-canvas + ADR 0115; M151 §6.1 archetype A6, §6.2 "declared DAG on the
// shared canvas"). Renders GET /api/workflows/{ns}/{name}.
//
// ── THE PAGE'S ONE IDEA: A DECLARED GRAPH HAS NO HEALTH ────────────────────
// Nothing on this page has run. It is the shape the author wrote down, so the
// only claim the page can honestly make about a step is WHAT IT IS — and the
// only claim it can make about the whole thing is whether the controller
// accepted it. That single fact is the page's one status; everything else is
// identity and renders in the neutral register (§2.2).
//
// This is why the old edge tints had to go. `branch`, `map`, `join` and `loop`
// were painted `text-info` — and `--info` now carries the HOLD violet, the hue
// that means "a person must decide" (§2.4). A declared fan-out was announcing a
// human gate on a page where no human has been asked for anything. Control-flow
// KIND is identity, so it is told apart by WORD and FORM: an uppercase mono
// chip naming the kind, and the dashed `open` chip for `catch`, which is the
// path the author hopes is never taken (§5.1). No hue at all.
//
// ── LAYOUT: RANKED COLUMNS, NOT SPAGHETTI ──────────────────────────────────
// Steps are placed in left→right columns by longest-path rank from the start,
// so the picture reads in execution order without a layout engine and without
// drawing a single crossing line. Back-edges (`loop`) are excluded from the
// ranking — a loop is by definition a return to an earlier step, and letting it
// push its target rightward would draw the cycle as if it were progress. Each
// node names its own outgoing edges, so the structure is readable even when two
// columns are far apart on the canvas.
//
// The canvas is a pan surface inside a FIXED frame (§4.6): a 40-step workflow
// pans inside the box, and the page around it never scrolls sideways.
//
// data-testid contract:
//   workflow-detail-page / workflow-detail-loading / workflow-detail-error
//   workflow-dag / workflow-node-{name}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; detail: WorkflowDetailResponse }
  | { kind: "error"; message: string; forbidden: boolean };

// ---------------------------------------------------------------------------
// The canvas frame (§6.1 A6, §4.6)
// ---------------------------------------------------------------------------

/**
 * The A6 frame. Shared VERBATIM with `topology-page.tsx`, the console's other
 * canvas — see the note there: this grammar belongs in `kit/` as a `Canvas`
 * primitive, and the M151 page-conversion fence puts `components/kit/` off
 * limits to this task, so the two pages carry the same strings and a pointer to
 * each other instead. Lifting it is carded for the backlog.
 */
const CANVAS_FRAME =
  "relative min-h-[35rem] max-h-[42rem] min-w-0 overflow-auto rounded-lg border border-border bg-card p-6";
const CANVAS_GRID =
  "bg-[radial-gradient(hsl(var(--border))_1px,transparent_1px)] [background-size:22px_22px]";

// ---------------------------------------------------------------------------
// Ranking — longest path from the start, cycles excluded
// ---------------------------------------------------------------------------

/** A `loop` edge is a back-edge by definition and must not drive the layout. */
function isBackEdge(edge: WorkflowGraphEdge): boolean {
  return edge.kind === "loop";
}

/**
 * name → column index. Longest-path relaxation, bounded by the node count, so a
 * declaration containing a cycle terminates rather than spinning; a step the
 * relaxation cannot place (an edge to a name that is not declared) simply stays
 * in column 0 rather than being dropped — a dangling edge is a fact about the
 * declaration, not a reason to hide a step.
 */
function rankNodes(nodes: WorkflowGraphNode[]): Map<string, number> {
  const rank = new Map<string, number>();
  for (const n of nodes) rank.set(n.name, 0);
  const cap = Math.max(nodes.length - 1, 0);
  for (let pass = 0; pass < nodes.length; pass++) {
    let changed = false;
    for (const n of nodes) {
      const from = rank.get(n.name) ?? 0;
      for (const e of n.edges) {
        if (isBackEdge(e)) continue;
        const current = rank.get(e.to);
        if (current === undefined) continue; // dangling target — nothing to place
        const next = Math.min(from + 1, cap);
        if (current < next) {
          rank.set(e.to, next);
          changed = true;
        }
      }
    }
    if (!changed) break;
  }
  return rank;
}

/** The ranked columns, each preserving the declared order within it. */
function columnsOf(nodes: WorkflowGraphNode[]): WorkflowGraphNode[][] {
  const rank = rankNodes(nodes);
  const byRank = new Map<number, WorkflowGraphNode[]>();
  for (const n of nodes) {
    const r = rank.get(n.name) ?? 0;
    const bucket = byRank.get(r);
    if (bucket) bucket.push(n);
    else byRank.set(r, [n]);
  }
  return Array.from(byRank.keys())
    .sort((a, b) => a - b)
    .map((r) => byRank.get(r) as WorkflowGraphNode[]);
}

// ---------------------------------------------------------------------------
// Node + edge grammar — identity only, never a hue
// ---------------------------------------------------------------------------

function NodeKindMark({ kind }: { kind: string }) {
  const className = "h-3.5 w-3.5 shrink-0 text-faint";
  if (kind === "choice") return <GitBranch className={className} aria-hidden />;
  if (kind === "map") return <Rows3 className={className} aria-hidden />;
  if (kind === "loop") return <Repeat className={className} aria-hidden />;
  return <span className="h-2 w-2 shrink-0 rounded-full bg-ghost" aria-hidden />;
}

function EdgeRow({ edge }: { edge: WorkflowGraphEdge }) {
  const showLabel = !!edge.label && edge.label !== edge.kind;
  return (
    <div className="min-w-0 font-mono text-2xs">
      <div className="flex min-w-0 items-center gap-1.5">
        <ArrowRight className="h-3 w-3 shrink-0 text-ghost" aria-hidden />
        {/* The kind is IDENTITY: an uppercase mono chip, one neutral register —
            except `catch`, the path the author hopes is never taken, which takes
            the dashed `open` chip. Form, not hue (§2.3). */}
        <Badge variant={edge.kind === "catch" ? "open" : "muted"} className="shrink-0">
          {edge.kind}
        </Badge>
        <span className="shrink-0 text-ghost" aria-hidden>
          →
        </span>
        {/* The TARGET is the structural fact and gets the room; sharing one line
            with the predicate truncated both to three characters apiece. */}
        <span className="truncate font-medium" title={edge.to}>
          {edge.to}
        </span>
      </div>
      {showLabel && (
        <p className="truncate pl-[1.3rem] text-faint" title={edge.label}>
          {edge.label}
        </p>
      )}
    </div>
  );
}

function WorkflowNodeCard({ node }: { node: WorkflowGraphNode }) {
  return (
    <div
      className="w-64 rounded-md border border-border bg-card px-3 py-2"
      data-testid={`workflow-node-${node.name}`}
    >
      <div className="flex items-center gap-1.5">
        <NodeKindMark kind={node.kind} />
        <p className="font-mono text-2xs uppercase tracking-wide text-faint">
          {node.kind}
        </p>
        {node.start && (
          <Badge variant="muted" className="ml-auto shrink-0">
            start
          </Badge>
        )}
      </div>
      <p className="mt-0.5 truncate font-mono text-sm font-medium" title={node.name}>
        {node.name}
      </p>
      {node.agentRef && (
        <p
          className="truncate font-mono text-2xs text-faint"
          title={`dispatches to ${node.agentRef}`}
        >
          → {node.agentRef}
        </p>
      )}
      {node.edges.length > 0 ? (
        <div className="mt-2 space-y-1 border-t border-border-soft pt-2">
          {node.edges.map((e, i) => (
            <EdgeRow key={`${e.kind}-${e.to}-${i}`} edge={e} />
          ))}
        </div>
      ) : (
        <p className="mt-2 border-t border-border-soft pt-2 font-mono text-2xs text-faint">
          terminal — the workflow ends here
        </p>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// WorkflowDetailPage
// ---------------------------------------------------------------------------

export function WorkflowDetailPage() {
  const { ns, name } = useParams<{ ns: string; name: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    if (!ns || !name) return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .getWorkflow(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [ns, name]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const detail = state.kind === "ready" ? state.detail : null;
  const columns = useMemo(
    () => (detail ? columnsOf(detail.nodes) : []),
    [detail],
  );

  const crumbs = [
    { label: "Workflows", to: "/workflows" },
    { label: name ?? "workflow" },
  ];

  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-5" data-testid="workflow-detail-page">
        <PageHeader breadcrumb={crumbs} title={name ?? "Workflow"} titleMono loading />
        <div
          className={cn(CANVAS_FRAME, CANVAS_GRID)}
          role="status"
          aria-busy="true"
          aria-label="Loading workflow"
          data-testid="workflow-detail-loading"
        >
          <div className="flex gap-8">
            {[0, 1, 2].map((c) => (
              <div key={c} className="space-y-4">
                {[0, 1].map((r) => (
                  <Skeleton decorative key={r} className="h-24 w-64 rounded-md" />
                ))}
              </div>
            ))}
          </div>
        </div>
      </div>
    );
  }

  if (state.kind === "error") {
    return (
      <div className="min-w-0 space-y-5" data-testid="workflow-detail-page">
        <PageHeader breadcrumb={crumbs} title={name ?? "Workflow"} titleMono />
        {state.forbidden ? (
          <ForbiddenInline
            title="You don't have permission to view this workflow."
            resource="workflows"
            detail={state.message}
          />
        ) : (
          <div data-testid="workflow-detail-error">
            <ErrorState
              title="The workflow didn't load."
              description="The declaration is unaffected — only this page failed to read it."
              detail={state.message}
              onRetry={load}
            />
          </div>
        )}
      </div>
    );
  }

  const d = state.detail;
  const stepCount = d.nodes.length;
  const terminals = d.nodes.filter((n) => n.edges.length === 0).length;

  return (
    <div className="min-w-0 space-y-5" data-testid="workflow-detail-page">
      <PageHeader
        breadcrumb={crumbs}
        title={d.name}
        titleMono
        status={
          <StatusBadge
            ready={d.validated}
            phase={d.validated ? "Validated" : "Not validated"}
            reason={d.reason}
          />
        }
        meta={`${d.namespace} · ${stepCount} step${stepCount === 1 ? "" : "s"}`}
        lede="The declared step graph — what the author wrote down, and how control flows between the steps. Nothing here has run; a run gets its own reader."
      />

      {/* The trust boundary (the same fence framing as the Team Sheet, ADR 0115)
          — now in the NEUTRAL register. It used to take `border-info`, which is
          the hold violet: a registry reference is not a human gate (§2.4). */}
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-border border-l-2 border-l-border-strong bg-surface-2 px-3 py-2 font-mono text-2xs">
        <Shield className="h-3.5 w-3.5 shrink-0 text-faint" aria-hidden />
        <span className="uppercase tracking-wide text-faint">
          trust boundary · registry
        </span>
        <span className="font-medium" title={d.registryRef}>
          {d.registryRef}
        </span>
        <span className="text-faint">· namespace {d.namespace}</span>
      </div>

      {/* A declaration the controller rejected will not run. Crit as a 2px left
          rule on a neutral ground — annotation, never a full-bleed alarm
          surface (§2.2); the tag in the header already carries the state. */}
      {!d.validated && (
        <div
          role="status"
          className="rounded-md border border-border border-l-2 border-l-destructive bg-card px-4 py-3"
        >
          <p className="font-serif text-md font-medium">
            This workflow isn&apos;t validated, so it will not run.
          </p>
          <p className="mt-1 max-w-[64ch] text-sm text-secondary-foreground">
            {d.reason ? (
              <>
                The controller rejected the declared graph:{" "}
                <span className="font-mono text-xs">{d.reason}</span>. The steps
                below are still what was declared — fix the declaration and it
                validates on the next reconcile.
              </>
            ) : (
              <>
                The controller has not accepted the declared graph. It reported
                no reason, so none is shown here — nothing about the cause is
                inferred.
              </>
            )}
          </p>
        </div>
      )}

      {/* The DAG on the shared canvas: ranked columns, left→right, panning
          inside a fixed frame (§4.6). */}
      <div className={cn(CANVAS_FRAME, CANVAS_GRID)} data-testid="workflow-dag">
        {/* `w-max` sizes the column row to its CONTENT, so the row itself never
            overflows: the frame above is the thing that scrolls (§4.6). Without
            it the row is pinned to the frame's width and reports its own
            overflow, which is exactly the "does not fit" defect the fit gate
            exists to catch. */}
        <div className="flex w-max items-start gap-8">
          {columns.map((column, i) => (
            <div key={i} className="flex shrink-0 flex-col gap-4">
              {column.map((node) => (
                <WorkflowNodeCard key={node.name} node={node} />
              ))}
            </div>
          ))}
        </div>
      </div>

      <ClosingNote>
        {stepCount === 1
          ? "One step, and it is where the workflow ends."
          : `${stepCount} steps in ${columns.length} ${columns.length === 1 ? "stage" : "stages"}, left to right — pan the canvas to follow them. ${
              terminals === 1
                ? "One of them is terminal."
                : `${terminals} of them are terminal.`
            }`}
      </ClosingNote>
    </div>
  );
}
