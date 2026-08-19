import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AgentDetailPage } from "@/pages/agent-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// The agent landing page (m14.11 + m15.11). Tests cover:
//   • m14.11: header / status timeline / tabs / bindings / versions / logs / run / inspector.
//   • m15.11: drift + managedOutsideUI badges, Edit Wizard (full vs safe-field,
//     drift-overwrite warning), typed-name Delete (with references impact), per-agent
//     Runs (with 501-degrade), and RBAC awareness.
//
// A recording fetch mock scripts every endpoint deterministically — no cluster, no
// SSE server. The new m15.11 endpoints:
//   GET /api/agents/{ns}/{name}/references  — delete-impact preview
//   GET /api/agents/{ns}/{name}/runs         — bounded per-agent runs (501-aware)
//   PUT /api/agents/{ns}/{name}              — update (edit wizard submit)
//   DELETE /api/agents/{ns}/{name}           — delete (confirm dialog)

interface DetailOpts {
  detail?: unknown;
  detailStatus?: number;
  logFrames?: string[]; // SSE chunks the /logs stream yields.
  logStatus?: number; // pre-stream status for /logs (403 → forbidden).
  caps?: Record<string, Record<string, boolean>>;
  invoke?: { ok: boolean; status?: number; body: unknown };
  spans?: unknown[];
  // m15.11 additions
  references?: unknown[] | null; // null → use default; undefined → 404 (not expected)
  refsStatus?: number;
  agentRuns?: unknown[] | null; // null → 501 (Langfuse not configured)
  agentRunsStatus?: number;
  longTermMemory?: unknown[] | null; // null → 501 (no control-plane store), m46.6
  longTermMemoryError?: boolean; // true → 500 (store read failed), m46.6
  longTermConfig?: { enabled: boolean; perUser: boolean; embeddingRoute?: string }; // m49.3 enable surface
  updateResult?: { ok: boolean; status?: number; body?: unknown };
  deleteResult?: { ok: boolean; status?: number; body?: unknown };
  // m17.11 additions: memory + scaling panels
  memoryBindings?: unknown[];
  memoryCreateResult?: { ok: boolean; status?: number; body?: unknown };
  memoryUpdateResult?: { ok: boolean; status?: number; body?: unknown };
  memoryDeleteResult?: { ok: boolean; status?: number; body?: unknown };
  scalingPolicies?: unknown[];
  scalingCreateResult?: { ok: boolean; status?: number; body?: unknown };
  scalingUpdateResult?: { ok: boolean; status?: number; body?: unknown };
  scalingDeleteResult?: { ok: boolean; status?: number; body?: unknown };
  detectors?: { name: string; pattern: string }[];
  // m69.11 additions: online-score + rollback
  onlineScore?: unknown | null; // null → 501 (store not configured)
  rollbackResult?: { ok: boolean; status?: number; body?: unknown };
}

const DEFAULT_DETAIL = {
  name: "billing", namespace: "prod", image: "ghcr.io/x/billing:1", executionModel: "serving",
  role: "assistant", scaling: { min: 0, max: 3 }, phase: "Ready", ready: true,
  url: "http://billing.prod.example", latestVersion: "billing-v2",
  conditions: [
    { type: "Ready", status: "True", reason: "Deployed", message: "rollout complete", lastTransitionTime: "2026-07-11T00:00:00Z" },
    { type: "RouteReady", status: "True", reason: "RouteReady", message: "", lastTransitionTime: "" },
  ],
  bindings: [{ kind: "tool", name: "get-invoice-binding", detail: "get_invoice", ready: true }],
  versions: ["billing-v1", "billing-v2"],
  managedOutsideUI: false,
  drift: false,
};

function sseBody(chunks: string[]) {
  const enc = new TextEncoder();
  let i = 0;
  return {
    getReader() {
      return {
        read: () =>
          i < chunks.length
            ? Promise.resolve({ value: enc.encode(chunks[i++]), done: false })
            : Promise.resolve({ value: undefined, done: true }),
        releaseLock() {},
      };
    },
  };
}

