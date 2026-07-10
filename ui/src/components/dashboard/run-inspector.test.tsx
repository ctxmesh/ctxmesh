import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { RunInspector } from "@/components/dashboard/run-inspector";
import type { SpanSummary } from "@/lib/api";

// The run inspector builds the tree/waterfall CLIENT-side from the BFF's FLAT
// span list (parentId-linked). These tests prove: the tree nests a tool span
// under a generation with its tokens/cost visible (the aha 🧪), a redacted span
// shows the marker (never a blank/leak), and a status:error span shows the dot.

function span(over: Partial<SpanSummary>): SpanSummary {
  return {
    id: "s", parentId: "", type: "SPAN", name: "span", startMs: 0, durationMs: 100,
    model: "", tokensIn: 0, tokensOut: 0, costUSD: 0, level: "", status: "ok",
    input: "", output: "", inputRedacted: false, outputRedacted: false,
    ...over,
  };
}

// A run with: a root SPAN, a GENERATION child (llm), a tool SPAN nested under the
// generation (tokens/cost), a redacted tool span, and an error span.
const SPANS: SpanSummary[] = [
  span({ id: "root", parentId: "", type: "SPAN", name: "run: billing-agent", startMs: 0, durationMs: 1840 }),
  span({ id: "gen", parentId: "root", type: "GENERATION", name: "generation", startMs: 20, durationMs: 520, model: "claude-sonnet-4", tokensIn: 620, tokensOut: 240, costUSD: 0.003, input: '{"q":1}', output: '{"a":2}' }),
  span({ id: "tool", parentId: "gen", type: "SPAN", name: "tool: get_invoice", startMs: 560, durationMs: 180, tokensIn: 12, tokensOut: 4, costUSD: 0.0001, input: '{"id":4021}', output: '{"status":"shipped"}' }),
  span({ id: "redacted", parentId: "root", type: "SPAN", name: "tool: search_docs", startMs: 760, durationMs: 320, inputRedacted: true, outputRedacted: true }),
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
  it("builds the tree — a tool span nested under a generation, tokens/cost visible", async () => {
    stubTrace(SPANS);
    render(<RunInspector traceId="t1" />);
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

    // The tool span is the default selection (the aha) — its tokens + cost show.
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("12 in / 4 out");
    expect(detail).toHaveTextContent("$0.00010");
  });

  it("shows a generation's model + tokens when selected", async () => {
    stubTrace(SPANS);
    render(<RunInspector traceId="t1" />);
    fireEvent.click(await screen.findByTestId("span-row-gen"));
    const detail = screen.getByTestId("span-detail");
    expect(detail).toHaveTextContent("claude-sonnet-4");
    expect(detail).toHaveTextContent("620 in / 240 out");
  });

  it("renders a redacted marker over a redacted span — never a blank or a leak", async () => {
    stubTrace(SPANS);
    render(<RunInspector traceId="t1" />);
    fireEvent.click(await screen.findByTestId("span-row-redacted"));
    const detail = screen.getByTestId("span-detail");
    // The redacted markers are present; no raw input/output pre blocks.
    expect(within(detail).getByTestId("span-input-redacted-redacted")).toBeInTheDocument();
    expect(within(detail).getByTestId("span-output-redacted-redacted")).toBeInTheDocument();
    expect(within(detail).queryByTestId("span-input-redacted")).toBeNull();
  });

  it("shows the error dot for a status:error span", async () => {
    stubTrace(SPANS);
    render(<RunInspector traceId="t1" />);
    await screen.findByTestId("span-row-err");
    expect(screen.getByTestId("span-error-dot-err")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("span-row-err"));
    expect(within(screen.getByTestId("span-detail")).getByText("error")).toBeInTheDocument();
  });

  it("keeps Langfuse as a link-out (not the primary surface)", async () => {
    stubTrace(SPANS);
    render(<RunInspector traceId="t1" />);
    const link = await screen.findByTestId("open-in-langfuse");
    expect(link).toHaveAttribute("href", "https://lf/trace/t1");
    expect(link).toHaveAttribute("target", "_blank");
  });

  it("a 403 renders ForbiddenInline", async () => {
    stubTrace(SPANS, false, 403);
    render(<RunInspector traceId="t1" />);
    await waitFor(() =>
      expect(screen.getByText("Not allowed to read this run")).toBeInTheDocument(),
    );
  });
});
