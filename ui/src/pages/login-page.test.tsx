import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { LoginPage } from "@/pages/login-page";
import { RequireAuth, SessionProvider } from "@/lib/session-provider";
import { logout } from "@/lib/session";

// End-to-end-ish (unit-level) coverage of the token login + auth routing (ADR
// 0012): a happy login lands on the console, a wrong token renders the honest
// inline error with no session persisted, an unauthenticated console visit is
// redirected to /login (preserving the return path), and a mid-session 401
// clears the session and bounces back to /login.

const TOKEN_KEY = "ctxmesh.session.token";

function whoamiOk(username = "alex.dev", groups = ["dev-team"]) {
  return { ok: true, status: 200, json: async () => ({ username, groups }) } as Response;
}
function reject(status: number) {
  return { ok: false, status, json: async () => ({ error: "no" }) } as Response;
}

// A tiny app that mirrors App.tsx's auth routing so the guard + redirect are
// exercised for real. The console shows the return path so we can assert it.
function TestApp() {
  return (
    <SessionProvider>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route
          path="/agents"
          element={
            <RequireAuth>
              <div>AGENTS CONSOLE</div>
            </RequireAuth>
          }
        />
        <Route
          path="/"
          element={
            <RequireAuth>
              <div>DASHBOARD CONSOLE</div>
            </RequireAuth>
          }
        />
      </Routes>
    </SessionProvider>
  );
}

beforeEach(() => {
  logout();
  sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  logout();
  sessionStorage.clear();
});

describe("LoginPage + auth routing", () => {
  it("happy path: valid token → whoami 200 → console", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    // The login form renders once boot restore() resolves (no persisted token).
    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "good-token" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    expect(await screen.findByText("DASHBOARD CONSOLE")).toBeInTheDocument();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("good-token");
  });

  it("wrong token: whoami 401 → honest inline error, NO session persisted", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reject(401)));

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "expired" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    // The inline error names the 401 + the fix; still on the login form.
    expect(await screen.findByRole("alert")).toHaveTextContent(/rejected \(401\)/i);
    expect(screen.getByLabelText(/bearer token/i)).toBeInTheDocument();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
    // The console is NOT shown.
    expect(screen.queryByText(/CONSOLE/)).toBeNull();
  });

  it("unauthenticated console visit redirects to /login preserving the return path", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reject(401)));

    render(
      <MemoryRouter initialEntries={["/agents"]}>
        <TestApp />
      </MemoryRouter>,
    );

    // Redirected to the login form (no session).
    const input = await screen.findByLabelText(/bearer token/i);

    // Now log in — the preserved return path (/agents) is honoured, not "/".
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));
    fireEvent.change(input, { target: { value: "good" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    expect(await screen.findByText("AGENTS CONSOLE")).toBeInTheDocument();
  });

  // Open-redirect guard (m13.3 review). A hostile `from` captured into the login
  // location state must NOT become the post-login navigate() target — a valid
  // login has to land on a same-origin in-app path, else it's a phishing
  // primitive on the trusted origin. Both a protocol-relative "//evil.com" and an
  // absolute "https://evil.com" from must fall back to "/".
  it.each([
    ["protocol-relative //evil.com", "//evil.com/steal"],
    ["absolute https://evil.com", "https://evil.com/steal"],
    ["backslash-coerced /\\evil.com", "/\\evil.com"],
  ])("rejects an off-origin login return path (%s) and lands on /", async (_label, pathname) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));

    render(
      <MemoryRouter
        initialEntries={[
          { pathname: "/login", state: { from: { pathname } } },
        ]}
      >
        <TestApp />
      </MemoryRouter>,
    );

    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "good" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    // Falls back to the in-app root, NOT the hostile /agents or off-origin.
    expect(await screen.findByText("DASHBOARD CONSOLE")).toBeInTheDocument();
    expect(screen.queryByText("AGENTS CONSOLE")).toBeNull();
    // MemoryRouter can't navigate off-origin, but assert the location is the
    // in-app root — the guard rewrote the hostile target to "/".
    expect(window.location.href).not.toContain("evil.com");
  });

  it("mid-session 401: an expired token clears the session and redirects to login (return path preserved)", async () => {
    // Boot with a valid persisted token → restore() resolves a live session and
    // the console renders.
    sessionStorage.setItem(TOKEN_KEY, "live-token");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));

    render(
      <MemoryRouter initialEntries={["/agents"]}>
        <TestApp />
      </MemoryRouter>,
    );
    expect(await screen.findByText("AGENTS CONSOLE")).toBeInTheDocument();

    // The token now expires: any /api/* call 401s → the session-expired handler
    // clears the session → the guard redirects to /login. Drive it through the
    // real api.ts seam (a plain listAgents call standing in for a surface fetch).
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reject(401)));
    const { api } = await import("@/lib/api");
    // The 401 handler clears the session (a React store update) — wrap in act so
    // the guard's re-render is flushed inside the test's act scope.
    await act(async () => {
      await api.listAgents().catch(() => {});
    });

    // The guard re-renders to login once the session is cleared.
    expect(await screen.findByLabelText(/bearer token/i)).toBeInTheDocument();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("token never appears in console output during login (spy)", async () => {
    const logSpy = vi.spyOn(console, "log").mockImplementation(() => {});
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const SECRET = "eyJ0-super-secret-login-token";
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );
    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: SECRET } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByText("DASHBOARD CONSOLE");

    const out = [logSpy, errSpy]
      .flatMap((s) => s.mock.calls)
      .flat()
      .map((a) => (typeof a === "string" ? a : JSON.stringify(a)))
      .join(" ");
    expect(out).not.toContain(SECRET);
  });

  it("visiting /login with a live session bounces to the console", async () => {
    sessionStorage.setItem(TOKEN_KEY, "live");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(whoamiOk()));

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );
    // restore() promotes the persisted token → the login page's effect bounces
    // to the console.
    await waitFor(() =>
      expect(screen.getByText("DASHBOARD CONSOLE")).toBeInTheDocument(),
    );
  });
});
