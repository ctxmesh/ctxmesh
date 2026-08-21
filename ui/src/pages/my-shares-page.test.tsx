import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { MySharesPage } from "@/pages/my-shares-page";
import type { MySharesItem } from "@/lib/api";

// MySharesPage (V13) — the caller's share links across all runs.
//
// Coverage:
//   (a) lists caller's shares with correct status badges (live/revoked/expired)
//   (b) clicking Revoke on a live share calls the revoke api method with (runId, shareId)
//       and the page refreshes (row updates)
//   (c) empty state renders "You have no active shares" when the list is empty
//   (d) error state renders a visible, retryable error
//   (e) 403 surfaces the forbidden state

function share(over: Partial<MySharesItem> = {}): MySharesItem {
  return {
    id: "share-1",
    runId: "run-abc",
    namespace: "team-a",
    createdAt: "2026-08-01T10:00:00Z",
    expiresAt: "2026-08-08T10:00:00Z",
    status: "live",
    includeContent: false,
    ...over,
  };
}

// installFetch stubs global fetch. The responder receives the URL string.
// Supports two endpoints:
//   GET  /api/my/shares         — list shares
//   DELETE /api/runs/.../shares/...  — revoke
function installFetch(
  responder: (
    url: string,
    method: string,
  ) => { ok: boolean; status?: number; body?: unknown },
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      const r = responder(url, method);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? [],
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/my-shares"]}>
      <Routes>
        <Route path="/my-shares" element={<MySharesPage />} />
        <Route
          path="/runs/:id"
          element={<div data-testid="run-page-stub" />}
        />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── (a) Basic rendering: status badges ────────────────────────────────────────

describe("MySharesPage — lists shares with status badges (V13)", () => {
  it("renders my-shares-page and the shares table with rows", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        share({ id: "s1", runId: "run-1", status: "live" }),
        share({ id: "s2", runId: "run-2", status: "revoked" }),
        share({ id: "s3", runId: "run-3", status: "expired" }),
      ],
    }));

    renderPage();

    expect(await screen.findByTestId("my-shares-page")).toBeInTheDocument();
    expect(
      screen.getByRole("table", { name: "My Shares" }),
    ).toBeInTheDocument();
  });

  it("shows Live badge for a live share", async () => {
    installFetch(() => ({
      ok: true,
      body: [share({ id: "s1", status: "live" })],
    }));

    renderPage();

    await screen.findByTestId("my-shares-page");
    // "Live" is in the Status column — badge text
    const liveBadges = screen.getAllByText("Live");
    expect(liveBadges.length).toBeGreaterThan(0);
    // At least one badge lives inside a <td>
    expect(liveBadges.some((el) => el.closest("td") !== null)).toBe(true);
  });

  it("shows Revoked badge for a revoked share", async () => {
    installFetch(() => ({
      ok: true,
      body: [share({ id: "s2", status: "revoked" })],
    }));

    renderPage();

    await screen.findByTestId("my-shares-page");
    const els = screen.getAllByText("Revoked");
    expect(els.some((el) => el.closest("td") !== null)).toBe(true);
  });

  it("shows Expired badge for an expired share", async () => {
    installFetch(() => ({
      ok: true,
      body: [share({ id: "s3", status: "expired" })],
    }));

    renderPage();

    await screen.findByTestId("my-shares-page");
    const els = screen.getAllByText("Expired");
    expect(els.some((el) => el.closest("td") !== null)).toBe(true);
  });

  it("shows no Revoke button for revoked/expired shares", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        share({ id: "s2", status: "revoked" }),
        share({ id: "s3", status: "expired" }),
      ],
    }));

    renderPage();

    await screen.findByTestId("my-shares-page");
    // Revoke buttons only appear for live shares
    expect(screen.queryByRole("button", { name: /Revoke/i })).toBeNull();
  });
});

// ── (b) Revoke action: calls revokeRunShare(runId, shareId) and refreshes ─────

