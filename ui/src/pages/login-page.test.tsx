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
//
// M151 adds three more contracts to the same page and they are asserted here:
//   • the THREE login failures are three different messages (a refused
//     credential, a server that never answered, and an install with no OIDC),
//     because a login that lies about why it failed sends people to fix the
//     wrong thing;
//   • no path renders the credential — the field is cleared the moment login()
//     returns, whatever the outcome;
//   • at an agent's own front door the card carries the end-user register and
//     NO operator vocabulary (no kubectl, no namespace, no cluster).

const TOKEN_KEY = "ctxmesh.session.token";

function whoamiOk(username = "alex.dev", groups = ["dev-team"]) {
  return { ok: true, status: 200, json: async () => ({ username, groups }) } as Response;
}
function reject(status: number) {
  return { ok: false, status, json: async () => ({ error: "no" }) } as Response;
}

// stubFetch answers the login page's three endpoints SEPARATELY, because the
// page's register now depends on which of them answers: /api/end-user-auth-config
// is host-derived on the server and a 404 there is what "this is the console
// origin, not an assistant's front door" looks like on the wire. A blanket stub
// that answers every URL with whoami would put the page on the wrong door.
function stubFetch(opts: {
  /** GET /api/whoami — the token check. */
  whoami?: () => Response | Promise<Response>;
  /** Reject the whoami fetch outright (a transport failure — nothing answered). */
  whoamiThrows?: boolean;
  /** GET /api/authconfig — console SSO. Default: off. */
  oidcEnabled?: boolean;
  /** GET /api/end-user-auth-config — present only at an agent origin. Default: 404. */
  endUser?: { issuer: string; clientId: string };
} = {}) {
  const fn = vi.fn((input: string | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.startsWith("/api/authconfig")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () =>
          opts.oidcEnabled
            ? {
                oidcEnabled: true,
                issuer: "https://dex.example.com",
                clientId: "ctxmesh-console",
              }
            : { oidcEnabled: false },
      } as Response);
    }
    if (url.startsWith("/api/end-user-auth-config")) {
      return Promise.resolve(
        opts.endUser
          ? ({ ok: true, status: 200, json: async () => opts.endUser } as Response)
          : ({ ok: false, status: 404, json: async () => ({}) } as Response),
      );
    }
    if (opts.whoamiThrows) return Promise.reject(new TypeError("Failed to fetch"));
    return Promise.resolve(opts.whoami ? opts.whoami() : whoamiOk());
  });
  vi.stubGlobal("fetch", fn);
  return fn;
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

/** The submit button of the token form — never the provider button beside it. */
function tokenSubmit() {
  return screen.getByTestId("token-login");
}

beforeEach(() => {
  logout();
  sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  logout();
  sessionStorage.clear();
});

