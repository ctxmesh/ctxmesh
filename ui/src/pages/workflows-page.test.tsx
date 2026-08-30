import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { WorkflowsPage } from "@/pages/workflows-page";
import type { WorkflowSummary } from "@/lib/api";

// WorkflowsPage (m67.9, m67.15) — the Workflow CR list surface (read-only, caller-scoped)
// with invoke affordance (m67.15): per-row Run button + invoke panel + createWorkflowRun.

// WorkflowsPage (m67.9) — the Workflow CR list surface (read-only, caller-scoped).

function wf(over: Partial<WorkflowSummary> = {}): WorkflowSummary {
  return {
    name: "my-pipeline",
    namespace: "default",
    stepCount: 3,
    registryRef: "prod-registry",
    validated: true,
    specHash: "sha256-abc",
    ...over,
  };
}

interface FetchRoute {
  list?: { ok: boolean; status?: number; body?: unknown };
  createRun?: { ok: boolean; status?: number; body?: unknown };
}

function installFetch(respond: () => { ok: boolean; status?: number; body?: unknown }): void;
function installFetch(routes: FetchRoute): { calls: { url: string; method: string; body: string }[] };
function installFetch(
  arg: (() => { ok: boolean; status?: number; body?: unknown }) | FetchRoute,
): { calls: { url: string; method: string; body: string }[] } | void {
  if (typeof arg === "function") {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        const r = arg();
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 500),
          json: async () => r.body ?? { items: [] },
          text: async () => JSON.stringify(r.body ?? { error: "err" }),
        } as Response);
      }),
    );
    return;
  }
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });

      // List workflows
      if (path === "/api/workflows" && method === "GET") {
        const listResp = arg.list ?? { ok: true, body: { items: [] } };
        return Promise.resolve({
          ok: listResp.ok,
          status: listResp.status ?? (listResp.ok ? 200 : 500),
          json: async () => listResp.body ?? { items: [] },
          text: async () => JSON.stringify(listResp.body ?? { error: "err" }),
        } as Response);
      }
      // createWorkflowRun: POST /api/workflows/{name}/runs
      if (path.startsWith("/api/workflows/") && path.endsWith("/runs") && method === "POST") {
        const runResp = arg.createRun ?? { ok: true, body: { id: "run-wf-1", status: "queued" } };
        return Promise.resolve({
          ok: runResp.ok,
          status: runResp.status ?? (runResp.ok ? 202 : 500),
          json: async () => runResp.body ?? {},
          text: async () => JSON.stringify(runResp.body ?? { error: "err" }),
        } as Response);
      }
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}), text: async () => "" } as Response);
    }),
  );
  return { calls };
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/workflows"]}>
      <Routes>
        <Route path="/workflows" element={<WorkflowsPage />} />
        <Route path="/traces/:id" element={<div data-testid="trace-page">Trace page</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("WorkflowsPage (m67.9)", () => {
  it("renders workflows with step count, registry, namespace, and validated status badge", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          wf({ name: "my-pipeline", validated: true, stepCount: 3, registryRef: "prod-registry" }),
          wf({ name: "broken-wf", validated: false, reason: "DanglingEdge", stepCount: 1, registryRef: "dev-registry" }),
        ],
      },
    }));

    renderPage();

    expect(await screen.findByTestId("workflows-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByText("my-pipeline")).toBeInTheDocument();
    expect(screen.getByText("broken-wf")).toBeInTheDocument();

    // Status badges (M144.1 semantic vocabulary): validated → "Ready" (green);
    // a validation failure → the humanized reason in the FAILED tone (red), not amber.
    expect(screen.getByText("Ready")).toBeInTheDocument();
    const validBadge = screen.getByText("Ready");
    expect(validBadge.className).toMatch(/bg-success/);

    expect(screen.getByText("Dangling edge")).toBeInTheDocument();
    const invalidBadge = screen.getByText("Dangling edge");
    expect(invalidBadge.className).toMatch(/bg-destructive/);

    // Step counts and registry.
    expect(screen.getByText("3 steps")).toBeInTheDocument();
    expect(screen.getByText("1 step")).toBeInTheDocument();
    expect(screen.getByText("prod-registry")).toBeInTheDocument();
  });

  it("filters workflows by name", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [wf({ name: "alpha-pipeline" }), wf({ name: "beta-pipeline" })],
      },
    }));
    renderPage();
    await screen.findByText("alpha-pipeline");

    fireEvent.change(screen.getByLabelText("Filter list"), { target: { value: "beta" } });
    await waitFor(() => expect(screen.queryByText("alpha-pipeline")).not.toBeInTheDocument());
    expect(screen.getByText("beta-pipeline")).toBeInTheDocument();
  });

  it("403 surfaces a forbidden state (never a fake empty list)", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you do not have permission to list workflows" },
    }));
    renderPage();
    expect(
      await screen.findByText("You don't have permission to view workflows"),
    ).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/you do not have permission to/)).toBeNull();
    expect(screen.queryByText("No workflows")).toBeNull();
  });

  it("empty → a teaching empty state", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("No workflows")).toBeInTheDocument());
  });

  it("singular step count shows '1 step' not '1 steps'", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [wf({ name: "solo", stepCount: 1 })] },
    }));
    renderPage();
    await screen.findByText("solo");
    expect(screen.getByText("1 step")).toBeInTheDocument();
  });
});

