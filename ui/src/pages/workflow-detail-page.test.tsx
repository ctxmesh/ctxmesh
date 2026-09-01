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
    // Same intent, the M151 §7 wording: a 403 is calm, names the resource, and
    // never surfaces the raw RBAC string.
    expect(
      await screen.findByText("You don't have permission to view this workflow."),
    ).toBeInTheDocument();
    expect(screen.getByText(/role that can read workflows/i)).toBeInTheDocument();
  });
});

// ── M151: a declared graph has no health (ADR 0128 §2.2/§2.4) ───────────────

describe("WorkflowDetailPage — the declared DAG carries no state hue", () => {
  it("draws control flow with words and form, never with the hold hue", async () => {
    stubFetch();
    const { container } = renderPage();
    await screen.findByTestId("workflow-dag");
    const dag = screen.getByTestId("workflow-dag");

    // `branch` / `map` / `join` / `loop` used to render `text-info` — and the
    // --info slot now carries the HOLD violet, which means "a person must
    // decide". Nothing on this page is waiting on a person: nothing has run.
    for (const hue of ["text-info", "text-hold", "bg-hold", "border-info", "bg-info"]) {
      expect(dag.innerHTML).not.toContain(hue);
    }
    // The kinds are still legible — they are named, not coloured.
    expect(dag).toHaveTextContent("branch");
    expect(dag).toHaveTextContent("default");
    expect(dag).toHaveTextContent("catch");
    // Pine is not a status here either (§2.1).
    expect(dag.innerHTML).not.toContain("bg-primary");
    // The trust boundary strip lost its violet rule too.
    expect(container.innerHTML).not.toContain("border-info");
  });

  it("marks the error path by FORM — the dashed `open` chip, not a hue", async () => {
    stubFetch();
    renderPage();
    await screen.findByTestId("workflow-node-escalate");
    const escalate = screen.getByTestId("workflow-node-escalate");
    const chip = Array.from(escalate.querySelectorAll("div")).find(
      (el) => el.textContent === "catch",
    );
    expect(chip).toBeDefined();
    expect(chip?.className).toContain("border-dashed");
    expect(chip?.className).not.toContain("text-destructive");
  });

  it("pans the DAG inside a fixed frame, so the page never scrolls sideways (§4.6)", async () => {
    stubFetch();
    renderPage();
    const dag = await screen.findByTestId("workflow-dag");
    expect(dag.className).toContain("overflow-auto");
    expect(dag.className).toContain("min-h-[35rem]");
  });

  it("ranks the steps into left→right columns from the start step", async () => {
    stubFetch();
    renderPage();
    const dag = await screen.findByTestId("workflow-dag");
    // triage → {escalate, resolve}; escalate → resolve. Longest path puts
    // resolve two columns right of triage, so the DAG is three columns wide.
    const columns = dag.firstElementChild?.children ?? [];
    expect(columns.length).toBe(3);
    expect(columns[0]).toContainElement(screen.getByTestId("workflow-node-triage"));
    expect(columns[2]).toContainElement(screen.getByTestId("workflow-node-resolve"));
  });
});