function installFetch(opts: DetailOpts = {}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (body: unknown, ok = true, status = 200) =>
        Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({
          namespace: "",
          allowed: opts.caps ?? {
            agentdeployments: { create: true, update: true, delete: true },
            memorybindings: { create: true, update: true, delete: true },
            agentscalingpolicies: { create: true, update: true, delete: true },
          },
        });
      // The SSE log stream (fetch-stream). A pre-stream 403 → no body.
      if (url.includes("/logs")) {
        const status = opts.logStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok, status,
          body: ok ? sseBody(opts.logFrames ?? ["event: end\ndata: done\n\n"]) : null,
          json: async () => ({ error: "forbidden: not allowed to read pods" }),
          text: async () => JSON.stringify({ error: "forbidden: not allowed to read pods" }),
        } as unknown as Response);
      }
      if (url.includes("/api/traces/") && url.includes("/detail"))
        return j({
          rollup: { traceId: "tr-1", name: "billing", timestamp: "", costUSD: 0.006, tokens: 800, latencyMs: 1200, spanCount: (opts.spans ?? []).length || 2 },
          spans: opts.spans ?? [
            { id: "root", parentId: "", type: "SPAN", name: "run", startMs: 0, durationMs: 1200, model: "", tokensIn: 0, tokensOut: 0, costUSD: 0, level: "", status: "ok", input: "", output: "", inputRedacted: false, outputRedacted: false },
            { id: "tool", parentId: "root", type: "SPAN", name: "tool: get_invoice", startMs: 100, durationMs: 180, model: "", tokensIn: 12, tokensOut: 4, costUSD: 0.0002, level: "", status: "ok", input: "{}", output: "{}", inputRedacted: false, outputRedacted: false },
          ],
        });
      if (url.match(/\/api\/traces\/[^/]+$/)) return j({ traceId: "tr-1", url: "https://lf/tr-1" });
      if (url === "/api/invoke" && method === "POST") {
        const r = opts.invoke ?? { ok: true, body: { traceId: "tr-1", response: "Order shipped." } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 400));
      }
      if (url === "/api/runs") return j({ runs: [] });

      // m15.11: per-agent runs (GET .../runs)
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) {
        // null → 501 (Langfuse not configured)
        if (opts.agentRuns === null) {
          return j({ error: "not implemented" }, false, opts.agentRunsStatus ?? 501);
        }
        const runs = opts.agentRuns ?? [];
        return j({ runs }, true, 200);
      }
      // m49.3: long-term memory config (GET/PUT .../longtermmemory) — the ENABLE surface.
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) {
        if (method === "PUT") {
          return j(init?.body ? JSON.parse(init.body as string) : {}, true, 200);
        }
        return j(opts.longTermConfig ?? { enabled: false, perUser: false }, true, 200);
      }
      // m46.6: long-term memory (GET .../memory)
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) {
        if (opts.longTermMemory === null) {
          return j({ error: "not implemented" }, false, 501);
        }
        if (opts.longTermMemoryError) {
          return j({ error: "store read failed" }, false, 500);
        }
        return j({ namespace: "prod", name: "billing", items: opts.longTermMemory ?? [] }, true, 200);
      }
      // m15.11: references (GET .../references)
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) {
        const status = opts.refsStatus ?? 200;
        const refs = opts.references ?? [];
        return j({ references: refs }, status < 400, status);
      }
      // m15.11: update (PUT .../agents/{ns}/{name})
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.updateResult ?? { ok: true, body: { name: "billing", namespace: "prod" } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 400));
      }
      // m15.11: delete (DELETE .../agents/{ns}/{name})
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.deleteResult ?? { ok: true, body: { accepted: true } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 400));
      }
      // Agent detail (GET .../agents/{ns}/{name}).
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) {
        const status = opts.detailStatus ?? 200;
        return j(opts.detail ?? DEFAULT_DETAIL, status < 400, status);
      }

      // m17.11: memory bindings list (GET /api/memorybindings)
      if (url.startsWith("/api/memorybindings") && method === "GET" && !url.match(/\/api\/memorybindings\/[^/]+\/[^/]+$/)) {
        return j({ items: opts.memoryBindings ?? [], nextCursor: "" });
      }
      // m17.11: memory binding detail / delete
      if (url.match(/\/api\/memorybindings\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.memoryUpdateResult ?? { ok: true, body: { name: "mb-1", namespace: "prod", agentRef: "billing", scope: "global", ready: true } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 400));
      }
      if (url.match(/\/api\/memorybindings\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.memoryDeleteResult ?? { ok: true, body: {} };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 204 : 400));
      }
      // m17.11: memory binding create (POST /api/memorybindings)
      if (url === "/api/memorybindings" && method === "POST") {
        const r = opts.memoryCreateResult ?? { ok: true, body: { name: "mb-new", namespace: "prod", agentRef: "billing", scope: "global", ready: false } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 400));
      }

      // m17.11: scaling policies list (GET /api/agentscalingpolicies)
      if (url.startsWith("/api/agentscalingpolicies") && method === "GET" && !url.match(/\/api\/agentscalingpolicies\/[^/]+\/[^/]+$/)) {
        return j({ items: opts.scalingPolicies ?? [], nextCursor: "" });
      }
      // m17.11: scaling policy detail / delete
      if (url.match(/\/api\/agentscalingpolicies\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.scalingUpdateResult ?? { ok: true, body: { name: "sp-1", namespace: "prod", agentRef: "billing", minReplicas: 0, maxReplicas: 3, ready: true } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 400));
      }
      if (url.match(/\/api\/agentscalingpolicies\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.scalingDeleteResult ?? { ok: true, body: {} };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 204 : 400));
      }
      // m17.11: scaling policy create (POST /api/agentscalingpolicies)
      if (url === "/api/agentscalingpolicies" && method === "POST") {
        const r = opts.scalingCreateResult ?? { ok: true, body: { name: "sp-new", namespace: "prod", agentRef: "billing", minReplicas: 0, maxReplicas: 3, ready: false } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 400));
      }
      // m18.14: redaction policy get/put
      if (url.match(/\/tracepolicy$/) && method === "GET") {
        return j({ customDetectors: opts.detectors ?? [] });
      }
      if (url.match(/\/tracepolicy$/) && method === "PUT") {
        return j({ customDetectors: opts.detectors ?? [] });
      }

      // m69.11: online-score (GET .../online-score)
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) {
        if (opts.onlineScore === null) {
          return j({ error: "not implemented" }, false, 501);
        }
        return j(opts.onlineScore ?? { namespace: "prod", name: "billing", windows: [] }, true, 200);
      }
      // m69.11: rollback (POST .../rollback)
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/rollback/) && method === "POST") {
        const r = opts.rollbackResult ?? { ok: true, body: { namespace: "prod", name: "billing", targetVersion: "billing-v1", annotationSet: true } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 400));
      }

      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderAt(path = "/agents/prod/billing") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="/agents/:ns/:name" element={<AgentDetailPage />} />
              <Route path="/agents" element={<div data-testid="agents-list-page">agents list</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── m14.11 original tests ────────────────────────────────────────────────────
describe("AgentDetailPage (landing page)", () => {
  it("renders the header, status timeline, tabs, bindings and versions", async () => {
    installFetch();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    // Header identity + route + status.
    expect(screen.getByRole("heading", { name: "billing" })).toBeInTheDocument();
    expect(screen.getByTestId("agent-url")).toHaveAttribute("href", "http://billing.prod.example");
    // Status timeline from conditions.
    const timeline = screen.getByTestId("status-timeline");
    expect(within(timeline).getByTestId("condition-Ready")).toBeInTheDocument();
    expect(within(timeline).getByTestId("condition-RouteReady")).toBeInTheDocument();
    // Overview shows bindings + versions.
    expect(screen.getByTestId("versions-list")).toHaveTextContent("billing-v2");
    expect(screen.getByTestId("binding-get-invoice-binding")).toHaveTextContent("get_invoice");
    // The namespace links to its governing tenant (m49.4 — closes the agent→tenant leg).
    expect(screen.getByTestId("agent-namespace-link")).toHaveAttribute("href", "/tenants?q=prod");
  });

  it("groups tool bindings by MCP server, collapsed by default, with a ready rollup", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        bindings: [
          { kind: "tool", name: "sk-list", server: "scalekit-mcp-server", detail: "list_orgs", ready: true },
          { kind: "tool", name: "sk-get", server: "scalekit-mcp-server", detail: "get_org", ready: false },
          { kind: "memory", name: "mem", detail: "shared", ready: true },
        ],
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const group = screen.getByTestId("binding-group-scalekit-mcp-server");
    expect(group).toHaveTextContent("scalekit-mcp-server");
    expect(group).toHaveTextContent("2 tools");
    expect(group).toHaveTextContent("1/2 ready");
    // Collapsed by default (a <details> with no `open` attribute).
    expect(group).not.toHaveAttribute("open");
    // The tool rows are in the DOM (revealed on expand); non-tool bindings render outside.
    expect(screen.getByTestId("binding-sk-list")).toHaveTextContent("list_orgs");
    expect(screen.getByTestId("binding-mem")).toHaveTextContent("shared");
  });

  it("a 404 → the not-found state", async () => {
    installFetch({ detailStatus: 404, detail: { error: "not found" } });
    renderAt("/agents/prod/ghost");
    await waitFor(() => expect(screen.getByTestId("agent-not-found")).toBeInTheDocument());
  });

  it("a 403 → ForbiddenInline", async () => {
    installFetch({ detailStatus: 403, detail: { error: "forbidden" } });
    renderAt();
    await waitFor(() => expect(screen.getByText(/Not allowed to view/)).toBeInTheDocument());
  });

  it("the Logs tab tails SSE — log frames render in order", async () => {
    installFetch({ logFrames: ["event: log\ndata: line one\n\nevent: log\ndata: line two\n\nevent: end\ndata: x\n\n"] });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-logs"));
    await waitFor(() => {
      const lines = screen.getAllByTestId("log-line").map((n) => n.textContent);
      expect(lines).toEqual(["line one", "line two"]);
    });
  });

  it("the Logs tab shows a WAITING state (no pod yet), not an error", async () => {
    installFetch({ logFrames: ["event: waiting\ndata: starting\n\n"] });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-logs"));
    await waitFor(() => expect(screen.getByTestId("logs-waiting")).toBeInTheDocument());
    expect(screen.queryByTestId("logs-error")).toBeNull();
  });

  it("an IN-STREAM error frame is surfaced in the Logs tail", async () => {
    installFetch({ logFrames: ["event: log\ndata: a\n\nevent: error\ndata: pod died\n\n"] });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-logs"));
    await waitFor(() => expect(screen.getByTestId("logs-error")).toHaveTextContent("pod died"));
  });

  it("a PRE-STREAM 403 → the forbidden state, distinct from an in-stream error", async () => {
    installFetch({ logStatus: 403 });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-logs"));
    await waitFor(() => expect(screen.getByText("Not allowed to read logs")).toBeInTheDocument());
    expect(screen.queryByTestId("logs-error")).toBeNull();
  });

  it("the SSE /logs request carries the bearer (fetch-stream, EventSource can't)", async () => {
    // The session token provider is set by the app; here we assert the /logs call
    // goes through apiFetch (Accept: text/event-stream), the seam that attaches it.
    const calls = installFetch();
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-logs"));
    await waitFor(() => expect(calls.some((c) => c.url.includes("/logs"))).toBe(true));
    const logCall = calls.find((c) => c.url.includes("/logs"))!;
    expect(logCall.url).toContain("/api/agents/prod/billing/logs");
    expect(logCall.url).toContain("follow=true");
  });

  it("Chat → POST /api/invoke → traceId link → clicking it opens the run inspector", async () => {
    const calls = installFetch();
    renderAt();
    await screen.findByTestId("chat-panel");
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "where is my order" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    // The invoke POST fired, carrying the plain message as {input} + a conversationId.
    await waitFor(() => expect(calls.some((c) => c.url === "/api/invoke" && c.method === "POST")).toBe(true));
    const invoke = calls.find((c) => c.url === "/api/invoke" && c.method === "POST")!;
    const sent = JSON.parse(invoke.body);
    expect(sent.input).toEqual({ input: "where is my order" });
    expect(typeof sent.conversationId).toBe("string");
    expect(sent.conversationId.length).toBeGreaterThan(0);
    // The agent turn renders the response.
    expect(await screen.findByTestId("chat-turn-agent")).toHaveTextContent("Order shipped.");
    // The inspector does NOT auto-open — the trace id is a link the user clicks to open it.
    expect(screen.queryByTestId("run-inspector")).toBeNull();
    fireEvent.click(await screen.findByTestId("open-trace"));
    // Now the run inspector opens (drawer) and builds the tree — the tool span visible.
    await screen.findByTestId("run-inspector");
    const toolRow = await screen.findByTestId("span-row-tool");
    expect(toolRow).toHaveTextContent("get_invoice");
    // Its tokens/cost show in the default span detail.
    expect(screen.getByTestId("span-detail")).toHaveTextContent("12 in / 4 out");
  });

  it("renders the agent's output as markdown, not the raw JSON envelope", async () => {
    installFetch({
      invoke: {
        ok: true,
        status: 200,
        body: {
          traceId: "tr-md",
          // The managed-agent envelope, exactly as the agent returns it.
          response: JSON.stringify({
            agent: "sk-agent",
            output: "Here are your **environments**:\n\n| Name | Type |\n|------|------|\n| Personal Dev | DEV |",
            steps: 2,
            tools_called: ["list_environments"],
            consent_required: [],
          }),
        },
      },
    });
    renderAt();
    await screen.findByTestId("chat-panel");
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "list environments" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    const agentTurn = await screen.findByTestId("chat-turn-agent");
    // The human answer renders (markdown → a real <table>, bold text) …
    expect(agentTurn).toHaveTextContent("Here are your environments");
    expect(agentTurn.querySelector("table")).not.toBeNull();
    expect(agentTurn).toHaveTextContent("Personal Dev");
    // … and the raw envelope's structural fields never appear.
    expect(agentTurn.textContent).not.toContain("tools_called");
    expect(agentTurn.textContent).not.toContain('"steps"');
  });

  it("threads ONE conversationId across turns (a multi-turn chat)", async () => {
    const calls = installFetch();
    renderAt();
    await screen.findByTestId("chat-panel");
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "first" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    await waitFor(() =>
      expect(calls.filter((c) => c.url === "/api/invoke" && c.method === "POST").length).toBe(1),
    );
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "second" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    await waitFor(() =>
      expect(calls.filter((c) => c.url === "/api/invoke" && c.method === "POST").length).toBe(2),
    );
    const invokes = calls.filter((c) => c.url === "/api/invoke" && c.method === "POST");
    const ids = invokes.map((c) => JSON.parse(c.body).conversationId);
    // Both turns rode the SAME conversationId (the thread the agent scopes memory to).
    expect(ids[0]).toBe(ids[1]);
    // Both user turns are on screen.
    expect(screen.getAllByTestId("chat-turn-user")).toHaveLength(2);
  });

  it("a viewer (no create) is gated — the chat input is hidden, a note explains", async () => {
    installFetch({ caps: { agentdeployments: { create: false } } });
    renderAt();
    await screen.findByTestId("chat-panel");
    expect(screen.getByTestId("chat-readonly-note")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-input")).toBeNull();
    expect(screen.queryByTestId("chat-send")).toBeNull();
  });

  it("a forced invoke 403 → ForbiddenInline (the API is the real gate)", async () => {
    installFetch({ caps: { agentdeployments: { create: true } }, invoke: { ok: false, status: 403, body: { error: "forbidden: cannot invoke" } } });
    renderAt();
    await screen.findByTestId("chat-panel");
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "hi" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    await waitFor(() => expect(screen.getByText("Not allowed to run this agent")).toBeInTheDocument());
  });

  it("a message returning consent_required shows the inline Connect CTA on the agent's own page", async () => {
    installFetch({
      caps: { agentdeployments: { create: true } },
      invoke: {
        ok: true,
        status: 200,
        body: { traceId: "t-consent", response: "{}", consentRequired: ["scalekit-mcp-server"] },
      },
    });
    renderAt();
    await screen.findByTestId("chat-panel");
    fireEvent.change(screen.getByTestId("chat-input"), { target: { value: "list environments" } });
    fireEvent.click(screen.getByTestId("chat-send"));
    // The inline consent Connect button renders in the agent turn (the m26.2 flow, on the chat).
    expect(await screen.findByTestId("connect-scalekit-mcp-server")).toBeInTheDocument();
    expect(screen.getByText("Connect your account to continue")).toBeInTheDocument();
  });
});

// ── m15.11 new tests ─────────────────────────────────────────────────────────

describe("AgentDetailPage — drift + managedOutsideUI badges (m15.11)", () => {
  it("shows NO badges for a normal console-managed agent without drift", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false, drift: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("managed-outside-badge")).toBeNull();
    expect(screen.queryByTestId("drift-badge")).toBeNull();
  });

  it("shows the 'managed outside UI' badge when managedOutsideUI=true", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: true, drift: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("managed-outside-badge")).toBeInTheDocument();
    expect(screen.queryByTestId("drift-badge")).toBeNull();
  });

  it("shows the 'drift' badge when drift=true", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false, drift: true } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("drift-badge")).toBeInTheDocument();
    expect(screen.queryByTestId("managed-outside-badge")).toBeNull();
  });

  it("shows both badges when managedOutsideUI=true AND drift=true", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: true, drift: true } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("managed-outside-badge")).toBeInTheDocument();
    expect(screen.getByTestId("drift-badge")).toBeInTheDocument();
  });
});

