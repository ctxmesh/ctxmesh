import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { RunDetailPage } from "@/pages/run-detail-page";
import { ToastProvider } from "@/components/kit";
import type { RunDetail } from "@/lib/api";

// RunDetailPage tests (V5, M112).
//
// Coverage:
//   (a) A plan_approval (requires_action=approval + status=requires_action) run
//       renders the Approve/Deny panel; clicking Approve POSTs the resume decision.
//   (b) descendantsRequiringAction renders as navigable links to /runs/:descId.
//   (c) A non-paused run renders its summary WITHOUT the approval panel.
//   (d) Loading skeleton shows; not-found / generic error states render correctly.

// ── Fixtures ──────────────────────────────────────────────────────────────────

const BASE_RUN: RunDetail = {
  id: "run-abc123",
  status: "succeeded",
};

const APPROVAL_RUN: RunDetail = {
  id: "run-approval-xyz",
  status: "requires_action",
  agent: "default/planner",
  namespace: "default",
  // plan_approval — the workflow PLAN gate, the primary case reached from the approval
  // queue + the alert deep-link. The panel must render for this kind (not only "approval").
  requiresAction: {
    kind: "plan_approval",
    key: "internal-resume-token-never-shown",
    message: "Please approve: agent wants to send email to user@example.com",
  },
};

const RUN_WITH_DESCENDANTS: RunDetail = {
  id: "run-parent-001",
  status: "requires_action",
  requiresAction: {
    kind: "approval",
    message: "Parent needs approval",
  },
  descendantsRequiringAction: [
    {
      runId: "sub-run-aaa",
      agent: "default/sub-agent",
      kind: "approval",
      message: "Sub-run needs approval too",
    },
    {
      runId: "sub-run-bbb",
      kind: "approval",
    },
  ],
};

// ── Fetch stub helpers ────────────────────────────────────────────────────────

function stubFetch(opts: {
  run?: RunDetail;
  runStatus?: number;
  resumeOk?: boolean;
} = {}) {
  const { run = BASE_RUN, runStatus = 200, resumeOk = true } = opts;

  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";

      // GET /api/runs/:id
      if (url.includes("/api/runs/") && (method === "GET" || !init?.method) && !url.includes("/resume")) {
        if (runStatus === 404) {
          return Promise.resolve({
            ok: false,
            status: 404,
            json: async () => ({ error: "not found" }),
            text: async () => "not found",
          } as Response);
        }
        if (runStatus === 403) {
          return Promise.resolve({
            ok: false,
            status: 403,
            json: async () => ({ error: "forbidden" }),
            text: async () => "forbidden",
          } as Response);
        }
        if (runStatus !== 200) {
          return Promise.resolve({
            ok: false,
            status: runStatus,
            json: async () => ({ error: "server error" }),
            text: async () => "server error",
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => run,
          text: async () => JSON.stringify(run),
        } as Response);
      }

      // POST /api/runs/:id/resume
      if (url.includes("/resume")) {
        if (!resumeOk) {
          return Promise.resolve({
            ok: false,
            status: 500,
            json: async () => ({ error: "resume failed" }),
            text: async () => "resume failed",
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ runId: run.id }),
          text: async () => "{}",
        } as Response);
      }

      // Fallback: return empty ok
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
        text: async () => "{}",
      } as Response);
    }),
  );
}

// ── Render helper ─────────────────────────────────────────────────────────────