// Invoke affordance (m67.15): per-row Run button + inline invoke panel.
describe("WorkflowsPage — invoke affordance (m67.15)", () => {
  it("renders a Run button per workflow row", async () => {
    installFetch({ list: { ok: true, body: { items: [wf({ name: "pipe-a" })] } } });
    renderPage();
    await screen.findByText("pipe-a");
    expect(screen.getByTestId("invoke-btn-pipe-a")).toBeInTheDocument();
  });

  it("clicking Run opens the invoke panel for that workflow", async () => {
    installFetch({ list: { ok: true, body: { items: [wf({ name: "pipe-a" })] } } });
    renderPage();
    await screen.findByText("pipe-a");

    fireEvent.click(screen.getByTestId("invoke-btn-pipe-a"));

    // The invoke panel appears with the workflow name in the title.
    expect(screen.getByTestId("invoke-panel")).toBeInTheDocument();
    expect(screen.getByTestId("invoke-input")).toBeInTheDocument();
    expect(screen.getByTestId("invoke-submit")).toBeInTheDocument();
    expect(screen.getByTestId("invoke-cancel")).toBeInTheDocument();
  });

  it("invoke panel dismiss (Cancel) closes it without invoking", async () => {
    const { calls } = installFetch({
      list: { ok: true, body: { items: [wf({ name: "pipe-a" })] } },
    })!;
    renderPage();
    await screen.findByText("pipe-a");

    fireEvent.click(screen.getByTestId("invoke-btn-pipe-a"));
    expect(screen.getByTestId("invoke-panel")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("invoke-cancel"));
    await waitFor(() => expect(screen.queryByTestId("invoke-panel")).not.toBeInTheDocument());
    expect(calls.filter((c) => c.method === "POST").length).toBe(0);
  });

  it("submitting calls createWorkflowRun and navigates to the run trace view on 202", async () => {
    const { calls } = installFetch({
      list: {
        ok: true,
        body: { items: [wf({ name: "pipe-a", namespace: "default" })] },
      },
      createRun: { ok: true, status: 202, body: { id: "run-wf-99", status: "queued" } },
    })!;

    renderPage();
    await screen.findByText("pipe-a");
    fireEvent.click(screen.getByTestId("invoke-btn-pipe-a"));

    // Provide a JSON input.
    fireEvent.change(screen.getByTestId("invoke-input"), { target: { value: '{"key":"val"}' } });
    fireEvent.click(screen.getByTestId("invoke-submit"));

    // The POST to /api/workflows/pipe-a/runs was made.
    await waitFor(() =>
      expect(calls.find((c) => c.url === "/api/workflows/pipe-a/runs" && c.method === "POST")).toBeDefined(),
    );
    const createCall = calls.find((c) => c.url === "/api/workflows/pipe-a/runs")!;
    const body = JSON.parse(createCall.body) as { input: unknown; namespace: string };
    expect(body.input).toEqual({ key: "val" });
    expect(body.namespace).toBe("default");

    // After 202, navigates to /traces/run-wf-99.
    await screen.findByTestId("trace-page");
  });

  it("invoke shows an error on malformed JSON input before any round-trip", async () => {
    const { calls } = installFetch({
      list: { ok: true, body: { items: [wf({ name: "pipe-a" })] } },
    })!;

    renderPage();
    await screen.findByText("pipe-a");
    fireEvent.click(screen.getByTestId("invoke-btn-pipe-a"));
    fireEvent.change(screen.getByTestId("invoke-input"), { target: { value: "bad json" } });
    fireEvent.click(screen.getByTestId("invoke-submit"));

    expect(await screen.findByTestId("invoke-error")).toHaveTextContent("must be valid JSON");
    expect(calls.filter((c) => c.method === "POST").length).toBe(0);
  });

  it("invoke shows an error on 500 from createWorkflowRun", async () => {
    installFetch({
      list: { ok: true, body: { items: [wf({ name: "pipe-a" })] } },
      createRun: { ok: false, status: 500, body: { error: "internal error" } },
    });
    renderPage();
    await screen.findByText("pipe-a");
    fireEvent.click(screen.getByTestId("invoke-btn-pipe-a"));
    fireEvent.click(screen.getByTestId("invoke-submit"));

    expect(await screen.findByTestId("invoke-error")).toBeInTheDocument();
  });

  it("disables the Run button for an invalid (non-validated) workflow", async () => {
    installFetch({
      list: {
        ok: true,
        body: { items: [wf({ name: "broken-wf", validated: false, reason: "DanglingEdge" })] },
      },
    });
    renderPage();
    await screen.findByText("broken-wf");
    const btn = screen.getByTestId("invoke-btn-broken-wf");
    expect(btn).toBeDisabled();
  });
});
