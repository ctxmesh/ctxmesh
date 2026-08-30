import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { WorkflowDetailPage } from "./workflow-detail-page";
import type { WorkflowDetailResponse } from "@/lib/api";

const DETAIL: WorkflowDetailResponse = {
  name: "triage-flow",
  namespace: "team-a",
  registryRef: "prod-tools",
  validated: true,
  nodes: [
    {
      name: "triage",
      agentRef: "triage-agent",
      kind: "choice",
      start: true,
      edges: [
        { to: "escalate", kind: "branch", label: "steps.triage.output.urgent" },
        { to: "resolve", kind: "default", label: "default" },
      ],
    },
    {
      name: "escalate",
      agentRef: "oncall-agent",
      kind: "task",
      edges: [{ to: "resolve", kind: "catch", label: "catch timeout" }],
    },
    { name: "resolve", agentRef: "resolver-agent", kind: "task", edges: [] },
  ],
};

function stubFetch(detail: WorkflowDetailResponse = DETAIL, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/workflows/")) {
        return Promise.resolve({
          ok: status === 200,
          status,
          json: async () => (status === 200 ? detail : { error: "forbidden" }),
          text: async () => (status === 200 ? JSON.stringify(detail) : "forbidden"),
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => ({}), text: async () => "" } as Response);
    }),
  );
}

function renderPage(ns = "team-a", name = "triage-flow") {
  return render(
    <MemoryRouter initialEntries={[`/workflows/${ns}/${name}`]}>
      <Routes>
        <Route path="/workflows/:ns/:name" element={<WorkflowDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("WorkflowDetailPage — declared DAG (M144-canvas)", () => {
  it("renders each step as a node with its labeled edges", async () => {
    stubFetch();
    renderPage();

    // every declared step is a node
    expect(await screen.findByTestId("workflow-node-triage")).toBeInTheDocument();
    expect(screen.getByTestId("workflow-node-escalate")).toBeInTheDocument();
    expect(screen.getByTestId("workflow-node-resolve")).toBeInTheDocument();

    // the start step is marked; the choice's branch predicate is shown as an edge label
    const triage = screen.getByTestId("workflow-node-triage");
    expect(triage).toHaveTextContent("start");
    expect(triage).toHaveTextContent("steps.triage.output.urgent");
    expect(triage).toHaveTextContent("escalate");

    // the terminal step says so
    expect(screen.getByTestId("workflow-node-resolve")).toHaveTextContent("terminal");
  });

  it("surfaces a forbidden read as the explain-and-suggest primitive", async () => {
    stubFetch(DETAIL, 403);
    renderPage();
    expect(await screen.findByText("Not allowed to view this workflow")).toBeInTheDocument();
  });
});
