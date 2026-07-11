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
  scoresAvailable: false,
  scoresUnavailableReason: "Langfuse not configured",
};

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
    // gate badge visible
    expect(screen.getByText(/gate: exact-match/)).toBeInTheDocument();
  });

  it("shows empty state when no suites", async () => {
    renderPage(EDITOR_CAPS, { suites: [] });
    expect(await screen.findByText(/No eval suites yet/i)).toBeInTheDocument();
  });

  it("results panel: gate outcome from conditions, scores when scoresAvailable=true", async () => {
    renderPage(EDITOR_CAPS, { resultsBody: RESULTS_WITH_SCORES });
    // Wait for suites to load
    await screen.findByTestId("eval-suite-my-eval");

    // Expand results
    fireEvent.click(screen.getByTestId("eval-results-my-eval"));

    // Wait for results panel
    await waitFor(() => {
      expect(screen.getByTestId("eval-results-panel-my-eval")).toBeInTheDocument();
    });

    // Gate outcome from conditions
    expect(screen.getByText("GatePassed")).toBeInTheDocument();
    expect(screen.getByText("(ThresholdMet)")).toBeInTheDocument();

    // Real scores shown (scoresAvailable=true)
    expect(screen.getByText("exact-match")).toBeInTheDocument();
    expect(screen.getByText("0.9")).toBeInTheDocument();

    // The "unavailable" message must NOT appear when scores are available
    expect(screen.queryByTestId("eval-scores-unavailable-my-eval")).not.toBeInTheDocument();
  });

  it("results: scoresAvailable=false → reason shown calmly, NO fabricated scores", async () => {
    renderPage(EDITOR_CAPS, { resultsBody: RESULTS_NO_SCORES });
    await screen.findByTestId("eval-suite-my-eval");

    fireEvent.click(screen.getByTestId("eval-results-my-eval"));

    await waitFor(() => {
      expect(screen.getByTestId("eval-results-panel-my-eval")).toBeInTheDocument();
    });

    // Gate outcome still shown
    expect(screen.getByText("GatePassed")).toBeInTheDocument();
    expect(screen.getByText("(PendingScores)")).toBeInTheDocument();

    // Unavailable reason shown calmly
    expect(screen.getByTestId("eval-scores-unavailable-my-eval")).toBeInTheDocument();
    expect(screen.getByText("Langfuse not configured")).toBeInTheDocument();

    // No fabricated numeric scores in the document
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
});