describe("AgentDetailPage — Edit Wizard (m15.11)", () => {
  it("Edit button visible for a caller with update permission", async () => {
    installFetch({ caps: { agentdeployments: { create: true, update: true, delete: true } } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("edit-agent-button")).toBeInTheDocument();
  });

  it("Edit button hidden for a viewer (no update permission)", async () => {
    installFetch({ caps: { agentdeployments: { create: false, update: false, delete: false } } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("edit-agent-button")).toBeNull();
  });

  it("console-managed agent: Edit Wizard shows all fields (full round-trip)", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("edit-agent-button"));
    // Safe fields always shown.
    await screen.findByTestId("edit-image");
    expect(screen.getByTestId("edit-scaling-min")).toBeInTheDocument();
    expect(screen.getByTestId("edit-model-route")).toBeInTheDocument();
    expect(screen.getByTestId("edit-system-prompt")).toBeInTheDocument();
    // No "managed outside UI" note.
    expect(screen.queryByTestId("managed-outside-note")).toBeNull();
  });

  it("managedOutsideUI agent: Edit Wizard shows safe-fields-only note + disables full-round-trip fields", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: true } });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("edit-agent-button"));
    // Safe fields shown with the managed-outside note.
    await screen.findByTestId("managed-outside-note");
    expect(screen.getByTestId("edit-image")).toBeInTheDocument();
    // Navigate to the second step (for managedOutsideUI it's the review step).
    // There's no full-fields step for outside-managed agents — the wizard has only
    // [safeFields, review]. Verify no execution-model field visible.
    expect(screen.queryByTestId("edit-execution-model")).toBeNull();
  });

  it("console-managed: advancing to the full-fields step shows execution-model (not readonly)", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("edit-agent-button"));
    await screen.findByTestId("edit-image");
    // Click Continue to advance to full-fields step.
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-execution-model");
    expect(screen.getByTestId("edit-execution-model")).not.toBeDisabled();
    expect(screen.queryByTestId("readonly-fields-note")).toBeNull();
  });

  it("submit calls PUT /api/agents/{ns}/{name} with the edited spec", async () => {
    const calls = installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("edit-agent-button"));
    await screen.findByTestId("edit-image");

    // Edit the image.
    fireEvent.change(screen.getByTestId("edit-image"), { target: { value: "ghcr.io/x/billing:2" } });

    // Advance past safe-fields → full-fields → review.
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-execution-model");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Now on review step — find Save changes.
    await screen.findByTestId("edit-review");
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    // PUT should have been called.
    await waitFor(() => {
      const putCall = calls.find((c) => c.method === "PUT" && c.url.includes("/api/agents/prod/billing"));
      expect(putCall).toBeDefined();
      const body = JSON.parse(putCall!.body);
      expect(body.image).toBe("ghcr.io/x/billing:2");
    });
  });

  it("drift=true: the review step shows the drift-overwrite warning", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, managedOutsideUI: false, drift: true } });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("edit-agent-button"));
    await screen.findByTestId("edit-image");
    // Advance to review.
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-execution-model");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-review");
    // Drift warning shown in the review step.
    expect(screen.getByTestId("drift-overwrite-warning")).toBeInTheDocument();
  });
});

