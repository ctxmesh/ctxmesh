// topology-page.test.tsx — m15.13
//
// Tests for the TopologyPage: grouped mode, list↔graph toggle, search,
// expand/collapse, click-through DetailDrawer, 403 forbidden, empty state.
//
// Assertions keyed on data-testid strings — stable against CSS changes.

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { TopologyPage } from "@/pages/topology-page";
import type { TopologyGroup, TopologyNode, TopologyResponse } from "@/lib/api";

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

function makeGroup(
  id: string,
  label: string,
  namespace: string,
  memberCount: number,
  health: TopologyGroup["health"] = { ready: memberCount, notReady: 0, pending: 0, unknown: 0 },
): TopologyGroup {
  return {
    id,
    kind: "registry",
    label,
    namespace,
    memberCount,
    health,
    truncated: false,
    shownCount: 0,
  };
}

function makeNode(name: string, namespace: string, health = "ready"): TopologyNode {
  return {
    id: `agent/${namespace}/${name}`,
    kind: "agent",
    name,
    namespace,
    health: health as TopologyNode["health"],
    detail: `ghcr.io/org/${name}:latest`,
  };
}

const GROUP_A = makeGroup(
  "registry/prod/billing-team",
  "billing-team",
  "prod",
  3,
  { ready: 2, notReady: 1, pending: 0, unknown: 0 },
);
const GROUP_B = makeGroup("registry/prod/support-team", "support-team", "prod", 2);

const NODE_INVOICE = makeNode("invoice-agent", "prod");
const NODE_REFUND = makeNode("refund-agent", "prod");
const NODE_SUPPORT = makeNode("support-agent", "prod");

const GROUPED_RESPONSE: TopologyResponse = {
  nodes: [],
  edges: [],
  groups: [GROUP_A, GROUP_B],
};

const EXPANDED_RESPONSE: TopologyResponse = {
  nodes: [NODE_INVOICE, NODE_REFUND],
  edges: [],
  groups: [
    { ...GROUP_A, shownCount: 2 },
    GROUP_B,
  ],
};

// ---------------------------------------------------------------------------
// Fetch mock
// ---------------------------------------------------------------------------

interface CapturedCall {
  url: string;
}

function installFetch(
  responder: (url: string) => { ok: boolean; status?: number; body: unknown },
): CapturedCall[] {
  const calls: CapturedCall[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      calls.push({ url });
      const r = responder(url);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body,
      } as Response);
    }),
  );
  return calls;
}

afterEach(() => {
  vi.restoreAllMocks();
  // Always restore real timers — tests that use vi.useFakeTimers() may timeout
  // and leave fake timers active, which blocks waitFor in subsequent tests.
  vi.useRealTimers();
});

// ---------------------------------------------------------------------------
// Render helpers
// ---------------------------------------------------------------------------

function renderPage(initialPath = "/topology") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <TopologyPage />
    </MemoryRouter>,
  );
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("TopologyPage — grouped graph view", () => {
  it("renders group cards from grouped BFF response", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    // Both group cards present
    expect(await screen.findByTestId(`group-card-${GROUP_A.id}`)).toBeInTheDocument();
    expect(screen.getByTestId(`group-card-${GROUP_B.id}`)).toBeInTheDocument();
    // Labels visible
    expect(screen.getByTestId(`group-label-${GROUP_A.id}`)).toHaveTextContent("billing-team");
    expect(screen.getByTestId(`group-label-${GROUP_B.id}`)).toHaveTextContent("support-team");
    // Health dots for GROUP_A (has 2 ready + 1 notReady)
    expect(screen.getAllByTestId("health-dots").length).toBeGreaterThan(0);
  });

  it("shows agent count badge in controls row", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    // GROUP_A=3 + GROUP_B=2 = 5 agents
    expect(await screen.findByTestId("topology-count-badge")).toHaveTextContent("5 agents");
  });

  it("shows graph view by default (graph-view testid present)", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    expect(screen.getByTestId("topology-graph-view")).toBeInTheDocument();
    expect(screen.queryByTestId("topology-list-view")).toBeNull();
  });

  it("switches to list view on toggle click", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    expect(screen.getByTestId("topology-list-view")).toBeInTheDocument();
    expect(screen.queryByTestId("topology-graph-view")).toBeNull();
  });

  it("switches back to graph view", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    fireEvent.click(screen.getByTestId("toggle-graph"));
    expect(screen.getByTestId("topology-graph-view")).toBeInTheDocument();
  });

  it("list view shows group rows with labels and agent counts", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    expect(screen.getByTestId(`group-row-${GROUP_A.id}`)).toBeInTheDocument();
    expect(screen.getByTestId(`group-row-label-${GROUP_A.id}`)).toHaveTextContent("billing-team");
    // GROUP_A has 3 agents. The list view is a TABLE now (M151 A6): the member
    // count lives in its own numeric column, so the figure is on the row and
    // the noun is on the column head — same fact, table register.
    expect(screen.getByTestId(`group-row-count-${GROUP_A.id}`)).toHaveTextContent("3");
    expect(screen.getByTestId("topology-list-view")).toHaveTextContent("Agents");
  });
});

