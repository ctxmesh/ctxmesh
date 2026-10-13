import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { RunInspector } from "@/components/dashboard/run-inspector";
import type { SpanSummary, TraceDetailResponse, TraceRunSteps } from "@/lib/api";
import { SETTLE_POLL_MS, SETTLE_WINDOW_MS } from "@/lib/trace-settle";

// The run inspector builds the tree/waterfall CLIENT-side from the BFF's FLAT
// span list (parentId-linked). These tests prove: the tree nests a tool call
// under the generation that made it, the tool call is the default selection, a
// redacted span shows the marker (never a blank/leak), and a status:error span
// shows the dot.
//
// The fixture has the shape Langfuse returns for a platform run: the model call
// is a GENERATION carrying the token usage, the tool call is a TOOL observation
// with none of its own.

function span(over: Partial<SpanSummary>): SpanSummary {
  return {
    id: "s", parentId: "", type: "SPAN", name: "span", startMs: 0, durationMs: 100,
    model: "", tokensIn: 0, tokensOut: 0, costUSD: 0, level: "", status: "ok",
    input: "", output: "", inputRedacted: false, outputRedacted: false,
    ...over,
  };
}

// A run with: a root SPAN, a GENERATION child (the model call, with tokens/cost),
// a TOOL call nested under the generation (no tokens of its own), a redacted
// tool call, and an error span.
const SPANS: SpanSummary[] = [
  span({ id: "root", parentId: "", type: "SPAN", name: "run: billing-agent", startMs: 0, durationMs: 1840 }),
  span({ id: "gen", parentId: "root", type: "GENERATION", name: "generation", startMs: 20, durationMs: 520, model: "claude-sonnet-4", tokensIn: 620, tokensOut: 240, costUSD: 0.003, input: '{"q":1}', output: '{"a":2}' }),
  span({ id: "tool", parentId: "gen", type: "TOOL", name: "get_invoice", startMs: 560, durationMs: 180, input: '{"id":4021}', output: '{"status":"shipped"}' }),
  span({ id: "redacted", parentId: "root", type: "TOOL", name: "search_docs", startMs: 760, durationMs: 320, inputRedacted: true, outputRedacted: true }),
  span({ id: "err", parentId: "root", type: "SPAN", name: "tool: broken", startMs: 1100, durationMs: 40, level: "ERROR", status: "error" }),
];