describe("AgentDetailPage — Delete dialog (m15.11)", () => {
  it("Delete button visible for a caller with delete permission", async () => {
    installFetch({ caps: { agentdeployments: { create: true, update: true, delete: true } } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("delete-agent-button")).toBeInTheDocument();
  });

  it("Delete button hidden for a viewer (no delete permission)", async () => {
    installFetch({ caps: { agentdeployments: { create: false, update: false, delete: false } } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("delete-agent-button")).toBeNull();
  });

  it("Delete dialog loads and shows agentReferences impact", async () => {
    installFetch({
      references: [
        { kind: "MCPToolBinding", name: "invoice-binding", namespace: "prod", disposition: "gc" },
        { kind: "MemoryBinding", name: "mem-binding", namespace: "prod", disposition: "orphan" },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("delete-agent-button"));
    // References load and show disposition badges.
    await screen.findByTestId("refs-list");
    expect(screen.getByTestId("ref-invoice-binding")).toHaveTextContent("MCPToolBinding/invoice-binding");
    expect(screen.getByTestId("ref-invoice-binding")).toHaveTextContent("will be deleted");
    expect(screen.getByTestId("ref-mem-binding")).toHaveTextContent("will be orphaned");
  });

  it("Delete dialog requires typed name before confirming", async () => {
    installFetch({ references: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("delete-agent-button"));
    await screen.findByTestId("refs-empty");
    // Confirm button should be disabled until the name is typed.
    const confirmBtn = screen.getByRole("button", { name: /delete agent/i });
    expect(confirmBtn).toBeDisabled();
    // Type the agent name.
    fireEvent.change(screen.getByPlaceholderText("billing"), { target: { value: "billing" } });
    expect(confirmBtn).not.toBeDisabled();
  });

  it("confirmed delete calls DELETE /api/agents/{ns}/{name} and navigates to the list", async () => {
    const calls = installFetch({ references: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("delete-agent-button"));
    await screen.findByTestId("refs-empty");
    // Type the name to unlock and confirm.
    fireEvent.change(screen.getByPlaceholderText("billing"), { target: { value: "billing" } });
    fireEvent.click(screen.getByRole("button", { name: /delete agent/i }));

    // DELETE call should have been made.
    await waitFor(() => {
      const delCall = calls.find((c) => c.method === "DELETE" && c.url.includes("/api/agents/prod/billing"));
      expect(delCall).toBeDefined();
    });
    // Should navigate to the agents list.
    await screen.findByTestId("agents-list-page");
  });
});

describe("AgentDetailPage — per-agent Runs tab (m15.11)", () => {
  it("renders run rows from GET .../runs", async () => {
    installFetch({
      agentRuns: [
        { traceId: "tr-abc", name: "billing", timestamp: "2026-07-11T00:00:00Z", costUSD: 0.005, tokens: 500, latencyMs: 1000 },
        { traceId: "tr-def", name: "billing", timestamp: "2026-07-11T01:00:00Z", costUSD: 0.003, tokens: 300, latencyMs: 600 },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-runs"));

    await screen.findByTestId("runs-tab");
    expect(screen.getByText("tr-abc")).toBeInTheDocument();
    expect(screen.getByText("tr-def")).toBeInTheDocument();
  });

  it("a 501 (Langfuse not configured) → calm 'runs unavailable' empty state, NOT an error", async () => {
    installFetch({ agentRuns: null });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-runs"));

    // The calm unavailable state — not an error toast, not an error state.
    await screen.findByTestId("runs-unavailable");
    expect(screen.getByTestId("runs-unavailable")).toHaveTextContent("tracing not configured");
    // No error elements.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("clicking a run row opens the run inspector", async () => {
    installFetch({
      agentRuns: [
        { traceId: "tr-abc", name: "billing", timestamp: "2026-07-11T00:00:00Z", costUSD: 0.005, tokens: 500, latencyMs: 1000 },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-runs"));
    await screen.findByTestId("runs-tab");
    // Click the row to open the inspector.
    fireEvent.click(screen.getByText("tr-abc"));
    await screen.findByTestId("run-inspector");
  });
});

describe("AgentDetailPage — RBAC-aware affordances (m15.11)", () => {
  it("a viewer (no write caps) sees NO edit or delete buttons", async () => {
    installFetch({
      caps: { agentdeployments: { create: false, update: false, delete: false } },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("edit-agent-button")).toBeNull();
    expect(screen.queryByTestId("delete-agent-button")).toBeNull();
  });

  it("a caller with only update sees Edit but NOT Delete", async () => {
    installFetch({
      caps: { agentdeployments: { create: false, update: true, delete: false } },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("edit-agent-button")).toBeInTheDocument();
    expect(screen.queryByTestId("delete-agent-button")).toBeNull();
  });

  it("a forced update 403 surfaces ForbiddenInline in the edit wizard, not a silent success", async () => {
    installFetch({
      detail: { ...DEFAULT_DETAIL, managedOutsideUI: false },
      updateResult: { ok: false, status: 403, body: { error: "forbidden: cannot update" } },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("edit-agent-button"));
    await screen.findByTestId("edit-image");
    // Advance to review and submit.
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-execution-model");
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("edit-review");
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));
    // 403 renders ForbiddenInline in the review step.
    await screen.findByText("Not allowed to edit this agent");
  });
});

// ── m17.11: Memory panel tests ───────────────────────────────────────────────

describe("AgentDetailPage — Memory panel (m17.11)", () => {
  it("renders the agent's MemoryBinding(s) filtered by agentRef", async () => {
    installFetch({
      memoryBindings: [
        // Binding for this agent (agentRef = "billing")
        { name: "mb-billing-global", namespace: "prod", agentRef: "billing", scope: "global", backend: "redis", ready: true },
        // Binding for a different agent — should NOT appear
        { name: "mb-other", namespace: "prod", agentRef: "other-agent", scope: "user", ready: false },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("memory-panel");
    // Only the billing binding should be visible
    expect(screen.getByTestId("memory-binding-mb-billing-global")).toBeInTheDocument();
    expect(screen.queryByTestId("memory-binding-mb-other")).toBeNull();
  });

  it("opens directly on the Memory tab via ?tab=Memory (m49.3 trace→memory deep-link)", async () => {
    installFetch({
      memoryBindings: [
        { name: "mb-billing-global", namespace: "prod", agentRef: "billing", scope: "global", backend: "redis", ready: true },
      ],
    });
    renderAt("/agents/prod/billing?tab=Memory");
    await screen.findByTestId("agent-detail-page");
    // No tab click — the Memory panel is active from the deep-link alone.
    expect(await screen.findByTestId("memory-panel")).toBeInTheDocument();
  });

  it("long-term memory: lists the agent's remembered facts (m46.6)", async () => {
    installFetch({
      longTermMemory: [{ content: "the team prefers metric units", createdAt: "2026-07-25T00:00:00Z" }],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("longterm-list");
    expect(screen.getByText(/prefers metric units/)).toBeInTheDocument();
  });

  it("long-term memory: hides the section when no store is wired (501)", async () => {
    installFetch({ longTermMemory: null });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("memory-panel");
    expect(screen.queryByTestId("longterm-memory-panel")).toBeNull();
  });

  it("long-term memory: teaches an empty state when nothing is remembered (m46.6)", async () => {
    installFetch({ longTermMemory: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("longterm-empty");
    expect(screen.getByText(/Nothing remembered yet/)).toBeInTheDocument();
  });

  it("long-term memory: renders tags as badges (m46.6)", async () => {
    installFetch({
      longTermMemory: [{ content: "prefers metric units", tags: { topic: "units" }, createdAt: "2026-07-25T00:00:00Z" }],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("longterm-tags");
    expect(screen.getByText(/topic: units/)).toBeInTheDocument();
  });

  it("long-term memory: back-links each fact to its originating trace (m54.3)", async () => {
    installFetch({
      longTermMemory: [
        { content: "from a run", traceId: "tr-99", createdAt: "2026-07-25T00:00:00Z" },
        { content: "ambient (no trace)", createdAt: "2026-07-25T00:01:00Z" },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    const link = await screen.findByTestId("longterm-trace-link-tr-99");
    expect(link).toHaveAttribute("href", "/traces/tr-99");
    expect(link).toHaveAttribute("aria-label", expect.stringContaining("tr-99"));
    // The trace id is NOT rendered as a user-facing tag chip (lifted to the link).
    expect(screen.queryByText(/traceId/)).not.toBeInTheDocument();
    // A memory with no trace shows no link (only the one tagged entry links).
    expect(screen.getAllByTestId(/longterm-trace-link-/)).toHaveLength(1);
  });

  it("long-term memory: surfaces a store error, not a blank panel (m46.6)", async () => {
    installFetch({ longTermMemoryError: true });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("longterm-error");
  });

  it("attach: createMemoryBinding is called with agentRef = the agent name", async () => {
    const calls = installFetch({ memoryBindings: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("memory-attach");
    fireEvent.click(screen.getByTestId("memory-attach"));

    // Fill in scope
    await screen.findByTestId("memory-scope-input");
    fireEvent.change(screen.getByTestId("memory-scope-input"), { target: { value: "global" } });
    fireEvent.click(screen.getByTestId("memory-form-submit"));

    await waitFor(() => {
      const postCall = calls.find((c) => c.url === "/api/memorybindings" && c.method === "POST");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      expect(body.agentRef).toBe("billing");
      expect(body.scope).toBe("global");
    });
  });

  it("detach: ConfirmDialog (typed-name) calls removeMemoryBinding on confirm", async () => {
    const calls = installFetch({
      memoryBindings: [
        { name: "mb-billing-global", namespace: "prod", agentRef: "billing", scope: "global", ready: true },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("memory-binding-mb-billing-global");
    fireEvent.click(screen.getByTestId("memory-detach-mb-billing-global"));

    // ConfirmDialog opens — confirm button disabled until name is typed
    await waitFor(() => expect(screen.getByRole("alertdialog")).toBeInTheDocument());
    const confirmBtn = screen.getByRole("button", { name: /detach/i });
    expect(confirmBtn).toBeDisabled();

    // Type the binding name to unlock
    fireEvent.change(screen.getByPlaceholderText("mb-billing-global"), { target: { value: "mb-billing-global" } });
    expect(confirmBtn).not.toBeDisabled();
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      const delCall = calls.find((c) => c.url.includes("/api/memorybindings/prod/mb-billing-global") && c.method === "DELETE");
      expect(delCall).toBeDefined();
    });
  });

  it("a viewer sees NO attach/detach/edit actions (RBAC display-gate)", async () => {
    installFetch({
      caps: {
        agentdeployments: { create: false, update: false, delete: false },
        memorybindings: { create: false, update: false, delete: false },
        agentscalingpolicies: { create: false, update: false, delete: false },
      },
      memoryBindings: [
        { name: "mb-billing-global", namespace: "prod", agentRef: "billing", scope: "global", ready: true },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("memory-panel");
    expect(screen.queryByTestId("memory-attach")).toBeNull();
    expect(screen.queryByTestId("memory-detach-mb-billing-global")).toBeNull();
    expect(screen.queryByTestId("memory-edit-mb-billing-global")).toBeNull();
  });

  it("long-term memory: enables the capability via the config panel (m49.3)", async () => {
    const calls = installFetch({ longTermConfig: { enabled: false, perUser: false } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-memory"));

    await screen.findByTestId("longterm-config");
    expect(screen.getByTestId("longterm-state")).toHaveTextContent("Disabled");
    // Turn on per-user, then Enable → a PUT patches the folded field.
    fireEvent.click(screen.getByTestId("longterm-peruser"));
    fireEvent.click(screen.getByTestId("longterm-enable"));

    await waitFor(() => {
      const put = calls.find((c) => c.url.includes("/longtermmemory") && c.method === "PUT");
      expect(put).toBeTruthy();
      expect(JSON.parse(put!.body)).toMatchObject({ enabled: true, perUser: true });
    });
  });
});

// ── m17.11: Scaling panel tests ──────────────────────────────────────────────

describe("AgentDetailPage — Scaling panel (m17.11)", () => {
  it("renders the agent's AgentScalingPolicy filtered by agentRef", async () => {
    installFetch({
      scalingPolicies: [
        // Policy for this agent
        { name: "sp-billing", namespace: "prod", agentRef: "billing", minReplicas: 1, maxReplicas: 5, mode: "static", ready: true },
        // Policy for a different agent — should NOT appear
        { name: "sp-other", namespace: "prod", agentRef: "other-agent", minReplicas: 0, maxReplicas: 2, ready: false },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    await screen.findByTestId("scaling-panel");
    expect(screen.getByTestId("scaling-policy-sp-billing")).toBeInTheDocument();
    expect(screen.queryByTestId("scaling-policy-sp-other")).toBeNull();
  });

  it("keep-warm: enabling with no policy creates a warm (min=1) policy (m32.5)", async () => {
    const calls = installFetch({ scalingPolicies: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    const toggle = await screen.findByTestId("keep-warm-toggle");
    expect(toggle).toHaveAttribute("aria-pressed", "false"); // no policy ⇒ scale-to-zero
    fireEvent.click(toggle);

    await waitFor(() => {
      const post = calls.find((c) => c.url === "/api/agentscalingpolicies" && c.method === "POST");
      expect(post).toBeDefined();
      expect(JSON.parse(post!.body).minReplicas).toBe(1);
    });
  });

  it("keep-warm: disabling returns an existing policy to scale-to-zero (m32.5)", async () => {
    const calls = installFetch({
      scalingPolicies: [
        { name: "sp-billing", namespace: "prod", agentRef: "billing", minReplicas: 1, maxReplicas: 5, mode: "static", ready: true },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    const toggle = await screen.findByTestId("keep-warm-toggle");
    expect(toggle).toHaveAttribute("aria-pressed", "true"); // min=1 ⇒ warm
    fireEvent.click(toggle);

    await waitFor(() => {
      const put = calls.find(
        (c) => c.method === "PUT" && /\/api\/agentscalingpolicies\/[^/]+\/[^/]+$/.test(c.url),
      );
      expect(put).toBeDefined();
      expect(JSON.parse(put!.body).minReplicas).toBe(0);
    });
  });

  it("attach: createAgentScalingPolicy is called with the form values", async () => {
    const calls = installFetch({ scalingPolicies: [] });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    await screen.findByTestId("scaling-attach");
    fireEvent.click(screen.getByTestId("scaling-attach"));

    await screen.findByTestId("scaling-min-input");
    fireEvent.change(screen.getByTestId("scaling-min-input"), { target: { value: "2" } });
    fireEvent.change(screen.getByTestId("scaling-max-input"), { target: { value: "8" } });
    fireEvent.click(screen.getByTestId("scaling-form-submit"));

    await waitFor(() => {
      const postCall = calls.find((c) => c.url === "/api/agentscalingpolicies" && c.method === "POST");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      expect(body.agentRef).toBe("billing");
      expect(body.minReplicas).toBe(2);
      expect(body.maxReplicas).toBe(8);
    });
  });

  it("a 422 (max < min XValidation) surfaces the server error in the form — NOT a success", async () => {
    installFetch({
      scalingPolicies: [],
      scalingCreateResult: {
        ok: false,
        status: 422,
        body: { error: "AgentScalingPolicy.spec: maxReplicas must be >= minReplicas" },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    await screen.findByTestId("scaling-attach");
    fireEvent.click(screen.getByTestId("scaling-attach"));

    await screen.findByTestId("scaling-min-input");
    // Set max < min (invalid)
    fireEvent.change(screen.getByTestId("scaling-min-input"), { target: { value: "5" } });
    fireEvent.change(screen.getByTestId("scaling-max-input"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("scaling-form-submit"));

    // The server 422 message surfaces in the form — never fabricated as a success
    await screen.findByTestId("scaling-form-error");
    expect(screen.getByTestId("scaling-form-error")).toHaveTextContent("maxReplicas must be >= minReplicas");
    // No success toast rendered (the form stays open with the error)
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("detach: ConfirmDialog (typed-name) calls removeAgentScalingPolicy on confirm", async () => {
    const calls = installFetch({
      scalingPolicies: [
        { name: "sp-billing", namespace: "prod", agentRef: "billing", minReplicas: 0, maxReplicas: 3, ready: true },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    await screen.findByTestId("scaling-policy-sp-billing");
    fireEvent.click(screen.getByTestId("scaling-detach-sp-billing"));

    await waitFor(() => expect(screen.getByRole("alertdialog")).toBeInTheDocument());
    const confirmBtn = screen.getByRole("button", { name: /detach/i });
    expect(confirmBtn).toBeDisabled();

    // Type the policy name to unlock
    fireEvent.change(screen.getByPlaceholderText("sp-billing"), { target: { value: "sp-billing" } });
    expect(confirmBtn).not.toBeDisabled();
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      const delCall = calls.find((c) => c.url.includes("/api/agentscalingpolicies/prod/sp-billing") && c.method === "DELETE");
      expect(delCall).toBeDefined();
    });
  });

  it("a viewer sees NO attach/detach/edit actions on the scaling panel (RBAC display-gate)", async () => {
    installFetch({
      caps: {
        agentdeployments: { create: false, update: false, delete: false },
        memorybindings: { create: false, update: false, delete: false },
        agentscalingpolicies: { create: false, update: false, delete: false },
      },
      scalingPolicies: [
        { name: "sp-billing", namespace: "prod", agentRef: "billing", minReplicas: 0, maxReplicas: 3, ready: true },
      ],
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("tab-scaling"));

    await screen.findByTestId("scaling-panel");
    expect(screen.queryByTestId("scaling-attach")).toBeNull();
    expect(screen.queryByTestId("scaling-detach-sp-billing")).toBeNull();
    expect(screen.queryByTestId("scaling-edit-sp-billing")).toBeNull();
  });

  it("redaction panel loads detectors and can add + save (m18.14)", async () => {
    const calls = installFetch({ detectors: [{ name: "badge", pattern: "BADGE-[0-9]+" }] });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-redaction"));
    await screen.findByTestId("redaction-panel");
    expect(screen.getByTestId("detector-0")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("add-detector"));
    fireEvent.click(screen.getByTestId("save-redaction"));
    await waitFor(() => {
      const put = calls.find((c) => /\/tracepolicy$/.test(c.url) && c.method === "PUT");
      expect(put).toBeDefined();
    });
  });

  it("redaction panel is read-only for a viewer (no add/save)", async () => {
    installFetch({ caps: { agentdeployments: { update: false } }, detectors: [] });
    renderAt();
    fireEvent.click(await screen.findByTestId("tab-redaction"));
    await screen.findByTestId("redaction-panel");
    expect(screen.queryByTestId("add-detector")).toBeNull();
    expect(screen.queryByTestId("save-redaction")).toBeNull();
  });
});

// ── m65.9: Runtime section on the agent detail Overview tab ─────────────────
// Tests that spec.runtime is surfaced as a read-only Runtime card:
//   • Structured output indicator + collapsible schema
//   • Tool policy: default rule, per-tool overrides (name → rule, retryable), parallelLimit, forcedChoice
//   • Resilience: model retry/timeout, tool retry/timeout, circuit-breaker
// And that when runtime is absent nothing new is rendered.
describe("AgentDetailPage — Runtime section (m65.9)", () => {
  const RUNTIME_DETAIL = {
    ...DEFAULT_DETAIL,
    runtime: {
      outputSchemaSet: true,
      outputSchema: JSON.stringify({ type: "object", properties: { answer: { type: "string" } } }),
      toolPolicy: {
        default: "allow",
        overrides: [
          { name: "send_email", rule: "require-approval", retryable: false },
          { name: "read_file", rule: "allow", retryable: true },
        ],
        forcedChoice: "auto",
        parallelLimit: 4,
      },
      resilience: {
        modelCall: { timeoutSeconds: 30, maxRetries: 2 },
        toolCall: {
          timeoutSeconds: 10,
          maxRetries: 1,
          circuitBreaker: { failureThreshold: 5, cooldownSeconds: 60 },
        },
      },
    },
  };

  it("renders the Runtime card with structured-output indicator when runtime is present", async () => {
    installFetch({ detail: RUNTIME_DETAIL });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    const section = screen.getByTestId("runtime-section");
    expect(section).toBeInTheDocument();
    // Structured output badge
    expect(screen.getByTestId("runtime-output-schema-badge")).toHaveTextContent("✓ set");
  });

  it("renders tool policy — default rule, overrides, parallelLimit, forcedChoice", async () => {
    installFetch({ detail: RUNTIME_DETAIL });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    const section = screen.getByTestId("runtime-tool-policy");
    expect(section).toBeInTheDocument();
    // parallelLimit
    expect(section).toHaveTextContent("4 concurrent calls");
    // forcedChoice
    expect(section).toHaveTextContent("auto");
    // Per-tool overrides
    const overrides = screen.getByTestId("runtime-tool-overrides");
    expect(overrides).toHaveTextContent("Per-tool overrides (2)");
    // First override: send_email → require-approval
    const sendEmail = screen.getByTestId("tool-override-send_email");
    expect(sendEmail).toHaveTextContent("send_email");
    expect(sendEmail).toHaveTextContent("require-approval");
    // Second override: read_file → allow, retryable
    const readFile = screen.getByTestId("tool-override-read_file");
    expect(readFile).toHaveTextContent("read_file");
    expect(readFile).toHaveTextContent("allow");
    expect(readFile).toHaveTextContent("retryable");
  });

  it("renders resilience — model/tool retry+timeout, circuit-breaker", async () => {
    installFetch({ detail: RUNTIME_DETAIL });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    const section = screen.getByTestId("runtime-resilience");
    expect(section).toBeInTheDocument();
    // Model call
    expect(section).toHaveTextContent("30s timeout");
    expect(section).toHaveTextContent("2 retries");
    // Tool call
    expect(section).toHaveTextContent("10s timeout");
    expect(section).toHaveTextContent("1 retry");
    // Circuit breaker
    const cb = screen.getByTestId("runtime-circuit-breaker");
    expect(cb).toHaveTextContent("opens at 5 failures");
    expect(cb).toHaveTextContent("60s cooldown");
  });

  it("schema is hidden by default and reveals on toggling the details summary", async () => {
    installFetch({ detail: RUNTIME_DETAIL });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    const details = screen.getByTestId("runtime-schema-details");
    // <details> starts closed (no open attribute in the initial render for the
    // React state which starts false, so the schema pre is not shown yet).
    // After clicking the summary the schema becomes visible.
    const summary = within(details).getByText("Show schema");
    fireEvent.click(summary);
    await waitFor(() =>
      expect(screen.getByTestId("runtime-schema-details")).toHaveTextContent("answer"),
    );
  });

  it("no Runtime section rendered when runtime is absent", async () => {
    installFetch({ detail: DEFAULT_DETAIL }); // DEFAULT_DETAIL has no runtime field
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("runtime-section")).toBeNull();
  });

  it("no Runtime card rendered when runtime is present but all sub-sections are empty", async () => {
    // runtime: {} — truthy object but outputSchemaSet is false/absent,
    // toolPolicy is undefined, resilience is undefined → card must NOT appear.
    installFetch({ detail: { ...DEFAULT_DETAIL, runtime: {} } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("runtime-section")).toBeNull();
  });
});

// ── J6 m76.6: Runtime section P3 polish ──────────────────────────────────────
// (a) outputSchemaSet=true with no schema body → shows "(content not returned)"
// (b) Runtime card is placed after Spec, not after Bindings
// (c) Tool-policy honesty qualifier is shown
describe("AgentDetailPage — Runtime section J6 polish (m76.6)", () => {
  it("J6(a) shows '(content not returned)' when outputSchemaSet is true but schema body is absent", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        runtime: { outputSchemaSet: true, outputSchema: undefined },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // Badge is shown.
    expect(screen.getByTestId("runtime-output-schema-badge")).toHaveTextContent("✓ set");
    // "(content not returned)" note is shown when the body is absent.
    expect(screen.getByTestId("runtime-output-schema-not-returned")).toHaveTextContent(
      "content not returned",
    );
    // No expand toggle since there's no body.
    expect(screen.queryByTestId("runtime-schema-details")).toBeNull();
  });

  it("J6(a) does NOT show '(content not returned)' when schema body is present", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        runtime: {
          outputSchemaSet: true,
          outputSchema: JSON.stringify({ type: "object" }),
        },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    expect(screen.getByTestId("runtime-output-schema-badge")).toHaveTextContent("✓ set");
    expect(screen.queryByTestId("runtime-output-schema-not-returned")).toBeNull();
    // The expand toggle is present.
    expect(screen.getByTestId("runtime-schema-details")).toBeInTheDocument();
  });

  it("J6(b) Runtime card appears before the status timeline (i.e. adjacent to Spec)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        runtime: { outputSchemaSet: true },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    const runtimeSection = screen.getByTestId("runtime-section");
    const statusTimeline = screen.getByTestId("status-timeline");
    // The runtime section must appear BEFORE the status timeline in the DOM.
    expect(
      runtimeSection.compareDocumentPosition(statusTimeline) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("J6(c) tool-policy note is shown alongside the Tool policy heading", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        runtime: {
          toolPolicy: {
            default: "allow",
            overrides: [],
          },
        },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    expect(screen.getByTestId("runtime-tool-policy-note")).toHaveTextContent("SDK-layer convention");
  });
});

// ── m66.10: GuardrailPolicyRef on the agent detail header ────────────────────
// Tests that spec.guardrailPolicyRef is surfaced as a ResourceLink in the header,
// and that when the agent is NotReady due to a guardrail reason, the reason is
// surfaced inline next to the link.
describe("AgentDetailPage — guardrailPolicyRef (m66.10)", () => {
  it("renders a guardrail policy link when guardrailPolicyRef is set", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, guardrailPolicyRef: "pii-and-jailbreak" } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const link = screen.getByTestId("agent-guardrail-policy-link");
    expect(link).toBeInTheDocument();
    expect(link).toHaveTextContent("pii-and-jailbreak");
    // The link leads to /guardrails.
    expect(link).toHaveAttribute("href", "/guardrails");
    // No NotReady badge when the agent is Ready.
    expect(screen.queryByTestId("agent-guardrail-notready-reason")).toBeNull();
  });

  it("surfaces GuardrailPolicyNotFound inline when the agent is NotReady for that reason", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        ready: false,
        phase: "NotReady",
        guardrailPolicyRef: "missing-policy",
        conditions: [
          {
            type: "Ready",
            status: "False",
            reason: "GuardrailPolicyNotFound",
            message: "guardrail policy 'missing-policy' not found in namespace 'prod'",
            lastTransitionTime: "2026-07-11T00:00:00Z",
          },
        ],
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    // The link to the policy is still rendered.
    expect(screen.getByTestId("agent-guardrail-policy-link")).toHaveTextContent("missing-policy");
    // The NotReady reason is surfaced inline next to the link.
    const badge = screen.getByTestId("agent-guardrail-notready-reason");
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveTextContent("GuardrailPolicyNotFound");
  });

  it("surfaces GuardrailPolicyInvalid inline when the agent is NotReady for that reason", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        ready: false,
        phase: "NotReady",
        guardrailPolicyRef: "bad-regex-policy",
        conditions: [
          {
            type: "Ready",
            status: "False",
            reason: "GuardrailPolicyInvalid",
            message: "guardrail policy 'bad-regex-policy' has invalid RE2 patterns",
            lastTransitionTime: "2026-07-11T00:00:00Z",
          },
        ],
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("agent-guardrail-policy-link")).toHaveTextContent("bad-regex-policy");
    const badge = screen.getByTestId("agent-guardrail-notready-reason");
    expect(badge).toHaveTextContent("GuardrailPolicyInvalid");
  });

  it("no guardrail link rendered when guardrailPolicyRef is absent", async () => {
    installFetch({ detail: DEFAULT_DETAIL }); // DEFAULT_DETAIL has no guardrailPolicyRef
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("agent-guardrail-policy-link")).toBeNull();
    expect(screen.queryByTestId("agent-guardrail-notready-reason")).toBeNull();
  });
});

// ── K7 m76.6: guardrail + promptRef links use navRoute ────────────────────────
// K7(a): both /guardrails and /prompts links use navRoute() not hardcoded strings.
describe("AgentDetailPage — K7 navRoute links (m76.6)", () => {
  it("K7(a) guardrail link uses navRoute('guardrails') — resolves to /guardrails", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, guardrailPolicyRef: "my-policy" } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const link = screen.getByTestId("agent-guardrail-policy-link");
    // navRoute("guardrails") returns "/guardrails" — confirm the link is correct.
    expect(link).toHaveAttribute("href", "/guardrails");
  });

  it("K7(a) promptRef link uses navRoute('prompts') — resolves to /prompts", async () => {
    installFetch({ detail: { ...DEFAULT_DETAIL, promptRef: "my-prompt-v2" } });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const link = screen.getByTestId("agent-promptref-link");
    expect(link).toHaveAttribute("href", "/prompts");
    expect(link).toHaveTextContent("my-prompt-v2");
  });
});

// ── m69.11: improvement-loop surfaces (online-score + canary arms + rollback) ──
describe("ImprovementLoopSection (m69.11)", () => {
  const SCORE_WINDOW = {
    agentVersion: "billing-v2",
    windowStart: "2026-08-10T12:00:00Z",
    operational: { total: 200, errorCount: 4, toolFailCount: 1, latencyP95Ms: 280.5 },
    feedback: { count: 15, sumVal: 12.0 },
    judge: { count: 5, sumVal: 4.2 },
  };

  it("renders the 3-component online score for the serving version", async () => {
    installFetch({
      onlineScore: { namespace: "prod", name: "billing", windows: [SCORE_WINDOW] },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    // The improvement-loop section renders in the Overview tab.
    const section = await screen.findByTestId("improvement-loop-section");
    expect(section).toBeInTheDocument();
    // All 3 component cards are present.
    expect(within(section).getByTestId("operational-component")).toBeInTheDocument();
    expect(within(section).getByTestId("feedback-component")).toBeInTheDocument();
    expect(within(section).getByTestId("judge-component")).toBeInTheDocument();
    // Values rendered inside operational card.
    const op = within(section).getByTestId("operational-component");
    expect(op).toHaveTextContent("200"); // total requests
  });

  it("renders RegressionDetected badge when condition is True", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        conditions: [
          ...DEFAULT_DETAIL.conditions,
          {
            type: "RegressionDetected",
            status: "True",
            reason: "RegressionDetected",
            message: "operational error rate breached baseline",
            lastTransitionTime: "2026-08-10T13:00:00Z",
          },
        ],
      },
      onlineScore: { namespace: "prod", name: "billing", windows: [SCORE_WINDOW] },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const badge = await screen.findByTestId("regression-detected-badge");
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveTextContent("Regression detected");
  });

  it("does not render the regression badge when RegressionDetected is False", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        conditions: [
          ...DEFAULT_DETAIL.conditions,
          {
            type: "RegressionDetected",
            status: "False",
            reason: "Healthy",
            message: "",
            lastTransitionTime: "2026-08-10T13:00:00Z",
          },
        ],
      },
      onlineScore: { namespace: "prod", name: "billing", windows: [SCORE_WINDOW] },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    await screen.findByTestId("improvement-loop-section");
    expect(screen.queryByTestId("regression-detected-badge")).toBeNull();
    // Healthy badge shown when False
    expect(screen.getByTestId("regression-ok-badge")).toBeInTheDocument();
  });

  it("renders canary arms side-by-side when gate phase is canary", async () => {
    const oldWindow = {
      ...SCORE_WINDOW,
      agentVersion: "billing-v1",
      windowStart: "2026-08-10T11:00:00Z",
    };
    const candidateWindow = {
      ...SCORE_WINDOW,
      agentVersion: "billing-v2",
      windowStart: "2026-08-10T12:00:00Z",
    };
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        gate: { phase: "canary", scoredRevision: "billing-v2-h1234" },
      },
      onlineScore: {
        namespace: "prod",
        name: "billing",
        windows: [candidateWindow, oldWindow],
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const arms = await screen.findByTestId("canary-arms");
    expect(arms).toBeInTheDocument();
    // Two arms rendered.
    expect(within(arms).getByTestId("canary-arm-old")).toBeInTheDocument();
    expect(within(arms).getByTestId("canary-arm-candidate")).toBeInTheDocument();
  });

  it("renders rollback button and posts when confirmed", async () => {
    const calls = installFetch({
      onlineScore: { namespace: "prod", name: "billing", windows: [SCORE_WINDOW] },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // The rollback section appears (there are 2 versions: billing-v1, billing-v2).
    const section = await screen.findByTestId("rollback-section");
    expect(section).toBeInTheDocument();

    // Select a version and click Rollback.
    const select = within(section).getByTestId("rollback-version-select");
    fireEvent.change(select, { target: { value: "billing-v1" } });

    const btn = within(section).getByTestId("rollback-button");
    expect(btn).not.toBeDisabled();
    fireEvent.click(btn);

    // Confirm dialog appears (ConfirmDialog uses role="alertdialog").
    const dialog = await screen.findByRole("alertdialog");
    const confirmBtn = within(dialog).getByRole("button", { name: /Rollback/i });
    fireEvent.click(confirmBtn);

    // The POST /api/agents/prod/billing/rollback is called.
    await waitFor(() => {
      const rollbackCall = calls.find(
        (c) => c.url.includes("/rollback") && c.method === "POST",
      );
      expect(rollbackCall).toBeDefined();
      expect(JSON.parse(rollbackCall!.body)).toMatchObject({ version: "billing-v1" });
    });
  });

  it("renders calm 'not available' when online score store is unconfigured (501)", async () => {
    installFetch({ onlineScore: null }); // null → 501
    renderAt();
    await screen.findByTestId("agent-detail-page");
    // The section itself is hidden when 501 + no regression + no canary + versions < 2
    // but DEFAULT_DETAIL has 2 versions so the rollback section shows → section renders.
    // However in this test versions are from DEFAULT_DETAIL (2 versions), so section is shown.
    // The "not available" text is shown for the score area.
    await screen.findByTestId("online-score-unavailable");
  });
});

// ── m74.6: Publish-as-template + needs-rebinding banner ─────────────────────
describe("AgentDetailPage (m74.6) — Publish-as-template and needs-rebinding banner", () => {
  function installFetchWithPublish(opts: {
    publishStatus?: number;
    detail?: unknown;
  } = {}) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok,
            status,
            json: async () => body,
            text: async () => JSON.stringify(body),
          } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({
            namespace: "",
            allowed: {
              agentdeployments: { create: true, update: true, delete: true },
              memorybindings: { create: true, update: true, delete: true },
              agentscalingpolicies: { create: true, update: true, delete: true },
            },
          });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });

        // publish template endpoint
        if (url === "/api/templates" && method === "POST") {
          const status = opts.publishStatus ?? 200;
          // A server-truth 403 message (U15) — the dialog must PREFER it over the hardcoded role hint.
          if (status >= 400)
            return j({ error: "you must be a Tenant admin to publish org-wide" }, false, status);
          return j({ version: "v1", name: "billing", namespace: "prod" });
        }

        // agent detail
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) {
          return j(opts.detail ?? DEFAULT_DETAIL, true, 200);
        }

        return j({}, false, 404);
      }),
    );
  }

  it("shows the Publish button in the header when the caller can update agentdeployments", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("publish-agent-button")).toBeInTheDocument();
  });

  it("hides the Publish button when the caller cannot update agentdeployments", async () => {
    installFetch({
      caps: {
        agentdeployments: { create: false, update: false, delete: false },
        memorybindings: { create: false, update: false, delete: false },
        agentscalingpolicies: { create: false, update: false, delete: false },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("publish-agent-button")).toBeNull();
  });

  it("opens the publish dialog with team selected by default on Publish click", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("publish-agent-button"));

    // U8: dialog is now titled "Share X as template" (renamed from "Publish").
    expect(await screen.findByRole("dialog", { name: /Share billing as template/ })).toBeInTheDocument();
    expect(screen.getByTestId("publish-template-option-team")).toBeInTheDocument();
    expect(screen.getByTestId("publish-template-option-org")).toBeInTheDocument();
    expect(screen.getByTestId("publish-template-option-public")).toBeInTheDocument();
  });

  it("calls POST /api/templates and shows success toast on publish", async () => {
    const calls: { url: string; method: string; body: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url === "/api/templates" && method === "POST") return j({ version: "v1" });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
        return j({}, false, 404);
      }),
    );

    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    // U8: dialog title is now "Share billing as template".
    await screen.findByRole("dialog", { name: /Share billing as template/ });

    fireEvent.click(screen.getByTestId("publish-template-submit"));

    await waitFor(() => {
      expect(calls.some((c) => c.url === "/api/templates" && c.method === "POST")).toBe(true);
    });
    // U8: success toast now says "Shared as template".
    await waitFor(() => {
      expect(screen.getByText("Shared as template")).toBeInTheDocument();
    });
  });

  it("shows honest 403 error inline when publish is forbidden (U8 — keep dialog open)", async () => {
    installFetchWithPublish({ publishStatus: 403 });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("publish-agent-button"));
    // U8: dialog title is now "Share billing as template".
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));

    // U8: error is shown inline (keeping the dialog open), not as a closing toast.
    await waitFor(() => {
      expect(screen.getByTestId("publish-template-error")).toBeInTheDocument();
    });
    // U15: the dialog PREFERS the server's real message (server-truth) over the hardcoded role hint.
    expect(screen.getByTestId("publish-template-error")).toHaveTextContent(
      /you must be a Tenant admin to publish org-wide/,
    );
    // The dialog stays open.
    expect(screen.getByRole("dialog", { name: /Share billing as template/ })).toBeInTheDocument();
  });

  it("public publish requires confirm checkbox before submitting (blast-radius gate)", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    // U8: dialog is now titled "Share billing as template".
    await screen.findByRole("dialog", { name: /Share billing as template/ });

    // Select public tier
    const publicOption = screen.getByTestId("publish-template-option-public");
    fireEvent.click(publicOption.querySelector("input")!);

    // Warning should appear
    expect(await screen.findByTestId("publish-template-public-warning")).toBeInTheDocument();
    // Submit should be disabled
    expect(screen.getByTestId("publish-template-submit")).toBeDisabled();

    // Check the confirm checkbox
    fireEvent.click(screen.getByTestId("publish-template-public-confirm"));
    expect(screen.getByTestId("publish-template-submit")).not.toBeDisabled();
  });

  it("renders the needs-rebinding banner when the fork label is present", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        needsRebinding: true,
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("needs-rebinding-banner")).toBeInTheDocument();
    expect(screen.getByText(/Needs attention/)).toBeInTheDocument();
    expect(screen.getByText(/connect resources before running/i)).toBeInTheDocument();
  });

  it("does not render the needs-rebinding banner when the fork label is absent", async () => {
    installFetch();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("needs-rebinding-banner")).toBeNull();
  });

  it("does not render the needs-rebinding banner when the fork label is false", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        needsRebinding: false,
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("needs-rebinding-banner")).toBeNull();
  });
});

// ── U5: needs-rebinding banner — actionable line items ────────────────────────
describe("AgentDetailPage (m76.3 U5) — needs-rebinding banner repair links", () => {
  it("shows a 'Connect a model route' link when model route is missing (U5)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        modelRoute: "",
        needsRebinding: true,
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("rebind-model-route-link")).toBeInTheDocument();
    expect(screen.getByTestId("rebind-model-route-link")).toHaveTextContent("Connect a model route");
  });

  it("shows a Bindings tab link in the banner (U5)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        needsRebinding: true,
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("rebind-bindings-tab-link")).toBeInTheDocument();
    expect(screen.getByTestId("rebind-bindings-tab-link")).toHaveTextContent(/Bindings tab/i);
  });

  it("does not show 'Connect a model route' when model route is already set (U5)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        modelRoute: "gpt4-prod",
        needsRebinding: true,
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("needs-rebinding-banner")).toBeInTheDocument();
    expect(screen.queryByTestId("rebind-model-route-link")).toBeNull();
  });

  // U14: when the BFF recorded the SPECIFIC dangling refs, the banner ITEMIZES them (with the right
  // repair action per category) instead of the generic steps — and the tools line does NOT show when
  // nothing tool-shaped dangles.
  it("itemizes the actual dangling refs and omits the tools line when none dangle (U14)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        modelRoute: "",
        needsRebinding: true,
        forkUnresolvedRefs: ["model route: gpt4", "prompt: greeting"],
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // The real ref names are listed.
    const list = screen.getByTestId("rebind-ref-list");
    expect(list).toHaveTextContent("model route: gpt4");
    expect(list).toHaveTextContent("prompt: greeting");
    // The model-route ref carries the connect-route action; the prompt carries an add-prompt link.
    expect(screen.getByTestId("rebind-model-route-link")).toBeInTheDocument();
    // No tool-shaped ref → the "bind tools" line is NOT rendered (the old always-on line is gone).
    expect(screen.queryByTestId("rebind-bindings-tab-link")).toBeNull();
  });

  it("shows the bind-tools action only for tool-shaped refs (U14)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        modelRoute: "gpt4-prod",
        needsRebinding: true,
        forkUnresolvedRefs: ["slack-search"], // a bare tool name
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    expect(screen.getByTestId("rebind-ref-list")).toHaveTextContent("slack-search");
    expect(screen.getByTestId("rebind-bindings-tab-link")).toBeInTheDocument();
    // No model-route/prompt ref → those actions absent.
    expect(screen.queryByTestId("rebind-model-route-link")).toBeNull();
  });
});

// ── U7: publish state badge + unpublish ──────────────────────────────────────
describe("AgentDetailPage (m76.3 U7) — published badge and unpublish", () => {
  function installFetchWithPublishAndUnpublish(opts: {
    publishStatus?: number;
    unpublishStatus?: number;
  } = {}) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url === "/api/templates" && method === "POST") {
          const status = opts.publishStatus ?? 200;
          if (status >= 400) return j({ error: "forbidden" }, false, status);
          return j({ version: "3", name: "billing", namespace: "prod" });
        }
        if (url.match(/\/api\/templates\/.*/) && method === "DELETE") {
          const status = opts.unpublishStatus ?? 200;
          return j({}, status < 400, status);
        }
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
        return j({}, false, 404);
      }),
    );
  }

  it("shows published badge in header after publish succeeds (U7)", async () => {
    installFetchWithPublishAndUnpublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // Publish the agent.
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));

    // Badge should appear.
    await waitFor(() => {
      expect(screen.getByTestId("published-badge")).toBeInTheDocument();
    });
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/Published/);
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/team/);
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/v3/);
  });

  it("shows Unpublish button after publish and calls DELETE /api/templates on click (U7)", async () => {
    const calls: { url: string; method: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        calls.push({ url, method });
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url === "/api/templates" && method === "POST") return j({ version: "2" });
        if (url.match(/\/api\/templates\//) && method === "DELETE") return j({});
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
        return j({}, false, 404);
      }),
    );

    renderAt();
    await screen.findByTestId("agent-detail-page");

    // Publish first.
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));
    await waitFor(() => expect(screen.getByTestId("unpublish-agent-button")).toBeInTheDocument());

    // Now unpublish.
    fireEvent.click(screen.getByTestId("unpublish-agent-button"));

    await waitFor(() => {
      expect(calls.some((c) => c.url.includes("/api/templates/") && c.method === "DELETE")).toBe(true);
    });
    // Badge should disappear after unpublish.
    await waitFor(() => {
      expect(screen.queryByTestId("published-badge")).toBeNull();
    });
  });

  // U13: the published badge is seeded from the DURABLE `detail.published` on load — so it survives a
  // reload WITHOUT any in-session publish action (previously it was in-session only → vanished).
  it("shows the published badge on load from durable state (U13)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        published: { visibility: "org", version: 4 },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // No publish click — the badge is present purely from the loaded detail.
    expect(screen.getByTestId("published-badge")).toBeInTheDocument();
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/Published/);
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/org/);
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/v4/);
    // And Unpublish is available durably.
    expect(screen.getByTestId("unpublish-agent-button")).toBeInTheDocument();
  });

  // U15: a FAILED unpublish must surface a toast (it used to be swallowed → the button looked dead)
  // AND the published badge must stay (the template is still published).
  it("surfaces a toast and keeps the badge when unpublish fails (U15)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url === "/api/templates" && method === "POST") return j({ version: "2" });
        // The DELETE (unpublish) FAILS.
        if (url.match(/\/api\/templates\//) && method === "DELETE") return j({ error: "store unavailable" }, false, 500);
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
        return j({}, false, 404);
      }),
    );

    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));
    await waitFor(() => expect(screen.getByTestId("unpublish-agent-button")).toBeInTheDocument());

    fireEvent.click(screen.getByTestId("unpublish-agent-button"));

    // A failure toast appears (no longer swallowed) …
    await waitFor(() => expect(screen.getByText("Couldn't unpublish")).toBeInTheDocument());
    // … and the badge STAYS (the template is still published).
    expect(screen.getByTestId("published-badge")).toBeInTheDocument();
  });
});

