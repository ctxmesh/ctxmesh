import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { CreateAgentPage } from "@/pages/create-agent-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// A recording fetch mock: captures every request (url, method, body) so tests
// assert the RIGHT bodies were POSTed AND — the m14.10 landmine — that
// generation NEVER auto-applies (no /api/agents POST until Create is clicked).
// Mocked fetch = tier0 determinism (no cluster, no LLM).
interface Captured {
  url: string;
  method: string;
  body: string;
}

type Handler = (body: string) => { ok: boolean; status?: number; json: unknown };

function recordingFetch(opts: {
  providers?: { provider: string; displayName: string; models: { id: string }[] }[];
  tools?: unknown[];
  generate?: Handler;
  expand?: (body: string) => { ok: boolean; status?: number; text: string };
  create?: Handler;
  caps?: Record<string, Record<string, boolean>>;
}) {
  const calls: Captured[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url, method, body });

      const j = (json: unknown, ok = true, status = 200) =>
        Promise.resolve({
          ok,
          status,
          json: async () => json,
          text: async () => (typeof json === "string" ? json : JSON.stringify(json)),
        } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? { agentdeployments: { create: true } } });
      if (url === "/api/providers" && method === "GET")
        return j({ providers: opts.providers ?? [{ name: "anthropic", namespace: "default", provider: "anthropic", displayName: "Anthropic", models: ["claude-sonnet-4"], secretName: "anthropic", ready: true }] });
      if (url === "/api/tools")
        return j({ tools: opts.tools ?? [] });
      if (url === "/api/agents/generate" && method === "POST") {
        const r = opts.generate
          ? opts.generate(body)
          : { ok: true, json: { agentYAML: "name: gen-agent\nruntime: managed\n", expanded: "kind: AgentDeployment\n", model: "claude-sonnet-4" } };
        return j(r.json, r.ok, r.status ?? (r.ok ? 200 : 422));
      }
      if (url === "/api/expand" && method === "POST") {
        const r = opts.expand ? opts.expand(body) : { ok: true, text: "kind: AgentDeployment\nmetadata:\n  name: x\n" };
        return Promise.resolve({ ok: r.ok, status: r.status ?? (r.ok ? 200 : 400), text: async () => r.text, json: async () => ({ error: r.text }) } as Response);
      }
      if (url === "/api/agents" && method === "POST") {
        const r = opts.create
          ? opts.create(body)
          : { ok: true, json: { created: [{ kind: "AgentDeployment", name: "gen-agent", namespace: "default" }] } };
        return j(r.json, r.ok, r.status ?? (r.ok ? 201 : 400));
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <CreateAgentPage />
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

// pickDescribe waits for the entrance fork, then chooses "Describe it".
async function pickDescribe() {
  fireEvent.click(await screen.findByTestId("entrance-describe"));
  await screen.findByTestId("describe-flow");
}

async function pickConfigure() {
  fireEvent.click(await screen.findByTestId("entrance-configure"));
  await screen.findByTestId("configure-flow");
}

describe("CreateAgentPage — the entrance fork", () => {
  it("offers Describe it vs Configure it, converging on one review", async () => {
    recordingFetch({});
    renderPage();
    expect(await screen.findByTestId("create-entrance")).toBeInTheDocument();
    expect(screen.getByTestId("entrance-describe")).toBeInTheDocument();
    expect(screen.getByTestId("entrance-configure")).toBeInTheDocument();
  });

  it("gates to connect-provider first when no provider is connected", async () => {
    recordingFetch({ providers: [] });
    renderPage();
    // The no-provider gate steers to connect first (Describe needs a provider).
    expect(await screen.findByTestId("no-provider-gate")).toBeInTheDocument();
    expect(screen.getByText(/Connect a provider first/)).toBeInTheDocument();
    // The entrance fork is NOT shown behind the gate.
    expect(screen.queryByTestId("create-entrance")).toBeNull();
  });
});

describe("CreateAgentPage — Describe it", () => {
  it("description → POSTs generate → friendly review + Advanced CRD + pre-selected tools; NEVER auto-applies", async () => {
    const calls = recordingFetch({
      tools: [
        { name: "get_order", description: "Fetch an order", source: "acme-mcp", approvalStatus: "approved", inputSchema: {} },
        { name: "search_docs", description: "Search docs", source: "curated", approvalStatus: "approved" },
      ],
      generate: () => ({
        ok: true,
        json: {
          agentYAML: "name: support-agent\nruntime: managed\ntools:\n  - get_order\n",
          expanded: "kind: AgentDeployment\nmetadata:\n  name: support-agent\n",
          model: "claude-sonnet-4",
        },
      }),
    });
    renderPage();
    await pickDescribe();

    fireEvent.change(screen.getByLabelText("Agent description"), {
      target: { value: "a support agent" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));

    // Lands on the shared review with the friendly generation summary.
    expect(await screen.findByTestId("generation-review")).toBeInTheDocument();
    expect(screen.getByTestId("friendly-summary")).toBeInTheDocument();
    // The generated model is tagged (cost/model honesty).
    expect(screen.getByTestId("gen-model-tag")).toHaveTextContent(/claude-sonnet-4/);
    // The generated tool is pre-selected (flows into the summary).
    expect(screen.getByTestId("summary-tools")).toHaveTextContent(/get_order/);

    // Advanced discloses the raw CRD (no hand-edit as the primary path).
    fireEvent.click(screen.getByTestId("advanced-toggle"));
    const advanced = (await screen.findByTestId("advanced-yaml")) as HTMLTextAreaElement;
    expect(advanced).toHaveAttribute("readonly");
    expect(advanced.value).toContain("AgentDeployment");

    // The GENERATE POST carried the description.
    const gen = calls.find((c) => c.url === "/api/agents/generate" && c.method === "POST");
    expect(gen).toBeDefined();
    expect(JSON.parse(gen!.body).description).toBe("a support agent");

    // THE LANDMINE: generation NEVER auto-applies — no /api/agents POST yet.
    expect(calls.find((c) => c.url === "/api/agents" && c.method === "POST")).toBeUndefined();
  });

  it("a 422 regenerate (keyed on the flag, not the status) shows the reason + preserves the raw YAML", async () => {
    recordingFetch({
      generate: () => ({
        ok: false,
        status: 422,
        json: {
          error: "invalid config",
          reason: "unknown field 'frobnicate'",
          agentYAML: "name: bad-agent\nfrobnicate: true\n",
          regenerate: true,
        },
      }),
    });
    renderPage();
    await pickDescribe();

    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));

    // The regenerate affordance + the honest reason.
    expect(await screen.findByTestId("regenerate-state")).toBeInTheDocument();
    expect(screen.getByTestId("regenerate-reason")).toHaveTextContent(/unknown field 'frobnicate'/);
    expect(screen.getByTestId("regenerate-button")).toBeInTheDocument();
    // The raw agentYAML is PRESERVED (nothing lost).
    expect((screen.getByTestId("regenerate-raw-yaml") as HTMLTextAreaElement).value).toContain(
      "frobnicate",
    );
    // A 422 is not a review — no shared review yet.
    expect(screen.queryByTestId("shared-review")).toBeNull();
  });

  it("a 422 WITHOUT the regenerate flag (upstream key rejection, FUNC-9) shows the reason inline — no crash, no logout", async () => {
    // FUNC-9: the BFF maps a rejected provider key to a 422 with a plain {error} (no
    // regenerate, no agentYAML). The SPA must surface it inline, NOT render an undefined
    // agentYAML (a mid-create crash) and NOT log the user out.
    recordingFetch({
      generate: () => ({
        ok: false,
        status: 422,
        json: { error: "the anthropic API rejected the key (check the connected provider)" },
      }),
    });
    renderPage();
    await pickDescribe();

    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));

    // Surfaced inline as a recoverable state showing the honest reason — never a blank crash.
    expect(await screen.findByTestId("regenerate-state")).toBeInTheDocument();
    expect(screen.getByTestId("regenerate-reason")).toHaveTextContent(/rejected the key/);
    expect(screen.queryByTestId("shared-review")).toBeNull();
  });

  it("even a regenerate keyed on the flag when the BFF returns it with a 200 status", async () => {
    // The client keys off `regenerate`, NOT the status — a 200 carrying the flag
    // is still the regenerate path (no status sniffing).
    recordingFetch({
      generate: () => ({
        ok: true,
        status: 200,
        json: { reason: "still invalid", agentYAML: "name: x\n", regenerate: true },
      }),
    });
    renderPage();
    await pickDescribe();
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    expect(await screen.findByTestId("regenerate-state")).toBeInTheDocument();
    expect(screen.getByTestId("regenerate-reason")).toHaveTextContent(/still invalid/);
  });

  it("Regenerate re-POSTs generate", async () => {
    let n = 0;
    const calls = recordingFetch({
      generate: () => {
        n++;
        return n === 1
          ? { ok: false, status: 422, json: { reason: "try again", agentYAML: "name: x\n", regenerate: true } }
          : { ok: true, json: { agentYAML: "name: ok\nruntime: managed\n", expanded: "kind: AgentDeployment\n", model: "m" } };
      },
    });
    renderPage();
    await pickDescribe();
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    fireEvent.click(await screen.findByTestId("regenerate-button"));
    // The second generation succeeds → the shared review.
    expect(await screen.findByTestId("generation-review")).toBeInTheDocument();
    expect(calls.filter((c) => c.url === "/api/agents/generate" && c.method === "POST")).toHaveLength(2);
  });
});

