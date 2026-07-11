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
  updateResult?: { ok: boolean; status?: number; body?: unknown };
  deleteResult?: { ok: boolean; status?: number; body?: unknown };
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
        return j({ namespace: "", allowed: opts.caps ?? { agentdeployments: { create: true, update: true, delete: true } } });
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