describe("MySharesPage — Revoke action (V13)", () => {
  it("Revoke button calls DELETE on the right URL (runId + shareId) and refreshes", async () => {
    const calls: { url: string; method: string }[] = [];

    // First GET returns a live share; after revoke, second GET returns it revoked.
    let callCount = 0;
    installFetch((url, method) => {
      calls.push({ url, method });
      if (method === "DELETE") {
        // The revoke endpoint: DELETE /api/runs/{runId}/shares/{shareId}
        return { ok: true, status: 204, body: null };
      }
      // GET /api/my/shares
      callCount++;
      if (callCount === 1) {
        return {
          ok: true,
          body: [share({ id: "share-live", runId: "run-xyz", status: "live" })],
        };
      }
      // After revoke, the share is revoked
      return {
        ok: true,
        body: [share({ id: "share-live", runId: "run-xyz", status: "revoked" })],
      };
    });

    renderPage();

    // Wait for the Revoke button to appear
    const revokeBtn = await screen.findByTestId("revoke-share-live");
    expect(revokeBtn).toBeInTheDocument();

    fireEvent.click(revokeBtn);

    // The DELETE call must target the right endpoint
    await waitFor(() => {
      const deleteCall = calls.find((c) => c.method === "DELETE");
      expect(deleteCall).toBeDefined();
      expect(deleteCall!.url).toContain("/api/runs/run-xyz/shares/share-live");
    });

    // After refresh, the Revoke button is gone (the share is now revoked)
    await waitFor(() => {
      expect(screen.queryByTestId("revoke-share-live")).toBeNull();
    });
  });

  it("disables the Revoke button while the revoke is in flight", async () => {
    // Delay the DELETE to keep the revoking state visible
    let resolveDelete!: () => void;
    const deletePromise = new Promise<void>((res) => {
      resolveDelete = res;
    });

    installFetch((_url, method) => {
      if (method === "DELETE") {
        // Return a promise that doesn't resolve yet — simulates in-flight revoke
        return { ok: true, status: 204, body: null };
      }
      return {
        ok: true,
        body: [share({ id: "s1", runId: "run-1", status: "live" })],
      };
    });

    // Override fetch to delay DELETE
    vi.stubGlobal(
      "fetch",
      vi.fn((_input: string | URL, init?: RequestInit) => {
        const method = (init?.method ?? "GET").toUpperCase();
        if (method === "DELETE") {
          return deletePromise.then(() =>
            Promise.resolve({
              ok: true,
              status: 204,
              json: async () => null,
              text: async () => "",
            } as Response),
          );
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => [share({ id: "s1", runId: "run-1", status: "live" })],
          text: async () => "[]",
        } as Response);
      }),
    );

    renderPage();

    const revokeBtn = await screen.findByTestId("revoke-s1");
    fireEvent.click(revokeBtn);

    // Button should be disabled while revoking
    await waitFor(() => {
      expect(screen.getByTestId("revoke-s1")).toBeDisabled();
    });

    resolveDelete();
  });
});

// ── (c) Empty state ────────────────────────────────────────────────────────────

describe("MySharesPage — empty state (V13)", () => {
  it("renders the empty state when the list is empty", async () => {
    installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() =>
      expect(
        screen.getByText("You have no active shares"),
      ).toBeInTheDocument(),
    );
  });
});

// ── (d) Error state / (e) 403 forbidden ───────────────────────────────────────

describe("MySharesPage — error states (V13)", () => {
  it("500 surfaces a visible, retryable error", async () => {
    installFetch(() => ({
      ok: false,
      status: 500,
      body: { error: "internal server error" },
    }));

    renderPage();

    await waitFor(() =>
      expect(
        screen.getByText(/internal server error/),
      ).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
  });

  it("403 surfaces the forbidden state, never a fake empty list", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you do not have permission" },
    }));

    renderPage();

    // DataTable renders a forbidden message; it must NOT show the empty state
    await waitFor(() =>
      expect(
        screen.getByText(/You don't have permission to view shares/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText("You have no active shares"),
    ).toBeNull();
    // Forbidden is terminal — no Retry
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });
});
