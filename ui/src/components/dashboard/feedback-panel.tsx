import * as React from "react";
import { MessageSquare } from "lucide-react";

import { EmptyState, SkeletonTable } from "@/components/kit";
import { api, type FeedbackScore } from "@/lib/api";

// FeedbackPanel — per-trace feedback scores (m16.9).
//
// Fetches GET /api/feedback?traceId=<id> and renders the scores as a compact
// table (name / value / comment / source). Three states:
//   • loading    — SkeletonTable (honest loading state).
//   • null       — 501 or 502: Langfuse not wired / unreachable → calm disabled
//                  state (NOT an error toast, matching the m15.11 runs pattern).
//   • empty list — "no feedback recorded" teaching empty state.
//   • scores     — compact table row per score.
//
// The panel NEVER shows an error alert on a 501/502 — those are expected "not
// configured" states. Any other error (e.g. 500) renders a brief error notice.
//
// data-testid contract:
//   feedback-panel          — root container
//   feedback-score-{id}     — one score row

type FeedbackState =
  | { kind: "loading" }
  | { kind: "unavailable" } // 501 / 502 — Langfuse not wired
  | { kind: "error"; message: string }
  | { kind: "ready"; scores: FeedbackScore[] };

export function FeedbackPanel({ traceId }: { traceId: string }) {
  const [state, setState] = React.useState<FeedbackState>({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .feedback(traceId, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        if (res === null) {
          setState({ kind: "unavailable" });
        } else {
          setState({ kind: "ready", scores: res.scores });
        }
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load feedback",
        });
      });
    return () => controller.abort();
  }, [traceId]);

  return (
    <section
      className="rounded-lg border bg-card shadow-card"
      data-testid="feedback-panel"
      aria-label="Trace feedback"
    >
      <div className="border-b bg-surface-2/60 px-4 py-2">
        <h3 className="text-sm font-medium text-card-foreground">Feedback scores</h3>
      </div>

      <div className="p-4">
        {state.kind === "loading" && (
          <SkeletonTable rows={3} cols={4} />
        )}

        {state.kind === "unavailable" && (
          // Calm disabled state — NOT an error. The user needs context so they
          // know this is expected, not a bug (matches the m15.11 runs pattern).
          <div className="flex items-center gap-3 rounded-md border border-dashed bg-surface-2/40 px-4 py-3 text-sm text-muted-foreground">
            <MessageSquare className="h-4 w-4 shrink-0" aria-hidden />
            <span>
              Feedback scores are unavailable — Langfuse is not connected to this
              cluster. Contact your operator to enable tracing.
            </span>
          </div>
        )}

        {state.kind === "error" && (
          <div
            className="rounded-md border border-destructive/40 bg-destructive/5 px-4 py-3 text-sm text-destructive"
            role="alert"
          >
            Couldn't load feedback scores: {state.message}
          </div>
        )}

        {state.kind === "ready" && state.scores.length === 0 && (
          <EmptyState
            icon={MessageSquare}
            title="No feedback recorded"
            description="No scores have been submitted for this trace yet."
            intent="filtered"
            className="border-0 bg-transparent py-6"
          />
        )}

        {state.kind === "ready" && state.scores.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-xs text-muted-foreground">
                  <th className="pb-2 pr-4 font-medium">Name</th>
                  <th className="pb-2 pr-4 font-medium">Value</th>
                  <th className="pb-2 pr-4 font-medium">Comment</th>
                  <th className="pb-2 font-medium">Source</th>
                </tr>
              </thead>
              <tbody>
                {state.scores.map((score) => (
                  <tr
                    key={score.id}
                    data-testid={`feedback-score-${score.id}`}
                    className="border-b border-border/40 last:border-0"
                  >
                    <td className="py-2 pr-4 font-medium">{score.name}</td>
                    <td className="py-2 pr-4 tabular-nums">
                      {score.stringValue !== undefined
                        ? score.stringValue
                        : score.value !== undefined
                          ? String(score.value)
                          : "—"}
                    </td>
                    <td className="py-2 pr-4 text-muted-foreground">
                      {score.comment || "—"}
                    </td>
                    <td className="py-2 text-muted-foreground">
                      {score.source || "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}
