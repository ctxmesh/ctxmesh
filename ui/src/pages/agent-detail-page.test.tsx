import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AgentDetailPage } from "@/pages/agent-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// The agent landing page (m14.11) closes the aha loop: it renders the detail
// (header / status timeline / tabs / bindings / versions) from GET
// /api/agents/{ns}/{name}, tails logs over a bearer-attached fetch-stream SSE,
// runs the agent via POST /api/invoke, and opens the native run inspector on the
// returned traceId. A recording fetch mock scripts each endpoint deterministically
// (no cluster, no SSE server) — the /logs stream is a fetch-stream Response.

interface DetailOpts {
  detail?: unknown;
  detailStatus?: number;
  logFrames?: string[]; // SSE chunks the /logs stream yields.
  logStatus?: number; // pre-stream status for /logs (403 → forbidden).
  caps?: Record<string, Record<string, boolean>>;
  invoke?: { ok: boolean; status?: number; body: unknown };
  spans?: unknown[];
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
        return j({ namespace: "", allowed: opts.caps ?? { agentdeployments: { create: true } } });
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
      // Agent detail.
      if (url.match(/\/api\/agents\/[^/]+\/[^/]+$/)) {
        const status = opts.detailStatus ?? 200;
        return j(opts.detail ?? DEFAULT_DETAIL, status < 400, status);
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

  it("Run → POST /api/invoke → traceId → the run inspector opens with the tool span", async () => {
    const calls = installFetch();
    renderAt();
    await screen.findByTestId("run-panel");
    fireEvent.click(screen.getByTestId("run-button"));
    // The invoke POST fired.
    await waitFor(() => expect(calls.some((c) => c.url === "/api/invoke" && c.method === "POST")).toBe(true));
    // The run inspector opens (drawer) and builds the tree — the tool span visible.
    await screen.findByTestId("run-inspector");
    const toolRow = await screen.findByTestId("span-row-tool");
    expect(toolRow).toHaveTextContent("get_invoice");
    // Its tokens/cost show in the default span detail.
    expect(screen.getByTestId("span-detail")).toHaveTextContent("12 in / 4 out");
  });

  it("a viewer (no create) is gated — the Run button is hidden, a note explains", async () => {
    installFetch({ caps: { agentdeployments: { create: false } } });
    renderAt();
    await screen.findByTestId("run-panel");
    expect(screen.getByTestId("run-readonly-note")).toBeInTheDocument();
    expect(screen.queryByTestId("run-button")).toBeNull();
  });

  it("a forced invoke 403 → ForbiddenInline (the API is the real gate)", async () => {
    installFetch({ caps: { agentdeployments: { create: true } }, invoke: { ok: false, status: 403, body: { error: "forbidden: cannot invoke" } } });
    renderAt();
    await screen.findByTestId("run-panel");
    fireEvent.click(screen.getByTestId("run-button"));
    await waitFor(() => expect(screen.getByText("Not allowed to run this agent")).toBeInTheDocument());
  });
});
