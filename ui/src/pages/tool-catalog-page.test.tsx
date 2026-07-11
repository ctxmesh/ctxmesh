import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ToolCatalogPage } from "@/pages/tool-catalog-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// ToolCatalogPage tests (m17.10):
//   1. Renders all three tool state badges (curated / user-added / pending-approval).
//   2. A pending-approval tool has its "Bind" button DISABLED.
//   3. Bind wizard: submitting calls createMcpToolBinding with agentRef+toolName.
//   4. Propagation status: a Ready=True binding surfaces "propagated"; a not-ready
//      binding shows the reason — NEVER faked.
//   5. RBAC: a viewer (no mcptoolbindings/create) sees all tools but the Bind
//      buttons are disabled (display-only gate; real gate is the API 403).

// ---- fixture data -----------------------------------------------------------

const CATALOG_TOOLS = [
  // curated — no source, approvalStatus absent
  { name: "code-search", description: "Search code repositories" },
  // user-added — has source, approvalStatus=approved
  {
    name: "slack-notify",
    description: "Send Slack messages",
    source: "slack-mcp",
    approvalStatus: "approved" as const,
    inputSchema: { type: "object" },
  },
  // pending-approval — approvalStatus=pending
  {
    name: "risky-tool",
    description: "A tool awaiting approval",
    source: "unknown-mcp",
    approvalStatus: "pending" as const,
  },
];

const AGENTS = [
  { name: "my-agent", namespace: "default", image: "my-image:latest", phase: "Running", ready: true },
  { name: "other-agent", namespace: "staging", image: "other:latest", phase: "Running", ready: true },
];

// propagated binding detail (Ready=True)
const PROPAGATED_DETAIL = {
  name: "my-agent-code-search",
  namespace: "default",
  agentName: "my-agent",
  agentNamespace: "default",
  toolName: "code-search",
  ready: true,
  propagationStatus: "propagated",
  conditions: [
    {
      type: "Ready",
      status: "True",
      reason: "Propagated",
      message: "Tool is live on the agent",
      lastTransitionTime: "2026-07-11T10:00:00Z",
    },
  ],
};

// not-ready binding detail (Ready=False, reason=Pending)
const PENDING_DETAIL = {
  name: "my-agent-code-search",
  namespace: "default",
  agentName: "my-agent",
  agentNamespace: "default",
  toolName: "code-search",
  ready: false,
  propagationStatus: "Pending",
  conditions: [
    {
      type: "Ready",
      status: "False",
      reason: "Pending",
      message: "Controller has not yet reconciled",
      lastTransitionTime: "2026-07-11T10:00:00Z",
    },
  ],
};

// ---- fetch stub helpers -----------------------------------------------------

type FetchSetup = {
  caps?: Record<string, Record<string, boolean>>;
  tools?: unknown[];
  toolsStatus?: number;
  agents?: unknown[];
  bindingDetail?: unknown; // returned by POST /api/mcptoolbindings
  bindingDetailGet?: unknown; // returned by GET /api/mcptoolbindings/:ns/:name
  bindOk?: boolean;
  bindStatus?: number;
};

const EDITOR_CAPS = {
  agentdeployments: { create: true, update: true, delete: true },
  mcptoolbindings: { create: true, update: true, delete: true },
};

const VIEWER_CAPS = {
  agentdeployments: { create: false, update: false, delete: false },
  mcptoolbindings: { create: false, update: false, delete: false },
};

function installFetch(opts: FetchSetup = {}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });

      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({ ok, status, json: async () => body } as Response);

      if (url.startsWith("/api/namespaces"))
        return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? EDITOR_CAPS });

      // tool catalog
      if (url === "/api/tools" && method === "GET") {
        const status = opts.toolsStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              ? { tools: opts.tools ?? CATALOG_TOOLS }
              : { error: "forbidden" },
        } as Response);
      }

      // agents list (for the bind wizard select-agent step)
      if (url.startsWith("/api/agents") && method === "GET" && !url.includes("/runs")) {
        return j({
          agents: opts.agents ?? AGENTS,
          items: opts.agents ?? AGENTS,
          nextCursor: "",
        });
      }

      // create binding (POST /api/mcptoolbindings)
      if (url === "/api/mcptoolbindings" && method === "POST") {
        const ok = opts.bindOk ?? true;
        const status = opts.bindStatus ?? (ok ? 201 : 403);
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              ? (opts.bindingDetail ?? PROPAGATED_DETAIL)
              : { error: "forbidden" },
        } as Response);
      }

      // binding detail GET (for propagation poll)
      if (url.match(/\/api\/mcptoolbindings\/[^/]+\/[^/]+$/) && method === "GET") {
        return j(opts.bindingDetailGet ?? PROPAGATED_DETAIL);
      }

      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderPage(
  caps: Record<string, Record<string, boolean>> = EDITOR_CAPS,
  setup: FetchSetup = {},
) {
  installFetch({ ...setup, caps });
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <ToolCatalogPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  sessionStorage.clear();
});

