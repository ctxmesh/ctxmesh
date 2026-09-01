import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
    // a validation failure → the FAILED tone (red), not amber.
    //
    // M151 §4.5 moved WHERE the reason renders, not whether it does. A
    // controller reason is a sentence, and a 62-character `whitespace-nowrap`
    // tag was setting the width of the table; the tag now carries the STATE and
    // the humanized reason CODE renders subordinate to it in the same cell.
    // Both halves of the original assertion survive — the red tone, and the
    // humanized reason.
    const validBadge = screen.getByText("Ready");
    expect(validBadge.className).toMatch(/bg-success/);

    const brokenState = screen.getByTestId("workflow-state-broken-wf");
    const invalidBadge = within(brokenState).getByText("Not ready");
    expect(invalidBadge.className).toMatch(/bg-destructive/);
    expect(within(brokenState).getByText("Dangling edge")).toBeInTheDocument();

    // Step counts and registry. A count is a machine-owned number, so the cell
    // is a mono tabular digit under a "Steps" header (§4.5) and the plural
    // phrase rides in `title` — the grammar is kept, the column alignment is
    // not sacrificed to it.
    expect(screen.getByTitle("3 steps")).toHaveTextContent("3");
    expect(screen.getByTitle("1 step")).toHaveTextContent("1");
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
    // The plural is carried by the cell's `title` now that the visible cell is
    // a mono tabular digit (§4.5); the grammar assertion is unchanged.
    expect(screen.getByTitle("1 step")).toBeInTheDocument();
    expect(screen.queryByTitle("1 steps")).toBeNull();
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

// ── Archetype A1 (M151 spec §6.1) ───────────────────────────────────────────
// The redesign's contract for this page: sorted by what is blocking, a Next
// step column that speaks the USER's next action, the §4.4 column budget, four
// distinct degraded states, and a closing note whose numbers come from the data.

/** Row identity in DOM order, read off each row's State cell test id. */
function rowOrder(): string[] {
  return Array.from(screen.getByRole("table").querySelectorAll("tbody tr"))
    .map(
      (tr) =>
        tr
          .querySelector("[data-testid^='workflow-state-']")
          ?.getAttribute("data-testid") ?? "",
    )
    .filter(Boolean)
    .map((id) => id.replace("workflow-state-", ""));
}

