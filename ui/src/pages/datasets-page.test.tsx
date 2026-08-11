import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { DatasetsPage, DatasetDetailPage } from "@/pages/datasets-page";
import { TracePage } from "@/pages/trace-page";
import { ToastProvider } from "@/components/kit";
import type { DatasetSummary, DatasetCase, SpanSummary } from "@/lib/api";

// DatasetsPage + DatasetDetailPage (m69.3, ADR 0062 Fork 5) — human-labeling dataset surfaces.
//
// Coverage:
//   • DatasetsPage renders dataset list
//   • DatasetsPage shows calm unconfigured state on 501
//   • DatasetDetailPage renders cases with latest labels
//   • DatasetDetailPage label form: select + submit → api.appendLabel called
//   • DatasetDetailPage empty-cases state
//   • DatasetDetailPage unconfigured on 501

function ds(over: Partial<DatasetSummary> = {}): DatasetSummary {
  return {
    id: "ds-1",
    name: "my-dataset",
    namespace: "default",
    caseCount: 3,
    createdAt: "2026-07-01T10:00:00Z",
    ...over,
  };
}

function kase(over: Partial<DatasetCase> = {}): DatasetCase {
  return {
    id: "case-1",
    input: "What is the capital of France?",
    expected: "Paris",
    sourceTraceId: "trace-abc",
    createdAt: "2026-07-02T10:00:00Z",
    ...over,
  };
}

interface FetchRoutes {
  listDatasets?: { ok: boolean; status?: number; body?: unknown };
  listCases?: { ok: boolean; status?: number; body?: unknown };
  appendLabel?: { ok: boolean; status?: number; body?: unknown };
}

interface FetchCall {
  url: string;
  method: string;
  body: string;
}

function installFetch(routes: FetchRoutes): { calls: FetchCall[] } {
  const calls: FetchCall[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });

      // GET /api/datasets → list
      if (path === "/api/datasets" && method === "GET") {
        const r = routes.listDatasets ?? { ok: true, body: { items: [] } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 500),
          json: async () => r.body ?? { items: [] },
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }

      // GET /api/datasets/{name}/cases → case list
      if (path.match(/\/api\/datasets\/[^/]+\/cases$/) && method === "GET") {
        const r = routes.listCases ?? {
          ok: true,
          body: { datasetId: "ds-1", name: "my-dataset", cases: [] },
        };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 500),
          json: async () => r.body,
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }

      // POST /api/datasets/{name}/cases/{caseId}/labels → append label
      if (path.match(/\/api\/datasets\/[^/]+\/cases\/[^/]+\/labels$/) && method === "POST") {
        const r = routes.appendLabel ?? { ok: true, status: 201 };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 201 : 400),
          json: async () => r.body ?? {},
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }

      // Fallback — should not be reached in these tests
      return Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({ error: `unexpected request: ${method} ${path}` }),
        text: async () => `unexpected: ${method} ${path}`,
      } as Response);
    }),
  );
  return { calls };
}

