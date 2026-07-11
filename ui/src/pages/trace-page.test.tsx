import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { TracePage } from "@/pages/trace-page";
import type { SpanSummary } from "@/lib/api";

// TracePage — m16.7 tests.
//
// Coverage:
//   • header shows agent name, timestamp, total tokens and cost from the rollup.
//   • TraceExplorer renders (span rows present).
//   • Langfuse link-out button is present and opens the resolved URL in a new tab.
//   • loading state renders skeleton (no trace-page).
//   • error state renders an error notice.
//   • 403 renders ForbiddenInline (not an error toast).

function span(over: Partial<SpanSummary>): SpanSummary {
  return {
    id: "s", parentId: "", type: "SPAN", name: "span",
    startMs: 0, durationMs: 100, model: "", tokensIn: 0, tokensOut: 0,
    costUSD: 0, level: "", status: "ok", input: "", output: "",
    inputRedacted: false, outputRedacted: false, nestingDepth: 0,
    ...over,
  };
}

const SPANS: SpanSummary[] = [
  span({ id: "root", name: "run: billing-agent", startMs: 0, durationMs: 1840, nestingDepth: 0 }),
  span({ id: "gen",  parentId: "root", type: "GENERATION", name: "generate", startMs: 20, durationMs: 520, nestingDepth: 1, tokensIn: 620, tokensOut: 240, costUSD: 0.003 }),
];

const ROLLUP = {
  traceId: "t1",
  name: "billing-agent",
  timestamp: "2026-07-11T12:00:00Z",
  costUSD: 0.003,
  tokens: 860,
  latencyMs: 1840,
  spanCount: 2,
};

function installFetch(opts: {
  detailOk?: boolean;
  detailStatus?: number;
  linkOk?: boolean;
  feedbackOk?: boolean;
  feedbackStatus?: number;
} = {}) {
  const {
    detailOk = true, detailStatus = 200,
    linkOk = true,
    feedbackOk = true, feedbackStatus = 200,
  } = opts;

  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();

      if (url.includes("/detail")) {
        return Promise.resolve({
          ok: detailOk, status: detailStatus,
          json: async () => ({ rollup: ROLLUP, spans: SPANS }),
          text: async () => JSON.stringify({ error: "forbidden" }),
        } as Response);
      }
      if (url.includes("/feedback")) {
        return Promise.resolve({
          ok: feedbackOk, status: feedbackStatus,
          json: async () => ({ scores: [] }),
          text: async () => "{}",
        } as Response);
      }
      // Langfuse link (GET /api/traces/{id}).
      return Promise.resolve({
        ok: linkOk, status: linkOk ? 200 : 404,
        json: async () => ({ traceId: "t1", url: "https://lf/trace/t1" }),
        text: async () => "{}",
      } as Response);
    }),
  );
}

function renderPage(traceId = "t1") {
  return render(
    <MemoryRouter initialEntries={[`/traces/${traceId}`]}>
      <Routes>
        <Route path="/traces/:id" element={<TracePage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("TracePage (m16.7)", () => {
  it("renders the header with agent name, timestamp, tokens, and cost", async () => {
    installFetch();
    renderPage();

    const header = await screen.findByTestId("trace-header");
    expect(header).toHaveTextContent("billing-agent");
    // Timestamp rendered (exact format depends on locale — just check it's present)
    expect(header).toHaveTextContent("2026");
    // Tokens from rollup.tokens = 860
    expect(header).toHaveTextContent("860");
    // Cost from rollup.costUSD = 0.003
    expect(header).toHaveTextContent("$0.003");
  });

  it("renders TraceExplorer with span rows", async () => {
    installFetch();
    renderPage();

    // trace-explorer wraps the span rows.
    await screen.findByTestId("trace-explorer");
    expect(screen.getByTestId("span-row-root")).toBeInTheDocument();
    expect(screen.getByTestId("span-row-gen")).toBeInTheDocument();
  });

  it("renders the Langfuse forensics link-out button pointing to the resolved URL", async () => {
    installFetch();
    renderPage();

    const linkout = await screen.findByTestId("trace-langfuse-linkout");
    expect(linkout).toHaveAttribute("href", "https://lf/trace/t1");
    expect(linkout).toHaveAttribute("target", "_blank");
  });

  it("does NOT render the link-out when Langfuse link resolution fails", async () => {
    installFetch({ linkOk: false });
    renderPage();

    // Wait for the page to fully load (trace-header appears).
    await screen.findByTestId("trace-header");
    expect(screen.queryByTestId("trace-langfuse-linkout")).toBeNull();
  });

  it("shows the loading skeleton before the data arrives", () => {
    // Don't resolve the fetch so we catch the transient loading state.
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    renderPage();
    // trace-page should NOT be present during loading.
    expect(screen.queryByTestId("trace-page")).toBeNull();
    // A skeleton-style container should be visible (role=status).
    expect(screen.getAllByRole("status").length).toBeGreaterThan(0);
  });

  it("renders an error notice when detail fetch fails", async () => {
    installFetch({ detailOk: false, detailStatus: 500 });
    renderPage();

    await waitFor(() =>
      expect(screen.getByTestId("trace-page-error")).toBeInTheDocument(),
    );
  });

  it("renders ForbiddenInline on a 403", async () => {
    installFetch({ detailOk: false, detailStatus: 403 });
    renderPage();

    await waitFor(() =>
      expect(screen.getByText("Not allowed to read this trace")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("trace-page")).toBeNull();
  });

  it("includes the FeedbackPanel (feedback-panel testid)", async () => {
    installFetch();
    renderPage();

    await screen.findByTestId("trace-page");
    expect(screen.getByTestId("feedback-panel")).toBeInTheDocument();
  });
});