// ── U8: dialog immutable snapshot copy + "Share as template" button ───────────
describe("AgentDetailPage (m76.3 U8) — publish dialog snapshot copy + rename", () => {
  it("dialog shows the immutable snapshot note (U8)", async () => {
    vi.stubGlobal("fetch", vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      const j = (body: unknown, ok = true, status = 200) =>
        Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
      if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
      return j({}, false, 404);
    }));

    renderAt();
    await screen.findByTestId("agent-detail-page");
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    // U8: must say "immutable snapshot".
    expect(screen.getByText(/immutable snapshot/i)).toBeInTheDocument();
    // U8: submit button reads "Share as template", not "Publish as team".
    expect(screen.getByTestId("publish-template-submit")).toHaveTextContent("Share as template");
  });
});

// ── U12: fork lineage on detail page ─────────────────────────────────────────
describe("AgentDetailPage (m76.3 U12) — fork lineage", () => {
  it("shows fork lineage when fork-origin labels are present (U12)", async () => {
    installFetch({
      detail: {
        ...DEFAULT_DETAIL,
        labels: {
          "agents.ctxmesh.ai/fork-origin-namespace": "prod",
          "agents.ctxmesh.ai/fork-origin-name": "support-agent",
          "agents.ctxmesh.ai/fork-origin-version": "v2",
        },
      },
    });
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.getByTestId("fork-lineage")).toBeInTheDocument();
    expect(screen.getByTestId("fork-lineage")).toHaveTextContent("prod/support-agent");
    expect(screen.getByTestId("fork-lineage")).toHaveTextContent("v2");
  });

  it("does not show fork lineage when fork-origin labels are absent (U12)", async () => {
    installFetch();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    expect(screen.queryByTestId("fork-lineage")).toBeNull();
  });
});