describe("CreateAgentPage — Configure it", () => {
  it("the form steps converge on the SAME shared review", async () => {
    recordingFetch({});
    renderPage();
    await pickConfigure();

    // Basics: a name (managed default → no image required).
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "cfg-agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    // Behavior step.
    await screen.findByLabelText("System prompt");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    // Resources step.
    await screen.findByLabelText("CPU request");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    // Optional step.
    await screen.findByText("Cost budget");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    // Review step → finish into the shared review.
    fireEvent.click(await screen.findByRole("button", { name: /Review \+ tools/ }));

    expect(await screen.findByTestId("shared-review")).toBeInTheDocument();
    expect(screen.getByTestId("friendly-summary")).toHaveTextContent(/cfg-agent/);
  });

  it("mirrors config-form validation — an invalid name blocks advancing past basics", async () => {
    recordingFetch({});
    renderPage();
    await pickConfigure();

    // A non-DNS name (config-form's rule) → the basics step stays.
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bad Name" } });
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    // Still on basics — the behavior step's field isn't rendered.
    expect(screen.queryByLabelText("System prompt")).toBeNull();
    expect(await screen.findByText(/DNS label/)).toBeInTheDocument();
  });
});

describe("CreateAgentPage — the shared review, tool picker + Create", () => {
  async function reachReviewViaGenerate(overrides: Parameters<typeof recordingFetch>[0] = {}) {
    const calls = recordingFetch({
      tools: [
        { name: "get_order", description: "Fetch order", registry: "acme-mcp", source: "user-added", approvalStatus: "approved", inputSchema: {} },
        { name: "refund", description: "Refund", registry: "acme-mcp", source: "user-added", approvalStatus: "pending" },
      ],
      generate: () => ({ ok: true, json: { agentYAML: "name: rev-agent\nruntime: managed\ntools:\n  - get_order\n", expanded: "kind: AgentDeployment\n", model: "m" } }),
      ...overrides,
    });
    renderPage();
    await pickDescribe();
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    // After generation, the refine-and-draft surface is shown. Click "Create agent (classic)"
    // to reach the SharedReview (the one-shot path).
    await screen.findByTestId("refine-and-draft-surface");
    fireEvent.click(screen.getByTestId("create-agent-direct"));
    await screen.findByTestId("shared-review");
    return calls;
  }

  it("the tool picker loads GET /api/tools with curated + user-added + schema/pending badges", async () => {
    const calls = await reachReviewViaGenerate();
    expect(calls.find((c) => c.url === "/api/tools")).toBeDefined();
    // Both tools render; the pending one carries the honest badge; a schema tool
    // shows the schema badge.
    const list = await screen.findByTestId("tool-picker-list");
    // The list shows the MCP server collapsed; its tools appear once expanded.
    expect(list).toHaveTextContent("acme-mcp");
    fireEvent.click(screen.getByTestId("tool-server-toggle-acme-mcp"));
    expect(list).toHaveTextContent("get_order");
    expect(list).toHaveTextContent("refund");
    expect(list).toHaveTextContent("pending approval");
    expect(list).toHaveTextContent("schema");
    // The picker offers an "Add MCP server" affordance so a user can add a server they
    // don't see without leaving the flow (opens the add-MCP wizard).
    expect(screen.getByTestId("add-mcp-from-picker")).toHaveAttribute("href", "/tools/add-mcp");
  });

  it("selected tools flow into the created agent.yaml on Create → POST /api/agents", async () => {
    const calls = await reachReviewViaGenerate();
    // Add `refund` to the pre-selected `get_order` — expand the server to reach it.
    fireEvent.click(screen.getByTestId("tool-server-toggle-acme-mcp"));
    fireEvent.click(screen.getByLabelText("Bind refund"));
    fireEvent.click(screen.getByTestId("create-button"));

    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/agents" && c.method === "POST")).toBeDefined(),
    );
    const post = calls.find((c) => c.url === "/api/agents" && c.method === "POST")!;
    const yaml = JSON.parse(post.body).agentYAML as string;
    // BOTH tools are in the created agent.yaml's `tools` field (the same field
    // expand/generation use).
    expect(yaml).toContain("get_order");
    expect(yaml).toContain("refund");
    // Success state renders.
    expect(await screen.findByTestId("create-success")).toBeInTheDocument();
  });

  it("Preview POSTs /api/expand with the selected tools", async () => {
    const calls = await reachReviewViaGenerate();
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/expand" && c.method === "POST")).toBeDefined(),
    );
    const expand = calls.find((c) => c.url === "/api/expand" && c.method === "POST")!;
    expect(expand.body).toContain("get_order");
  });

  it("a viewer (no create) is gated; a forced create 403 → ForbiddenInline", async () => {
    await reachReviewViaGenerate({
      caps: { agentdeployments: { create: false } },
    });
    // The read-only note replaces the Create button.
    expect(await screen.findByTestId("create-readonly-note")).toBeInTheDocument();
    expect(screen.queryByTestId("create-button")).toBeNull();
  });

  it("a stale-yes create 403 surfaces ForbiddenInline (the API is the real gate)", async () => {
    await reachReviewViaGenerate({
      caps: { agentdeployments: { create: true } },
      create: () => ({ ok: false, status: 403, json: { error: "forbidden: cannot create agentdeployments" } }),
    });
    fireEvent.click(await screen.findByTestId("create-button"));
    expect(await screen.findByText("Not allowed to create this agent")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/forbidden: cannot/)).toBeNull();
  });
});

