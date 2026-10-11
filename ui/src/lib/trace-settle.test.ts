import { describe, expect, it } from "vitest";

import type { SpanSummary, TraceDetailResponse, TraceRunSteps } from "@/lib/api";
import {
  countSpanSteps,
  decideSettle,
  SETTLE_STABLE_MS,
  SETTLE_WINDOW_MS,
  settleNotice,
} from "@/lib/trace-settle";

function span(over: Partial<SpanSummary>): SpanSummary {
  return {
    id: "s", parentId: "", type: "SPAN", name: "span", startMs: 0, durationMs: 10,
    model: "", tokensIn: 0, tokensOut: 0, costUSD: 0, level: "", status: "ok",
    input: "", output: "", inputRedacted: false, outputRedacted: false,
    ...over,
  };
}

const ROOT = span({ id: "root", name: "agent.invoke" });
const GEN1 = span({ id: "gen1", parentId: "root", type: "GENERATION" });
const TOOL = span({ id: "tool", parentId: "gen1", type: "TOOL", name: "echo" });
const GEN2 = span({ id: "gen2", parentId: "root", type: "GENERATION" });

const T0 = Date.parse("2026-10-11T12:00:00Z");

function trace(spans: SpanSummary[], over: Partial<TraceDetailResponse> = {}): TraceDetailResponse {
  return {
    rollup: { traceId: "t1", name: "default/aha-agent", timestamp: "", costUSD: 0, tokens: 0, latencyMs: 0, spanCount: spans.length },
    spans,
    ...over,
  };
}

function ranAndEnded(endedAt: number, modelSteps = 2, toolSteps = 1): TraceRunSteps {
  return { runId: "r1", status: "succeeded", endedAt: new Date(endedAt).toISOString(), modelSteps, toolSteps };
}

describe("countSpanSteps", () => {
  it("counts generations as model steps and TOOL spans as tool steps", () => {
    expect(countSpanSteps([ROOT, GEN1, TOOL, GEN2])).toEqual({ model: 2, tool: 1 });
  });

  it("counts a legacy SPAN-typed tool under the root only when no span is typed TOOL", () => {
    const legacy = span({ id: "legacy", parentId: "gen1", type: "SPAN", name: "echo" });
    expect(countSpanSteps([ROOT, GEN1, legacy])).toEqual({ model: 1, tool: 1 });
    expect(countSpanSteps([ROOT, GEN1, legacy, TOOL])).toEqual({ model: 1, tool: 1 });
    expect(countSpanSteps([ROOT])).toEqual({ model: 0, tool: 0 });
  });
});

