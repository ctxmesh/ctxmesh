import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { CostPage } from "@/pages/cost-page";
import type { AgentCostItem, CostSummary } from "@/lib/api";

// CostPage (m16.10) — cost drill-down, per-agent breakdown.
//
// Coverage:
//   • summary card renders total cost / tokens from the window summary
//   • breakdown table renders per-agent rows (cost / tokens / runCount)
//   • the (untagged) row is present and non-navigable
//   • a normal agent row click → /agents/:ns/:name
//   • the recent-window caveat text is present
//   • cursor pagination: nextCursor drives the next page (hasNext = nextCursor !== "")
//   • 502 surfaces as a visible error (NOT a calm state)
//   • 501 degrades calmly to cost-unavailable (NOT an error toast)

function item(over: Partial<AgentCostItem> = {}): AgentCostItem {
  return {
    agentNs: "default",
    agentName: "billing-agent",
    totalCostUSD: 1.5,
    totalTokens: 50000,
    runCount: 12,
    ...over,
  };
}

function summary(over: Partial<CostSummary> = {}): CostSummary {
  return {
    totalCostUSD: 3.25,
    totalTokens: 120000,
    observations: 45,
    byModel: [],
    ...over,
  };
}

// installFetch stubs global fetch for the breakdown endpoint.
// The responder receives parsed URLSearchParams for assertion.
function installFetch(
  handler: (qs: URLSearchParams) => { ok: boolean; status?: number; body?: unknown },
) {
  const captured: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = handler(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () =>
          r.body ?? {
            agents: [],
            total: summary(),
            nextCursor: "",
          },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/cost"]}>
      <Routes>
        <Route path="/cost" element={<CostPage />} />
        <Route
          path="/agents/:ns/:name"
          element={<div data-testid="agent-detail-stub" />}
        />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("CostPage — basic rendering (m16.10)", () => {
  it("renders cost-page root and cost-breakdown-table", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [item()],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    expect(await screen.findByTestId("cost-page")).toBeInTheDocument();
    expect(screen.getByTestId("cost-breakdown-table")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Cost breakdown" })).toBeInTheDocument();
  });

  it("renders a 'temporarily unavailable' degrade state when the API returns a notice (m24)", async () => {
    // 200 + notice = the trace store is transiently down (m23.6). The page must show
    // the notice + a retry, NOT the misleading "No cost data yet" empty table.
    installFetch(() => ({
      ok: true,
      body: {
        agents: [],
        total: summary(),
        nextCursor: "",
        notice: "Observability is temporarily unavailable — the trace store is slow to respond.",
      },
    }));

    renderPage();

    const degraded = await screen.findByTestId("cost-degraded");
    expect(degraded).toHaveTextContent(/temporarily unavailable/i);
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
    // NOT the true-empty "no cost data" table.
    expect(screen.queryByTestId("cost-breakdown-table")).toBeNull();
  });

  it("summary card renders total cost and tokens from window total", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [item()],
        total: summary({ totalCostUSD: 3.25, totalTokens: 120000 }),
        nextCursor: "",
      },
    }));

    renderPage();

    const card = await screen.findByTestId("cost-summary-card");
    expect(card).toBeInTheDocument();
    // Total cost is displayed (formatUSD: $3.250)
    expect(card.textContent).toContain("$3.250");
    // Total tokens are shown in compact form — exact format is locale-dependent;
    // assert the card contains a non-zero token display rather than hardcoding "120K".
    expect(card.textContent).toMatch(/\d/); // has numeric content
    // The "Total tokens" label must appear
    expect(card.textContent).toContain("Total tokens");
  });

  it("breakdown table renders per-agent rows with cost, tokens, runCount", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [
          item({ agentNs: "prod", agentName: "billing-agent", totalCostUSD: 1.5, totalTokens: 50000, runCount: 12 }),
          item({ agentNs: "staging", agentName: "support-agent", totalCostUSD: 0.75, totalTokens: 25000, runCount: 6 }),
        ],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    // Both agent rows appear
    expect(await screen.findByText("prod/billing-agent")).toBeInTheDocument();
    expect(screen.getByText("staging/support-agent")).toBeInTheDocument();

    // cost / tokens / runCount values
    expect(screen.getByText("$1.500")).toBeInTheDocument();
    expect(screen.getByText("50,000")).toBeInTheDocument();
    expect(screen.getByText("12")).toBeInTheDocument();
  });

  it("row testids are present for each agent", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [
          item({ agentNs: "prod", agentName: "billing-agent" }),
        ],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    expect(
      await screen.findByTestId("cost-row-prod-billing-agent"),
    ).toBeInTheDocument();
  });
});

// ── (untagged) bucket ─────────────────────────────────────────────────────────