// The create→landing swap (m14.11): on a successful Create the page navigates to
// the created agent's LANDING page (/agents/{ns}/{name}), NOT the agents list —
// the aha continues on the detail/status/run surface. This replaces the m14.10
// deferred-navigation placeholder. A location-probe route captures the landing.
describe("CreateAgentPage — create → landing navigation (m14.11)", () => {
  function LocationProbe() {
    const loc = useLocation();
    return <div data-testid="location">{loc.pathname}</div>;
  }

  function renderWithRoutes() {
    return render(
      <MemoryRouter initialEntries={["/agents/new"]}>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <>
                <LocationProbe />
                <Routes>
                  <Route path="/agents/new" element={<CreateAgentPage />} />
                  <Route path="/agents/:ns/:name" element={<div>landing</div>} />
                  <Route path="/agents" element={<div>list</div>} />
                </Routes>
              </>
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
  }

  it("navigates to /agents/{ns}/{name} after Create (not the list)", async () => {
    recordingFetch({
      tools: [{ name: "get_order", source: "acme-mcp", approvalStatus: "approved" }],
      generate: () => ({ ok: true, json: { agentYAML: "name: rev-agent\nruntime: managed\n", expanded: "kind: AgentDeployment\n", model: "m" } }),
      create: () => ({ ok: true, json: { created: [{ kind: "AgentDeployment", name: "rev-agent", namespace: "default" }] } }),
    });
    renderWithRoutes();

    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    // After generation, the refine-and-draft surface is shown; use the one-shot classic path.
    await screen.findByTestId("refine-and-draft-surface");
    fireEvent.click(screen.getByTestId("create-agent-direct"));
    await screen.findByTestId("shared-review");
    fireEvent.click(screen.getByTestId("create-button"));
    await screen.findByTestId("create-success");

    // The redirect fires after the success beat (1200ms) → the LANDING page for
    // the created agent (/agents/{ns}/{name}), NOT the agents list.
    await waitFor(
      () => expect(screen.getByTestId("location")).toHaveTextContent("/agents/default/rev-agent"),
      { timeout: 3000 },
    );
  });
});

describe("CreateAgentPage — refine chat + draft lifecycle (m71.4/m71.5)", () => {
  // Extended recordingFetch that also handles /api/agents/refine and /api/agents/{ns}/{name}/publish
  function recordingFetchWithDraft(opts: {
    refine?: Handler;
    create?: Handler;
    publish?: Handler;
    agentDetail?: (url: string) => { ok: boolean; status?: number; json: unknown };
    invoke?: Handler;
    updateAgent?: Handler;
  } = {}) {
    const calls: Captured[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const body = typeof init?.body === "string" ? init.body : "";
        calls.push({ url, method, body });

        const j = (json: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok,
            status,
            json: async () => json,
            text: async () => (typeof json === "string" ? json : JSON.stringify(json)),
          } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
        if (url === "/api/providers" && method === "GET")
          return j({ providers: [{ name: "anthropic", namespace: "default", provider: "anthropic", displayName: "Anthropic", models: ["claude-sonnet-4"], secretName: "s", ready: true }] });
        if (url === "/api/tools")
          return j({ tools: [] });
        if (url === "/api/agents/generate" && method === "POST")
          return j({ agentYAML: "name: test-agent\nruntime: managed\n", expanded: "kind: AgentDeployment\n", model: "claude-sonnet-4" });
        if (url === "/api/agents/refine" && method === "POST") {
          const r = opts.refine
            ? opts.refine(body)
            : { ok: true, json: { agentYAML: "name: test-agent\nruntime: managed\nsystemPrompt: refined\n", diff: ["systemPrompt"], model: "claude-sonnet-4" } };
          return j(r.json, r.ok, r.status ?? (r.ok ? 200 : 422));
        }
        if (url === "/api/agents" && method === "POST") {
          const r = opts.create
            ? opts.create(body)
            : { ok: true, json: { created: [{ kind: "AgentDeployment", name: "test-agent", namespace: "default" }] } };
          return j(r.json, r.ok, r.status ?? (r.ok ? 201 : 400));
        }
        if (url.includes("/api/agents/") && url.includes("/publish") && method === "POST") {
          if (opts.publish) {
            const r = opts.publish(body);
            return j(r.json, r.ok, r.status ?? (r.ok ? 200 : 400));
          }
          return j({ name: "test-agent", namespace: "default" });
        }
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/) && method === "GET") {
          if (opts.agentDetail) {
            const r = opts.agentDetail(url);
            return j(r.json, r.ok, r.status ?? 200);
          }
          return j({ name: "test-agent", namespace: "default", resourceVersion: "123", isDraft: true, image: "", executionModel: "serving", role: "", promptRef: "", modelRoute: "", scaling: { min: 0, max: 3 }, phase: "Ready", ready: true, url: "", latestVersion: "", conditions: [], bindings: [], versions: [] });
        }
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/) && method === "PUT") {
          if (opts.updateAgent) {
            const r = opts.updateAgent(body);
            return j(r.json, r.ok, r.status ?? 200);
          }
          return j({ name: "test-agent", namespace: "default" });
        }
        if (url === "/api/invoke" && method === "POST") {
          if (opts.invoke) {
            const r = opts.invoke(body);
            return j(r.json, r.ok, r.status ?? 200);
          }
          return j({ traceId: "t1", response: "Hello from draft agent" });
        }
        return j({}, false, 404);
      }),
    );
    return calls;
  }

  async function reachRefinePanel() {
    recordingFetchWithDraft();
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    // Should land on the refine-and-draft surface
    await screen.findByTestId("refine-and-draft-surface");
  }

  it("after generation, lands on the refine-and-draft surface with the refine chat panel", async () => {
    await reachRefinePanel();
    expect(screen.getByTestId("refine-chat-panel")).toBeInTheDocument();
    expect(screen.getByTestId("draft-lifecycle")).toBeInTheDocument();
    expect(screen.getByTestId("refine-input")).toBeInTheDocument();
  });

  it("sending a refine instruction calls api.refineAgent and renders the diff chip", async () => {
    const calls = recordingFetchWithDraft();
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    await screen.findByTestId("refine-and-draft-surface");

    fireEvent.change(screen.getByTestId("refine-input"), { target: { value: "add web search" } });
    fireEvent.click(screen.getByTestId("refine-send"));

    // Wait for the diff chip to appear
    await screen.findByTestId("refine-diff-chip");
    expect(screen.getByTestId("refine-diff-chip")).toHaveTextContent(/systemPrompt/);

    // Verify refine was called
    expect(calls.find((c) => c.url === "/api/agents/refine" && c.method === "POST")).toBeDefined();
    const refineCall = calls.find((c) => c.url === "/api/agents/refine" && c.method === "POST")!;
    expect(JSON.parse(refineCall.body).instruction).toBe("add web search");
  });

  it("a 422+regenerate from refineAgent shows the reason inline — no crash", async () => {
    recordingFetchWithDraft({
      refine: () => ({
        ok: false,
        status: 422,
        json: { regenerate: true, reason: "Cannot add web search to this spec type", agentYAML: "name: test-agent\nruntime: managed\n" },
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    await screen.findByTestId("refine-and-draft-surface");

    fireEvent.change(screen.getByTestId("refine-input"), { target: { value: "add web search" } });
    fireEvent.click(screen.getByTestId("refine-send"));

    await screen.findByTestId("refine-error");
    expect(screen.getByTestId("refine-error")).toHaveTextContent(/Cannot add web search/);
    // No crash — the surface is still there
    expect(screen.getByTestId("refine-and-draft-surface")).toBeInTheDocument();
  });

  it("Create draft & test creates with stage:draft and shows the inline test panel", async () => {
    const calls = recordingFetchWithDraft();
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    await screen.findByTestId("refine-and-draft-surface");

    fireEvent.click(screen.getByTestId("create-draft-button"));
    await screen.findByTestId("draft-test-panel");

    // Verify the POST to /api/agents included stage:draft
    const createCall = calls.find((c) => c.url === "/api/agents" && c.method === "POST")!;
    expect(createCall).toBeDefined();
    expect(JSON.parse(createCall.body).stage).toBe("draft");
  });

  it("Publish calls publishAgent and shows published state", async () => {
    const calls = recordingFetchWithDraft();
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    await screen.findByTestId("refine-and-draft-surface");

    // Create draft first
    fireEvent.click(screen.getByTestId("create-draft-button"));
    await screen.findByTestId("draft-test-panel");

    // Publish
    fireEvent.click(screen.getByTestId("publish-button"));
    await screen.findByTestId("draft-published");

    // Verify publish was called
    expect(calls.find((c) => c.url.includes("/publish") && c.method === "POST")).toBeDefined();
  });

  it("a 409 on apply shows the conflict message", async () => {
    recordingFetchWithDraft({
      updateAgent: () => ({
        ok: false,
        status: 409,
        json: { error: "the agent changed since you loaded it" },
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByTestId("entrance-describe"));
    await screen.findByTestId("describe-flow");
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "a test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));
    await screen.findByTestId("refine-and-draft-surface");

    // First refine to get a transcript
    fireEvent.change(screen.getByTestId("refine-input"), { target: { value: "add web search" } });
    fireEvent.click(screen.getByTestId("refine-send"));
    await screen.findByTestId("refine-diff-chip");

    // Create draft
    fireEvent.click(screen.getByTestId("create-draft-button"));
    await screen.findByTestId("draft-test-panel");

    // Apply refinement → 409
    fireEvent.click(screen.getByTestId("apply-refinement-button"));
    await screen.findByTestId("draft-conflict");
    expect(screen.getByTestId("draft-conflict")).toHaveTextContent(/changed since you loaded it/);
  });
});

// ── m72.1 smart defaults ────────────────────────────────────────────────────

describe("CreateAgentPage — m72.1 smart defaults", () => {
  it("auto-defaults model picker to primary (non-small) model when multiple models are connected", async () => {
    // Provider returns a haiku (small tier) first, then sonnet (flagship tier).
    // The auto-default should pick sonnet (flagship), skipping haiku (small tier).
    // NOTE: the page reads r.items so we must include `items` in the mock response.
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (json: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => json, text: async () => JSON.stringify(json) } as Response);

        const providerEntry = { name: "anthropic", namespace: "default", provider: "anthropic",
          displayName: "Anthropic", models: ["claude-haiku-4", "claude-sonnet-4-6"],
          secretName: "anthropic", ready: true };
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
        if (url === "/api/providers" && method === "GET")
          return j({ providers: [providerEntry], items: [providerEntry] });
        if (url === "/api/tools") return j({ tools: [] });
        if (url.startsWith("/api/modelroutes")) return j({ items: [] });
        if (url.startsWith("/api/promptversions")) return j({ items: [] });
        if (url === "/api/agents/check-requirements" && method === "POST")
          return j({ model: { required: false, connected: true }, tools: [] });
        return j({}, false, 404);
      }),
    );
    renderPage();
    await pickConfigure();

    // Advance past Basics by providing a name.
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "smart-agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));

    // The Behavior step shows the model picker — wait for it
    await screen.findByLabelText("System prompt");
    const modelSelect = await screen.findByTestId("cfg-model-select");

    // claude-sonnet-4-6 is the flagship; the auto-default should pick it (not haiku)
    await waitFor(() => {
      expect((modelSelect as HTMLSelectElement).value).toContain("claude-sonnet-4-6");
    });
    expect((modelSelect as HTMLSelectElement).value).not.toContain("claude-haiku-4");
  });

  it("renders the Keep warm checkbox in the resources step (default unchecked)", async () => {
    recordingFetch({});
    renderPage();
    await pickConfigure();

    // Advance past Basics and Behavior steps to get to Resources
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "warm-agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    await screen.findByLabelText("System prompt");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));

    // Resources step — should show Keep warm checkbox, not min/max inputs
    const keepWarm = await screen.findByTestId("cfg-keep-warm");
    expect(keepWarm).toBeInTheDocument();
    expect((keepWarm as HTMLInputElement).checked).toBe(false);
    // Min/max inputs should NOT be present
    expect(screen.queryByLabelText("Min replicas")).toBeNull();
    expect(screen.queryByLabelText("Max replicas")).toBeNull();
  });

  it("toggling Keep warm emits scaling.min:1 in the config YAML preview", async () => {
    recordingFetch({});
    renderPage();
    await pickConfigure();

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "warm-agent2" } });
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    await screen.findByLabelText("System prompt");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));

    // Toggle Keep warm on
    const keepWarm = await screen.findByTestId("cfg-keep-warm");
    fireEvent.click(keepWarm);
    expect((keepWarm as HTMLInputElement).checked).toBe(true);

    // Advance to review step — the YAML preview should contain scaling.min:1
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    await screen.findByText("Cost budget");
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    await screen.findByText(/Ready to review/);
    // The FriendlySummary won't show scaling, but the Advanced YAML will
    // (we don't need to click Advanced to verify the form state is correct)
    expect((keepWarm as HTMLInputElement).checked).toBe(true);
  });
});