describe("TopologyPage — expand group → agent nodes", () => {
  it("clicking a group card sends ?expand= and shows agent nodes", async () => {
    // First call: grouped with no expand; second call (after click): with expand
    const calls = installFetch((url) => {
      if (url.includes("expand=")) {
        return { ok: true, body: EXPANDED_RESPONSE };
      }
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    // Click GROUP_A to expand
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    // Wait for the expanded agent nodes
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    // The refund-agent is also shown
    expect(screen.getByTestId(`agent-node-${NODE_REFUND.id}`)).toBeInTheDocument();
    // The second fetch included expand param
    const expandCall = calls.find((c) => c.url.includes("expand="));
    expect(expandCall).toBeDefined();
  });

  it("clicking agent node opens the detail drawer", async () => {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: EXPANDED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`));
    // Drawer should show the agent name
    expect(await screen.findByTestId("drawer-agent-name")).toHaveTextContent("invoice-agent");
    expect(screen.getByTestId("drawer-agent-ns")).toHaveTextContent("prod");
  });

  it("drawer has 'Open detail' button linking to /agents/:ns/:name", async () => {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: EXPANDED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`));
    await screen.findByTestId("drawer-open-detail");
    expect(screen.getByTestId("drawer-open-detail")).toBeInTheDocument();
  });

  it("closing the drawer removes agent detail from view", async () => {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: EXPANDED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`));
    await screen.findByTestId("drawer-agent-name");
    // Close via X button
    fireEvent.click(screen.getByRole("button", { name: /close panel/i }));
    expect(screen.queryByTestId("drawer-agent-name")).toBeNull();
  });
});

describe("TopologyPage — list view expand", () => {
  it("clicking group row in list view shows agent nodes beneath it", async () => {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: EXPANDED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    await screen.findByTestId(`group-row-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-row-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`list-agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    expect(screen.getByTestId(`list-agent-node-${NODE_REFUND.id}`)).toBeInTheDocument();
  });

  it("clicking agent in list view opens the drawer", async () => {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: EXPANDED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    await screen.findByTestId(`group-row-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-row-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`list-agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByTestId(`list-agent-node-${NODE_INVOICE.id}`));
    expect(await screen.findByTestId("drawer-agent-name")).toHaveTextContent("invoice-agent");
  });
});

describe("TopologyPage — search", () => {
  it("search input triggers ?q= in the API call (after debounce)", async () => {
    // Use real timers — the debounce is 300ms, well within the 5s timeout.
    const calls = installFetch((url) => {
      if (url.includes("q=invoice")) {
        return {
          ok: true,
          body: { nodes: [NODE_INVOICE], edges: [], groups: [{ ...GROUP_A, memberCount: 1, health: { ready: 1, notReady: 0, pending: 0, unknown: 0 } }] } satisfies TopologyResponse,
        };
      }
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId("topology-page");
    const searchInput = screen.getByTestId("topology-search");
    fireEvent.change(searchInput, { target: { value: "invoice" } });
    // Wait for the debounce (300ms) to fire and the fetch to include q=invoice.
    await waitFor(
      () => {
        expect(calls.some((c) => c.url.includes("q=invoice"))).toBe(true);
      },
      { timeout: 2000 },
    );
  });
});

describe("TopologyPage — error & empty states", () => {
  it("shows ForbiddenInline on 403", async () => {
    installFetch(() => ({ ok: false, status: 403, body: { message: "forbidden" } }));
    renderPage();
    expect(await screen.findByTestId("topology-forbidden")).toBeInTheDocument();
  });

  it("shows error alert on 500", async () => {
    installFetch(() => ({ ok: false, status: 500, body: { message: "internal error" } }));
    renderPage();
    expect(await screen.findByTestId("topology-error")).toBeInTheDocument();
  });

  it("shows empty state when no groups returned", async () => {
    installFetch(() => ({ ok: true, body: { nodes: [], edges: [], groups: [] } }));
    renderPage();
    await screen.findByTestId("topology-page");
    expect(screen.queryByTestId("topology-graph-view")).toBeNull();
    expect(screen.queryByTestId("topology-list-view")).toBeNull();
  });

  it("shows loading state initially", () => {
    // Never resolves
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    renderPage();
    expect(screen.getByTestId("topology-loading")).toBeInTheDocument();
  });

  it("search empty state: no results returned for a query (topology-loading gone)", async () => {
    // Use real timers — debounce is 300ms.
    installFetch(() => ({ ok: true, body: { nodes: [], edges: [], groups: [] } satisfies TopologyResponse }));
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.change(screen.getByTestId("topology-search"), { target: { value: "xyz" } });
    // After debounce fires and data loads, loading spinner is gone.
    await waitFor(
      () => expect(screen.queryByTestId("topology-loading")).toBeNull(),
      { timeout: 2000 },
    );
  });
});

describe("TopologyPage — truncated group", () => {
  it("shows +N more when a group is truncated", async () => {
    const truncatedResponse: TopologyResponse = {
      nodes: [NODE_INVOICE, NODE_REFUND],
      edges: [],
      groups: [
        {
          ...GROUP_A,
          memberCount: 5,
          truncated: true,
          shownCount: 2,
        },
        GROUP_B,
      ],
    };
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: truncatedResponse };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
    // Truncation note: memberCount=5, shownCount=2 → "+3 more"
    expect(screen.getByTestId(`truncated-${GROUP_A.id}`)).toHaveTextContent("+3 more");
  });
});

describe("TopologyPage — API call params", () => {
  it("always sends ?group=registry (grouped mode)", async () => {
    const calls = installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    expect(calls[0]?.url).toContain("group=registry");
  });

  it("does NOT call /api/topology with no group param (raw mode is dashboard only)", async () => {
    const calls = installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    // The topology-page ALWAYS uses ?group=registry
    expect(calls.every((c) => c.url.includes("group="))).toBe(true);
  });
});

// ── M151: health is the only hue, and unknown is not a failure ─────────────

const NODE_UNKNOWN = makeNode("ingest-coordinator", "prod", "unknown");
const NODE_BROKEN = makeNode("billing-agent", "prod", "notReady");

const MIXED_RESPONSE: TopologyResponse = {
  nodes: [NODE_INVOICE, NODE_BROKEN, NODE_UNKNOWN],
  edges: [],
  groups: [{ ...GROUP_A, memberCount: 3, shownCount: 3 }, GROUP_B],
};

describe("TopologyPage — the status vocabulary (M151 §2.2)", () => {
  async function expandGroupA() {
    installFetch((url) => {
      if (url.includes("expand=")) return { ok: true, body: MIXED_RESPONSE };
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId(`group-card-${GROUP_A.id}`);
    fireEvent.click(screen.getByTestId(`group-card-${GROUP_A.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`)).toBeInTheDocument(),
    );
  }

  it("renders an unreported status as UNKNOWN in the open register — never as a failure", async () => {
    await expandGroupA();
    const node = screen.getByTestId(`agent-node-${NODE_UNKNOWN.id}`);
    expect(node).toHaveTextContent("Unknown");
    // A cluster that reported nothing has not reported a failure. Crit means
    // "it will not proceed without a change" and this is not a claim we have.
    expect(node).not.toHaveTextContent("Not ready");
    expect(node.querySelector(".text-destructive")).toBeNull();
    // The §6.1 A6 node grammar: "declared-or-missing" is the dashed frame.
    expect(node.className).toContain("border-dashed");
  });

  it("marks only the failing node with the crit rule", async () => {
    await expandGroupA();
    expect(screen.getByTestId(`agent-node-${NODE_BROKEN.id}`).className).toContain(
      "border-l-destructive",
    );
    // A healthy node carries no rule at all — an accent that is always on says
    // nothing (§2.2, annotation not alarm).
    expect(screen.getByTestId(`agent-node-${NODE_INVOICE.id}`).className).not.toContain(
      "border-l-destructive",
    );
  });

  it("never paints a node or a group in the brand (§2.1 — pine is never a status)", async () => {
    await expandGroupA();
    for (const el of [
      screen.getByTestId(`agent-node-${NODE_INVOICE.id}`),
      screen.getByTestId(`agent-node-${NODE_BROKEN.id}`),
      screen.getByTestId(`group-card-${GROUP_A.id}`),
    ]) {
      expect(el.innerHTML).not.toContain("bg-primary");
      expect(el.innerHTML).not.toContain("border-primary");
    }
  });

  it("states a group's rollup worst-first, with the hue on the number that carries it", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    // GROUP_A is 2 ready + 1 notReady.
    const card = await screen.findByTestId(`group-card-${GROUP_A.id}`);
    const line = card.querySelector('[data-testid="health-dots"]');
    expect(line).not.toBeNull();
    expect(line).toHaveTextContent("1 failed");
    expect(line).toHaveTextContent("2 ready");
    // Attention order: the failure is read before the healthy majority.
    expect((line as HTMLElement).textContent?.indexOf("failed")).toBeLessThan(
      (line as HTMLElement).textContent?.indexOf("ready") ?? -1,
    );
  });
});

describe("TopologyPage — the canvas is a pan surface in a fixed frame (§4.6)", () => {
  it("keeps the map inside its own scroller, never widening the page", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    const frame = await screen.findByTestId("topology-graph-view");
    expect(frame.className).toContain("overflow-auto");
    expect(frame.className).toContain("min-h-[35rem]");
  });

  it("scrolls the LIST inside its own container too", async () => {
    installFetch(() => ({ ok: true, body: GROUPED_RESPONSE }));
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    expect(screen.getByTestId("topology-list-view").className).toContain("overflow-x-auto");
  });

  it("offers a way out of an empty search rather than a dead end", async () => {
    installFetch((url) =>
      url.includes("q=")
        ? { ok: true, body: { nodes: [], edges: [], groups: [] } satisfies TopologyResponse }
        : { ok: true, body: GROUPED_RESPONSE },
    );
    renderPage("/topology?q=nothing-matches");
    const empty = await screen.findByTestId("topology-empty");
    expect(empty).toHaveTextContent("No nodes match");
    expect(screen.getByRole("button", { name: /clear the search/i })).toBeInTheDocument();
  });
});

describe("TopologyPage — SUPPORT node kinds in list view", () => {
  it("also shows support-agent nodes when group-b expanded (namespace match)", async () => {
    const supportExpanded: TopologyResponse = {
      nodes: [NODE_SUPPORT],
      edges: [],
      groups: [GROUP_A, { ...GROUP_B, shownCount: 1 }],
    };
    installFetch((url) => {
      if (url.includes(`expand=${encodeURIComponent(GROUP_B.id)}`)) {
        return { ok: true, body: supportExpanded };
      }
      if (url.includes("expand=")) {
        return { ok: true, body: EXPANDED_RESPONSE };
      }
      return { ok: true, body: GROUPED_RESPONSE };
    });
    renderPage();
    await screen.findByTestId("topology-page");
    fireEvent.click(screen.getByTestId("toggle-list"));
    // Expand GROUP_B
    await screen.findByTestId(`group-row-${GROUP_B.id}`);
    fireEvent.click(screen.getByTestId(`group-row-${GROUP_B.id}`));
    await waitFor(() =>
      expect(screen.getByTestId(`list-agent-node-${NODE_SUPPORT.id}`)).toBeInTheDocument(),
    );
    expect(screen.getByTestId(`list-agent-node-${NODE_SUPPORT.id}`)).toHaveTextContent("support-agent");
  });
});
