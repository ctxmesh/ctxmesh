import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { DevModeContext } from "@/lib/dev-mode";
import { RequireAuth, SessionProvider } from "@/lib/session-provider";
import { AppShell } from "@/components/app-shell";
import { ToastProvider } from "@/components/kit";
import { forceClear } from "@/lib/session";

// Dev mode (`agentry dev --ui`, ADR 0021): the BFF confirms devMode via an
// unauthenticated GET /api/devmode. When true the SPA drops the login wall and shows
// the dev banner; the cluster surfaces (namespaces/capabilities) 501 by design and must
// degrade calmly, never bounce to login or crash. SessionProvider resolves the probe
// during boot (behind its splash) so no guard ever sees a transient false.

// stubFetch answers /api/devmode with the given flag and everything else benignly, so
// the shell + session boot render without unhandled rejections. Cluster endpoints
// (namespaces/capabilities) return 501 to mirror the real dev substrate.
function stubFetch(devMode: boolean) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/devmode")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ devMode }),
        } as Response);
      }
      if (
        url.startsWith("/api/namespaces") ||
        url.startsWith("/api/capabilities")
      ) {
        return Promise.resolve({
          ok: false,
          status: 501,
          json: async () => ({ error: "not available in dev mode" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({
          nodes: [],
          edges: [],
          items: [],
          agents: [],
          nextCursor: "",
        }),
      } as Response);
    }),
  );
}

afterEach(() => {
  forceClear(); // session store is a module singleton — clear it between tests.
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function renderGuarded() {
  return render(
    <MemoryRouter initialEntries={["/protected"]}>
      <SessionProvider>
        <Routes>
          <Route path="/login" element={<div>login page</div>} />
          <Route
            path="/protected"
            element={
              <RequireAuth>
                <div>protected content</div>
              </RequireAuth>
            }
          />
        </Routes>
      </SessionProvider>
    </MemoryRouter>,
  );
}

describe("RequireAuth × dev mode", () => {
  it("renders the console WITHOUT a session when the BFF confirms dev mode", async () => {
    stubFetch(true);
    renderGuarded();
    // No login, no token — dev mode drops the wall (devMode resolved before the guard).
    expect(await screen.findByText("protected content")).toBeInTheDocument();
    expect(screen.queryByText("login page")).not.toBeInTheDocument();
  });

  it("still redirects an unauthenticated visit to /login when NOT dev mode", async () => {
    stubFetch(false);
    renderGuarded();
    expect(await screen.findByText("login page")).toBeInTheDocument();
    expect(screen.queryByText("protected content")).not.toBeInTheDocument();
  });
});

describe("shell chrome × dev mode", () => {
  function renderShell(devMode: boolean) {
    stubFetch(devMode);
    return render(
      <MemoryRouter initialEntries={["/"]}>
        <DevModeContext.Provider value={devMode}>
          <ToastProvider>
            <Routes>
              <Route path="/" element={<AppShell />}>
                <Route index element={<div>index surface</div>} />
              </Route>
            </Routes>
          </ToastProvider>
        </DevModeContext.Provider>
      </MemoryRouter>,
    );
  }

  it("shows the dev-mode banner and hides Sign out in dev mode", async () => {
    renderShell(true);
    expect(await screen.findByTestId("dev-mode-banner")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Sign out" }),
    ).not.toBeInTheDocument();
    // The cluster-only capability warning is suppressed — the dev banner covers it.
    expect(screen.queryByTestId("capability-banner")).not.toBeInTheDocument();
  });

  it("shows no dev banner and keeps Sign out on a real cluster", async () => {
    renderShell(false);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Sign out" }),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("dev-mode-banner")).not.toBeInTheDocument();
  });
});
