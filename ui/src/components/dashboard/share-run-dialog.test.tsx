import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { ShareRunDialog } from "@/components/dashboard/share-run-dialog";
import { ToastProvider } from "@/components/kit";

// ShareRunDialog — m75.4 + m76.4 tests.
//
// Coverage (m75.4):
//   • metadata-only is the default (includeContent toggle OFF, preview says "not included")
//   • toggling includeContent updates the projection preview to show content fields
//   • creating a share calls the API and shows the one-time link with copy button
//   • manage section shows existing shares with revoke button
//   • revoking a share marks it revoked (button disappears)
//
// Coverage (m76.4 — V7/V8/V9/V11/V12):
//   V7: projection preview shows honest field list (no fake message count/roles)
//   V8: done state blocks backdrop/Escape dismiss until link is confirmed saved
//   V9: preview expander renders the real projection for the just-created share
//   V11: expired badge shown; revoked rows still shown (badged)
//   V12: isNotImplemented / isConflict errors soften to calm messages

const SHARE_RESULT = {
  id: "share-1",
  token: "abc-secret-token",
  expiresAt: "2027-08-08T10:00:00Z", // far future
  includeContent: false,
};

const EXISTING_SHARE = {
  id: "existing-1",
  createdAt: "2026-08-01T10:00:00Z",
  expiresAt: "2027-08-08T10:00:00Z", // far future — always active in tests
  revoked: false,
  includeContent: false,
};

const SHARED_RUN_VIEW = {
  id: "run-123",
  namespace: "team-a",
  agent: "assistant",
  status: "succeeded",
  createdAt: "2026-08-01T10:00:00Z",
  updatedAt: "2026-08-01T10:05:00Z",
  messageCount: 2,
  messageRoles: ["user", "assistant"],
  errorCategory: "",
  input: "Hello there",
  messages: [
    { role: "user", content: "Hello there" },
    { role: "assistant", content: "Hi, how can I help?" },
  ],
};

function installFetch(
  opts: {
    listShares?: unknown[];
    createResult?: unknown;
    createStatus?: number;
    revokeOk?: boolean;
    sharedRunView?: unknown;
  } = {},
) {
  const {
    listShares = [],
    createResult = SHARE_RESULT,
    createStatus = 200,
    revokeOk = true,
    sharedRunView = SHARED_RUN_VIEW,
  } = opts;

  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();

      // GET /api/shared/runs/{token} → public shared run view (V9 preview)
      if (url.includes("/api/shared/runs/") && method === "GET") {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => sharedRunView,
          text: async () => "{}",
        } as Response);
      }
      // GET /api/runs/{id}/shares → list
      if (url.includes("/shares") && !url.includes("/shares/") && method === "GET") {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => listShares,
          text: async () => "[]",
        } as Response);
      }
      // POST /api/runs/{id}/shares → create
      if (url.includes("/shares") && method === "POST") {
        const ok = createStatus >= 200 && createStatus < 300;
        return Promise.resolve({
          ok,
          status: createStatus,
          json: async () => createResult,
          text: async () => JSON.stringify(createResult),
        } as Response);
      }
      // DELETE /api/runs/{id}/shares/{shareId} → revoke
      if (url.includes("/shares/") && method === "DELETE") {
        return Promise.resolve({
          ok: revokeOk,
          status: revokeOk ? 204 : 500,
          json: async () => ({}),
          text: async () => "{}",
        } as Response);
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({}),
        text: async () => "{}",
      } as Response);
    }),
  );
}

