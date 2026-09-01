import * as React from "react";
import { MessageSquare } from "lucide-react";

import { EmptyState, ErrorState, QuietNote, SkeletonTable } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { api, type FeedbackScore } from "@/lib/api";

// FeedbackPanel — per-trace feedback scores (m16.9), on the editorial panel
// register in M151 (spec §5.4 / §5.27 / §7).
//
// Fetches GET /api/feedback?traceId=<id> and renders the scores as a compact
// table (name / value / comment / source). Four states:
//   • loading    — SkeletonTable (honest loading state).
//   • null       — 501 ONLY: Langfuse not wired → a QuietNote (§5.27), the calm
//                  "this install cannot answer" register. NOT an error toast,
//                  NOT a zero (matching the m15.11 runs pattern).
//                  502 (Langfuse configured but upstream failed) throws → error.
//   • empty list — "no feedback recorded" teaching empty state.
//   • scores     — compact table row per score.
//
// The panel NEVER shows an error alert on a 501 — that is an expected "not
// configured" state. 502 and other errors render the kit ErrorState.
//
// ── TWO M151 FIXES ─────────────────────────────────────────────────────────
// 1. THE HEADER REGISTER. The panel wore a hand-rolled sans `<h3>` on a tinted
//    band while every other panel on the trace page is a Card + PanelHeader —
//    a serif title on a hairline rule (§5.4). One panel in a different voice
//    reads as a different system, so it now composes the primitive instead of
//    re-inventing a lighter version of it.
// 2. ATTRIBUTION IS IDENTITY, NOT BRAND. The attribution chip rendered
//    `bg-primary/10 text-primary` — pine, on a fact. Pine means "you can act
//    here, and this is us" and is NEVER a status or a taxonomy (§2.1). WHO
//    left a score is identity: it takes the neutral tag. The one distinction
//    worth drawing is the honest one — a score whose source the platform could
//    not attribute takes the dashed `open` tag, because "we do not know who"
//    and "a named source" must not look alike (§7.1).
//
// data-testid contract:
//   feedback-panel                — root container
//   feedback-score-{id}           — one score row
//   feedback-attribution-{id}     — the attributed-source tag

type FeedbackState =
  | { kind: "loading" }
  | { kind: "unavailable" } // 501 only — Langfuse not wired (502 throws → error)
  | { kind: "error"; message: string }
  | { kind: "ready"; scores: FeedbackScore[] };

// attributedSourceLabel renders the CRD-declared feedback source (M139, ADR 0112) as a friendly label:
// "human" → "Human", "external:<channel>" → "External · <channel>", "unattributed" → "Unattributed".
// Returns null when the agent binds no FeedbackStore (no attribution to show).
function attributedSourceLabel(s: string | undefined): string | null {
  if (!s) return null;
  if (s === "human") return "Human";
  if (s === "unattributed") return "Unattributed";
  if (s.startsWith("external:")) return `External · ${s.slice("external:".length)}`;
  return s;
}

/** Column heads in the §4.8 mono eyebrow register. */
const TH = "pb-2 pr-4 text-left font-mono text-2xs font-medium uppercase tracking-wide text-faint";

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

  const meta =
    state.kind === "ready" && state.scores.length > 0
      ? `${state.scores.length} score${state.scores.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <Card
      className="min-w-0"
      data-testid="feedback-panel"
      role="region"
      aria-label="Trace feedback"
    >
      <PanelHeader title="Feedback scores" meta={meta} />

      <CardContent>
        {state.kind === "loading" && <SkeletonTable rows={3} cols={4} />}

        {state.kind === "unavailable" && (
          // §7.1 verbatim pattern — calm, hueless, and explicitly NOT a zero.
          <QuietNote title="Feedback is unavailable on this install.">
            No trace backend is connected, so the scores a person or a webhook
            left against this run cannot be read. Wiring up Langfuse turns this
            panel on. Nothing here is estimated — the scores are simply absent.
          </QuietNote>
        )}

        {state.kind === "error" && (
          <ErrorState
            title="Couldn't load feedback scores"
            description="The run itself is unaffected — only its scores failed to read."
            detail={state.message}
            className="py-8"
          />
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
          // §4.6: the wide artifact scrolls inside its own container.
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border">
                  <th className={TH}>Name</th>
                  <th className={TH}>Value</th>
                  <th className={TH}>Comment</th>
                  <th className={TH + " pr-0"}>Source</th>
                </tr>
              </thead>
              <tbody>
                {state.scores.map((score) => {
                  const attribution = attributedSourceLabel(score.attributedSource);
                  return (
                    <tr
                      key={score.id}
                      data-testid={`feedback-score-${score.id}`}
                      className="border-b border-border-soft last:border-0"
                    >
                      <td className="py-2 pr-4 font-mono text-xs">{score.name}</td>
                      <td className="py-2 pr-4 font-mono text-xs tabular-nums">
                        {score.stringValue !== undefined
                          ? score.stringValue
                          : score.value !== undefined
                            ? String(score.value)
                            : // Unknown, not zero — the backend recorded no value.
                              <span
                                className="text-faint"
                                title="No value was recorded for this score."
                              >
                                —
                              </span>}
                      </td>
                      <td className="py-2 pr-4 text-secondary-foreground">
                        {score.comment || (
                          <span
                            className="font-mono text-faint"
                            title="No comment was left with this score."
                          >
                            —
                          </span>
                        )}
                      </td>
                      <td className="py-2 text-faint">
                        {attribution !== null ? (
                          <div className="flex flex-col items-start gap-0.5">
                            {/* Identity, never brand: neutral tag for a named
                                source, the dashed `open` tag when the platform
                                could not attribute it at all. */}
                            <Badge
                              data-testid={`feedback-attribution-${score.id}`}
                              variant={
                                score.attributedSource === "unattributed"
                                  ? "open"
                                  : "muted"
                              }
                            >
                              {attribution}
                            </Badge>
                            {score.source && (
                              <span className="font-mono text-2xs text-faint">
                                {score.source}
                              </span>
                            )}
                          </div>
                        ) : (
                          <span className="font-mono text-xs">
                            {score.source || (
                              <span title="This score carries no source.">—</span>
                            )}
                          </span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
