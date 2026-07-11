import * as React from "react";
import { useParams } from "react-router-dom";
import { ExternalLink } from "lucide-react";

import { buttonVariants } from "@/components/ui/button";
import { ForbiddenInline, SkeletonCard } from "@/components/kit";
import { TraceExplorer } from "@/components/dashboard/trace-explorer";
import { FeedbackPanel } from "@/components/dashboard/feedback-panel";
import { api, ApiError, type TraceDetailResponse } from "@/lib/api";

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
//   • FeedbackPanel: per-trace scores (m16.9, calm 501/502 degrade).
//
// data-testid contract:
//   trace-page              — root container
//   trace-header            — the header block (name / timestamp / totals)
//   trace-langfuse-linkout  — the link-out button/anchor

type PageState =
  | { kind: "loading" }
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
        </div>

        <dl className="flex shrink-0 flex-wrap gap-x-6 gap-y-2 text-sm">
          <div className="flex flex-col items-end">
            <dt className="text-xs text-muted-foreground">Tokens</dt>
            <dd className="tabular-nums font-medium">
              {rollup.tokens.toLocaleString()}
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
    </div>
  );
}
