import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { DashboardPage } from "@/pages/dashboard-page";

// A URL-routed fetch mock: the dashboard fans out to /api/topology, /api/cost,
// /api/runs (and /api/traces/{id} once a run is selected). Each returns canned,
// deterministic data — no live cluster/Langfuse/Prometheus (tier0 determinism).
function routeFetch(routes: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      // Exact match first, then a prefix match for /api/traces/{id}.
      const body =
        routes[path] ??
        (path.startsWith("/api/traces/")
          ? {
              traceId: path.slice("/api/traces/".length),
              url: "https://lf.test/trace/x",
            }
          : undefined);
      if (body === undefined) {
        return Promise.resolve({
          ok: false,
          status: 404,
          json: async () => ({}),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => body,
      } as Response);
    }),
  );
}

const topology = {
  nodes: [
    {
      id: "registry/prod/team",
      kind: "registry",
      name: "team-registry",
      namespace: "prod",
      health: "ready",
      detail: "team-a",
    },
    {
      id: "agent/prod/echo",
      kind: "agent",
      name: "echo-agent",
      namespace: "prod",
      health: "ready",
      detail: "echo:1",
    },
    {
      id: "tool/prod/echo-search",
      kind: "tool",
      name: "search-tool",
      namespace: "prod",
      health: "ready",
      detail: "remote",
    },
  ],
  edges: [
    {
      id: "registry/prod/team->agent/prod/echo",
      source: "registry/prod/team",
      target: "agent/prod/echo",
    },
    {
      id: "agent/prod/echo->tool/prod/echo-search",
      source: "agent/prod/echo",
      target: "tool/prod/echo-search",
    },
  ],
};

const cost = {
  summary: {
    totalCostUSD: 1.75,
    totalTokens: 1500,
    observations: 3,
    byModel: [{ label: "gpt-4o", value: 1.75 }],
  },
  latency: [{ label: "billing-agent", value: 120 }],
  scale: [{ label: "billing-agent", value: 3 }],
};

const runs = {
  runs: [
    {
      traceId: "t-abc",
      name: "checkout-flow",
      timestamp: "2026-07-01T00:00:00Z",
      costUSD: 0.5,
      tokens: 900,
      latencyMs: 120,
    },
  ],
};

// A non-empty provider list — the default for the render-proof tests so the
// first-run CTA does NOT show (a connected cluster, not a fresh install).
const providersConnected = {
  providers: [
    {
      provider: "anthropic",
      displayName: "Anthropic",
      models: [{ id: "claude-opus-4" }],
    },
  ],
};