function renderPage(runId = "run-abc123") {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={[`/runs/${runId}`]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailPage />} />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("RunDetailPage (V5, M112)", () => {
  // ── (a) Approve/Deny panel ──────────────────────────────────────────────────

  describe("plan_approval run — requires_action=approval", () => {
    it("renders the approval panel with Approve and Deny buttons", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      const panel = await screen.findByTestId("run-approval-panel");
      expect(panel).toBeInTheDocument();
      expect(
        screen.getByTestId("run-approve-btn"),
      ).toBeInTheDocument();
      expect(
        screen.getByTestId("run-deny-btn"),
      ).toBeInTheDocument();
    });

    it("renders the approval message inside the panel", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-approval-panel");
      expect(screen.getByText(/send email to user@example.com/i)).toBeInTheDocument();
    });

    it("clicking Approve POSTs decision=approve to /api/runs/{id}/resume", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-approve-btn");
      fireEvent.click(screen.getByTestId("run-approve-btn"));

      const fetchMock = vi.mocked(globalThis.fetch);
      const resumeCall = fetchMock.mock.calls.find(
        ([url]) =>
          typeof url === "string" &&
          url.includes(`/api/runs/${encodeURIComponent(APPROVAL_RUN.id)}/resume`),
      );
      expect(resumeCall).toBeDefined();
      // Verify the body includes decision=approve
      const body = resumeCall![1]?.body;
      expect(body).toBeTruthy();
      const parsed = JSON.parse(body as string) as { decision: string };
      expect(parsed.decision).toBe("approve");
    });

    it("clicking Deny POSTs decision=deny to /api/runs/{id}/resume", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-deny-btn");
      fireEvent.click(screen.getByTestId("run-deny-btn"));

      const fetchMock = vi.mocked(globalThis.fetch);
      const resumeCall = fetchMock.mock.calls.find(
        ([url]) =>
          typeof url === "string" &&
          url.includes(`/api/runs/${encodeURIComponent(APPROVAL_RUN.id)}/resume`),
      );
      expect(resumeCall).toBeDefined();
      const body = resumeCall![1]?.body;
      const parsed = JSON.parse(body as string) as { decision: string };
      expect(parsed.decision).toBe("deny");
    });

    it("disables both buttons while the decision is in flight", async () => {
      // Never resolve the resume fetch so the submitting state stays
      vi.stubGlobal(
        "fetch",
        vi.fn((input: string | URL, _init?: RequestInit) => {
          const url = typeof input === "string" ? input : input.toString();
          if (url.includes("/resume")) return new Promise(() => {});
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => APPROVAL_RUN,
            text: async () => JSON.stringify(APPROVAL_RUN),
          } as Response);
        }),
      );
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-approve-btn");
      fireEvent.click(screen.getByTestId("run-approve-btn"));

      expect(screen.getByTestId("run-approve-btn")).toBeDisabled();
      expect(screen.getByTestId("run-deny-btn")).toBeDisabled();
    });

    it("does NOT display the approval key to the user", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-approval-panel");
      // The internal key must never appear on screen
      expect(
        screen.queryByText("internal-resume-token-never-shown"),
      ).toBeNull();
    });
  });

  // ── (b) Descendants requiring action ─────────────────────────────────────────

  describe("descendantsRequiringAction — nested approvals section", () => {
    it("renders a nested-approvals section with links to each sub-run", async () => {
      stubFetch({ run: RUN_WITH_DESCENDANTS });
      renderPage(RUN_WITH_DESCENDANTS.id);

      await screen.findByTestId("run-nested-approvals");

      const linkA = screen.getByTestId("nested-run-sub-run-aaa");
      const linkB = screen.getByTestId("nested-run-sub-run-bbb");

      expect(linkA).toHaveAttribute("href", "/runs/sub-run-aaa");
      expect(linkB).toHaveAttribute("href", "/runs/sub-run-bbb");
    });

    it("shows each descendant's agent and message when present", async () => {
      stubFetch({ run: RUN_WITH_DESCENDANTS });
      renderPage(RUN_WITH_DESCENDANTS.id);

      await screen.findByTestId("run-nested-approvals");
      expect(screen.getByText(/default\/sub-agent/)).toBeInTheDocument();
      expect(screen.getByText(/Sub-run needs approval too/)).toBeInTheDocument();
    });
  });

  // ── (c) Non-paused run — no approval panel ────────────────────────────────

  describe("non-paused run", () => {
    it("renders the run summary header without the approval panel", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-approval-panel")).toBeNull();
      expect(screen.queryByTestId("run-approve-btn")).toBeNull();
      expect(screen.queryByTestId("run-deny-btn")).toBeNull();
    });

    it("shows the run id in the header", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      const header = await screen.findByTestId("run-detail-header");
      expect(header).toHaveTextContent("run-abc123");
    });

    it("does not render nested-approvals when descendantsRequiringAction is absent", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-nested-approvals")).toBeNull();
    });
  });

  // ── (d) Loading / not-found / error states ────────────────────────────────

  describe("loading state", () => {
    it("shows skeleton cards before data arrives and no run-detail-page", () => {
      vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
      renderPage();

      expect(screen.queryByTestId("run-detail-page")).toBeNull();
      // Skeleton cards render as role=status
      expect(screen.getAllByRole("status").length).toBeGreaterThan(0);
    });
  });

  describe("not-found state (404)", () => {
    it("renders the not-found message", async () => {
      stubFetch({ runStatus: 404 });
      renderPage("ghost-id");

      await screen.findByTestId("run-detail-not-found");
      expect(screen.getByText(/run not found/i)).toBeInTheDocument();
    });
  });

  describe("generic error state (500)", () => {
    it("renders an error notice when the fetch fails", async () => {
      stubFetch({ runStatus: 500 });
      renderPage("some-id");

      await waitFor(() =>
        expect(screen.getByTestId("run-detail-error")).toBeInTheDocument(),
      );
    });
  });

  describe("forbidden error state (403)", () => {
    it("renders ForbiddenInline when the API returns 403", async () => {
      stubFetch({ runStatus: 403 });
      renderPage("secret-id");

      // ForbiddenInline delegates to ErrorState which renders role="alert".
      // It also shows the "Not allowed to read this run" title we pass in.
      await screen.findByRole("alert");
      expect(screen.getByText(/not allowed to read this run/i)).toBeInTheDocument();
    });
  });
});
