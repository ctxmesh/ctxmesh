import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { McpApprovalsPage } from "@/pages/mcp-approvals-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// McpApprovalsPage tests (m17.9):
//   1. Renders pending approval rows from GET /api/mcp/approvals.
//   2. Approve button calls POST /api/mcp/approvals/{ns}/{name}.
//   3. Reject (via ConfirmDialog) calls POST /api/mcp/approvals/{ns}/{name}/reject.
//   4. Non-operator (no update on agentregistries) sees NO approve/reject buttons.
//   5. A forced 403 on approve surfaces honestly (action-error testid).
//   6. Empty queue shows the empty state (not an error).

type FetchSetup = {
  caps?: Record<string, Record<string, boolean>>;
  approvals?: unknown[];
  approvalsStatus?: number;
  approveOk?: boolean;
  approveStatus?: number;
  rejectOk?: boolean;
  rejectStatus?: number;
};

const OPERATOR_CAPS = { agentregistries: { create: true, update: true, delete: true } };
const VIEWER_CAPS = { agentregistries: { create: false, update: false, delete: false } };

const PENDING_APPROVALS = [
  {
    namespace: "default",
    name: "acme-mcp",
    submittedBy: "alice",
    submittedAt: "2026-07-11T10:00:00Z",
    url: "https://mcp.acme.dev/sse",
    toolCount: 3,
  },
  {
    namespace: "staging",
    name: "beta-mcp",
    toolCount: 1,
  },
];

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

      if (url.startsWith("/api/namespaces"))
        return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? OPERATOR_CAPS });
      if (url === "/api/mcp/approvals" && method === "GET") {
        const status = opts.approvalsStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              // Match the REAL BFF shape (MCPServerListResponse: servers + items),
              // not a `{approvals}` wrapper — the integration-shape the crash bug hid.
              ? {
                  servers: opts.approvals ?? PENDING_APPROVALS,
                  items: opts.approvals ?? PENDING_APPROVALS,
                }
              : { error: "forbidden" },
        } as Response);
      }
      if (url.match(/\/api\/mcp\/approvals\/[^/]+\/[^/]+$/) && method === "POST") {
        const ok = opts.approveOk ?? true;
        const status = opts.approveStatus ?? (ok ? 204 : 403);
        return Promise.resolve({
          ok,
          status,
          json: async () => (ok ? {} : { error: `cannot approve: forbidden` }),
        } as Response);
      }
      if (url.match(/\/api\/mcp\/approvals\/[^/]+\/[^/]+\/reject$/) && method === "POST") {
        const ok = opts.rejectOk ?? true;
        const status = opts.rejectStatus ?? (ok ? 204 : 403);
        return Promise.resolve({
          ok,
          status,
          json: async () => (ok ? {} : { error: `cannot reject: forbidden` }),
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
            <McpApprovalsPage />
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

describe("McpApprovalsPage", () => {
  it("renders pending approval rows", async () => {
    renderPage(OPERATOR_CAPS);

    // Both MCPs should appear
    expect(await screen.findByTestId("mcp-approval-row-default-acme-mcp")).toBeInTheDocument();
    expect(screen.getByTestId("mcp-approval-row-staging-beta-mcp")).toBeInTheDocument();

    // Row content is visible
    expect(screen.getByText("acme-mcp")).toBeInTheDocument();
    expect(screen.getByText("beta-mcp")).toBeInTheDocument();
    expect(screen.getByText("https://mcp.acme.dev/sse")).toBeInTheDocument();
    expect(screen.getByText(/alice/)).toBeInTheDocument();
  });

  it("approve button calls approveMcp", async () => {
    const calls = installFetch({ caps: OPERATOR_CAPS });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <McpApprovalsPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    const btn = await screen.findByTestId("mcp-approve-default-acme-mcp");
    fireEvent.click(btn);

    await waitFor(() => {
      const approveCall = calls.find(
        (c) => c.url === "/api/mcp/approvals/default/acme-mcp" && c.method === "POST",
      );
      expect(approveCall).toBeDefined();
    });
  });

  it("reject goes through ConfirmDialog before calling rejectMcp", async () => {
    const calls = installFetch({ caps: OPERATOR_CAPS });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <McpApprovalsPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    // Click reject to open the confirmation dialog
    const rejectBtn = await screen.findByTestId("mcp-reject-default-acme-mcp");
    fireEvent.click(rejectBtn);

    // Confirmation dialog should appear
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toBeInTheDocument();

    // Confirm the rejection — find the confirm button INSIDE the dialog to avoid
    // matching the row-level Reject button(s) that remain visible behind the overlay.
    const { getByRole: getDlgRole } = within(dialog);
    const confirmBtn = getDlgRole("button", { name: /reject/i });
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      const rejectCall = calls.find(
        (c) =>
          c.url === "/api/mcp/approvals/default/acme-mcp/reject" && c.method === "POST",
      );
      expect(rejectCall).toBeDefined();
    });
  });

  it("cancel on ConfirmDialog does NOT call rejectMcp", async () => {
    const calls = installFetch({ caps: OPERATOR_CAPS });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <McpApprovalsPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    const rejectBtn = await screen.findByTestId("mcp-reject-default-acme-mcp");
    fireEvent.click(rejectBtn);
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: /cancel/i }));

    // Dialog closes, no reject call
    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    });
    const rejectCall = calls.find((c) => c.url.includes("/reject"));
    expect(rejectCall).toBeUndefined();
  });

  it("non-operator sees NO approve/reject actions", async () => {
    renderPage(VIEWER_CAPS);

    // Rows are visible but no action buttons
    await screen.findByTestId("mcp-approval-row-default-acme-mcp");
    expect(screen.queryByTestId("mcp-approve-default-acme-mcp")).not.toBeInTheDocument();
    expect(screen.queryByTestId("mcp-reject-default-acme-mcp")).not.toBeInTheDocument();
    expect(screen.queryByTestId("mcp-approve-staging-beta-mcp")).not.toBeInTheDocument();
    expect(screen.queryByTestId("mcp-reject-staging-beta-mcp")).not.toBeInTheDocument();
  });

  it("a forced 403 on approve surfaces honestly via action-error", async () => {
    renderPage(OPERATOR_CAPS, { approveOk: false, approveStatus: 403 });

    const btn = await screen.findByTestId("mcp-approve-default-acme-mcp");
    fireEvent.click(btn);

    expect(await screen.findByTestId("action-error")).toBeInTheDocument();
    expect(screen.getByTestId("action-error")).toHaveTextContent(/Not allowed/);
  });

  it("shows empty state when there are no pending approvals", async () => {
    renderPage(OPERATOR_CAPS, { approvals: [] });

    expect(
      await screen.findByText(/No pending approvals/),
    ).toBeInTheDocument();
  });

  it("shows disabled state on 501 (feature not enabled)", async () => {
    renderPage(OPERATOR_CAPS, { approvalsStatus: 501 });
    expect(await screen.findByText(/not enabled/i)).toBeInTheDocument();
  });
});