// renderDashboard wraps the page in a MemoryRouter — the empty-state CTA calls
// useNavigate, so the page needs a router context in tests.
function renderDashboard() {
  return render(
    <MemoryRouter>
      <DashboardPage />
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("DashboardPage (render proof)", () => {
  it("renders topology, cost, and recent runs from mocked BFF data", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Topology renders as a scale-first SUMMARY card (m22.6/U5) — counts +
    // health rollup, not a node-per-agent graph.
    expect(await screen.findByTestId("topology-summary")).toBeInTheDocument();
    // Cost cards rendered the Langfuse rollup (headline stat + the by-model chart).
    expect(screen.getByText("Total cost")).toBeInTheDocument();
    expect(screen.getByText("Cost by model")).toBeInTheDocument();
    expect(screen.getByText("gpt-4o")).toBeInTheDocument();
    // Recent runs rendered the traced run.
    expect(screen.getByText("checkout-flow")).toBeInTheDocument();
    expect(screen.getByText("t-abc")).toBeInTheDocument();
  });

  it("degrades cost + runs CALMLY (not an error) when Langfuse is not configured (501)", async () => {
    // m20.6: a 501 (adapter not wired) is not a failure — the dashboard shows a
    // calm "not configured" state, never a red "Failed to load …" (the user's
    // "Failed to load cost: … not implemented yet" complaint).
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL) => {
        const path = (
          typeof input === "string" ? input : input.toString()
        ).split("?")[0];
        if (path === "/api/cost" || path === "/api/runs") {
          return Promise.resolve({
            ok: false,
            status: 501,
            json: async () => ({
              error: "Langfuse cost adapter is not implemented yet",
            }),
          } as Response);
        }
        const body =
          path === "/api/topology"
            ? topology
            : path === "/api/providers"
              ? providersConnected
              : {};
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => body,
        } as Response);
      }),
    );
    renderDashboard();

    expect(await screen.findByTestId("cost-unavailable")).toBeInTheDocument();
    expect(screen.getByTestId("runs-unavailable")).toBeInTheDocument();
    // NOT a destructive error.
    expect(screen.queryByText(/Failed to load cost/)).toBeNull();
    expect(screen.queryByText(/Failed to load runs/)).toBeNull();
  });

  it("links the dashboard topology to the full interactive /topology view (m20.7)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    await screen.findByTestId("topology-summary");
    const link = screen.getByTestId("view-full-topology");
    expect(link).toHaveAttribute("href", "/topology");
  });

  it("does NOT render any Langfuse embedded iframe (m16.11: iframe demoted)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Wait for data to load.
    await screen.findByText("checkout-flow");

    // No iframe in the dashboard — the Langfuse door is now the link-out on
    // /traces/:id, not an embedded iframe here.
    expect(document.querySelector("iframe")).toBeNull();
    // No legacy "Select a run to open its embedded Langfuse trace" prompt.
    expect(
      screen.queryByText(/Select a run to open its embedded Langfuse trace/),
    ).toBeNull();
    // No "Langfuse deep-view" section heading.
    expect(screen.queryByText("Langfuse deep-view")).toBeNull();
  });

  it("cost card links to the native /cost page (m16.11)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Wait for cost data to render.
    await screen.findByText("Total cost");

    // The "View cost details" link leads to the native /cost page.
    const costLink = screen.getByTestId("view-cost-details");
    expect(costLink).toBeInTheDocument();
    expect(costLink).toHaveAttribute("href", "/cost");
  });

  it("recent runs list has a View-all-runs link to /runs (m16.11)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Wait for runs to render.
    await screen.findByText("checkout-flow");

    // "View all runs" links to the native /runs page.
    const viewAllLink = screen.getByTestId("view-all-runs");
    expect(viewAllLink).toBeInTheDocument();
    expect(viewAllLink).toHaveAttribute("href", "/runs");
  });

  it("recent run rows link to /traces/:id (m16.11)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Wait for the run row to appear.
    await screen.findByText("checkout-flow");

    // The run row should be a link to the native trace page.
    const traceLink = screen.getByRole("link", { name: /checkout-flow/ });
    expect(traceLink).toHaveAttribute("href", "/traces/t-abc");
  });

  it("shows an error state when the topology fetch fails (e.g. RBAC 403)", async () => {
    routeFetch({
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    }); // topology 404s
    renderDashboard();
    expect(
      await screen.findByText(/Failed to load topology/),
    ).toBeInTheDocument();
  });
});

describe("DashboardPage — first-run provider CTA (the aha entry point)", () => {
  it("renders the first-run checklist when setup is incomplete (no providers)", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": { providers: [] },
    });
    renderDashboard();

    // The guided 3-step checklist appears; the CTA leads to the first incomplete
    // step (connect a provider).
    expect(
      await screen.findByTestId("first-run-checklist"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("first-run-cta")).toHaveTextContent(
      /Connect a provider/,
    );
    // Step 0 (connect) is not done yet.
    expect(screen.getByTestId("first-run-step-0")).toBeInTheDocument();
  });

  it("does NOT render the CTA when providers exist", async () => {
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();

    // Wait for the page to settle, then assert the CTA is absent.
    await screen.findByText("checkout-flow");
    expect(screen.queryByTestId("first-run-checklist")).toBeNull();
  });

  it("does NOT render the CTA when the providers list fails to load (no false invitation)", async () => {
    // /api/providers 404s (kill-switch or error) → not treated as 'empty'.
    routeFetch({
      "/api/topology": topology,
      "/api/cost": cost,
      "/api/runs": runs,
    });
    renderDashboard();

    await screen.findByText("checkout-flow");
    await waitFor(() =>
      expect(screen.queryByTestId("first-run-checklist")).toBeNull(),
    );
  });
});

describe("CostPanel bar chart", () => {
  it("degrades to an empty-series hint when Prometheus is not wired", async () => {
    const costNoProm = {
      summary: {
        totalCostUSD: 1,
        totalTokens: 10,
        observations: 1,
        byModel: [],
      },
      latency: [],
      scale: [],
    };
    routeFetch({
      "/api/topology": topology,
      "/api/cost": costNoProm,
      "/api/runs": runs,
      "/api/providers": providersConnected,
    });
    renderDashboard();
    const hints = await screen.findAllByText(/Prometheus not wired/);
    // One hint each for scale + latency.
    expect(hints.length).toBeGreaterThanOrEqual(2);
    const chart = screen.getByText("Cost by model").closest("div");
    expect(within(chart as HTMLElement).queryByRole("progressbar")).toBeNull();
  });
});