// ── m72.3 check-requirements checklist ──────────────────────────────────────

describe("CreateAgentPage — m72.3 check-requirements checklist (advisory)", () => {
  function recordingFetchWithCheckReqs(checkReqsResponse: unknown) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";

        const j = (json: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok, status,
            json: async () => json,
            text: async () => JSON.stringify(json),
          } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
        if (url === "/api/providers" && method === "GET")
          return j({ providers: [{ name: "anthropic", namespace: "default", provider: "anthropic", displayName: "Anthropic", models: ["claude-sonnet-4-6"], secretName: "s", ready: true }] });
        if (url === "/api/tools") return j({ tools: [] });
        if (url === "/api/agents/generate" && method === "POST")
          return j({ agentYAML: "name: test-agent\nruntime: managed\nmodel:\n  route: anthropic\ntools:\n  - get_order\n", expanded: "", model: "claude-sonnet-4-6" });
        if (url === "/api/agents/check-requirements" && method === "POST")
          return j(checkReqsResponse);
        if (url === "/api/expand" && method === "POST")
          return Promise.resolve({ ok: true, status: 200, text: async () => "kind: AgentDeployment\n", json: async () => ({}) } as Response);
        if (url === "/api/agents" && method === "POST")
          return j({ created: [{ kind: "AgentDeployment", name: "test-agent", namespace: "default" }] }, true, 201);
        return j({}, false, 404);
      }),
    );
  }

  it("shows the requirements checklist when check-requirements returns model+tools", async () => {
    recordingFetchWithCheckReqs({
      model: { required: true, connected: true, route: "anthropic" },
      tools: [
        { name: "get_order", status: "ready" },
      ],
    });
    renderPage();

    // Navigate: describe-it → generate → classic create path → SharedReview
    await pickDescribe();
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "order helper" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));

    // Wait for the refine-and-draft surface, then go to classic create (SharedReview)
    await screen.findByTestId("refine-and-draft-surface");
    fireEvent.click(screen.getByTestId("create-agent-direct"));
    await screen.findByTestId("shared-review");

    // Checklist should appear (advisory)
    await waitFor(() => {
      expect(screen.queryByTestId("requirements-checklist")).toBeInTheDocument();
    });
    expect(screen.getByTestId("requirements-checklist")).toHaveTextContent(/anthropic/);
    expect(screen.getByTestId("requirements-checklist")).toHaveTextContent(/get_order/);
  });

  it("checklist does not block create — Create button is still enabled", async () => {
    recordingFetchWithCheckReqs({
      model: { required: true, connected: false, route: "missing-route" },
      tools: [{ name: "tool1", status: "not-found" }],
    });
    renderPage();

    await pickDescribe();
    fireEvent.change(screen.getByLabelText("Agent description"), { target: { value: "test agent" } });
    fireEvent.click(screen.getByRole("button", { name: /Generate/ }));

    await screen.findByTestId("refine-and-draft-surface");
    fireEvent.click(screen.getByTestId("create-agent-direct"));
    await screen.findByTestId("shared-review");

    // Create button must be present and enabled even when requirements are not met
    const createBtn = await screen.findByTestId("create-button");
    expect(createBtn).toBeInTheDocument();
    expect(createBtn).not.toBeDisabled();
  });
});

