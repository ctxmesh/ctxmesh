import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { WorkflowsPage } from "@/pages/workflows-page";
import type { WorkflowSummary } from "@/lib/api";

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

function installFetch(respond: () => { ok: boolean; status?: number; body?: unknown }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      const r = respond();
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? { items: [] },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/workflows"]}>
      <Routes>
        <Route path="/workflows" element={<WorkflowsPage />} />
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

    // Status badges: validated → "valid", invalid → shows the reason.
    expect(screen.getByText("valid")).toBeInTheDocument();
    const validBadge = screen.getByText("valid");
    expect(validBadge.className).toMatch(/bg-success/);

    expect(screen.getByText("DanglingEdge")).toBeInTheDocument();
    const invalidBadge = screen.getByText("DanglingEdge");
    expect(invalidBadge.className).toMatch(/bg-warning/);

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
    await waitFor(() =>
      expect(
        screen.getByText(/you do not have permission to list workflows/),
      ).toBeInTheDocument(),
    );
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
