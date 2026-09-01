import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { EvalsPage } from "@/pages/evals-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// EvalsPage tests (m17.12):
//   1. EvalSuite builder: create calls createEvalSuite with dataset/scorers/gate.
//   2. Results: conditions gate outcome rendered; scoresAvailable=true → scores.
//   3. scoresAvailable=false → scoresUnavailableReason shown CALMLY (no fabricated numbers).
//   4. RBAC: a viewer (no evalsuites/create) sees no "New eval suite" button.

// ---- fixtures ----------------------------------------------------------------

const EDITOR_CAPS = {
  evalsuites: { create: true, delete: true, update: true },
};
const VIEWER_CAPS = {
  evalsuites: { create: false, delete: false, update: false },
};

const SUITE_LIST = [
  {
    name: "my-eval",
    namespace: "default",
    datasetRef: "my-dataset",
    scorers: ["exact-match"],
    gate: "exact-match",
    threshold: 0.8,
    ready: true,
  },
  {
    name: "other-eval",
    namespace: "default",
    datasetRef: "other-ds",
    scorers: ["bleu"],
    ready: false,
  },
];

// Results with scores available (Langfuse wired)
const RESULTS_WITH_SCORES = {
  conditions: [
    {
      type: "GatePassed",
      status: "True",
      reason: "ThresholdMet",
      message: "Score 0.9 >= 0.8",
      lastTransitionTime: "2026-07-11T10:00:00Z",
    },
  ],
  gateResults: [
    {
      agent: "prod-agent",
      decision: "promoted",
      phase: "awaiting-promotion",
      reason: "AwaitingHumanPromotion",
      score: "0.9182",
      scoredRevision: "prod-agent-abc",
      threshold: "0.8000",
      pending: false,
    },
  ],
  gateResultsAvailable: true,
  scoresAvailable: true,
  scores: [
    { scorer: "exact-match", value: 0.9 },
  ],
};

// Results without scores (Langfuse not wired)
const RESULTS_NO_SCORES = {
  conditions: [
    {
      type: "GatePassed",
      status: "Unknown",
      reason: "PendingScores",
      message: "Scores not yet available",
      lastTransitionTime: "2026-07-11T10:00:00Z",
    },
  ],
  gateResults: [],
  gateResultsAvailable: true,
  scoresAvailable: false,
  scoresUnavailableReason: "Langfuse not configured",
};

// Default eval-gated metric fixture: 3 of 5 deployments are gated → 60%.
const METRIC_DEFAULT = { total: 5, gated: 3, percent: 60 };
// Fixture that meets the >50% PRD §5 target.
const METRIC_ABOVE_TARGET = { total: 4, gated: 3, percent: 75 };
// Fixture that is below the target.
const METRIC_BELOW_TARGET = { total: 4, gated: 1, percent: 25 };

type FetchSetup = {
  caps?: Record<string, Record<string, boolean>>;
  suites?: unknown[];
  suitesStatus?: number;
  createOk?: boolean;
  createStatus?: number;
  createBody?: unknown;
  resultsBody?: unknown;
  resultsStatus?: number;
  deleteOk?: boolean;
  // eval-gated metric setup
  metricBody?: unknown;
  metricStatus?: number;
};