// ---- tests ------------------------------------------------------------------

describe("ToolCatalogPage — catalog listing", () => {
  it("renders tool list with all three state badges", async () => {
    renderPage();

    // All three tool rows should appear
    expect(await screen.findByTestId("catalog-tool-code-search")).toBeInTheDocument();
    expect(screen.getByTestId("catalog-tool-slack-notify")).toBeInTheDocument();
    expect(screen.getByTestId("catalog-tool-risky-tool")).toBeInTheDocument();

    // State badges
    expect(screen.getByTestId("catalog-tool-state-code-search")).toHaveTextContent("curated");
    expect(screen.getByTestId("catalog-tool-state-slack-notify")).toHaveTextContent("user-added");
    expect(screen.getByTestId("catalog-tool-state-risky-tool")).toHaveTextContent("pending-approval");
  });

  it("shows schema badge for a tool with an inputSchema", async () => {
    renderPage();

    // slack-notify has an inputSchema
    const row = await screen.findByTestId("catalog-tool-slack-notify");
    expect(row).toHaveTextContent("schema");
  });

  it("a pending-approval tool has its Bind button disabled", async () => {
    renderPage();

    await screen.findByTestId("catalog-tool-risky-tool");

    const bindBtn = screen.getByTestId("catalog-bind-risky-tool");
    expect(bindBtn).toBeDisabled();
  });

  it("curated and user-added tools have an enabled Bind button for an editor", async () => {
    renderPage(EDITOR_CAPS);

    await screen.findByTestId("catalog-tool-code-search");

    expect(screen.getByTestId("catalog-bind-code-search")).not.toBeDisabled();
    expect(screen.getByTestId("catalog-bind-slack-notify")).not.toBeDisabled();
  });

  it("renders empty state when catalog is empty", async () => {
    renderPage(EDITOR_CAPS, { tools: [] });

    expect(await screen.findByText(/No tools in the catalog/)).toBeInTheDocument();
  });

  it("renders forbidden state on a 403", async () => {
    renderPage(EDITOR_CAPS, { toolsStatus: 403 });

    expect(
      await screen.findByText(/Not allowed to view the tool catalog/),
    ).toBeInTheDocument();
  });
});