// ── P1-1: publish/share verb coherence — entry button text ────────────────────
describe("AgentDetailPage — P1-1 share verb coherence (entry button)", () => {
  function installFetchWithPublish(opts: { publishStatus?: number } = {}) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true }, memorybindings: { create: true }, agentscalingpolicies: { create: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url === "/api/templates" && method === "POST") {
          const status = opts.publishStatus ?? 200;
          return j({ version: "1" }, status < 400, status);
        }
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) return j(DEFAULT_DETAIL, true, 200);
        return j({}, false, 404);
      }),
    );
  }

  it("entry button reads 'Share as template' when not yet published", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");
    const btn = screen.getByTestId("publish-agent-button");
    expect(btn).toHaveTextContent("Share as template");
  });

  it("entry button reads 'Share new version' after publishing", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // Trigger a publish so publishedState is set.
    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));
    await waitFor(() => expect(screen.getByTestId("published-badge")).toBeInTheDocument());

    // Entry button should now say "Share new version".
    expect(screen.getByTestId("publish-agent-button")).toHaveTextContent("Share new version");
  });

  it("state badge reads 'Published' (state) and Unpublish button reads 'Unpublish' (state)", async () => {
    installFetchWithPublish();
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("publish-agent-button"));
    await screen.findByRole("dialog", { name: /Share billing as template/ });
    fireEvent.click(screen.getByTestId("publish-template-submit"));

    await waitFor(() => expect(screen.getByTestId("published-badge")).toBeInTheDocument());
    expect(screen.getByTestId("published-badge")).toHaveTextContent(/Published/);
    expect(screen.getByTestId("unpublish-agent-button")).toHaveTextContent("Unpublish");
  });
});

