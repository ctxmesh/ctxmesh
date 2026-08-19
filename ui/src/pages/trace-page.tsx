import * as React from "react";
import { Link, useParams } from "react-router-dom";
import { ExternalLink, Boxes, Brain, PlusCircle, Share2 } from "lucide-react";

import { Button, buttonVariants } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { formatTokens } from "@/lib/format";
import { ForbiddenInline, SkeletonCard } from "@/components/kit";
import { TraceExplorer } from "@/components/dashboard/trace-explorer";
import { FeedbackPanel } from "@/components/dashboard/feedback-panel";
import { api, ApiError, type TraceDetailResponse } from "@/lib/api";
import { ShareRunDialog } from "@/components/dashboard/share-run-dialog";

// TracePage — the full-page single-trace view (m16.7). Reached via /traces/:id
// and from the "View full trace" link in RunInspector.
//
// Layout:
//   • Header: agent name, timestamp, total tokens/cost (from the rollup).
//   • TraceExplorer: the span tree + waterfall.
//   • Langfuse forensics link-out: a button that opens GET /api/traces/{id}
//     (the Langfuse deep link, resolved server-side) in a new tab.
//     NOTE: the embedded iframe from the old TraceView is NOT on this page —
//     Langfuse is link-out only on this surface (m16.7 demotion).
//   • FeedbackPanel: per-trace scores (m16.9, 501-calm / 502-error).
//
// data-testid contract:
//   trace-page              — root container
//   trace-header            — the header block (name / timestamp / totals)
//   trace-langfuse-linkout  — the link-out button/anchor

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
    <Card data-testid="add-to-dataset-panel">
      <CardHeader>
        <CardTitle className="text-base">Add to dataset</CardTitle>
        <CardDescription>
          Redact PII from this trace and add it as a labeled case in a dataset (m69.3,
          ADR 0062 Fork 5). Creates the dataset if it does not exist.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={(e) => void onSubmit(e)}
          className="flex items-center gap-3"
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
            <span className="font-mono">{state.caseId}</span>.{" "}
            <a
              href={`/datasets/${encodeURIComponent(datasetName.trim())}`}
              className="underline hover:no-underline"
            >
              View dataset →
            </a>
          </p>
        )}
        {state.kind === "unconfigured" && (
          <p
            className="mt-3 text-sm text-muted-foreground"
            data-testid="add-to-dataset-result"
          >
            Dataset store not configured — set{" "}
            <span className="font-mono">CONTROLPLANE_DSN</span> to enable this.
          </p>
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

function fmtCost(usd: number): string {
  if (usd === 0) return "$0.000";
  return usd < 0.001 ? `$${usd.toFixed(5)}` : `$${usd.toFixed(3)}`;
}

function fmtTimestamp(ts: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

export function TracePage() {
  const { id = "" } = useParams();
  const [state, setState] = React.useState<PageState>({ kind: "loading" });
  const [shareOpen, setShareOpen] = React.useState(false);

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
  }, [id]);

  if (state.kind === "loading") {
    return (
      <div className="mx-auto max-w-5xl space-y-6 px-4 py-6">
        <SkeletonCard />
        <SkeletonCard />
      </div>
    );
  }

  if (state.kind === "unconfigured") {
    // 501: no trace backend wired. Calm empty state, never a red error.
    return (
      <div
        className="mx-auto max-w-5xl px-4 py-6"
        data-testid="trace-page-unconfigured"
      >
        <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground shadow-card">
          <p className="font-medium text-foreground">Trace view not configured</p>
          <p className="mt-1">
            No trace backend is wired in this cluster, so per-run traces aren't
            available. Runs still execute and return their results — wire a trace
            backend (Langfuse) to see the step-by-step trace here.
          </p>
        </div>
      </div>
    );
  }

  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="mx-auto max-w-5xl px-4 py-6">
          <ForbiddenInline
            title="Not allowed to read this trace"
            description="Your account can't read traces in this cluster."
            detail={state.message}
          />
        </div>
      );
    }
    return (
      <div
        className="mx-auto max-w-5xl px-4 py-6"
        role="alert"
        data-testid="trace-page-error"
      >
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-6 text-sm text-destructive">
          Couldn't load the trace: {state.message}
        </div>
      </div>
    );
  }

  const { rollup, spans } = state.detail;

  return (
    <div
      className="mx-auto max-w-5xl space-y-6 px-4 py-6"
      data-testid="trace-page"
    >
      {/* ── Header ─────────────────────────────────────────────────────────── */}
      <div
        className="flex flex-wrap items-start justify-between gap-4 rounded-lg border bg-card p-5 shadow-card"
        data-testid="trace-header"
      >
        <div className="min-w-0 space-y-1">
          <h1 className="truncate text-lg font-semibold tracking-snug">
            {rollup.name || "Trace"}
          </h1>
          <p className="truncate font-mono text-xs text-muted-foreground">{id}</p>
          <p className="text-sm text-muted-foreground">
            {fmtTimestamp(rollup.timestamp)}
          </p>
          {/* Trace→agent + trace→memory back-links (m49.3) — close the loop from an
              observed run back to the agent that produced it AND to that agent's memory
              (memory is otherwise undiscoverable, M46 review P1). Only when the trace
              carries a full agent identity. */}
          {rollup.agentNs && rollup.agentName && (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <Link
                to={`/agents/${encodeURIComponent(rollup.agentNs)}/${encodeURIComponent(rollup.agentName)}`}
                data-testid="trace-agent-link"
                className="inline-flex items-center gap-1 text-sm font-medium text-primary hover:underline"
              >
                <Boxes className="h-3.5 w-3.5" />
                {rollup.agentNs}/{rollup.agentName}
              </Link>
              <Link
                to={`/agents/${encodeURIComponent(rollup.agentNs)}/${encodeURIComponent(rollup.agentName)}?tab=Memory`}
                data-testid="trace-memory-link"
                className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground hover:underline"
              >
                <Brain className="h-3.5 w-3.5" />
                Memory
              </Link>
            </div>
          )}
        </div>

        <div className="flex flex-wrap items-start gap-4">
          <dl className="flex shrink-0 flex-wrap gap-x-6 gap-y-2 text-sm">
            <div className="flex flex-col items-end">
              <dt className="text-xs text-muted-foreground">Tokens</dt>
              <dd className="tabular-nums font-medium">
                {formatTokens(rollup.tokens)}
              </dd>
            </div>
            <div className="flex flex-col items-end">
              <dt className="text-xs text-muted-foreground">Cost</dt>
              <dd className="tabular-nums font-medium">{fmtCost(rollup.costUSD)}</dd>
            </div>
            <div className="flex flex-col items-end">
              <dt className="text-xs text-muted-foreground">Latency</dt>
              <dd className="tabular-nums font-medium">{Math.round(rollup.latencyMs)}ms</dd>
            </div>
            <div className="flex flex-col items-end">
              <dt className="text-xs text-muted-foreground">Spans</dt>
              <dd className="tabular-nums font-medium">{rollup.spanCount}</dd>
            </div>
          </dl>
          {/* Share button (m75.4) */}
          <Button
            variant="outline"
            size="sm"
            onClick={() => setShareOpen(true)}
            data-testid="trace-share-btn"
            className="shrink-0"
          >
            <Share2 className="h-3.5 w-3.5" />
            Share
          </Button>
        </div>
      </div>

      {/* ── Span tree ──────────────────────────────────────────────────────── */}
      <TraceExplorer spans={spans} />

      {/* ── Langfuse link-out — the ONE forensics escape hatch (m16.7) ─────── */}
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
        </div>
      )}

      {/* ── Feedback panel (m16.9) ─────────────────────────────────────────── */}
      <FeedbackPanel traceId={id} />

      {/* ── Add to dataset (m69.3, ADR 0062 Fork 5) ──────────────────────── */}
      <AddToDatasetPanel traceId={id} />

      {/* ── Share dialog (m75.4) ───────────────────────────────────────────── */}
      <ShareRunDialog
        open={shareOpen}
        onClose={() => setShareOpen(false)}
        runId={id}
      />
    </div>
  );
}
