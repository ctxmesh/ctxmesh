import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { buttonVariants } from "@/components/ui/button";
import type { RunSummary } from "@/lib/api";

// RecentRuns lists the latest traced runs (from the BFF's /api/runs, Langfuse-
// backed). Selecting a run opens its Langfuse deep-view (the embedded iframe +
// link-out). Token-driven throughout.

function formatUSD(n: number): string {
  return `$${n.toFixed(4)}`;
}

export function RecentRuns({
  runs,
  selectedTraceId,
  onSelect,
}: {
  runs: RunSummary[];
  selectedTraceId: string | null;
  onSelect: (traceId: string) => void;
}) {
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
    <div className="divide-y rounded-lg border bg-card">
      {runs.map((run) => {
        const active = run.traceId === selectedTraceId;
        return (
          <button
            key={run.traceId}
            type="button"
            onClick={() => onSelect(run.traceId)}
            className={cn(
              "flex w-full items-center justify-between gap-4 px-4 py-3 text-left transition-colors hover:bg-accent/60",
              active && "bg-accent",
            )}
          >
            <div className="min-w-0">
              <p className="truncate text-sm font-medium text-card-foreground">
                {run.name || "unnamed run"}
              </p>
              <p className="truncate font-mono text-xs text-muted-foreground">
                {run.traceId}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-4 text-xs text-muted-foreground">
              <span className="font-mono">{formatUSD(run.costUSD)}</span>
              <span className="font-mono">{run.tokens} tok</span>
              <span className="font-mono">{Math.round(run.latencyMs)}ms</span>
              {/* Visual affordance only — the whole row is the button, so this
                  is a styled span (a nested <button> is invalid HTML). */}
              <span className={buttonVariants({ variant: "outline", size: "sm" })}>
                View trace
              </span>
            </div>
          </button>
        );
      })}
    </div>
  );
}
