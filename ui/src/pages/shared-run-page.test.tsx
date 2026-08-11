import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SharedRunPage, SharedRunErrorBoundaryForTest } from "@/pages/shared-run-page";
import type { SharedRunView } from "@/lib/api";

// SharedRunPage — m75.4 / m75.5 tests.
//
// Coverage:
//   • metadata-only view renders run details (no transcript block)
//   • with-content view renders transcript (input + messages)
//   • object input (json.RawMessage) renders as JSON string — no "Objects are not valid" crash
//   • ErrorBoundary shows a friendly fallback on a thrown child render error
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

// OBJECT_INPUT_VIEW simulates a console-created run where the backend field is
// json.RawMessage and the /invoke body is an object ({"input":"Hello"}).
// SharedRunView.input is typed `unknown` to reflect this — rendering it
// directly as a React child would crash with "Objects are not valid as a React
// child". The page must JSON.stringify it instead.
const OBJECT_INPUT_VIEW: SharedRunView = {
  ...METADATA_VIEW,
  // Simulate a deserialized json.RawMessage object (not a plain string).
  input: { input: "Hello from console" } as unknown as string,
  messages: [],
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

  // P1-1 regression: an object input (json.RawMessage) must render as its
  // JSON string representation, NOT crash with "Objects are not valid as a
  // React child".
  it("renders an object input as a JSON string — no crash on non-string input", async () => {
    installFetch({ view: OBJECT_INPUT_VIEW });
    renderPage();

    await screen.findByTestId("shared-run-content");
    // The transcript block must be present (input is defined).
    expect(screen.getByTestId("shared-run-transcript")).toBeInTheDocument();
    // The input is rendered as the JSON-stringified form, not as a React object.
    expect(screen.getByText(/"input":\s*"Hello from console"/)).toBeInTheDocument();
  });

  // P1-1 regression: the SharedRunErrorBoundary (wrapping SharedRunPage) must
  // catch a synchronous render throw and show a friendly fallback — not a white
  // screen. Since the boundary is local to the module we test it via the
  // exported SharedRunErrorBoundaryForTest wrapper (exported for test-only).
  it("ErrorBoundary shows a friendly fallback when a child throws during render", async () => {
    // Suppress the expected React error-boundary console.error output.
    const spy = vi.spyOn(console, "error").mockImplementation(() => undefined);

    // A component that always throws during render — simulates "Objects are not
    // valid as a React child" (the real crash this boundary defends against).
    function Bomber(): React.ReactElement {
      throw new Error("render bomb");
    }

    render(
      <SharedRunErrorBoundaryForTest>
        <Bomber />
      </SharedRunErrorBoundaryForTest>,
    );

    // The boundary fallback — not a blank page — must be visible.
    expect(
      screen.getByText(/This shared run couldn't be displayed/i),
    ).toBeInTheDocument();

    spy.mockRestore();
  });
});