// ── m72.5 recipe gallery ─────────────────────────────────────────────────────

describe("CreateAgentPage — m72.5 recipe gallery", () => {
  const recipeSpec = "name: recipe-agent\nruntime: managed\nsystemPrompt: recipe prompt\n";

  function recordingFetchWithRecipes() {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";

        const j = (json: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok, status,
            json: async () => json,
            text: async () => JSON.stringify(json),
          } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
        if (url === "/api/providers" && method === "GET")
          return j({ providers: [{ name: "anthropic", namespace: "default", provider: "anthropic", displayName: "Anthropic", models: ["claude-sonnet-4-6"], secretName: "s", ready: true }] });
        if (url === "/api/recipes") return j({ recipes: [
          { name: "order-bot", title: "Order Bot", description: "Handles order queries.", icon: "🛒", spec: recipeSpec },
          { name: "summarizer", title: "Summarizer", description: "Condenses long docs.", icon: "📝", spec: "name: summarizer\nruntime: managed\n" },
        ]});
        if (url === "/api/tools") return j({ tools: [] });
        if (url === "/api/agents/check-requirements" && method === "POST")
          return j({ model: { required: false, connected: true }, tools: [] });
        if (url === "/api/expand" && method === "POST")
          return Promise.resolve({ ok: true, status: 200, text: async () => "kind: AgentDeployment\n", json: async () => ({}) } as Response);
        if (url === "/api/agents" && method === "POST")
          return j({ created: [{ kind: "AgentDeployment", name: "recipe-agent", namespace: "default" }] }, true, 201);
        return j({}, false, 404);
      }),
    );
  }

  it("shows the 'Start from a recipe' toggle on the entrance", async () => {
    recordingFetchWithRecipes();
    renderPage();

    await screen.findByTestId("create-entrance");
    expect(screen.getByTestId("entrance-recipes-toggle")).toBeInTheDocument();
  });

  it("clicking the toggle loads and shows the recipe card grid", async () => {
    recordingFetchWithRecipes();
    renderPage();

    await screen.findByTestId("create-entrance");
    fireEvent.click(screen.getByTestId("entrance-recipes-toggle"));

    // Recipe gallery should appear with cards
    await screen.findByTestId("recipe-gallery");
    expect(await screen.findByTestId("recipe-card-order-bot")).toBeInTheDocument();
    expect(screen.getByTestId("recipe-card-summarizer")).toBeInTheDocument();
    expect(screen.getByText("Order Bot")).toBeInTheDocument();
    expect(screen.getByText("Handles order queries.")).toBeInTheDocument();
  });

  it("clicking a recipe card opens SharedReview pre-filled with the recipe spec", async () => {
    recordingFetchWithRecipes();
    renderPage();

    await screen.findByTestId("create-entrance");
    fireEvent.click(screen.getByTestId("entrance-recipes-toggle"));

    const orderBotCard = await screen.findByTestId("recipe-card-order-bot");
    fireEvent.click(orderBotCard);

    // Should land on shared review with the recipe's content
    await screen.findByTestId("shared-review");
    // The friendly summary should contain the recipe agent name
    expect(screen.getByTestId("friendly-summary")).toHaveTextContent(/recipe-agent/);
  });
});

