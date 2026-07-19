import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";
import { buttonVariants } from "@/components/ui/button";
import type { RunSummary } from "@/lib/api";
import {
  formatDateTime,
  formatLatency,
  formatRelativeTime,
  formatUSD,
  shortTraceId,
} from "@/lib/format";
import { cn } from "@/lib/utils";

// RecentRuns renders the latest traced runs (BFF /api/runs, Langfuse-backed) as a compact
// table: when the run happened (relative + absolute tooltip), which agent ran, its latency
// and cost, and the trace id (the metadata handle). Each row navigates to the native trace
// explorer (/traces/:id); "View all runs" leads to /runs. Fully token-driven.

// latencyTone maps a run latency (ms) to a semantic text tone so slow runs stand out.
function latencyTone(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "text-muted-foreground";
  if (ms < 2000) return "text-success";
  if (ms < 6000) return "text-warning";
  return "text-destructive";
}

// Column widths shared by the header and every row so they stay aligned.
const gridCols =
  "grid grid-cols-[7rem_minmax(0,1fr)_5rem_5.5rem_9rem_1.25rem] items-center gap-3";

export function RecentRuns({ runs }: { runs: RunSummary[] }) {
  if (runs.length === 0) {
    return (
      <Card>
        <CardContent className="py-8 text-center text-sm text-muted-foreground">
          No runs in the recent window.
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-2">
      <div className="overflow-hidden rounded-lg border bg-card">
        {/* Header */}
        <div
          className={cn(
            gridCols,
            "border-b bg-muted/40 px-4 py-2 text-[0.7rem] font-medium uppercase tracking-wide text-muted-foreground",
          )}
        >
          <span>When</span>
          <span>Agent</span>
          <span className="text-right">Latency</span>
          <span className="text-right">Cost</span>
          <span>Trace</span>
          <span className="sr-only">Open</span>
        </div>
        {/* Rows */}
        <div className="divide-y">
          {runs.map((run) => (
            <Link
              key={run.traceId}
              to={`/traces/${encodeURIComponent(run.traceId)}`}
              data-testid="run-row"
              className={cn(
                gridCols,
                "px-4 py-2.5 text-sm transition-colors hover:bg-accent/60",
              )}
            >
              <span
                className="truncate text-xs text-muted-foreground"
                title={formatDateTime(run.timestamp)}
              >
                {formatRelativeTime(run.timestamp) || "—"}
              </span>
              <span className="truncate font-medium text-card-foreground">
                {run.name || "unnamed run"}
              </span>
              <span
                className={cn(
                  "text-right font-mono text-xs",
                  latencyTone(run.latencyMs),
                )}
              >
                {formatLatency(run.latencyMs)}
              </span>
              <span className="text-right font-mono text-xs text-card-foreground">
                {formatUSD(run.costUSD)}
              </span>
              <span
                className="truncate font-mono text-xs text-muted-foreground"
                title={run.traceId}
              >
                {shortTraceId(run.traceId)}
              </span>
              <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
            </Link>
          ))}
        </div>
      </div>
      <div className="flex justify-end">
        <Link
          to="/runs"
          data-testid="view-all-runs"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          View all runs
        </Link>
      </div>
    </div>
  );
}
