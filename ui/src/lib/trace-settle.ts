import * as React from "react";

import { api, type SpanSummary, type TraceDetailResponse } from "@/lib/api";

// A run's trace does not arrive in one piece: the trace backend ingests the launcher's root span first
// and the run's generation and tool spans seconds later. The first read of a fresh run's trace is
// often the root alone, so a reader that fetches once shows a partial tree as if it were the whole
// run. useSettlingTrace keeps reading, bounded, until the trace matches what the run recorded — or,
// with no run to compare against, until the span set stops changing — and says so when it gives up
// short.

/** How often a still-arriving trace is re-read. */
export const SETTLE_POLL_MS = 2500;
/**
 * How long after a run ends its spans are still expected. The bound is also measured from when the
 * reader opened the trace, so nothing is re-read for longer than this, and a trace that ended longer
 * ago than this is never re-read at all.
 */
export const SETTLE_WINDOW_MS = 60_000;
/** With no run to compare against, a span set unchanged for this long is taken as complete. */
export const SETTLE_STABLE_MS = 15_000;

export interface StepCounts {
  model: number;
  tool: number;
}

/**
 * The run's model and tool steps as the trace shows them: GENERATION spans are model calls, TOOL
 * spans are tool calls. Traces recorded before the backend typed tool calls carry them as plain
 * SPANs under the root, so those count as tools when no TOOL span exists (the inspector draws them
 * as tools too).
 */
export function countSpanSteps(spans: SpanSummary[]): StepCounts {
  const ids = new Set(spans.map((s) => s.id));
  let model = 0;
  let typedTools = 0;
  let legacyTools = 0;
  for (const s of spans) {
    if (s.type === "GENERATION") model += 1;
    else if (s.type === "TOOL") typedTools += 1;
    else if (s.type === "SPAN" && s.parentId !== "" && ids.has(s.parentId)) legacyTools += 1;
  }
  return { model, tool: typedTools > 0 ? typedTools : legacyTools };
}

export type SettleStatus =
  /** Still re-reading: more spans may land. */
  | { kind: "settling" }
  /** Matches the run's recorded steps, or stopped changing, or was never expected to change. */
  | { kind: "settled" }
  /** The bound ran out with fewer steps than the run recorded. */
  | { kind: "incomplete"; seen: StepCounts; recorded: StepCounts }
  /** The bound ran out while the trace was still changing, or while its run was still going. */
  | { kind: "unsettled" };

export interface SettleInput {
  detail: TraceDetailResponse;
  /** When the reader first received this trace (ms since epoch). */
  openedAt: number;
  now: number;
  /** When the span set last changed; openedAt until it does. */
  lastChangeAt: number;
}

export interface SettleDecision {
  status: SettleStatus;
  pollAgain: boolean;
}

function parseMs(iso: string | undefined): number | null {
  if (!iso) return null;
  const ms = Date.parse(iso);
  return Number.isNaN(ms) ? null : ms;
}

/**
 * The moment spans stop being expected to follow, before the window: when the run ended; for a run
 * still going, now; with no run, the end of the trace as recorded. null when nothing dates the trace,
 * so its age is unknown and it is not re-read.
 */
function settleAnchor(detail: TraceDetailResponse, openedAt: number): number | null {
  const run = detail.run;
  if (run) return parseMs(run.endedAt) ?? openedAt;
  const start = parseMs(detail.rollup.timestamp);
  return start === null ? null : start + Math.max(detail.rollup.latencyMs, 0);
}