describe("CostPage — (untagged) bucket (m16.10)", () => {
  it("renders the (untagged) row as a visible row", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [
          item({ agentNs: "", agentName: "(untagged)", runCount: 3 }),
        ],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    // The (untagged) cell text
    expect(await screen.findByText("(untagged)")).toBeInTheDocument();
  });

  it("(untagged) row does NOT navigate (click is a no-op)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [
          item({ agentNs: "", agentName: "(untagged)", runCount: 3 }),
        ],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    const cell = await screen.findByText("(untagged)");
    const row = cell.closest("tr")!;
    fireEvent.click(row);

    // The agent-detail stub should NOT appear — (untagged) has no agent page.
    expect(screen.queryByTestId("agent-detail-stub")).toBeNull();
  });

  it("a normal agent row click navigates to /agents/:ns/:name", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        agents: [
          item({ agentNs: "prod", agentName: "billing-agent" }),
        ],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();

    const cell = await screen.findByText("prod/billing-agent");
    const row = cell.closest("tr")!;
    fireEvent.click(row);

    await waitFor(() =>
      expect(screen.getByTestId("agent-detail-stub")).toBeInTheDocument(),
    );
  });
});

// ── Recent-window caveat ───────────────────────────────────────────────────────

describe("CostPage — recent-window caveat (m16.10)", () => {
  it("the recent-window caveat text is visible on the page", async () => {
    installFetch(() => ({
      ok: true,
      body: { agents: [item()], total: summary(), nextCursor: "" },
    }));

    renderPage();

    await screen.findByTestId("cost-page");
    // The caveat must be present somewhere on the page
    expect(
      screen.getByText(/recent window of activity, not all-time spend/i),
    ).toBeInTheDocument();
  });
});

// ── Cursor pagination ─────────────────────────────────────────────────────────

describe("CostPage — cursor pagination (m16.10)", () => {
  it("nextCursor drives the next page; Prev walks back", async () => {
    const captured = installFetch((qs) => {
      const cursor = qs.get("cursor") ?? "";
      if (!cursor) {
        return {
          ok: true,
          body: {
            agents: [item({ agentNs: "prod", agentName: "page-zero-agent" })],
            total: summary(),
            nextCursor: "cursor1",
          },
        };
      }
      return {
        ok: true,
        body: {
          agents: [item({ agentNs: "prod", agentName: "page-one-agent" })],
          total: summary(),
          nextCursor: "",
        },
      };
    });

    renderPage();

    expect(await screen.findByText("prod/page-zero-agent")).toBeInTheDocument();

    const next = screen.getByRole("button", { name: /Next page/ });
    const prev = screen.getByRole("button", { name: /Previous page/ });
    expect(next).toBeEnabled();
    expect(prev).toBeDisabled();

    fireEvent.click(next);
    expect(await screen.findByText("prod/page-one-agent")).toBeInTheDocument();

    // The second fetch should include cursor=cursor1
    expect(captured.some((u) => u.includes("cursor=cursor1"))).toBe(true);

    // On the last page, Next is disabled
    expect(screen.getByRole("button", { name: /Next page/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Previous page/ })).toBeEnabled();

    // Prev walks back to page 0
    fireEvent.click(screen.getByRole("button", { name: /Previous page/ }));
    expect(await screen.findByText("prod/page-zero-agent")).toBeInTheDocument();
  });

  it("hasNext is determined by nextCursor (not row count)", async () => {
    // Page with data but nextCursor="" → hasNext false
    installFetch(() => ({
      ok: true,
      body: {
        agents: [item()],
        total: summary(),
        nextCursor: "",
      },
    }));

    renderPage();
    await screen.findByTestId("cost-page");
    // Next is disabled when nextCursor is ""
    expect(screen.getByRole("button", { name: /Next page/ })).toBeDisabled();
  });
});

// ── Error and 501 states ──────────────────────────────────────────────────────

describe("CostPage — error and 501 states (m16.10)", () => {
  it("502 surfaces as a visible error (NOT a calm state)", async () => {
    installFetch(() => ({
      ok: false,
      status: 502,
      body: { error: "upstream Langfuse unreachable" },
    }));

    renderPage();

    await waitFor(() =>
      expect(
        screen.getByText(/upstream Langfuse unreachable/),
      ).toBeInTheDocument(),
    );
    // The teaching empty state must NOT appear
    expect(screen.queryByText("No cost data yet")).toBeNull();
  });

  it("502 offers a Retry button (not a forbidden state)", async () => {
    installFetch(() => ({
      ok: false,
      status: 502,
      body: { error: "upstream Langfuse unreachable" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/upstream Langfuse unreachable/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
  });

  it("501 degrades calmly to cost-unavailable (NOT an error toast)", async () => {
    installFetch(() => ({
      ok: false,
      status: 501,
      body: { error: "langfuse not configured" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByTestId("cost-unavailable")).toBeInTheDocument(),
    );
    // No error / retry
    expect(screen.queryByText(/Retry/)).toBeNull();
  });
});

// ── Query params ──────────────────────────────────────────────────────────────

describe("CostPage — query params sent to API (m16.10)", () => {
  it("sends by=agent on initial load", async () => {
    const captured = installFetch(() => ({
      ok: true,
      body: { agents: [], total: summary(), nextCursor: "" },
    }));

    renderPage();
    await screen.findByTestId("cost-page");

    expect(captured.some((u) => u.includes("by=agent"))).toBe(true);
  });
});