describe("decideSettle with the run's recorded steps", () => {
  const ended = T0 - 1_000;

  it("re-reads a fresh trace that is short of the run's steps", () => {
    const d = decideSettle({ detail: trace([ROOT], { run: ranAndEnded(ended) }), openedAt: T0, now: T0, lastChangeAt: T0 });
    expect(d).toEqual({ status: { kind: "settling" }, pollAgain: true });
  });

  it("stops as soon as the trace has every recorded step", () => {
    const d = decideSettle({ detail: trace([ROOT, GEN1, TOOL, GEN2], { run: ranAndEnded(ended) }), openedAt: T0, now: T0 + 5_000, lastChangeAt: T0 + 5_000 });
    expect(d).toEqual({ status: { kind: "settled" }, pollAgain: false });
  });

  it("stops at the bound and reports what is missing", () => {
    const now = ended + SETTLE_WINDOW_MS;
    const d = decideSettle({ detail: trace([ROOT, GEN1], { run: ranAndEnded(ended) }), openedAt: T0, now, lastChangeAt: T0 });
    expect(d.pollAgain).toBe(false);
    expect(d.status).toEqual({ kind: "incomplete", seen: { model: 1, tool: 0 }, recorded: { model: 2, tool: 1 } });
    expect(settleNotice(d.status)).toMatch(/Spans may still be arriving: this trace shows 1 of the 3 model and tool steps/);
  });

  it("never re-reads a trace whose run ended longer ago than the window", () => {
    const long = T0 - SETTLE_WINDOW_MS - 1;
    const d = decideSettle({ detail: trace([ROOT], { run: ranAndEnded(long) }), openedAt: T0, now: T0, lastChangeAt: T0 });
    expect(d.pollAgain).toBe(false);
    expect(d.status.kind).toBe("incomplete");
  });

  it("bounds the re-reads from the moment the reader opened the trace too", () => {
    // A run ending after the reader opened it (clock skew, or still finishing) gets no more than
    // the window from the open.
    const d = decideSettle({ detail: trace([ROOT], { run: ranAndEnded(T0 + 10 * SETTLE_WINDOW_MS) }), openedAt: T0, now: T0 + SETTLE_WINDOW_MS, lastChangeAt: T0 });
    expect(d.pollAgain).toBe(false);
  });

  it("keeps reading while the run is still going, within the window, and then says it gave up", () => {
    const live: TraceRunSteps = { runId: "r1", status: "running", modelSteps: 0, toolSteps: 0 };
    expect(decideSettle({ detail: trace([ROOT], { run: live }), openedAt: T0, now: T0 + 30_000, lastChangeAt: T0 }).pollAgain).toBe(true);
    const late = decideSettle({ detail: trace([ROOT], { run: live }), openedAt: T0, now: T0 + SETTLE_WINDOW_MS, lastChangeAt: T0 });
    expect(late).toEqual({ status: { kind: "unsettled" }, pollAgain: false });
  });
});

describe("decideSettle with no run to compare against", () => {
  const fresh = trace([ROOT], { rollup: { traceId: "t1", name: "x", timestamp: new Date(T0 - 2_000).toISOString(), costUSD: 0, tokens: 0, latencyMs: 1_000, spanCount: 1 } });

  it("watches a fresh trace until its span set stops changing", () => {
    expect(decideSettle({ detail: fresh, openedAt: T0, now: T0, lastChangeAt: T0 }).pollAgain).toBe(true);
    const stable = decideSettle({ detail: fresh, openedAt: T0, now: T0 + SETTLE_STABLE_MS + 2_500, lastChangeAt: T0 + 2_500 });
    expect(stable).toEqual({ status: { kind: "settled" }, pollAgain: false });
  });

  it("says so when the window closes on a trace that was still changing", () => {
    const now = T0 - 1_000 + SETTLE_WINDOW_MS;
    const d = decideSettle({ detail: fresh, openedAt: T0, now, lastChangeAt: now - 1_000 });
    expect(d).toEqual({ status: { kind: "unsettled" }, pollAgain: false });
    expect(settleNotice(d.status)).toMatch(/still changing when the console stopped checking/);
  });

  it("never re-reads an old trace, or one nothing dates", () => {
    const old = trace([ROOT], { rollup: { ...fresh.rollup, timestamp: new Date(T0 - 10 * 60_000).toISOString() } });
    expect(decideSettle({ detail: old, openedAt: T0, now: T0, lastChangeAt: T0 })).toEqual({ status: { kind: "settled" }, pollAgain: false });
    expect(decideSettle({ detail: trace([ROOT]), openedAt: T0, now: T0, lastChangeAt: T0 })).toEqual({ status: { kind: "settled" }, pollAgain: false });
  });

  it("treats a terminal run that recorded no steps the same way", () => {
    const noSteps = trace([ROOT], { run: ranAndEnded(T0 - 1_000, 0, 0) });
    expect(decideSettle({ detail: noSteps, openedAt: T0, now: T0, lastChangeAt: T0 }).pollAgain).toBe(true);
    expect(decideSettle({ detail: noSteps, openedAt: T0, now: T0 + SETTLE_STABLE_MS, lastChangeAt: T0 }).pollAgain).toBe(false);
  });
});