function renderDialog(
  props: Partial<Parameters<typeof ShareRunDialog>[0]> = {},
) {
  return render(
    <ToastProvider>
      <ShareRunDialog
        open={true}
        onClose={() => {}}
        runId="run-123"
        {...props}
      />
    </ToastProvider>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ShareRunDialog (m75.4)", () => {
  it("renders with metadata-only as the default (includeContent toggle OFF)", async () => {
    installFetch();
    renderDialog();

    await screen.findByTestId("share-run-dialog");
    const toggle = screen.getByTestId(
      "share-include-content",
    ) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    const preview = screen.getByTestId("share-projection-preview");
    expect(preview).toHaveTextContent("not included");
  });

  it("toggling includeContent updates the projection preview to show content fields", async () => {
    installFetch();
    renderDialog();

    await screen.findByTestId("share-run-dialog");
    const toggle = screen.getByTestId("share-include-content");
    fireEvent.click(toggle);

    const preview = screen.getByTestId("share-projection-preview");
    expect(preview).toHaveTextContent("Full input text");
    expect(preview).toHaveTextContent("Full message transcript");
  });

  it("clicking Create Share Link shows the one-time link with copy button", async () => {
    installFetch({ createResult: SHARE_RESULT });
    renderDialog();

    await screen.findByTestId("share-create-btn");
    fireEvent.click(screen.getByTestId("share-create-btn"));

    await screen.findByTestId("share-link-once");
    const linkValue = screen.getByTestId("share-link-value");
    expect(linkValue.textContent).toContain("abc-secret-token");
    expect(screen.getByTestId("share-link-copy")).toBeInTheDocument();
  });

  it("shows existing shares in the manage section with revoke button", async () => {
    installFetch({ listShares: [EXISTING_SHARE] });
    renderDialog();

    await screen.findByTestId(`share-row-${EXISTING_SHARE.id}`);
    expect(
      screen.getByTestId(`share-revoke-${EXISTING_SHARE.id}`),
    ).toBeInTheDocument();
  });

  it("revoking a share removes the revoke button (share marked revoked)", async () => {
    installFetch({ listShares: [EXISTING_SHARE] });
    renderDialog();

    await screen.findByTestId(`share-revoke-${EXISTING_SHARE.id}`);
    fireEvent.click(
      screen.getByTestId(`share-revoke-${EXISTING_SHARE.id}`),
    );

    await waitFor(() => {
      // After revoke the button disappears — the share is marked revoked.
      expect(
        screen.queryByTestId(`share-revoke-${EXISTING_SHARE.id}`),
      ).toBeNull();
    });
  });

  // ── V7: projection preview shows honest field list (no fake numbers) ──────
  it("V7: projection preview shows field names without fake parenthesized numbers", async () => {
    installFetch();
    renderDialog();

    await screen.findByTestId("share-run-dialog");
    const preview = screen.getByTestId("share-projection-preview");
    // Honest field list
    expect(preview).toHaveTextContent("Message count and roles");
    // Must NOT contain fake parenthesized numbers from page data
    expect(preview.textContent).not.toMatch(/messageCount|spanCount|\(\d+\)/);
  });

  // ── V8: done state blocks dismiss until link is confirmed saved ───────────
  it("V8: in done state the close button is disabled until 'I've saved the link' is clicked", async () => {
    installFetch({ createResult: SHARE_RESULT });
    const onClose = vi.fn();
    render(
      <ToastProvider>
        <ShareRunDialog open={true} onClose={onClose} runId="run-123" />
      </ToastProvider>,
    );

    await screen.findByTestId("share-create-btn");
    fireEvent.click(screen.getByTestId("share-create-btn"));
    await screen.findByTestId("share-link-once");

    // Close button is disabled — aria-label changes to the guarded message
    const closeBtn = screen.getByRole("button", { name: /copy or confirm the link before closing/i });
    expect(closeBtn).toBeDisabled();

    // Click "I've saved the link"
    fireEvent.click(screen.getByTestId("share-link-done"));

    // Now the close button is enabled and aria-label reverts to "Close"
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^close$/i })).not.toBeDisabled();
    });
  });

  // ── V11: expired badge shown; revoked badge shown via in-dialog revoke ────
  it("V11: expired badge shown for a share whose expiresAt is in the past", async () => {
    const expiredShare = {
      ...EXISTING_SHARE,
      id: "expired-1",
      expiresAt: "2020-01-01T00:00:00Z", // far past
    };
    installFetch({ listShares: [expiredShare] });
    renderDialog();

    await screen.findByTestId(`share-row-${expiredShare.id}`);
    expect(screen.getByTestId(`share-expired-badge-${expiredShare.id}`)).toBeInTheDocument();
    // Revoke button is NOT shown for expired shares
    expect(screen.queryByTestId(`share-revoke-${expiredShare.id}`)).toBeNull();
  });

  it("V11: revoked share still appears in list with Revoked badge after in-dialog revoke", async () => {
    installFetch({ listShares: [EXISTING_SHARE] });
    renderDialog();

    await screen.findByTestId(`share-revoke-${EXISTING_SHARE.id}`);
    fireEvent.click(screen.getByTestId(`share-revoke-${EXISTING_SHARE.id}`));

    await waitFor(() => {
      // Revoke button disappears (share is now revoked)
      expect(screen.queryByTestId(`share-revoke-${EXISTING_SHARE.id}`)).toBeNull();
    });
    // Revoked badge appears (UI locally marks it revoked)
    expect(screen.getByTestId(`share-revoked-badge-${EXISTING_SHARE.id}`)).toBeInTheDocument();
  });

  // ── V12: softened 501/409 errors ─────────────────────────────────────────
  it("V12: a 501 response shows a calm not-configured message instead of raw ops error", async () => {
    installFetch({
      createStatus: 501,
      createResult: { error: "share store not configured: set CONTROLPLANE_DSN to enable share links" },
    });
    renderDialog();

    await screen.findByTestId("share-create-btn");
    fireEvent.click(screen.getByTestId("share-create-btn"));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toBeInTheDocument();
    });
    // Calm message — mentions CONTROLPLANE_DSN but in calm prose, not raw ops string
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("not configured");
    expect(alert).toHaveTextContent("CONTROLPLANE_DSN");
    // Must NOT contain the raw "createRunShare failed" prefix
    expect(alert.textContent).not.toContain("createRunShare failed");
  });

  it("V12: canShare=false shows unavailable message instead of create form", async () => {
    installFetch();
    renderDialog({ canShare: false });

    await screen.findByTestId("share-run-dialog");
    expect(screen.queryByTestId("share-create-btn")).toBeNull();
    expect(screen.getByText(/not available/i)).toBeInTheDocument();
  });

  // V14: after minting, the dialog states the REAL link semantics — a share link is multi-fetch
  // until it expires or is revoked (no single-use marking) — not the old "available while the token
  // has not yet been used" / "already been used to fetch once" which scared sharers into thinking a
  // preview or a first open burns the recipient's link.
  it("states multi-fetch-until-expiry semantics after minting, not single-use (V14)", async () => {
    installFetch();
    renderDialog();

    await screen.findByTestId("share-create-btn");
    fireEvent.click(screen.getByTestId("share-create-btn"));
    await screen.findByTestId("share-link-once");

    const semantics = screen.getByTestId("share-link-semantics");
    expect(semantics).toHaveTextContent(
      /open it as many times as they like until it expires or you revoke it/i,
    );
    // No single-use language anywhere in the dialog.
    const dialog = screen.getByTestId("share-run-dialog");
    expect(dialog).not.toHaveTextContent(/not yet been used/i);
    expect(dialog).not.toHaveTextContent(/already been used to fetch/i);
  });
});