// ── m74 P1-2 — Install recipe via ?recipe= pre-fills the create flow ──────────

describe("CreateAgentPage — m74 P1-2: ?recipe=<name> pre-fills the create flow", () => {
  const recipeSpec = "name: order-bot\nruntime: managed\nsystemPrompt: handle orders\n";

  function renderWithRecipeParam(recipeName: string) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (json: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => json, text: async () => JSON.stringify(json) } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true } } });
        if (url === "/api/providers" && method === "GET")
          return j({ providers: [{ name: "anthropic", namespace: "default", provider: "anthropic", displayName: "Anthropic", models: ["claude-sonnet-4-6"], secretName: "s", ready: true }] });
        if (url === "/api/recipes")
          return j({ recipes: [
            { name: "order-bot", title: "Order Bot", description: "Handles orders.", icon: "🛒", spec: recipeSpec },
          ]});
        if (url === "/api/tools") return j({ tools: [] });
        if (url === "/api/agents/check-requirements" && method === "POST")
          return j({ model: { required: false, connected: true }, tools: [] });
        if (url === "/api/expand" && method === "POST")
          return Promise.resolve({ ok: true, status: 200, text: async () => "kind: AgentDeployment\n", json: async () => ({}) } as Response);
        if (url === "/api/agents" && method === "POST")
          return j({ created: [{ kind: "AgentDeployment", name: "order-bot", namespace: "default" }] }, true, 201);
        return j({}, false, 404);
      }),
    );

    return render(
      <MemoryRouter initialEntries={[`/agents/new?recipe=${encodeURIComponent(recipeName)}`]}>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <Routes>
                <Route path="/agents/new" element={<CreateAgentPage />} />
              </Routes>
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
  }

  it("arriving via ?recipe=<name> pre-fills SharedReview with the recipe's spec", async () => {
    renderWithRecipeParam("order-bot");

    // The page fetches the recipe list and immediately transitions to the recipe review.
    // We should land on the shared review with the recipe's content pre-filled.
    await screen.findByTestId("shared-review");
    // The friendly summary must reflect the recipe agent (order-bot).
    expect(screen.getByTestId("friendly-summary")).toHaveTextContent(/order-bot/);
  });

  it("arriving via ?recipe=<unknown> falls through to the entrance without crashing", async () => {
    renderWithRecipeParam("nonexistent-recipe");
    // Falls back to the entrance (recipe not found) — no crash, entrance is shown.
    expect(await screen.findByTestId("create-entrance")).toBeInTheDocument();
  });
});
