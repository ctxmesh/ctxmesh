import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { RunDetailPage } from "@/pages/run-detail-page";
import { ToastProvider } from "@/components/kit";
import type { RunDetail } from "@/lib/api";

// RunDetailPage tests (V5, M112; M113 additions).
//
// Coverage:
//   (a) A plan_approval (requires_action=approval + status=requires_action) run
//       renders the Approve/Deny panel; clicking Approve POSTs the resume decision.
//   (b) descendantsRequiringAction renders as navigable links to /runs/:descId.
//   (c) A non-paused run renders its summary WITHOUT the approval panel.
//   (d) Loading skeleton shows; not-found / generic error states render correctly.
//   (e) M113: Original request context (messages[0] or input) renders for paused runs.
//   (f) M113: Waiting-since renders from updatedAt for paused runs.

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

// APPROVAL_RUN_WITH_CONTEXT — paused run that carries messages + updatedAt (M113).
const APPROVAL_RUN_WITH_CONTEXT: RunDetail = {
  id: "run-context-xyz",
  status: "requires_action",
  agent: "default/planner",
  namespace: "default",
  updatedAt: new Date(Date.now() - 30 * 60 * 1000).toISOString(), // 30 min ago
  messages: [
    { role: "user", content: "Please draft a summary of Q3 sales." },
    { role: "assistant", content: "I will now draft the Q3 sales summary…" },
  ],
  requiresAction: {
    kind: "plan_approval",
    key: "internal-token",
    message: "Agent will access the sales DB.",
  },
};