/** decideSettle is the whole policy, pure, so it can be tested without timers. */
export function decideSettle({ detail, openedAt, now, lastChangeAt }: SettleInput): SettleDecision {
  const anchor = settleAnchor(detail, openedAt);
  const inWindow = anchor !== null && now < Math.min(anchor, openedAt) + SETTLE_WINDOW_MS;
  const run = detail.run;
  const recordedTotal = run ? run.modelSteps + run.toolSteps : 0;

  if (run?.endedAt && recordedTotal > 0) {
    const seen = countSpanSteps(detail.spans);
    const recorded = { model: run.modelSteps, tool: run.toolSteps };
    if (seen.model >= recorded.model && seen.tool >= recorded.tool) {
      return { status: { kind: "settled" }, pollAgain: false };
    }
    if (inWindow) return { status: { kind: "settling" }, pollAgain: true };
    return { status: { kind: "incomplete", seen, recorded }, pollAgain: false };
  }
  if (run && !run.endedAt) {
    // The run is still going, so its step count is not final yet.
    if (inWindow) return { status: { kind: "settling" }, pollAgain: true };
    return { status: { kind: "unsettled" }, pollAgain: false };
  }
  // Nothing to compare against: watch the span set settle.
  if (now - lastChangeAt >= SETTLE_STABLE_MS) return { status: { kind: "settled" }, pollAgain: false };
  if (inWindow) return { status: { kind: "settling" }, pollAgain: true };
  return {
    status: lastChangeAt > openedAt ? { kind: "unsettled" } : { kind: "settled" },
    pollAgain: false,
  };
}

function spanSetKey(spans: SpanSummary[]): string {
  return spans
    .map((s) => s.id)
    .sort()
    .join("\n");
}

export interface SettlingTrace {
  /** The newest read of the trace: `initial` until a re-read lands. */
  detail: TraceDetailResponse | null;
  status: SettleStatus | null;
}

/**
 * useSettlingTrace re-reads a trace the caller already loaded (`initial`) while its spans are still
 * arriving, under decideSettle's bound. A new `initial` (a new trace, or the caller's own retry)
 * starts the bound over.
 */
export function useSettlingTrace(traceId: string, initial: TraceDetailResponse | null): SettlingTrace {
  const [latest, setLatest] = React.useState<{
    initial: TraceDetailResponse;
    detail: TraceDetailResponse;
    status: SettleStatus;
  } | null>(null);

  React.useEffect(() => {
    if (!initial) return;
    const controller = new AbortController();
    const openedAt = Date.now();
    let lastChangeAt = openedAt;
    let lastKey = spanSetKey(initial.spans);
    let current = initial;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const settle = (detail: TraceDetailResponse) => {
      current = detail;
      const decision = decideSettle({ detail, openedAt, now: Date.now(), lastChangeAt });
      setLatest({ initial, detail, status: decision.status });
      if (decision.pollAgain) timer = setTimeout(reread, SETTLE_POLL_MS);
    };
    const reread = () => {
      api
        .traceDetail(traceId, controller.signal)
        .then((detail) => {
          if (controller.signal.aborted) return;
          const key = spanSetKey(detail.spans);
          if (key !== lastKey) {
            lastKey = key;
            lastChangeAt = Date.now();
          }
          settle(detail);
        })
        .catch(() => {
          if (controller.signal.aborted) return;
          // A failed re-read is a read that saw nothing new: the trace already on screen stays, and
          // the same bound decides whether to try again or report the trace as short.
          settle(current);
        });
    };
    settle(initial);

    return () => {
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [traceId, initial]);

  if (!initial) return { detail: null, status: null };
  if (!latest || latest.initial !== initial) return { detail: initial, status: null };
  return { detail: latest.detail, status: latest.status };
}

/** One plain sentence for a trace the reader stopped watching before it was whole; null otherwise. */
export function settleNotice(status: SettleStatus | null): string | null {
  if (!status) return null;
  if (status.kind === "incomplete") {
    const seen = status.seen.model + status.seen.tool;
    const recorded = status.recorded.model + status.recorded.tool;
    return `Spans may still be arriving: this trace shows ${seen} of the ${recorded} model and tool steps the run recorded. Reopen it in a moment to see the rest.`;
  }
  if (status.kind === "unsettled") {
    return "Spans may still be arriving: this trace was still changing when the console stopped checking. Reopen it in a moment to see the rest.";
  }
  return null;
}