describe("ToolCatalogPage — bind wizard", () => {
  it("opens the bind wizard when clicking Bind on an approved tool", async () => {
    renderPage();

    await screen.findByTestId("catalog-tool-code-search");
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));

    expect(screen.getByTestId("bind-tool-wizard")).toBeInTheDocument();
  });

  it("submits createMcpToolBinding with agentRef + toolName", async () => {
    const calls = installFetch({ caps: EDITOR_CAPS });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ToolCatalogPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    // Open wizard for code-search
    await screen.findByTestId("catalog-tool-code-search");
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));

    // Step 0: wait for agents to load and select my-agent/default
    const agentBtn = await screen.findByTestId("bind-agent-default-my-agent");
    fireEvent.click(agentBtn);

    // Navigate to confirm step via Next (the wizard's Next/Finish button)
    const nextBtn = screen.getByRole("button", { name: /next/i });
    fireEvent.click(nextBtn);

    // Step 1 (confirm): click Next again to trigger binding creation
    await screen.findByTestId("bind-tool-confirm");
    const finishBtn = screen.getByRole("button", { name: /next/i });
    fireEvent.click(finishBtn);

    // The POST /api/mcptoolbindings should have been called with correct body
    await waitFor(() => {
      const bindCall = calls.find(
        (c) => c.url === "/api/mcptoolbindings" && c.method === "POST",
      );
      expect(bindCall).toBeDefined();
      const body = JSON.parse(bindCall!.body) as {
        agentRef: { namespace: string; name: string };
        toolName: string;
      };
      expect(body.agentRef.name).toBe("my-agent");
      expect(body.agentRef.namespace).toBe("default");
      expect(body.toolName).toBe("code-search");
    });
  });

  it("shows 'propagated' when the binding detail has ready=true", async () => {
    installFetch({
      caps: EDITOR_CAPS,
      bindingDetail: PROPAGATED_DETAIL,
      bindingDetailGet: PROPAGATED_DETAIL,
    });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ToolCatalogPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    await screen.findByTestId("catalog-tool-code-search");
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));

    const agentBtn = await screen.findByTestId("bind-agent-default-my-agent");
    fireEvent.click(agentBtn);
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    await screen.findByTestId("bind-tool-confirm");
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    // The propagation status element should appear and show "propagated"
    const statusEl = await screen.findByTestId("binding-propagation-status");
    expect(statusEl).toHaveAttribute("data-status", "propagated");
    expect(statusEl).toHaveTextContent(/propagated/i);
    expect(statusEl).toHaveTextContent(/hot-updated live/i);
  });

  it("shows the controller reason (NOT 'propagated') when binding is not ready", async () => {
    // Use fake timers so the polling loop completes instantly without waiting
    // MAX_POLL_ATTEMPTS × POLL_INTERVAL_MS (30 s) in real time.
    vi.useFakeTimers();

    installFetch({
      caps: EDITOR_CAPS,
      bindingDetail: PENDING_DETAIL,
      bindingDetailGet: PENDING_DETAIL,
    });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ToolCatalogPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    // Restore real timers before any awaits so findBy* works correctly.
    // We need to await the initial catalog load first.
    vi.useRealTimers();

    await screen.findByTestId("catalog-tool-code-search");
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));

    const agentBtn = await screen.findByTestId("bind-agent-default-my-agent");
    fireEvent.click(agentBtn);
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    await screen.findByTestId("bind-tool-confirm");
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    // After creating the binding (PENDING_DETAIL: ready=false), the component
    // immediately shows the propagation status from the create response.
    // Since bindingDetail.ready=false, it starts polling. But since we need to
    // verify the honest "not propagated" state, we check the binding poll shows
    // the correct final state from the controller.
    //
    // Use fake timers to advance past MAX_POLL_ATTEMPTS × POLL_INTERVAL_MS
    // so the exhausted-poll path is hit and the pending detail is shown.
    vi.useFakeTimers();
    // Advance time past all poll iterations (15 × 2000ms = 30000ms)
    await vi.runAllTimersAsync();
    vi.useRealTimers();

    // Status element must NOT say "propagated" (ready=false with reason=Pending)
    const statusEl = await screen.findByTestId("binding-propagation-status");
    // Should NOT be "propagated" data-status
    expect(statusEl).not.toHaveAttribute("data-status", "propagated");
    // Should show the controller's reason
    expect(statusEl).toHaveTextContent(/Pending/);
    // NEVER claim hot-updated when not ready
    expect(statusEl).not.toHaveTextContent(/hot-updated live/i);
  });

  it("surfaces a 403 from the API as an error (not silently ignored)", async () => {
    installFetch({
      caps: EDITOR_CAPS,
      bindOk: false,
      bindStatus: 403,
    });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ToolCatalogPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    await screen.findByTestId("catalog-tool-code-search");
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));

    const agentBtn = await screen.findByTestId("bind-agent-default-my-agent");
    fireEvent.click(agentBtn);
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    await screen.findByTestId("bind-tool-confirm");
    fireEvent.click(screen.getByRole("button", { name: /next/i }));

    const statusEl = await screen.findByTestId("binding-propagation-status");
    expect(statusEl).toHaveAttribute("data-status", "error");
    expect(statusEl).toHaveTextContent(/Not allowed/i);
  });
});

describe("ToolCatalogPage — RBAC (viewer)", () => {
  it("a viewer sees all tool rows but all Bind buttons are disabled", async () => {
    renderPage(VIEWER_CAPS);

    // All tools visible
    expect(await screen.findByTestId("catalog-tool-code-search")).toBeInTheDocument();
    expect(screen.getByTestId("catalog-tool-slack-notify")).toBeInTheDocument();
    expect(screen.getByTestId("catalog-tool-risky-tool")).toBeInTheDocument();

    // All bind buttons disabled (pending disabled for approval reason; others
    // disabled because viewer has no mcptoolbindings/create permission)
    expect(screen.getByTestId("catalog-bind-code-search")).toBeDisabled();
    expect(screen.getByTestId("catalog-bind-slack-notify")).toBeDisabled();
    expect(screen.getByTestId("catalog-bind-risky-tool")).toBeDisabled();
  });
});