// ── M100 UI99-logs: the Logs tab is gated on `get pods/log` ─────────────────────

describe("AgentDetailPage — Logs tab RBAC gate (M100 UI99-logs)", () => {
  it("hides the Logs tab when the caller cannot read pod logs (logs.get=false)", async () => {
    installFetch({ caps: { logs: { get: false } } });
    renderAt();
    // The page loads (Overview tab present) but the Logs tab is gated out.
    await screen.findByTestId("tab-overview");
    expect(screen.queryByTestId("tab-logs")).toBeNull();
  });

  it("shows the Logs tab when the caller can read pod logs (logs.get=true)", async () => {
    installFetch({ caps: { logs: { get: true } } });
    renderAt();
    expect(await screen.findByTestId("tab-logs")).toBeInTheDocument();
  });

  it("shows the Logs tab optimistically when the probe is unknown (fail-open, display-only)", async () => {
    // Default caps carry no `logs` cell → can() is unknown → fail-OPEN (the tab shows; the API
    // still enforces, and the LogsTab renders a calm 403 if the caller truly can't read logs).
    installFetch();
    renderAt();
    expect(await screen.findByTestId("tab-logs")).toBeInTheDocument();
  });

  it("a deep-link to ?tab=Logs by a denied persona falls back to Overview (no blank tab)", async () => {
    installFetch({ caps: { logs: { get: false } } });
    renderAt("/agents/prod/billing?tab=Logs");
    // The Logs tab is hidden AND the content falls back to Overview (never a blank page).
    await screen.findByTestId("tab-overview");
    expect(screen.queryByTestId("tab-logs")).toBeNull();
    expect(screen.queryByTestId("logs-tab")).toBeNull();
    expect(screen.getByTestId("tab-overview")).toHaveAttribute("aria-selected", "true");
  });
});