function installFetch(opts: FetchSetup = {}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({ ok, status, json: async () => body } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? EDITOR_CAPS });

      // List
      if (url.startsWith("/api/evalsuites") && !url.includes("/results") && method === "GET") {
        const status = opts.suitesStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              ? { items: opts.suites ?? SUITE_LIST, nextCursor: "" }
              : { error: "forbidden" },
        } as Response);
      }

      // Results
      if (url.match(/\/api\/evalsuites\/[^/]+\/[^/]+\/results$/) && method === "GET") {
        const status = opts.resultsStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok ? (opts.resultsBody ?? RESULTS_WITH_SCORES) : { error: "failed" },
        } as Response);
      }

      // Create
      if (url === "/api/evalsuites" && method === "POST") {
        const ok = opts.createOk ?? true;
        const status = opts.createStatus ?? (ok ? 201 : 403);
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              ? (opts.createBody ?? {
                  name: "my-eval",
                  namespace: "default",
                  datasetRef: "my-dataset",
                  scorers: ["exact-match"],
                  gate: "exact-match",
                  threshold: 0.8,
                  ready: false,
                })
              : { error: "not allowed" },
        } as Response);
      }

      // Delete
      if (url.match(/\/api\/evalsuites\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const ok = opts.deleteOk ?? true;
        return Promise.resolve({
          ok,
          status: ok ? 204 : 403,
          json: async () => (ok ? {} : { error: "forbidden" }),
        } as Response);
      }

      // Eval-gated metric
      if (url.startsWith("/api/metrics/eval-gated") && method === "GET") {
        const status = opts.metricStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok ? (opts.metricBody ?? METRIC_DEFAULT) : { error: "failed" },
        } as Response);
      }

      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderPage(caps?: Record<string, Record<string, boolean>>, setup: FetchSetup = {}) {
  installFetch({ ...setup, caps: caps ?? setup.caps });
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <EvalsPage />
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

describe("EvalsPage", () => {
  it("renders eval suite list", async () => {
    renderPage(EDITOR_CAPS);
    expect(await screen.findByTestId("eval-suite-my-eval")).toBeInTheDocument();
    expect(screen.getByTestId("eval-suite-other-eval")).toBeInTheDocument();
    expect(screen.getByText("my-eval")).toBeInTheDocument();
    expect(screen.getByText("other-eval")).toBeInTheDocument();
    // The gate and its threshold render as one string in the Gate column.
    expect(screen.getByText("exact-match ≥ 0.8")).toBeInTheDocument();
  });

  it("shows empty state when no suites", async () => {
    renderPage(EDITOR_CAPS, { suites: [] });
    expect(await screen.findByText(/No eval suites yet/i)).toBeInTheDocument();
  });

  it("results panel: gate outcome from conditions, scores when scoresAvailable=true", async () => {
    renderPage(EDITOR_CAPS, { resultsBody: RESULTS_WITH_SCORES });
    // Wait for suites to load
    await screen.findByTestId("eval-suite-my-eval");

    // Open the row — the results live in its drawer (§4.4 dropped ≠ lost).
    fireEvent.click(screen.getByTestId("eval-suite-my-eval"));

    // Wait for results panel
    await waitFor(() => {
      expect(screen.getByTestId("eval-results-panel-my-eval")).toBeInTheDocument();
    });

    // Gate outcome from conditions (secondary — only when the CRD status carries them)
    expect(screen.getByText("GatePassed")).toBeInTheDocument();
    expect(screen.getByText("(ThresholdMet)")).toBeInTheDocument();

    // Real per-agent gate result (ADR 0094) — the primary gate view: agent, score,
    // threshold, decision.
    expect(screen.getByTestId("eval-gate-result-prod-agent")).toBeInTheDocument();
    expect(screen.getByText(/score 0\.9182/)).toBeInTheDocument();
    expect(screen.getByText("promoted")).toBeInTheDocument();

    // Real scores shown (scoresAvailable=true) — asserted on the score row so
    // the scorer name matching elsewhere on the page can't satisfy it.
    expect(screen.getByTestId("eval-score-exact-match")).toHaveTextContent(
      "exact-match",
    );
    expect(screen.getByTestId("eval-score-exact-match")).toHaveTextContent("0.9");

    // The "unavailable" message must NOT appear when scores are available
    expect(screen.queryByTestId("eval-scores-unavailable-my-eval")).not.toBeInTheDocument();
  });

  it("results: scoresAvailable=false → reason shown calmly, NO fabricated scores", async () => {
    renderPage(EDITOR_CAPS, { resultsBody: RESULTS_NO_SCORES });
    await screen.findByTestId("eval-suite-my-eval");

    fireEvent.click(screen.getByTestId("eval-suite-my-eval"));

    await waitFor(() => {
      expect(screen.getByTestId("eval-results-panel-my-eval")).toBeInTheDocument();
    });

    // Gate outcome still shown
    expect(screen.getByText("GatePassed")).toBeInTheDocument();
    expect(screen.getByText("(PendingScores)")).toBeInTheDocument();

    // Unavailable reason shown calmly
    expect(screen.getByTestId("eval-scores-unavailable-my-eval")).toBeInTheDocument();
    expect(screen.getByText("Langfuse not configured")).toBeInTheDocument();

    // No fabricated numeric scores in the document — no score row at all, and
    // no bare figure anywhere that could be read as one.
    expect(screen.queryByTestId("eval-score-exact-match")).not.toBeInTheDocument();
    expect(screen.queryByText("0.9")).not.toBeInTheDocument();
    expect(screen.queryByText("0.8")).not.toBeInTheDocument();
  });

  it("builder: create calls createEvalSuite with dataset/scorers/gate", async () => {
    const calls = installFetch({ caps: EDITOR_CAPS });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <EvalsPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    // Open builder
    await screen.findByTestId("evals-new-btn");
    fireEvent.click(screen.getByTestId("evals-new-btn"));

    // Wait for builder wizard
    expect(await screen.findByTestId("eval-builder")).toBeInTheDocument();

    // Step 1: dataset ref
    fireEvent.change(screen.getByTestId("eval-dataset-ref-input"), {
      target: { value: "my-dataset" },
    });
    // Next
    fireEvent.click(screen.getByText("Next"));

    // Step 2: scorers
    await waitFor(() =>
      expect(screen.getByTestId("eval-scorers-input")).toBeInTheDocument(),
    );
    fireEvent.change(screen.getByTestId("eval-scorers-input"), {
      target: { value: "exact-match" },
    });
    fireEvent.click(screen.getByText("Next"));

    // Step 3: gate
    await waitFor(() =>
      expect(screen.getByTestId("eval-gate-input")).toBeInTheDocument(),
    );
    fireEvent.change(screen.getByTestId("eval-gate-input"), {
      target: { value: "exact-match" },
    });
    fireEvent.change(screen.getByTestId("eval-threshold-input"), {
      target: { value: "0.8" },
    });
    fireEvent.click(screen.getByText("Next"));

    // Step 4: review + create
    await waitFor(() =>
      expect(screen.getByTestId("eval-builder-review-step")).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByText("Create"));

    // Verify the POST body
    await waitFor(() => {
      const postCall = calls.find(
        (c) => c.url === "/api/evalsuites" && c.method === "POST",
      );
      expect(postCall).toBeTruthy();
      const body = JSON.parse(postCall!.body) as {
        datasetRef: string;
        scorers: string[];
        gate: string;
        threshold: number;
      };
      expect(body.datasetRef).toBe("my-dataset");
      expect(body.scorers).toContain("exact-match");
      expect(body.gate).toBe("exact-match");
      expect(body.threshold).toBe(0.8);
    });
  });

  it("RBAC: viewer sees no 'New eval suite' button and no delete buttons", async () => {
    renderPage(VIEWER_CAPS);
    await screen.findByTestId("eval-suite-my-eval");

    expect(screen.queryByTestId("evals-new-btn")).not.toBeInTheDocument();
    expect(screen.queryByTestId("eval-delete-my-eval")).not.toBeInTheDocument();
  });

  // ---- eval-gated metric stat card (M69, ADR 0062 governance #2) -----------

  it("stat: renders percent and gated/total from the BFF metric", async () => {
    renderPage(EDITOR_CAPS, { metricBody: METRIC_ABOVE_TARGET });
    // Wait for the metric card to appear (it loads independently of the suite list).
    await waitFor(() => {
      expect(screen.getByTestId("eval-gated-stat")).toBeInTheDocument();
    });
    // Percent value is rendered.
    expect(screen.getByTestId("eval-gated-percent")).toHaveTextContent("75.0%");
    // Sub-label shows gated/total.
    expect(screen.getByTestId("eval-gated-sub")).toHaveTextContent("3/4 deployments");
  });

  it("stat: >50% target indicator appears when metric meets the target", async () => {
    renderPage(EDITOR_CAPS, { metricBody: METRIC_ABOVE_TARGET });
    await waitFor(() => {
      expect(screen.getByTestId("eval-gated-target")).toBeInTheDocument();
    });
    expect(screen.getByTestId("eval-gated-target")).toHaveTextContent(/Above 50% target/);
  });

  it("stat: target indicator shows goal when below 50%", async () => {
    renderPage(EDITOR_CAPS, { metricBody: METRIC_BELOW_TARGET });
    await waitFor(() => {
      expect(screen.getByTestId("eval-gated-target")).toBeInTheDocument();
    });
    expect(screen.getByTestId("eval-gated-target")).toHaveTextContent(/Target:.*50%/);
  });

  it("stat: shows 0.0% honestly when no deployments are gated", async () => {
    renderPage(EDITOR_CAPS, { metricBody: { total: 3, gated: 0, percent: 0 } });
    await waitFor(() => {
      expect(screen.getByTestId("eval-gated-percent")).toHaveTextContent("0.0%");
    });
    expect(screen.getByTestId("eval-gated-sub")).toHaveTextContent("0/3 deployments");
  });

  it("stat: hides the card entirely when the endpoint returns 501 (dev substrate)", async () => {
    renderPage(EDITOR_CAPS, { metricStatus: 501 });
    // Suite list still loads.
    await screen.findByTestId("eval-suite-my-eval");
    // Stat card absent — no fabricated 0/0.
    expect(screen.queryByTestId("eval-gated-stat")).not.toBeInTheDocument();
    // ...and the absence is STATED (§7.1), not silent: a capability the reader
    // cannot see is indistinguishable from a page that failed to render.
    expect(screen.getByTestId("eval-gated-unavailable")).toBeInTheDocument();
  });

  // ---- attention-first order + the Next step column (§6.1 A1) --------------

  it("sorts by what is blocking, and says what to do about it", async () => {
    renderPage(EDITOR_CAPS, {
      suites: [
        // Ready, scored — needs nothing from this list.
        {
          name: "quiet-suite",
          namespace: "default",
          datasetRef: "ds",
          scorers: ["exact-match"],
          ready: true,
        },
        // Not ready — worth a look, but well formed.
        {
          name: "pending-suite",
          namespace: "default",
          datasetRef: "ds",
          scorers: ["bleu"],
          ready: false,
        },
        // No scorers at all — it can never produce a result to gate on, so it
        // is the most urgent row on the page.
        { name: "inert-suite", namespace: "default", datasetRef: "ds", ready: true },
      ],
    });
    await screen.findByTestId("eval-suite-inert-suite");

    expect(screen.getByTestId("next-step-inert-suite")).toHaveTextContent(
      "Add a scorer",
    );
    expect(screen.getByTestId("next-step-pending-suite")).toHaveTextContent(
      "Review the suite",
    );
    // A row that needs nothing says so, and never invents an errand.
    expect(screen.getByTestId("next-step-quiet-suite")).toHaveTextContent(
      "Nothing needed",
    );

    // An empty scorer list is a REAL answer (the API omits it), so it renders
    // the "declared but never exercised" Tag — never a dash, never a zero.
    expect(screen.getByText("no scorers")).toBeInTheDocument();

    // Attention-first: the two rows that need a person sort above the one that
    // does not, and "Nothing needed" is last.
    const order = screen
      .getAllByTestId(/^next-step-/)
      .map((el) => el.textContent ?? "");
    expect(order[0]).toContain("Add a scorer");
    expect(order[order.length - 1]).toContain("Nothing needed");
  });

  it("a chip view that matches nothing is the filtered state, not the first-run one", async () => {
    renderPage(EDITOR_CAPS, {
      suites: [
        {
          name: "quiet-suite",
          namespace: "default",
          datasetRef: "ds",
          scorers: ["exact-match"],
          ready: true,
        },
      ],
    });
    await screen.findByTestId("eval-suite-quiet-suite");

    fireEvent.click(screen.getByRole("radio", { name: /Needs you/ }));

    expect(await screen.findByText("Nothing needs a person")).toBeInTheDocument();
    // The way back out is offered; the first-run teaching copy is NOT shown.
    expect(
      screen.getByRole("button", { name: /Show everything/ }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/No eval suites yet/i)).not.toBeInTheDocument();
  });
});
