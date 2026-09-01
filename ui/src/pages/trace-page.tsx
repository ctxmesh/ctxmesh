import * as React from "react";
import { Link, useParams } from "react-router-dom";
import { Boxes, Brain, ExternalLink, PlusCircle, Share2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ClosingNote,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  Skeleton,
  TimelineSkeleton,
  truncateId,
  type KeyValueItem,
} from "@/components/kit";
import { TraceExplorer } from "@/components/dashboard/trace-explorer";
import { FixtureStepper } from "@/components/dashboard/fixture-stepper";
import { FeedbackPanel } from "@/components/dashboard/feedback-panel";
import { api, ApiError, type SpanSummary, type TraceDetailResponse } from "@/lib/api";
import { formatDateTime, formatLatency, formatUSD } from "@/lib/format";
import { ShareRunDialog } from "@/components/dashboard/share-run-dialog";

// TracePage — the forensic run reader (m16.7; M151 §6.1 archetype A5, trace
// variant). Reached via /traces/:id and from "View full trace" in RunInspector.
//
// ── THE PAGE'S ONE IDEA: THE SPAN TREE *IS* THE TIMELINE ────────────────────
// §6.2 assigns this page A5 with one substitution: where run-detail draws the
// kit Timeline, this page draws the span tree + waterfall. Both are the same
// claim — a guardrail, an approval and a retry are steps of the run, sitting in
// one stream between the model and tool calls that surround them, at the depth
// where they happened. There is no governance tab and no second lane, because a
// reader asking "why did this stop here?" must find the answer on the next row,
// not by correlating two clocks.
//
// The tree is a WIDE artifact, so it lives in its own horizontal scroll
// container (§4.6): the page body never scrolls sideways, and the waterfall
// keeps a legible floor instead of being crushed to a smear at 1024.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// The rollup's cost/token/latency figures are only as real as the trace
// backend's attribution. A zero here is almost always "not attributed", not
// "measured nought" — a run with eleven spans and two generations did not cost
// nothing — so a zero renders as the honest absence with the canonical A5
// sentence beside it, never as `$0.0000`.
//
// ── LANGFUSE IS A LINK-OUT, NOT A HOME ──────────────────────────────────────
// The old embedded iframe is gone (m16.7 demotion). One outline link at the
// foot of the story, after everything the console can answer itself.
//
// data-testid contract:
//   trace-page              — root container
//   trace-header            — the page band (name / id / timestamp)
//   trace-cost              — the rollup facts panel (tokens / cost / latency)
//   trace-langfuse-linkout  — the link-out anchor
//   trace-agent-link        — trace → agent back-link
//   trace-memory-link       — trace → that agent's memory
//   trace-share-btn         — opens the share-mint dialog
//   trace-page-unconfigured — 501: no trace backend on this install
//   trace-page-error        — the generic error state

// AddToDatasetPanel — the "add to dataset" on-ramp (m69.3, ADR 0062 Fork 5).
// POSTs to /api/datasets/{name}/cases/from-run which fetches the trace, redacts PII,
// and appends the result as a case. 501-calm when the dataset store is unconfigured.
//
// data-testid contract:
//   add-to-dataset-panel   — root container
//   add-to-dataset-input   — dataset name text input
//   add-to-dataset-submit  — the submit button
//   add-to-dataset-result  — success/error message after submission

type AddToDatasetState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "success"; caseId: string }
  | { kind: "unconfigured" } // 501 calm
  | { kind: "error"; message: string };

