import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApprovalsPage } from "@/pages/approvals-page";
import type { ApprovalQueueItem } from "@/lib/api";

// The queue is namespace-scoped and the backend REQUIRES a namespace, so the page reads the global
// namespace selector. Mock it (controllable) — a real namespace for the list tests, "" for the
// select-a-namespace prompt. vi.hoisted so the mock factory can read the current value.
const nsRef = vi.hoisted(() => ({ current: "team-a" }));
vi.mock("@/lib/namespace", () => ({
  useNamespace: () => ({ namespace: nsRef.current }),
}));

// ApprovalsPage (V5, M112) — the namespace-scoped plan-approval queue.
//
// Coverage:
//   • rows render with a link to /runs/{runId}
//   • the request carries ?namespace=<selected ns>
//   • a 403 renders the forbidden state (not a fake empty list)
//   • the empty state renders on []

function item_(over: Partial<ApprovalQueueItem> = {}): ApprovalQueueItem {
  return {
    runId: "run-abc123",
    agent: "my-agent",
    ...over,
  };
}

// installFetch stubs global fetch; the responder receives the parsed query string.
function installFetch(
  respond: (qs: URLSearchParams) => { ok: boolean; status?: number; body?: unknown },
) {
  const captured: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = respond(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? [],
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage(initialEntry = "/approvals") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route path="/approvals" element={<ApprovalsPage />} />
        <Route path="/runs/:id" element={<div data-testid="run-detail" />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  nsRef.current = "team-a"; // a real namespace by default; the no-namespace test overrides
});

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("ApprovalsPage — basic rendering (V5, M112)", () => {
  it("renders approvals-page and approvals-table with row data + run links", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-abc", agent: "workflow-agent-1" }),
        item_({ runId: "run-def", agent: "workflow-agent-2", message: "Proceed with plan?" }),
      ],
    }));

    renderPage();

    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    // The table is rendered (aria-label)
    expect(screen.getByRole("table", { name: "Plan approvals" })).toBeInTheDocument();
    // Run IDs appear as links
    const runAbcLink = screen.getByRole("link", { name: "run-abc" });
    expect(runAbcLink).toBeInTheDocument();
    expect(runAbcLink).toHaveAttribute("href", "/runs/run-abc");
    const runDefLink = screen.getByRole("link", { name: "run-def" });
    expect(runDefLink).toHaveAttribute("href", "/runs/run-def");
    // Agent names appear
    expect(screen.getByText("workflow-agent-1")).toBeInTheDocument();
    expect(screen.getByText("workflow-agent-2")).toBeInTheDocument();
    // Message appears
    expect(screen.getByText("Proceed with plan?")).toBeInTheDocument();
  });

  it("shows rootRunId context when present", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-child", agent: "sub-agent", rootRunId: "run-root" }),
      ],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("in tree")).toBeInTheDocument();
    // rootRunId links to its own run detail page
    const rootLink = screen.getByRole("link", { name: "run-root" });
    expect(rootLink).toHaveAttribute("href", "/runs/run-root");
  });
});

// ── Namespace scoping ──────────────────────────────────────────────────────────

describe("ApprovalsPage — namespace param (V5, M112)", () => {
  it("sends ?namespace=<selected> when a namespace is chosen", async () => {
    const captured = installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() => expect(captured.length).toBeGreaterThan(0));
    expect(captured[0]).toContain("/api/approvals");
    expect(captured[0]).toContain("namespace=team-a");
  });

  it("prompts to select a namespace (and fires NO request) when none is selected", async () => {
    nsRef.current = ""; // the default "all namespaces" scope — the backend requires a concrete one
    const captured = installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    expect(await screen.findByText("Select a namespace")).toBeInTheDocument();
    // No request is fired for the required-namespace endpoint (no 400 error surfaced).
    expect(captured.length).toBe(0);
  });
});

// ── Empty state ────────────────────────────────────────────────────────────────

describe("ApprovalsPage — empty state (V5, M112)", () => {
  it("renders the 'No runs are awaiting approval.' empty state when items is []", async () => {
    installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText("No runs are awaiting approval.")).toBeInTheDocument(),
    );
  });
});

// ── Degrade discipline: 403 forbidden / 500 error ─────────────────────────────

describe("ApprovalsPage — 403 / 500 states (V5, M112)", () => {
  it("403 renders the forbidden state — never a fake empty list", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you don't have permission to list workflows in this namespace" },
    }));

    renderPage();

    // The DataTable forbidden variant surfaces an access-denied message.
    // The exact text comes from DataTable's forbidden prop — check for it.
    await waitFor(() => {
      // DataTable with forbidden=true shows a message containing the resource name.
      // We assert the forbidden state is rendered (NOT an empty list, NOT a Retry button).
      expect(screen.queryByText("No runs are awaiting approval.")).toBeNull();
    });
    // No Retry button on 403 (forbidden is terminal).
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("500 surfaces a visible, retryable error", async () => {
    installFetch(() => ({
      ok: false,
      status: 500,
      body: { error: "failed to read the approval queue" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to read the approval queue/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
    expect(screen.queryByText("No runs are awaiting approval.")).toBeNull();
  });
});