describe("LoginPage + auth routing", () => {
  it("happy path: valid token → whoami 200 → console", async () => {
    stubFetch();

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    // The login form renders once boot restore() resolves (no persisted token).
    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "good-token" } });
    fireEvent.click(tokenSubmit());

    expect(await screen.findByText("DASHBOARD CONSOLE")).toBeInTheDocument();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("good-token");
  });

  it("wrong token: whoami 401 → honest inline error, NO session persisted", async () => {
    stubFetch({ whoami: () => reject(401) });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "expired" } });
    fireEvent.click(tokenSubmit());

    // The inline error names the 401 + the fix; still on the login form.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/rejected \(401\)/i);
    expect(screen.getByLabelText(/bearer token/i)).toBeInTheDocument();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
    // The console is NOT shown.
    expect(screen.queryByText(/CONSOLE/)).toBeNull();
  });

  // FAILURE 2 of 3. A refused credential and a server that never answered are
  // different facts: one means "get a new token", the other means "the token may
  // be fine, the network is not". The page must not say the first when it means
  // the second.
  it("transport failure: nothing answered → says the token was never CHECKED, not rejected", async () => {
    stubFetch({ whoamiThrows: true });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: "probably-fine" } });
    fireEvent.click(tokenSubmit());

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/never answered/i);
    expect(alert).toHaveTextContent(/not checked/i);
    expect(alert).toHaveTextContent(/connection problem/i);
    // It must NOT claim the cluster refused the credential — the whole point of
    // separating the two is that this one is not a rejection.
    expect(alert.textContent).not.toMatch(/That token was rejected/i);
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  // FAILURE 3 of 3. An install with no OIDC gets no provider button at all
  // (§7 A7: absent, never disabled) — and says so, once, so nobody hunts for a
  // control that was never rendered.
  it("no OIDC configured: no SSO button, and the absence is STATED", async () => {
    stubFetch({ oidcEnabled: false });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    await screen.findByLabelText(/bearer token/i);
    await waitFor(() =>
      expect(
        screen.getByText(/Single sign-on isn't configured on this install/i),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("sso-login")).toBeNull();
  });

  it("OIDC configured: the SSO button is offered and the 'not configured' line is absent", async () => {
    stubFetch({ oidcEnabled: true });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    expect(await screen.findByTestId("sso-login")).toBeInTheDocument();
    expect(
      screen.queryByText(/Single sign-on isn't configured on this install/i),
    ).toBeNull();
  });

  it("unauthenticated console visit redirects to /login preserving the return path", async () => {
    stubFetch({ whoami: () => reject(401) });

    render(
      <MemoryRouter initialEntries={["/agents"]}>
        <TestApp />
      </MemoryRouter>,
    );

    // Redirected to the login form (no session).
    const input = await screen.findByLabelText(/bearer token/i);

    // Now log in — the preserved return path (/agents) is honoured, not "/".
    stubFetch();
    fireEvent.change(input, { target: { value: "good" } });
    fireEvent.click(tokenSubmit());

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
    stubFetch();

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
    fireEvent.click(tokenSubmit());

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
    stubFetch();

    render(
      <MemoryRouter initialEntries={["/agents"]}>
        <TestApp />
      </MemoryRouter>,
    );
    expect(await screen.findByText("AGENTS CONSOLE")).toBeInTheDocument();

    // The token now expires: any /api/* call 401s → the session-expired handler
    // clears the session → the guard redirects to /login. Drive it through the
    // real api.ts seam (a plain listAgents call standing in for a surface fetch).
    stubFetch({ whoami: () => reject(401) });
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
    stubFetch();

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );
    const input = await screen.findByLabelText(/bearer token/i);
    fireEvent.change(input, { target: { value: SECRET } });
    fireEvent.click(tokenSubmit());
    await screen.findByText("DASHBOARD CONSOLE");

    const out = [logSpy, errSpy]
      .flatMap((s) => s.mock.calls)
      .flat()
      .map((a) => (typeof a === "string" ? a : JSON.stringify(a)))
      .join(" ");
    expect(out).not.toContain(SECRET);
  });

  // Nothing renders a credential: the field is cleared the moment login()
  // returns, so a REJECTED token does not sit in the DOM of a login screen
  // somebody walks away from — and the error copy never echoes it either.
  it("a rejected token does not survive in the DOM after submit", async () => {
    const SECRET = "eyJ0-rejected-but-still-a-secret";
    stubFetch({ whoami: () => reject(401) });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    const input = (await screen.findByLabelText(
      /bearer token/i,
    )) as HTMLInputElement;
    fireEvent.change(input, { target: { value: SECRET } });
    fireEvent.click(tokenSubmit());

    await screen.findByRole("alert");
    expect(
      (screen.getByLabelText(/bearer token/i) as HTMLInputElement).value,
    ).toBe("");
    expect(document.body.innerHTML).not.toContain(SECRET);
  });

  it("visiting /login with a live session bounces to the console", async () => {
    sessionStorage.setItem(TOKEN_KEY, "live");
    stubFetch();

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

// ── The assistant's own front door (M137/EU1b × M151 §6.1 A7) ───────────────
//
// The only screen someone outside the operator team ever meets. Everything an
// operator knows is meaningless here, so the assertions below are as much about
// what the page must NOT say as what it must.
describe("LoginPage at an assistant's own front door", () => {
  const AGENT_IDP = {
    issuer: "https://login.acme.example.com",
    clientId: "acme-agent-chat",
  };

  it("carries the end-user register: the two honesty grafs, and no operator vocabulary", async () => {
    stubFetch({ endUser: AGENT_IDP });

    render(
      <MemoryRouter initialEntries={["/login"]}>
        <TestApp />
      </MemoryRouter>,
    );

    await screen.findByTestId("end-user-login");

    const page = document.body.textContent ?? "";
    // Graf one: what you are signing in to, what it can see, what it cannot.
    expect(page).toContain("not");
    expect(page).toContain("to the platform that runs it");
    expect(page).toContain("It cannot see anyone else");
    // Graf two: provenance, and the promise about not knowing.
    expect(page).toContain("Every answer shows where it came from.");
    expect(page).toContain("it says so instead of guessing");

    // No operator concept reaches this reader.
    expect(page).not.toMatch(/kubectl/i);
    expect(page).not.toMatch(/namespace/i);
    expect(page).not.toMatch(/cluster/i);
    expect(page).not.toMatch(/RBAC/i);
    expect(page).not.toMatch(/Kubernetes/i);
  });

  it("names the assistant from the agent-pin meta the BFF injects at its hostname", async () => {
    const meta = document.createElement("meta");
    meta.setAttribute("name", "agent-pin");
    meta.setAttribute("content", "team-a/acme-support");
    document.head.appendChild(meta);
    stubFetch({ endUser: AGENT_IDP });

    try {
      render(
        <MemoryRouter initialEntries={["/login"]}>
          <TestApp />
        </MemoryRouter>,
      );
      expect(
        await screen.findByRole("heading", { name: "acme-support" }),
      ).toBeInTheDocument();
    } finally {
      meta.remove();
    }
  });
});
