import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { ShareRunDialog } from "@/components/dashboard/share-run-dialog";
import { ToastProvider } from "@/components/kit";

// ShareRunDialog — m75.4 tests.
//
// Coverage:
//   • metadata-only is the default (includeContent toggle OFF, preview says "not included")
//   • toggling includeContent updates the projection preview to show content fields
//   • creating a share calls the API and shows the one-time link with copy button
//   • manage section shows existing shares with revoke button
//   • revoking a share marks it revoked (button disappears)

const SHARE_RESULT = {
  id: "share-1",
  token: "abc-secret-token",
  expiresAt: "2026-08-08T10:00:00Z",
  includeContent: false,
};

const EXISTING_SHARE = {
  id: "existing-1",
  createdAt: "2026-08-01T10:00:00Z",
  expiresAt: "2026-08-08T10:00:00Z",
  revoked: false,
  includeContent: false,
};

function installFetch(
  opts: {
    listShares?: unknown[];
    createResult?: unknown;
    revokeOk?: boolean;
  } = {},
) {
  const {
    listShares = [],
    createResult = SHARE_RESULT,
    revokeOk = true,
  } = opts;

  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();

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
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => createResult,
          text: async () => "{}",
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
});