describe("WorkflowsPage — archetype A1 (M151)", () => {
  it("renders the §4.4 resource-list column budget, in visual order", async () => {
    installFetch(() => ({ ok: true, body: { items: [wf({ name: "pipe-a" })] } }));
    renderPage();
    await screen.findByText("pipe-a");

    const heads = Array.from(
      screen.getByRole("table").querySelectorAll("thead th"),
    ).map((th) => th.textContent?.trim());
    // Entity · p4 · p3 · State · Next step, plus the trailing actions slot.
    // Run is an ACTION, not a column: a column with an empty header only earns
    // an `sr-only` head, which is what used to escape the table's scroll frame.
    expect(heads).toEqual([
      "Workflow",
      "Registry",
      "Steps",
      "State",
      "Next step",
      "Actions",
    ]);
  });

  it("sorts by what is blocking, not alphabetically", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          wf({ name: "aaa-valid", validated: true, stepCount: 2 }),
          wf({ name: "zzz-valid", validated: true, stepCount: 4 }),
          // Valid but empty — needs a person, though not urgently.
          wf({ name: "mmm-empty", validated: true, stepCount: 0 }),
          // Will not run at all — the first row on the page.
          wf({ name: "nnn-broken", validated: false, reason: "RegistryNotFound" }),
        ],
      },
    }));
    renderPage();
    await screen.findByText("aaa-valid");

    expect(rowOrder()).toEqual([
      "nnn-broken", //  needs a person, and is failing
      "mmm-empty", //   needs a person, but is not failing
      "aaa-valid", //   "Nothing needed" sinks, and only then sorts by name
      "zzz-valid",
    ]);
  });

  it("the Next step column says what the user should do, and 'Nothing needed' when nothing is", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          wf({ name: "broken", validated: false, reason: "DanglingEdge" }),
          wf({ name: "empty", validated: true, stepCount: 0 }),
          wf({ name: "fine", validated: true, stepCount: 3 }),
        ],
      },
    }));
    renderPage();
    await screen.findByText("fine");

    expect(screen.getByTestId("next-step-broken")).toHaveTextContent("Fix the workflow");
    expect(screen.getByTestId("next-step-empty")).toHaveTextContent("Add a step");
    // The inert state is the literal kit words, not an invented errand.
    expect(screen.getByTestId("next-step-fine")).toHaveTextContent("Nothing needed");

    // Every label is verb-first and inside the §7.2 22-character budget.
    for (const id of ["next-step-broken", "next-step-empty"]) {
      const label = screen.getByTestId(id).textContent!.replace(" →", "").trim();
      expect(label.length).toBeLessThanOrEqual(22);
      expect(label).toMatch(/^[A-Z][a-z]+ /);
    }

    // Crit tone only when the target is a failure (§2.3); a setup gap stays pine.
    expect(screen.getByTestId("next-step-broken").className).toMatch(/text-destructive/);
    expect(screen.getByTestId("next-step-empty").className).toMatch(/text-primary/);
    // "Nothing needed" is not an anchor and not focusable.
    expect(screen.getByTestId("next-step-fine").tagName).toBe("SPAN");
  });

  it("chip views ask one question at a time, and an emptied view offers the way back", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [wf({ name: "fine-a" }), wf({ name: "fine-b" })],
      },
    }));
    renderPage();
    await screen.findByText("fine-a");

    // The chips are a radiogroup — one answer at a time, never an AND of boxes.
    const group = screen.getByRole("radiogroup", { name: "Filter workflows" });
    expect(
      within(group)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["Needs you", "Not valid", "Runnable", "Everything"]);

    fireEvent.click(within(group).getByRole("radio", { name: "Not valid" }));

    // Filtered-to-nothing is its OWN state: it names the view, and it offers a
    // way out rather than teaching a user with two workflows what a workflow is.
    expect(await screen.findByText("Nothing is failing validation")).toBeInTheDocument();
    expect(screen.queryByText("No workflows")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show everything" }));
    expect(await screen.findByText("fine-a")).toBeInTheDocument();
  });

  it("first-run empty is the teaching state — no filter to clear", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await screen.findByText("No workflows");
    expect(screen.queryByRole("button", { name: "Show everything" })).toBeNull();
    // No chips and no closing ratio when there is nothing to state a ratio about.
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByText(/needs a person/)).toBeNull();
  });

  it("says once, calmly, that run history is not in this response (§7.1)", async () => {
    installFetch(() => ({ ok: true, body: { items: [wf({ name: "pipe-a" })] } }));
    renderPage();
    await screen.findByText("pipe-a");

    const notes = screen.getAllByRole("note");
    expect(notes).toHaveLength(1);
    expect(notes[0]).toHaveTextContent("Run history isn’t in the workflow list.");
    expect(notes[0]).toHaveTextContent("Nothing is estimated");
    // Not an error, and never a zero: no alert role, and no fabricated count.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("the closing note states the ratio from the data, and is grammatical at n=1", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [wf({ name: "solo", validated: true, stepCount: 2 })] },
    }));
    const { unmount } = renderPage();
    expect(
      await screen.findByText("The one workflow here needs nothing from you."),
    ).toBeInTheDocument();
    unmount();

    installFetch(() => ({
      ok: true,
      body: {
        items: [
          wf({ name: "broken", validated: false, reason: "DanglingEdge" }),
          wf({ name: "fine-a", validated: true, stepCount: 2 }),
          wf({ name: "fine-b", validated: true, stepCount: 3 }),
        ],
      },
    }));
    renderPage();
    expect(
      await screen.findByText(
        "1 of the 3 workflows needs a person. One of them won’t run until it is fixed. The other 2 need nothing from you.",
      ),
    ).toBeInTheDocument();
  });

  it("a 63-character name truncates on one line; a deep namespace middle-truncates (§4.5)", async () => {
    const LONG_NAME = `wf-${"x".repeat(60)}`; // the K8s 63-character limit
    expect(LONG_NAME).toHaveLength(63);
    const DEEP_NS = "acme-platform-eu-west-1-team-d-shared-ingest";

    installFetch(() => ({
      ok: true,
      body: {
        items: [
          wf({ name: LONG_NAME, namespace: DEEP_NS, registryRef: "core-registry" }),
        ],
      },
    }));
    renderPage();

    // One line, end-ellipsis, full value in `title` — never `break-all`, which
    // turns a name into a five-line paragraph.
    const nameCell = await screen.findByTitle(LONG_NAME);
    expect(nameCell.className).toMatch(/truncate/);
    expect(nameCell.className).not.toMatch(/break-all/);

    // The namespace is the entity cell's SECOND line, mono, middle-truncated:
    // the tail is what disambiguates two sibling namespaces, so it survives.
    const ns = screen.getByTestId(`namespace-${LONG_NAME}`);
    expect(ns).toHaveAttribute("title", DEEP_NS);
    expect(ns).toHaveTextContent("acme…shared-ingest");
    expect(ns.closest("div")!.className).toMatch(/font-mono/);
  });
});
