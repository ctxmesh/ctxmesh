import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SharedRunPage } from "@/pages/shared-run-page";
import type { SharedRunView } from "@/lib/api";

// SharedRunPage — m75.4 tests.
//
// Coverage:
//   • metadata-only view renders run details (no transcript block)
//   • with-content view renders transcript (input + messages)
//   • 404 (uniform) → friendly unavailable state (expired/revoked/bad token)
//   • network error → friendly unavailable state
//   • getSharedRun sends NO Authorization header (no-auth plain fetch)

const METADATA_VIEW: SharedRunView = {
  id: "run-1",
  namespace: "prod",
  agent: "billing-agent",
  status: "succeeded",
  createdAt: "2026-08-01T10:00:00Z",
  updatedAt: "2026-08-01T10:05:00Z",
  messageCount: 3,
  messageRoles: ["user", "assistant"],
};

const CONTENT_VIEW: SharedRunView = {
  ...METADATA_VIEW,
  input: "Hello, world",
  messages: [
    { role: "user", content: "Hello" },
    { role: "assistant", content: "Hi there!" },
  ],
};

function installFetch(
  opts: { status?: number; view?: SharedRunView } = {},
) {
  const { status = 200, view = METADATA_VIEW } = opts;
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/shared/runs/")) {
        // The key invariant: getSharedRun MUST NOT send an Authorization
        // header. If it does, fail the test by returning 403 so the page
        // renders the unavailable state instead of the content.
        const headers = init?.headers as Record<string, string> | undefined;
        if (headers?.["Authorization"]) {
          return Promise.resolve({
            ok: false,
            status: 403,
            json: async () => ({
              error: "FAIL: getSharedRun sent Authorization header",
            }),
            text: async () =>
              JSON.stringify({
                error: "FAIL: getSharedRun sent Authorization header",
              }),
          } as Response);
        }
        return Promise.resolve({
          ok: status === 200,
          status,
          json: async () => view,
          text: async () => JSON.stringify({ error: "not found" }),
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

function renderPage(token = "test-token-abc") {
  return render(
    <MemoryRouter initialEntries={[`/shared/runs/${token}`]}>
      <Routes>
        <Route path="/shared/runs/:token" element={<SharedRunPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("SharedRunPage (m75.4)", () => {
  it("renders the metadata-only view — run details visible, no transcript", async () => {
    installFetch({ view: METADATA_VIEW });
    renderPage();

    await screen.findByTestId("shared-run-content");
    expect(screen.getByTestId("shared-run-metadata")).toBeInTheDocument();
    // Agent name appears in both the subtitle and the dl
    expect(screen.getAllByText("billing-agent").length).toBeGreaterThan(0);
    expect(screen.getAllByText("prod").length).toBeGreaterThan(0);
    // No transcript — includeContent=false (fields absent)
    expect(screen.queryByTestId("shared-run-transcript")).toBeNull();
  });

  it("renders the with-content view including transcript", async () => {
    installFetch({ view: CONTENT_VIEW });
    renderPage();

    await screen.findByTestId("shared-run-content");
    expect(screen.getByTestId("shared-run-transcript")).toBeInTheDocument();
    expect(screen.getByText("Hello, world")).toBeInTheDocument();
    expect(screen.getByText("Hi there!")).toBeInTheDocument();
  });

  it("shows the friendly unavailable message on 404 (expired/revoked/bad token)", async () => {
    installFetch({ status: 404 });
    renderPage();

    const unavail = await screen.findByTestId("shared-run-unavailable");
    expect(unavail).toBeInTheDocument();
  });

  it("shows the friendly unavailable message on network error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new Error("network error"))),
    );
    renderPage();

    await screen.findByTestId("shared-run-unavailable");
  });

  it("does NOT send an Authorization header — getSharedRun is no-auth", async () => {
    // installFetch returns 403 (causing unavailable) if Authorization is sent.
    // If this test renders shared-run-content (not unavailable), the header
    // was not sent — the no-auth invariant holds.
    installFetch({ view: METADATA_VIEW });
    renderPage();

    await screen.findByTestId("shared-run-content");
    expect(screen.queryByTestId("shared-run-unavailable")).toBeNull();
  });
});