function AddToDatasetPanel({ traceId }: { traceId: string }) {
  const [datasetName, setDatasetName] = React.useState("");
  const [state, setState] = React.useState<AddToDatasetState>({ kind: "idle" });

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    const name = datasetName.trim();
    if (!name) return;
    setState({ kind: "submitting" });
    try {
      const result = await api.addRunToDataset(name, { traceId });
      setState({ kind: "success", caseId: result.caseId });
    } catch (err) {
      if (err instanceof ApiError && err.isNotImplemented) {
        setState({ kind: "unconfigured" });
        return;
      }
      setState({
        kind: "error",
        message: err instanceof Error ? err.message : "failed to add to dataset",
      });
    }
  }

  return (
    <Card className="min-w-0" data-testid="add-to-dataset-panel">
      <PanelHeader title="Keep this run as a test case" />
      <CardContent>
        <p className="mb-3 text-sm text-secondary-foreground">
          Personal data is redacted, then the run is appended to a dataset as a
          labelled case. The dataset is created if it does not exist yet.
        </p>
        <form
          onSubmit={(e) => void onSubmit(e)}
          className="flex flex-wrap items-center gap-3"
        >
          <Input
            data-testid="add-to-dataset-input"
            placeholder="dataset-name"
            value={datasetName}
            onChange={(e) => setDatasetName(e.target.value)}
            disabled={state.kind === "submitting"}
            className="max-w-xs font-mono"
          />
          <Button
            type="submit"
            size="sm"
            data-testid="add-to-dataset-submit"
            disabled={!datasetName.trim() || state.kind === "submitting"}
          >
            <PlusCircle className="h-3.5 w-3.5" />
            {state.kind === "submitting" ? "Adding…" : "Add to dataset"}
          </Button>
        </form>
        {state.kind === "success" && (
          <p className="mt-3 text-sm text-success" data-testid="add-to-dataset-result">
            Added as case{" "}
            <span className="font-mono text-xs">{state.caseId}</span>.{" "}
            <Link
              to={`/datasets/${encodeURIComponent(datasetName.trim())}`}
              className="font-medium text-primary hover:underline"
            >
              View dataset →
            </Link>
          </p>
        )}
        {state.kind === "unconfigured" && (
          <div className="mt-3" data-testid="add-to-dataset-result">
            <QuietNote title="The dataset store isn't configured on this install.">
              Nothing was added and nothing was lost — there is simply nowhere to
              put it. Enabling it needs the control-plane store the rest of the
              platform uses (<span className="font-mono text-xs">CONTROLPLANE_DSN</span>).
            </QuietNote>
          </div>
        )}
        {state.kind === "error" && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="add-to-dataset-result"
          >
            {state.message}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

type PageState =
  | { kind: "loading" }
  | { kind: "unconfigured" } // 501 → no trace backend wired; calm degrade, not a failure
  | { kind: "ready"; detail: TraceDetailResponse; langfuseUrl: string | null }
  | { kind: "error"; message: string; forbidden: boolean };

/**
 * The canonical §7 A5 absence sentence, said once beside the figures it
 * explains. A trace whose spans ran but whose cost reads 0 was not free; it was
 * unattributed, and the two must never share a glyph.
 */
const UNATTRIBUTED_SPEND =
  "Tool spend is attributed when the run closes. It reads not yet known rather than $0.0000, because zero would be a claim we can't make. What the trace backend did attribute is shown above; nothing here is estimated.";

const UNATTRIBUTED_TITLE =
  "The trace backend attributed no figure here. It is unknown — not zero.";

/** A span the backend marked as failed. Drives the header tag and the closing line. */
function failedSpans(spans: SpanSummary[]): number {
  return spans.filter((s) => s.status === "error" || s.level === "ERROR").length;
}

export function TracePage() {
  const { id = "" } = useParams();
  const [state, setState] = React.useState<PageState>({ kind: "loading" });
  const [shareOpen, setShareOpen] = React.useState(false);
  // Retry is a re-run of the same effect, not a second fetch path — §7 A5's
  // "ErrorState + Retry" with exactly one place that knows how to load a trace.
  const [attempt, setAttempt] = React.useState(0);

  React.useEffect(() => {
    if (!id) return;
    const controller = new AbortController();
    setState({ kind: "loading" });

    // Fetch detail + Langfuse link-out in parallel. If the link-out fails we
    // simply don't show the button (best-effort, matching RunInspector).
    Promise.all([
      api.traceDetail(id, controller.signal),
      api.traceLink(id, controller.signal).catch(() => null),
    ])
      .then(([detail, linkRes]) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "ready",
          detail,
          langfuseUrl: linkRes?.url ?? null,
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 501 → no trace backend wired: calm "not configured", never a red error.
        if (err instanceof ApiError && err.isNotImplemented) {
          setState({ kind: "unconfigured" });
          return;
        }
        const forbidden = err instanceof ApiError && err.isForbidden;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the trace",
          forbidden,
        });
      });

    return () => controller.abort();
  }, [id, attempt]);

  // ── Loading (§7 A5) ─────────────────────────────────────────────────────────
  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6">
        <PageHeader title="Trace" loading />
        <div className="grid gap-5 xl:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)]">
          <div className="min-w-0 rounded-lg border border-border bg-card p-5">
            <TimelineSkeleton />
          </div>
          <Card className="min-w-0">
            <PanelHeader title="What it cost" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label="Loading trace totals">
                {[0, 1, 2, 3].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  // ── 501: no trace backend on this install (calm, never a red error) ─────────
  if (state.kind === "unconfigured") {
    return (
      <div className="min-w-0 space-y-6" data-testid="trace-page-unconfigured">
        <PageHeader
          title="Trace"
          lede="The step-by-step record of one run — when a trace backend is wired up."
        />
        <QuietNote title="Tracing isn't configured on this install.">
          No trace backend is wired in this cluster, so there is no per-run span
          record to read. Runs still execute and still return their results —
          only their forensics are absent. Wiring one up (Langfuse) turns this
          page on. Nothing here is estimated; the trace is simply not collected.
        </QuietNote>
      </div>
    );
  }

  // ── Error / forbidden ───────────────────────────────────────────────────────
  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader title="Trace" />
          <ForbiddenInline
            title="You don't have permission to view this trace."
            resource="traces"
            detail={state.message}
          />
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="trace-page-error">
        <PageHeader title="Trace" />
        <ErrorState
          title="The trace didn't load."
          description="The run itself is unaffected — only its forensic record failed to read."
          detail={state.message}
          onRetry={() => setAttempt((n) => n + 1)}
        />
      </div>
    );
  }

  // ── Ready ───────────────────────────────────────────────────────────────────
  const { rollup, spans } = state.detail;
  const failed = failedSpans(spans);
  // Zero is not a measurement here — see the header note. `undefined` routes the
  // row to KeyValueList's stated-absence branch instead of a formatted nought.
  const knownCost = rollup.costUSD > 0 ? rollup.costUSD : undefined;
  const knownTokens = rollup.tokens > 0 ? rollup.tokens : undefined;
  const knownLatency = rollup.latencyMs > 0 ? rollup.latencyMs : undefined;
  const anyUnattributed = knownCost === undefined || knownTokens === undefined;

  const totals: KeyValueItem[] = [
    {
      key: "Tokens",
      // Machine-owned figures are mono tabular (§4.8) — QuantityValue is the
      // one place the console decides how a number is drawn.
      value:
        knownTokens === undefined ? undefined : <QuantityValue value={knownTokens} />,
      absent: "not yet known",
      title: UNATTRIBUTED_TITLE,
    },
    {
      // Money is never truncated, never wrapped, never elided (§4.5).
      key: "Cost",
      value:
        knownCost === undefined ? undefined : (
          <span className="whitespace-nowrap tabular-nums">{formatUSD(knownCost)}</span>
        ),
      absent: "not yet known",
      title: UNATTRIBUTED_TITLE,
    },
    {
      key: "Latency",
      value:
        knownLatency === undefined ? undefined : (
          <span className="whitespace-nowrap tabular-nums">
            {formatLatency(knownLatency)}
          </span>
        ),
      absent: "not recorded",
      title: "The trace backend recorded no wall-clock duration for this run.",
    },
    // Both counts ARE real measurements — the console counted them itself from
    // the spans in hand — so a zero here is a true zero and renders as `0`.
    { key: "Spans", value: <QuantityValue value={spans.length} /> },
    {
      key: "Failed steps",
      // §2.2 allows the hue on a numeric cell that CARRIES the state. A zero
      // takes QuantityValue's unremarkable-but-real register instead.
      value: (
        <QuantityValue
          value={failed}
          className={failed > 0 ? "text-destructive" : undefined}
        />
      ),
    },
  ];

  const provenance: KeyValueItem[] = [
    { key: "Trace id", value: rollup.traceId || id, title: rollup.traceId || id },
    {
      key: "Recorded",
      value: rollup.timestamp ? formatDateTime(rollup.timestamp) : undefined,
      absent: "not recorded",
    },
    {
      key: "Agent",
      value:
        rollup.agentNs && rollup.agentName
          ? `${rollup.agentNs}/${rollup.agentName}`
          : undefined,
      absent: "not tagged",
      title:
        "This trace carries no agent tag, so it cannot be linked back to one.",
    },
  ];

  const spanLede =
    spans.length === 0
      ? "The trace backend holds this trace but recorded no spans against it."
      : `${spans.length} span${spans.length === 1 ? "" : "s"}, in the order they ran. Guardrails, approvals and retries sit in this same tree, at the depth where they happened — they are steps of the run, not a separate system.`;

  return (
    <div className="min-w-0 space-y-6" data-testid="trace-page">
      <div data-testid="trace-header">
        <PageHeader
          breadcrumb={[{ label: "Runs", to: "/runs" }, { label: truncateId(id) }]}
          title={rollup.name || "Trace"}
          status={
            failed > 0 ? (
              <Badge variant="crit">
                {failed === 1 ? "1 failed step" : `${failed} failed steps`}
              </Badge>
            ) : undefined
          }
          meta={[truncateId(id), formatDateTime(rollup.timestamp)]
            .filter(Boolean)
            .join(" · ")}
          lede="Everything this run did, step by step — what the model was asked, what each tool returned, and every guardrail and approval in between."
          // actionsSlot rather than a structured action: the black-box suites
          // open the share dialog by `trace-share-btn`, and PageHeaderAction
          // carries no testId.
          actionsSlot={
            <Button
              variant="outline"
              size="sm"
              className="text-sm"
              onClick={() => setShareOpen(true)}
              data-testid="trace-share-btn"
            >
              <Share2 className="h-4 w-4" />
              Share
            </Button>
          }
        />
      </div>

      {/* §4.7 g2 — deviation, recorded: this pair collapses at `xl`, not at the
          §4.7 default of `lg`. The left column holds a waterfall whose meaning
          IS its width; at 1024 a 1fr rail would squeeze it to ~410px and every
          bar would read the same length. Stacking one breakpoint earlier gives
          the artifact the room the artifact needs. */}
      <div className="grid gap-5 xl:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)]">
        {/* Explicit placement, because DOM order is also the stacked reading
            order below `xl`: the story, then the totals that describe it, then
            the secondary panels. In one flat column the totals sat four panels
            below the tree they summarise. */}
        <div className="min-w-0 space-y-6 xl:col-start-1 xl:row-start-1">
          <section aria-labelledby="trace-spans-head" className="min-w-0">
            <SectionHeader
              id="trace-spans-head"
              title="What happened"
              lede={spanLede}
            />
            {/* §4.6: the wide artifact scrolls inside its own container, and the
                floor keeps the waterfall legible instead of letting the grid
                crush it. The page body never scrolls sideways. */}
            <div className="overflow-x-auto">
              <div className="min-w-[34rem]">
                <TraceExplorer spans={spans} />
              </div>
            </div>
            {spans.length > 0 && (
              <ClosingNote>
                {failed === 0
                  ? `${spans.length} steps, and none of them failed.`
                  : failed === 1
                    ? `${spans.length} steps, and one of them failed. It is the row with the red mark.`
                    : `${spans.length} steps, and ${failed} of them failed. They are the rows with the red marks.`}
              </ClosingNote>
            )}
          </section>

        </div>

        <div className="min-w-0 space-y-5 xl:col-start-2 xl:row-start-1 xl:row-span-2">
          <Card className="min-w-0" data-testid="trace-cost">
            <PanelHeader title="What it cost" />
            <CardContent>
              <KeyValueList items={totals} />
              {anyUnattributed && (
                <QuietNote className="mt-4" title="Spend isn't fully attributed.">
                  {UNATTRIBUTED_SPEND}
                </QuietNote>
              )}
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="Where it came from" />
            <CardContent>
              <KeyValueList items={provenance} />
              {/* Trace → agent + trace → memory (m49.3): close the loop from an
                  observed run back to the agent that produced it, and to that
                  agent's memory (otherwise undiscoverable, M46 review P1). Only
                  when the trace carries a full agent identity. */}
              {rollup.agentNs && rollup.agentName && (
                <div className="mt-4 flex flex-wrap items-center gap-x-4 gap-y-2">
                  <Link
                    to={`/agents/${encodeURIComponent(rollup.agentNs)}/${encodeURIComponent(rollup.agentName)}`}
                    data-testid="trace-agent-link"
                    className="inline-flex min-w-0 items-center gap-1.5 font-mono text-xs font-medium text-primary hover:underline"
                  >
                    <Boxes className="h-3.5 w-3.5 shrink-0" />
                    <span className="truncate">
                      {rollup.agentNs}/{rollup.agentName}
                    </span>
                  </Link>
                  <Link
                    to={`/agents/${encodeURIComponent(rollup.agentNs)}/${encodeURIComponent(rollup.agentName)}?tab=Memory`}
                    data-testid="trace-memory-link"
                    className="inline-flex items-center gap-1.5 text-sm text-primary hover:underline"
                  >
                    <Brain className="h-3.5 w-3.5 shrink-0" />
                    Memory
                  </Link>
                </div>
              )}
            </CardContent>
          </Card>
        </div>

        <div className="min-w-0 space-y-6 xl:col-start-1 xl:row-start-2">
          {/* The recorded fixture (O10a, ADR 0071 §5): the wire-exact I/O
              `dev --replay` re-serves in CI. It renders unframed (and renders
              NOTHING at all when this run has no readable fixture), so the page
              lends it a panel frame — and `empty:hidden` retires that frame in
              the same render where the component decides to say nothing, rather
              than leaving an empty box on the page. */}
          <div className="min-w-0 rounded-lg border border-border bg-card p-5 empty:hidden">
            <FixtureStepper runId={id} />
          </div>

          <FeedbackPanel traceId={id} />

          <AddToDatasetPanel traceId={id} />

          {/* The ONE forensics escape hatch (m16.7), deliberately last and
              deliberately quiet: everything the console can answer itself is
              above it. */}
          {state.langfuseUrl && (
            <div>
              <a
                href={state.langfuseUrl}
                target="_blank"
                rel="noreferrer"
                data-testid="trace-langfuse-linkout"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                <ExternalLink className="h-4 w-4" />
                Open forensics in Langfuse
              </a>
              <p className="mt-2 max-w-[64ch] text-sm text-faint">
                The raw trace, in the backend that stored it. Nothing above is
                hidden there — it is the same record, with the vendor's own
                tooling around it.
              </p>
            </div>
          )}
        </div>
      </div>

      <ShareRunDialog
        open={shareOpen}
        onClose={() => setShareOpen(false)}
        runId={id}
      />
    </div>
  );
}