function stubTrace(spans: SpanSummary[], ok = true, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/detail")) {
        return Promise.resolve({
          ok, status,
          json: async () => ({
            rollup: { traceId: "t1", name: "billing-agent", timestamp: "", costUSD: 0.0061, tokens: 876, latencyMs: 1840, spanCount: spans.length },
            spans,
          }),
          text: async () => JSON.stringify({ error: "forbidden" }),
        } as Response);
      }
      // The Langfuse link-out (GET /api/traces/{id}) — best-effort.
      return Promise.resolve({ ok: true, status: 200, json: async () => ({ traceId: "t1", url: "https://lf/trace/t1" }) } as Response);
    }),
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("RunInspector (native summary from flat spans)", () => {
  it("builds the tree — a tool call nested under the generation that made it", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    // The tool span row renders.
    const toolRow = await screen.findByTestId("span-row-tool");
    expect(toolRow).toHaveTextContent("get_invoice");
    // It is INDENTED deeper than its parent generation (depth 2 vs 1) — the tree
    // was built from parentId (root → gen → tool). The indent lives on the label
    // container (the padded flex span, first child of each row button).
    const padOf = (row: HTMLElement) => {
      const label = row.querySelector<HTMLElement>("span[style]");
      return parseInt((label?.style.paddingLeft || "0").replace("px", ""), 10);
    };
    expect(padOf(toolRow)).toBeGreaterThan(padOf(screen.getByTestId("span-row-gen")));

    // The tool call is the default selection (the aha): its kind, input and
    // output show. It reports no token usage, so there is no Tokens row to fake.
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("get_invoice");
    expect(detail).toHaveTextContent("tool");
    expect(detail).toHaveTextContent('{"status":"shipped"}');
    expect(within(detail).queryByText("Tokens")).toBeNull();
  });

  it("classifies a TOOL observation as a tool and selects it over a SPAN-typed fallback", async () => {
    // A SPAN that the legacy heuristic would also call a tool starts FIRST; the
    // real tool call (TOOL) must still be the default selection.
    stubTrace([
      span({ id: "root", parentId: "", type: "SPAN", name: "run", startMs: 0, durationMs: 900 }),
      span({ id: "step", parentId: "root", type: "SPAN", name: "plan", startMs: 10, durationMs: 50 }),
      span({ id: "gen", parentId: "root", type: "GENERATION", name: "generation", startMs: 100, durationMs: 300, model: "claude-haiku-4-5", tokensIn: 410, tokensOut: 37, costUSD: 0.0004 }),
      span({ id: "wc", parentId: "root", type: "TOOL", name: "word-count", startMs: 420, durationMs: 12, input: '{"text":"a b c"}', output: "3" }),
    ]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    await screen.findByTestId("span-row-wc");
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("word-count");
    expect(within(detail).getByText("tool")).toBeInTheDocument();
    expect(within(detail).queryByText("Tokens")).toBeNull();
  });

  it("shows Tokens on the generation row, where the usage is", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    fireEvent.click(await screen.findByTestId("span-row-gen"));
    const detail = screen.getByTestId("span-detail");
    expect(within(detail).getByText("Tokens")).toBeInTheDocument();
    expect(detail).toHaveTextContent("620 in / 240 out");
    expect(detail).toHaveTextContent("$0.003");
  });

  it("shows a generation's model + tokens when selected", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    fireEvent.click(await screen.findByTestId("span-row-gen"));
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("claude-sonnet-4");
    expect(detail).toHaveTextContent("620 in / 240 out");
  });

  it("renders a redacted marker over a redacted span — never a blank or a leak", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    fireEvent.click(await screen.findByTestId("span-row-redacted"));
    const detail = screen.getByTestId("span-detail");
    // The redacted markers are present; no raw input/output pre blocks.
    expect(within(detail).getByTestId("span-input-redacted-redacted")).toBeInTheDocument();
    expect(within(detail).getByTestId("span-output-redacted-redacted")).toBeInTheDocument();
    expect(within(detail).queryByTestId("span-input-redacted")).toBeNull();
  });

  it("shows the error dot for a status:error span", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    await screen.findByTestId("span-row-err");
    expect(screen.getByTestId("span-error-dot-err")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("span-row-err"));
    expect(within(screen.getByTestId("span-detail")).getByText("error")).toBeInTheDocument();
  });

  it("keeps Langfuse as a link-out (not the primary surface)", async () => {
    stubTrace(SPANS);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    const link = await screen.findByTestId("open-in-langfuse");
    expect(link).toHaveAttribute("href", "https://lf/trace/t1");
    expect(link).toHaveAttribute("target", "_blank");
  });

  it("a 403 renders ForbiddenInline", async () => {
    stubTrace(SPANS, false, 403);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    await waitFor(() =>
      expect(screen.getByText("Not allowed to read this run")).toBeInTheDocument(),
    );
  });

  it("shows the calm 'ingesting' state on a 404 (Langfuse lag), not a hard error", async () => {
    // A freshly-completed run 404s until Langfuse ingests it (~20s). The inspector
    // polls and shows a calm "still landing" state, never "couldn't load" (m18.7).
    stubTrace([], false, 404);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    expect(await screen.findByTestId("run-inspector-ingesting")).toBeInTheDocument();
    expect(screen.queryByText(/couldn't load the run/i)).not.toBeInTheDocument();
  });

  it("degrades calmly on a 501 (no trace backend), never a hard error (m22.2/U1)", async () => {
    // 501 = the Langfuse trace adapter isn't wired — an unwired optional
    // integration is NOT a failure. Show a calm "not configured" state; the run's
    // result is still shown by the Run panel (spec console-usability).
    stubTrace([], false, 501);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);
    expect(await screen.findByTestId("run-inspector-unconfigured")).toBeInTheDocument();
    expect(screen.queryByText(/couldn't load the run/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/not implemented/i)).not.toBeInTheDocument();
  });
});

// A run's trace lands in pieces: the backend has the launcher's root span first and the generation
// and tool spans seconds later. The inspector re-reads it, bounded, until it matches the steps the
// run recorded (nightly 38144932219: it read once, showed one span, and never looked again).
describe("RunInspector — a trace that is still arriving", () => {
  const ROOT = span({ id: "root", type: "SPAN", name: "agent.invoke", durationMs: 1200 });
  const GEN = span({ id: "gen", parentId: "root", type: "GENERATION", name: "generation", startMs: 10, durationMs: 400, model: "m", tokensIn: 189, tokensOut: 28 });
  const TOOL = span({ id: "tool", parentId: "gen", type: "TOOL", name: "echo", startMs: 420, durationMs: 5, output: "ping" });

  function ended(agoMs: number, modelSteps = 1, toolSteps = 1): TraceRunSteps {
    return { runId: "r1", status: "succeeded", endedAt: new Date(Date.now() - agoMs).toISOString(), modelSteps, toolSteps };
  }

  // Serves the /detail reads in order (the last one repeats) and counts them.
  function stubReads(reads: (() => TraceDetailResponse)[]) {
    const count = { detail: 0 };
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.includes("/detail")) {
          const body = reads[Math.min(count.detail, reads.length - 1)]();
          count.detail += 1;
          return Promise.resolve({ ok: true, status: 200, json: async () => body } as Response);
        }
        return Promise.resolve({ ok: true, status: 200, json: async () => ({ traceId: "t1", url: "https://lf/trace/t1" }) } as Response);
      }),
    );
    return count;
  }

  function detailOf(spans: SpanSummary[], run?: TraceRunSteps, timestamp = ""): TraceDetailResponse {
    return {
      rollup: { traceId: "t1", name: "default/aha-agent", timestamp, costUSD: 0, tokens: 0, latencyMs: 1200, spanCount: spans.length },
      spans,
      run,
    };
  }

  const advance = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("re-reads a trace that grows from one span to three and lands on the tool span", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const run = ended(1_000);
    const count = stubReads([
      () => detailOf([ROOT], run),
      () => detailOf([ROOT, GEN], run),
      () => detailOf([ROOT, GEN, TOOL], run),
    ]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    await screen.findByTestId("span-row-root");
    expect(await screen.findByTestId("run-inspector-settling")).toBeInTheDocument();
    await advance(SETTLE_POLL_MS);
    await screen.findByTestId("span-row-gen");
    await advance(SETTLE_POLL_MS);
    await screen.findByTestId("span-row-tool");

    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("echo");
    expect(detail).toHaveTextContent("tool");
    expect(screen.getByTestId("run-inspector-title")).toHaveTextContent("3 spans");
    expect(screen.queryByTestId("run-inspector-settling")).toBeNull();
    expect(screen.queryByTestId("run-inspector-incomplete")).toBeNull();
    expect(count.detail).toBe(3);
  });

  it("stops re-reading as soon as the trace matches the run's recorded steps", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const run = ended(1_000);
    const count = stubReads([() => detailOf([ROOT], run), () => detailOf([ROOT, GEN, TOOL], run)]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    await screen.findByTestId("span-row-root");
    await advance(SETTLE_POLL_MS);
    await screen.findByTestId("span-row-tool");
    await advance(10 * SETTLE_POLL_MS);
    expect(count.detail).toBe(2);
  });

  it("stops at the bound and says the spans may still be arriving", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const run = ended(1_000);
    const count = stubReads([() => detailOf([ROOT], run)]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    await screen.findByTestId("span-row-root");
    expect(screen.queryByTestId("run-inspector-incomplete")).toBeNull();
    await advance(SETTLE_WINDOW_MS);
    const notice = await screen.findByTestId("run-inspector-incomplete");
    expect(notice).toHaveTextContent("Spans may still be arriving");
    expect(notice).toHaveTextContent("0 of the 2 model and tool steps the run recorded");
    expect(screen.queryByTestId("run-inspector-settling")).toBeNull();

    const atBound = count.detail;
    expect(atBound).toBeLessThanOrEqual(SETTLE_WINDOW_MS / SETTLE_POLL_MS + 1);
    await advance(SETTLE_WINDOW_MS);
    expect(count.detail).toBe(atBound);
  });

  it("never re-reads an old trace, and says plainly when it is short", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const count = stubReads([() => detailOf([ROOT], ended(10 * 60_000))]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    await screen.findByTestId("span-row-root");
    expect(await screen.findByTestId("run-inspector-incomplete")).toBeInTheDocument();
    await advance(5 * SETTLE_POLL_MS);
    expect(count.detail).toBe(1);
  });

  it("never re-reads an old trace that no run maps", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const old = new Date(Date.now() - 10 * 60_000).toISOString();
    const count = stubReads([() => detailOf([ROOT, GEN], undefined, old)]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    await screen.findByTestId("span-row-gen");
    await advance(5 * SETTLE_POLL_MS);
    expect(count.detail).toBe(1);
    expect(screen.queryByTestId("run-inspector-incomplete")).toBeNull();
  });

  it("keeps the span the user picked when new spans land", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const run = ended(1_000);
    const count = stubReads([() => detailOf([ROOT, GEN], run), () => detailOf([ROOT, GEN, TOOL], run)]);
    render(<MemoryRouter><RunInspector traceId="t1" /></MemoryRouter>);

    fireEvent.click(await screen.findByTestId("span-row-gen"));
    expect(screen.getByTestId("span-detail")).toHaveTextContent("189 in / 28 out");

    await advance(SETTLE_POLL_MS);
    await screen.findByTestId("span-row-tool");
    expect(count.detail).toBe(2);
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("generation");
    expect(detail).toHaveTextContent("189 in / 28 out");
    await waitFor(() => expect(screen.getByTestId("span-row-gen").className).toContain("bg-accent"));
  });
});