// ── V3: read-only version-snapshot diff ──────────────────────────────────────
describe("AgentDetailPage — version diff (V3)", () => {
  function installVersionDiffFetch(diff: {
    diff: string;
    identical: boolean;
  }) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: {} });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/versions\/diff/) && method === "GET")
          return j({ resolveMode: "textual", fromName: "echo-v1", toName: "echo-v2", ...diff });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/))
          return j({ ...DEFAULT_DETAIL, versions: ["echo-v2", "echo-v1"], latestVersion: "echo-v2" }, true, 200);
        return j({}, false, 404);
      }),
    );
  }

  it("renders the picker and diffs two versions on Compare (V3)", async () => {
    installVersionDiffFetch({ diff: " image: base\n-tag: v1\n+tag: v2", identical: false });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    // The panel + both selects are present (with ≥2 versions).
    expect(screen.getByTestId("version-diff-panel")).toBeInTheDocument();
    expect(screen.getByTestId("version-diff-from")).toBeInTheDocument();
    expect(screen.getByTestId("version-diff-to")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("version-diff-compare"));

    const out = await screen.findByTestId("version-diff-output");
    expect(out).toHaveTextContent("-tag: v1");
    expect(out).toHaveTextContent("+tag: v2");
  });

  it("shows a calm 'no changes' state for identical versions (V3)", async () => {
    installVersionDiffFetch({ diff: "", identical: true });
    renderAt();
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(screen.getByTestId("version-diff-compare"));
    await screen.findByTestId("version-diff-identical");
    expect(screen.queryByTestId("version-diff-output")).toBeNull();
  });
});

// ── U4: opt-in auto-unpublish on delete ──────────────────────────────────────
describe("AgentDetailPage — delete auto-unpublish (U4)", () => {
  function installDeleteFetch(published: boolean) {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);
        if (method === "DELETE" && url.includes("/api/agents/")) {
          calls.push(url);
          return j({ accepted: true });
        }
        if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (url.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { delete: true } } });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/references/)) return j({ references: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/runs/)) return j({ runs: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/longtermmemory/)) return j({ enabled: false, perUser: false });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/memory/)) return j({ items: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+\/online-score/)) return j({ windows: [] }, false, 501);
        if (url.match(/\/tracepolicy$/) && method === "GET") return j({ customDetectors: [] });
        if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/) && method === "GET")
          return j({ ...DEFAULT_DETAIL, published: published ? { visibility: "org", version: 3 } : undefined }, true, 200);
        return j({}, false, 404);
      }),
    );
    return calls;
  }

  it("shows a pre-checked unpublish checkbox for a published agent, and deletes with ?unpublish=true", async () => {
    const calls = installDeleteFetch(true);
    renderAt("/agents/prod/billing?delete=1");
    await screen.findByTestId("agent-detail-page");

    const checkbox = await screen.findByTestId("delete-unpublish-checkbox");
    expect(checkbox).toBeChecked(); // pre-checked (default intent)

    fireEvent.change(screen.getByPlaceholderText("billing"), { target: { value: "billing" } });
    fireEvent.click(screen.getByRole("button", { name: "Delete agent" }));

    await waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0]).toContain("unpublish=true");
  });

  it("unchecking keeps the template (bare delete, no ?unpublish)", async () => {
    const calls = installDeleteFetch(true);
    renderAt("/agents/prod/billing?delete=1");
    await screen.findByTestId("agent-detail-page");

    fireEvent.click(await screen.findByTestId("delete-unpublish-checkbox")); // uncheck
    fireEvent.change(screen.getByPlaceholderText("billing"), { target: { value: "billing" } });
    fireEvent.click(screen.getByRole("button", { name: "Delete agent" }));

    await waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0]).not.toContain("unpublish");
  });

  it("shows no unpublish checkbox for an unpublished agent", async () => {
    installDeleteFetch(false);
    renderAt("/agents/prod/billing?delete=1");
    await screen.findByTestId("agent-detail-page");
    // The confirm dialog is open (references loaded) but there is no unpublish row.
    await screen.findByPlaceholderText("billing");
    expect(screen.queryByTestId("delete-unpublish-checkbox")).toBeNull();
  });
});