// APPROVAL_RUN_INPUT_ONLY — paused run with no messages but an input field (M113 fallback).
const APPROVAL_RUN_INPUT_ONLY: RunDetail = {
  id: "run-input-only",
  status: "requires_action",
  input: { task: "analyse the report" },
  requiresAction: { kind: "plan_approval" },
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

    // V16 (m115.4): a reason typed into the textarea is sent with a deny.
    it("sends the typed reason with a deny", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      const reason = await screen.findByTestId("run-deny-reason");
      fireEvent.change(reason, { target: { value: "scope too broad" } });
      fireEvent.click(screen.getByTestId("run-deny-btn"));

      const fetchMock = vi.mocked(globalThis.fetch);
      const resumeCall = fetchMock.mock.calls.find(
        ([url]) =>
          typeof url === "string" &&
          url.includes(`/api/runs/${encodeURIComponent(APPROVAL_RUN.id)}/resume`),
      );
      expect(resumeCall).toBeDefined();
      const parsed = JSON.parse(resumeCall![1]?.body as string) as {
        decision: string;
        reason?: string;
      };
      expect(parsed.decision).toBe("deny");
      expect(parsed.reason).toBe("scope too broad");
    });

    // Approve ignores the reason field even if text was typed.
    it("does NOT send a reason on approve", async () => {
      stubFetch({ run: APPROVAL_RUN });
      renderPage(APPROVAL_RUN.id);

      const reason = await screen.findByTestId("run-deny-reason");
      fireEvent.change(reason, { target: { value: "typed but approving" } });
      fireEvent.click(screen.getByTestId("run-approve-btn"));

      const fetchMock = vi.mocked(globalThis.fetch);
      const resumeCall = fetchMock.mock.calls.find(
        ([url]) =>
          typeof url === "string" &&
          url.includes(`/api/runs/${encodeURIComponent(APPROVAL_RUN.id)}/resume`),
      );
      const parsed = JSON.parse(resumeCall![1]?.body as string) as {
        decision: string;
        reason?: string;
      };
      expect(parsed.decision).toBe("approve");
      expect(parsed.reason).toBeUndefined();
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

    // V16 (M115): a pending-count badge orients the reviewer on how many sub-runs still wait.
    it("badges the pending descendant count", async () => {
      stubFetch({ run: RUN_WITH_DESCENDANTS });
      renderPage(RUN_WITH_DESCENDANTS.id);

      const badge = await screen.findByTestId("nested-approvals-count");
      expect(badge).toHaveTextContent("2");
    });
  });

  // ── V16 (M115): nav-out — resolved-run back-link + back-to-parent ──────────────

  describe("V16 nav-out (M115)", () => {
    // F7a: a run a colleague already resolved, reached by deep-link, still has a way back to the queue —
    // the "← Approvals" link is gated on the DURABLE approval signal (requiresAction kind), not on the
    // transient requires_action status.
    it("shows '← Approvals' on a RESOLVED approval run (no approval panel)", async () => {
      const RESOLVED_APPROVAL: RunDetail = {
        id: "run-resolved",
        status: "cancelled",
        requiresAction: { kind: "approval", message: "approval denied" },
      };
      stubFetch({ run: RESOLVED_APPROVAL });
      renderPage(RESOLVED_APPROVAL.id);

      const back = await screen.findByTestId("run-back-approvals");
      expect(back).toHaveAttribute("href", "/approvals");
      // …but the approve/deny panel must NOT render (the run is no longer paused).
      expect(screen.queryByTestId("run-approval-panel")).toBeNull();
    });

    // F3: a sub-run offers a back-to-parent nav (to the tree root where nested approvals are overviewed).
    it("shows '← Parent run' for a sub-run, linking to its parent/root", async () => {
      const SUB_RUN: RunDetail = {
        id: "run-sub",
        status: "running",
        parentRunId: "run-parent-001",
        rootRunId: "run-parent-001",
      };
      stubFetch({ run: SUB_RUN });
      renderPage(SUB_RUN.id);

      const back = await screen.findByTestId("run-back-parent");
      expect(back).toHaveAttribute("href", "/runs/run-parent-001");
    });

    // A plain root run (no approval, no lineage) shows neither nav-out link.
    it("shows no nav-out on a plain non-approval root run", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-back-approvals")).toBeNull();
      expect(screen.queryByTestId("run-back-parent")).toBeNull();
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
      // The copy is §7 A5's forbidden sentence, resource-named (M100 UI99-403):
      // a permission boundary, never the raw RBAC string.
      await screen.findByRole("alert");
      expect(
        screen.getByText(/permission to view this run/i),
      ).toBeInTheDocument();
    });
  });

  // ── (e) M113: Original request context ───────────────────────────────────────

  describe("M113 — original request context for paused runs", () => {
    it("renders the first user message as the original request when messages are present", async () => {
      stubFetch({ run: APPROVAL_RUN_WITH_CONTEXT });
      renderPage(APPROVAL_RUN_WITH_CONTEXT.id);

      const panel = await screen.findByTestId("run-original-request");
      // The first user message content is shown IN THE PANEL. Scoped, because
      // the page now also renders the run's story as a Timeline: the assertion
      // was always about what this panel carries, not about the whole document.
      expect(
        within(panel).getByText("Please draft a summary of Q3 sales."),
      ).toBeInTheDocument();
      // The assistant message is NOT the one the panel shows (only the ask).
      expect(
        within(panel).queryByText("I will now draft the Q3 sales summary…"),
      ).toBeNull();
    });

    it("falls back to detail.input when messages is absent", async () => {
      stubFetch({ run: APPROVAL_RUN_INPUT_ONLY });
      renderPage(APPROVAL_RUN_INPUT_ONLY.id);

      await screen.findByTestId("run-original-request");
      // The input object is JSON-stringified and shown
      expect(screen.getByText(/analyse the report/)).toBeInTheDocument();
    });

    it("does NOT render original-request section for non-paused runs", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-original-request")).toBeNull();
    });

    it("does NOT render original-request when paused but no messages/input", async () => {
      stubFetch({ run: APPROVAL_RUN }); // APPROVAL_RUN has no messages or input
      renderPage(APPROVAL_RUN.id);

      await screen.findByTestId("run-approval-panel");
      expect(screen.queryByTestId("run-original-request")).toBeNull();
    });
  });

  // ── (f) M113: Waiting-since in header ────────────────────────────────────────

  describe("M113 — waiting-since in header", () => {
    it("renders waiting-since for a paused run with updatedAt", async () => {
      stubFetch({ run: APPROVAL_RUN_WITH_CONTEXT });
      renderPage(APPROVAL_RUN_WITH_CONTEXT.id);

      const waitingEl = await screen.findByTestId("run-waiting-since");
      // Should contain "Waiting since" + a relative time
      expect(waitingEl).toHaveTextContent(/Waiting since/);
      expect(waitingEl).toHaveTextContent(/ago/);
    });

    it("does NOT render waiting-since for a non-paused run", async () => {
      stubFetch({ run: BASE_RUN });
      renderPage(BASE_RUN.id);

      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-waiting-since")).toBeNull();
    });
  });

  // ── (g) m130: Result card — a completed run shows its final answer (+ spotlight strip) ──────────
  describe("Result card (m130)", () => {
    it("renders the final assistant answer for a succeeded run", async () => {
      stubFetch({
        run: {
          id: "run-result-1",
          status: "succeeded",
          messages: [
            { role: "user", content: "Prepare a briefing." },
            { role: "assistant", content: "Here is your final briefing, ready to publish." },
          ],
        },
      });
      renderPage("run-result-1");
      const card = await screen.findByTestId("run-result");
      expect(card).toHaveTextContent("Here is your final briefing, ready to publish.");
    });

    it("strips leaked K1 spotlight delimiters from the answer", async () => {
      stubFetch({
        run: {
          id: "run-result-2",
          status: "succeeded",
          messages: [
            { role: "assistant", content: "⟦tool-output:abc123⟧\nFinal polished copy.⟦/tool-output:abc123⟧" },
          ],
        },
      });
      renderPage("run-result-2");
      const card = await screen.findByTestId("run-result");
      expect(card).toHaveTextContent("Final polished copy.");
      expect(card).not.toHaveTextContent("tool-output:");
    });

    it("does NOT render a Result card for a non-succeeded run", async () => {
      stubFetch({
        run: { id: "run-running", status: "running", messages: [{ role: "assistant", content: "partial" }] },
      });
      renderPage("run-running");
      await screen.findByTestId("run-detail-header");
      expect(screen.queryByTestId("run-result")).toBeNull();
    });
  });
});
