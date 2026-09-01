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
//   7. M151: the §4.4 queue budget on the kit DataTable, the §2.4 hue fix
//      (pending = hold, never warn), the endpoint moved into the row's drawer
//      per §4.5, and the four §7 states kept visibly distinct.

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
    expect(screen.getByText(/alice/)).toBeInTheDocument();

    // The endpoint moved OUT of the bare table cell (§4.5 — a URL belongs in a
    // code well, and the 63-char name + URL is what overflowed this row at 768)
    // and into the row's record. Same assertion, one click further in.
    fireEvent.click(screen.getByTestId("mcp-review-default-acme-mcp"));
    expect(
      await within(screen.getByRole("dialog")).findByText("https://mcp.acme.dev/sse"),
    ).toBeInTheDocument();
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

    // The Decide cell is never blank: a viewer gets the read-only next step
    // rather than two controls that would 403.
    expect(screen.getByTestId("mcp-review-default-acme-mcp")).toHaveTextContent(
      "Review the ask",
    );
  });

  it("a forced 403 on approve surfaces honestly via action-error", async () => {
    renderPage(OPERATOR_CAPS, { approveOk: false, approveStatus: 403 });

    const btn = await screen.findByTestId("mcp-approve-default-acme-mcp");
    fireEvent.click(btn);

    expect(await screen.findByTestId("action-error")).toBeInTheDocument();
    expect(screen.getByTestId("action-error")).toHaveTextContent(/Not allowed/);
  });

  it("an empty queue reads as good news, not as a bare 'no results'", async () => {
    renderPage(OPERATOR_CAPS, { approvals: [] });

    expect(await screen.findByText("Nothing is waiting on you.")).toBeInTheDocument();
  });

  it("shows the absent-backend state on 501 (feature not enabled)", async () => {
    renderPage(OPERATOR_CAPS, { approvalsStatus: 501 });
    expect(await screen.findByText(/enabled on this install/i)).toBeInTheDocument();
    // Distinct from good news: an install without the queue is not an empty queue.
    expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
  });
});

// ── M151: colour doctrine + the queue archetype ───────────────────────────────

describe("McpApprovalsPage — colour doctrine (§2.2 / §2.4)", () => {
  it("a person-must-decide row wears HOLD, never warn", async () => {
    renderPage(OPERATOR_CAPS);

    const tags = await screen.findAllByText("Awaiting review");
    // hold = bg-hold-surface / text-hold. Amber now means only "a bound is near
    // or crossed", which a queued submission is not.
    expect(tags[0].className).toContain("bg-hold-surface");
    expect(tags[0].className).not.toContain("bg-warning-surface");
    // The old word is gone with the old hue: "pending" reads as "the machine is
    // working on it", which is the pine-tint `progressing` meaning.
    expect(screen.queryByText("pending")).toBeNull();
  });

  it("the hold hue is spent once more, on the page's own count", async () => {
    renderPage(OPERATOR_CAPS);

    const chip = await screen.findByTestId("mcp-approvals-waiting-count");
    expect(chip).toHaveTextContent("2 awaiting a person");
  });
});

describe("McpApprovalsPage — the closing line (§5.18)", () => {
  it("states what approving would actually grant, counted from the rows", async () => {
    renderPage(OPERATOR_CAPS);

    // 3 + 1 tools across the two fixture rows.
    expect(
      await screen.findByText(
        /2 MCP servers are waiting on an operator\. Approving them all would add 4 tools to the catalog\./,
      ),
    ).toBeInTheDocument();
  });

  it("never counts a submission that did not report its tools", async () => {
    renderPage(OPERATOR_CAPS, {
      approvals: [
        { namespace: "default", name: "acme-mcp", toolCount: 3 },
        { namespace: "staging", name: "beta-mcp" }, // no toolCount — unknown, not zero
      ],
    });

    expect(
      await screen.findByText(/Approving the 1 that reported their tools would add 3 tools/),
    ).toBeInTheDocument();
    // An unreported count renders the dash, never a 0 (§7.1).
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });
});