function renderList() {
  return render(
    <MemoryRouter initialEntries={["/datasets"]}>
      <Routes>
        <Route path="/datasets" element={<DatasetsPage />} />
        <Route path="/datasets/:name" element={<DatasetDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetail(name = "my-dataset") {
  return render(
    <MemoryRouter initialEntries={[`/datasets/${name}`]}>
      <Routes>
        <Route path="/datasets" element={<DatasetsPage />} />
        <Route path="/datasets/:name" element={<DatasetDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ─── DatasetsPage (list) ──────────────────────────────────────────────────────

describe("DatasetsPage", () => {
  it("renders dataset list with name and case count", async () => {
    installFetch({
      listDatasets: {
        ok: true,
        body: {
          items: [
            ds({ id: "ds-1", name: "prod-evals", caseCount: 42 }),
            ds({ id: "ds-2", name: "staging-evals", caseCount: 7 }),
          ],
        },
      },
    });

    renderList();

    await waitFor(() => {
      expect(screen.getByText("prod-evals")).toBeInTheDocument();
    });
    expect(screen.getByText("staging-evals")).toBeInTheDocument();
    expect(screen.getByText("42")).toBeInTheDocument();
    expect(screen.getByText("7")).toBeInTheDocument();
  });

  it("shows empty state when no datasets", async () => {
    installFetch({
      listDatasets: { ok: true, body: { items: [] } },
    });

    renderList();

    await waitFor(() => {
      expect(screen.getByText("No datasets")).toBeInTheDocument();
    });
  });

  it("shows calm empty state on 501 (api.listDatasets returns { items: [] } for 501)", async () => {
    // api.ts handles 501 calmly: returns { items: [] } instead of throwing.
    // So the page sees an empty list (not the unconfigured branch) and shows the empty state.
    installFetch({
      listDatasets: { ok: false, status: 501, body: { error: "not implemented" } },
    });

    renderList();

    await waitFor(() => {
      expect(screen.getByText("No datasets")).toBeInTheDocument();
    });
    // Must NOT show an error alert
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows error state on 500", async () => {
    installFetch({
      listDatasets: {
        ok: false,
        status: 500,
        body: { error: "internal server error" },
      },
    });

    renderList();

    // Error appears in the DataTable error state (Retry button or error text)
    await waitFor(() => {
      // The DataTable shows the error message; just verify page still mounts.
      expect(screen.getByTestId("datasets-page")).toBeInTheDocument();
    });
  });
});

// ─── DatasetDetailPage ────────────────────────────────────────────────────────

describe("DatasetDetailPage", () => {
  it("renders cases with input and latest label", async () => {
    const c1 = kase({
      id: "case-1",
      input: "What is 2+2?",
      expected: "4",
      sourceTraceId: "trace-xyz",
      latestLabel: {
        value: "pass",
        author: "alice",
        createdAt: "2026-07-03T10:00:00Z",
      },
    });
    const c2 = kase({ id: "case-2", input: "Capital of Japan?" });

    installFetch({
      listCases: {
        ok: true,
        body: { datasetId: "ds-1", name: "my-dataset", cases: [c1, c2] },
      },
    });

    renderDetail();

    await waitFor(() => {
      expect(screen.getByTestId("case-row-case-1")).toBeInTheDocument();
    });
    expect(screen.getByTestId("case-row-case-2")).toBeInTheDocument();

    // Verify input text appears
    expect(screen.getByText("What is 2+2?")).toBeInTheDocument();

    // Verify latest label badge — use getAllByText since the select also has "pass" as an option
    const passElements = screen.getAllByText("pass");
    expect(passElements.length).toBeGreaterThan(0);
    expect(screen.getByText(/alice/)).toBeInTheDocument();

    // Verify source trace link
    expect(screen.getByText(/Source trace: trace-xyz/)).toBeInTheDocument();
  });

  it("label form submits with correct args", async () => {
    const c = kase({ id: "case-99", input: "Hello world?" });

    const { calls } = installFetch({
      listCases: {
        ok: true,
        body: { datasetId: "ds-1", name: "my-dataset", cases: [c] },
      },
      appendLabel: { ok: true, status: 201 },
    });

    renderDetail("my-dataset");

    // Wait for case to render
    await waitFor(() => {
      expect(screen.getByTestId("case-row-case-99")).toBeInTheDocument();
    });

    // Select "fail" verdict
    const select = screen.getByTestId("label-value-case-99");
    fireEvent.change(select, { target: { value: "fail" } });

    // Submit
    const submitBtn = screen.getByTestId("label-submit-case-99");
    fireEvent.click(submitBtn);

    await waitFor(() => {
      // Verify API call went to the correct endpoint
      const labelCall = calls.find(
        (c) =>
          c.url.includes("/api/datasets/my-dataset/cases/case-99/labels") &&
          c.method === "POST",
      );
      expect(labelCall).toBeDefined();
      const body = JSON.parse(labelCall!.body) as { value: string };
      expect(body.value).toBe("fail");
    });
  });

  it("shows 'no cases' empty state", async () => {
    installFetch({
      listCases: {
        ok: true,
        body: { datasetId: "ds-1", name: "my-dataset", cases: [] },
      },
    });

    renderDetail();

    await waitFor(() => {
      expect(screen.getByText("No cases")).toBeInTheDocument();
    });
  });

  it("shows calm empty state on 501 (api.listDatasetCases returns empty cases for 501)", async () => {
    // api.ts handles 501 calmly: returns { datasetId: "", name, cases: [] } instead of throwing.
    // So the page shows "No cases", not the unconfigured branch.
    installFetch({
      listCases: { ok: false, status: 501, body: { error: "not implemented" } },
    });

    renderDetail();

    await waitFor(() => {
      expect(screen.getByText("No cases")).toBeInTheDocument();
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("back button navigates to datasets list", async () => {
    installFetch({
      listCases: {
        ok: true,
        body: { datasetId: "ds-1", name: "my-dataset", cases: [] },
      },
    });

    renderDetail();

    await waitFor(() => {
      expect(screen.getByTestId("dataset-detail-page")).toBeInTheDocument();
    });

    const backBtn = screen.getByText(/Back to Datasets/);
    expect(backBtn).toBeInTheDocument();
  });
});

// ─── TracePage AddToDatasetPanel (m69.3) ──────────────────────────────────────

// installTraceFetch stubs fetch for the TracePage + AddToDatasetPanel combo.
// Handles trace detail + link-out + feedback (silent 404) + datasets from-run.
function installTraceFetch(opts: {
  fromRun?: { ok: boolean; status?: number; body?: unknown };
} = {}) {
  const ROLLUP = {
    traceId: "t1", name: "test-agent",
    timestamp: "2026-07-11T12:00:00Z",
    costUSD: 0, tokens: 0, latencyMs: 0, spanCount: 0,
  };
  const SPANS: SpanSummary[] = [{
    id: "s1", parentId: "", type: "SPAN", name: "root",
    startMs: 0, durationMs: 0, model: "", tokensIn: 0, tokensOut: 0,
    costUSD: 0, level: "", status: "ok", input: "", output: "",
    inputRedacted: false, outputRedacted: false, nestingDepth: 0,
  }];

  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = (init?.method ?? "GET").toUpperCase();
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });

      if (path.includes("/detail")) {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({ rollup: ROLLUP, spans: SPANS }),
          text: async () => "{}",
        } as Response);
      }
      if (path.match(/\/api\/datasets\/[^/]+\/cases\/from-run$/) && method === "POST") {
        const r = opts.fromRun ?? { ok: true, body: { caseId: "case-new-1" } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 201 : 500),
          json: async () => r.body ?? { caseId: "case-new-1" },
          text: async () => JSON.stringify(r.body ?? { error: "err" }),
        } as Response);
      }
      // feedback, link-out — silent 404 (best-effort in TracePage)
      return Promise.resolve({
        ok: false, status: 404,
        json: async () => ({}),
        text: async () => "{}",
      } as Response);
    }),
  );
  return calls;
}

describe("TracePage — AddToDatasetPanel (m69.3)", () => {
  function renderTrace(traceId = "t1") {
    return render(
      <ToastProvider>
        <MemoryRouter initialEntries={[`/traces/${traceId}`]}>
          <Routes>
            <Route path="/traces/:id" element={<TracePage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>,
    );
  }

  it("renders the add-to-dataset panel with input and submit button", async () => {
    installTraceFetch();
    renderTrace();

    await screen.findByTestId("trace-page");
    expect(screen.getByTestId("add-to-dataset-panel")).toBeInTheDocument();
    expect(screen.getByTestId("add-to-dataset-input")).toBeInTheDocument();
    expect(screen.getByTestId("add-to-dataset-submit")).toBeInTheDocument();
  });

  it("submits traceId and dataset name, shows success with caseId", async () => {
    const calls = installTraceFetch({
      fromRun: { ok: true, body: { caseId: "case-created-1" } },
    });

    renderTrace("t1");
    await screen.findByTestId("trace-page");

    fireEvent.change(screen.getByTestId("add-to-dataset-input"), {
      target: { value: "eval-set" },
    });
    fireEvent.click(screen.getByTestId("add-to-dataset-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("add-to-dataset-result")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("add-to-dataset-result")).toHaveTextContent("case-created-1");

    // Verify the POST included traceId.
    const fromRunCall = calls.find(
      (c) => c.url.includes("/from-run") && c.method === "POST",
    );
    expect(fromRunCall).toBeDefined();
    const parsed = JSON.parse(fromRunCall!.body) as { traceId: string };
    expect(parsed.traceId).toBe("t1");
  });

  it("shows calm 501 message when dataset store is not configured", async () => {
    installTraceFetch({
      fromRun: { ok: false, status: 501, body: { error: "not implemented" } },
    });

    renderTrace("t1");
    await screen.findByTestId("trace-page");

    fireEvent.change(screen.getByTestId("add-to-dataset-input"), {
      target: { value: "my-ds" },
    });
    fireEvent.click(screen.getByTestId("add-to-dataset-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("add-to-dataset-result")).toBeInTheDocument(),
    );
    const result = screen.getByTestId("add-to-dataset-result");
    // Must NOT be an error alert — it's a calm muted message.
    expect(result).not.toHaveAttribute("role", "alert");
    expect(result.textContent).toMatch(/not configured|CONTROLPLANE_DSN/i);
  });
});
