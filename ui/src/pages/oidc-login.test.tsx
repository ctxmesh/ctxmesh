import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { LoginPage } from "@/pages/login-page";
import { AuthCallbackPage } from "@/pages/auth-callback-page";
import { forceClear } from "@/lib/session";
import * as oidc from "@/lib/oidc";

// OIDC console login UI (ADR 0020): the login page offers "Sign in with SSO" only when
// /api/authconfig advertises it, and the /auth/callback page completes the flow (→ the
// session seam) or shows an honest error. The PKCE/exchange internals are covered in
// lib/oidc.test.ts; here we pin the page wiring.

vi.mock("@/lib/oidc", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/oidc")>();
  return {
    ...actual,
    startLogin: vi.fn(),
    startEndUserLogin: vi.fn(),
    completeLogin: vi.fn(),
  };
});

afterEach(() => {
  forceClear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// stubFetch answers /api/authconfig (SSO on/off) and /api/whoami (200), everything
// else benignly, so the pages render without unhandled rejections.
function stubFetch(
  oidcEnabled: boolean,
  endUserCfg?: { issuer: string; clientId: string },
  whoamiStatus = 200,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/authconfig")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () =>
            oidcEnabled
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
          endUserCfg
            ? ({ ok: true, status: 200, json: async () => endUserCfg } as Response)
            : ({ ok: false, status: 404, json: async () => ({}) } as Response),
        );
      }
      if (url.startsWith("/api/whoami")) {
        return Promise.resolve({
          ok: whoamiStatus === 200,
          status: whoamiStatus,
          json: async () =>
            whoamiStatus === 200
              ? { username: "admin", groups: ["operators"] }
              : { error: "no role binding" },
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
      } as Response);
    }),
  );
}

describe("LoginPage × SSO", () => {
  it("offers 'Sign in with SSO' when authconfig advertises OIDC, and starts the flow", async () => {
    stubFetch(true);
    render(
      <MemoryRouter initialEntries={["/login"]}>
        <LoginPage />
      </MemoryRouter>,
    );
    const btn = await screen.findByTestId("sso-login");
    fireEvent.click(btn);
    await waitFor(() => expect(oidc.startLogin).toHaveBeenCalledTimes(1));
    // Token login stays available as the fallback.
    expect(screen.getByLabelText("Bearer token")).toBeInTheDocument();
  });

  it("hides SSO and shows only token login when OIDC is off", async () => {
    stubFetch(false);
    render(
      <MemoryRouter initialEntries={["/login"]}>
        <LoginPage />
      </MemoryRouter>,
    );
    // Give the authconfig probe a tick to resolve to false.
    await waitFor(() =>
      expect(screen.getByLabelText("Bearer token")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("sso-login")).not.toBeInTheDocument();
    // Nor the end-user button (a 404 from end-user-auth-config off an agent origin).
    expect(screen.queryByTestId("end-user-login")).not.toBeInTheDocument();
  });

  it("offers end-user login when the tenant has an end-user IdP, and starts that flow", async () => {
    stubFetch(false, {
      issuer: "https://dex-eu.example.com",
      clientId: "ctxmesh-enduser",
    });
    render(
      <MemoryRouter initialEntries={["/login"]}>
        <LoginPage />
      </MemoryRouter>,
    );
    const btn = await screen.findByTestId("end-user-login");
    fireEvent.click(btn);
    await waitFor(() =>
      expect(oidc.startEndUserLogin).toHaveBeenCalledTimes(1),
    );
  });
});

describe("AuthCallbackPage", () => {
  function renderCallback() {
    return render(
      <MemoryRouter initialEntries={["/auth/callback?code=abc&state=st4te"]}>
        <Routes>
          <Route path="/auth/callback" element={<AuthCallbackPage />} />
          <Route path="/agents" element={<div>agents surface</div>} />
          <Route path="/login" element={<div>login page</div>} />
        </Routes>
      </MemoryRouter>,
    );
  }

  it("completes the exchange → session → routes to returnTo", async () => {
    stubFetch(true); // whoami 200 so login() succeeds
    vi.mocked(oidc.completeLogin).mockResolvedValue({
      idToken: "ID.TOK.EN",
      returnTo: "/agents",
    });
    renderCallback();
    expect(await screen.findByText("agents surface")).toBeInTheDocument();
  });

  it("shows an honest error (with a way back) when the flow fails", async () => {
    stubFetch(true);
    vi.mocked(oidc.completeLogin).mockRejectedValue(
      new Error("State mismatch — possible CSRF; please log in again."),
    );
    renderCallback();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /state mismatch/i,
    );
    expect(
      screen.getByRole("button", { name: "Back to sign in" }),
    ).toBeInTheDocument();
  });

  // M151 §6.2: "progress + honest error with a way back". The way back is not a
  // failure affordance — a round trip that HANGS is exactly how a person gets
  // locked out of a console with no chrome and no navigation, so the exit is on
  // screen before anything has gone wrong.
  it("offers the way back WHILE the exchange is still running", async () => {
    stubFetch(true);
    vi.mocked(oidc.completeLogin).mockReturnValue(new Promise(() => {}));
    renderCallback();
    await screen.findByTestId("auth-callback-working");
    expect(
      screen.getByRole("button", { name: "Back to sign in" }),
    ).toBeInTheDocument();
  });

  // The provider signing you in and the cluster accepting the identity are two
  // different steps, and only one of them failed here. Saying "sign-in didn't
  // complete" would send the user to re-authenticate against a provider that
  // already said yes; the fix is an RBAC binding.
  it("separates 'the provider signed you in' from 'the cluster refused the identity'", async () => {
    stubFetch(true, undefined, 403);
    vi.mocked(oidc.completeLogin).mockResolvedValue({
      idToken: "ID.TOK.EN",
      returnTo: "/agents",
    });
    renderCallback();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/Your provider signed you in/i);
    expect(alert).toHaveTextContent(/would not accept that identity \(403\)/i);
    expect(alert).toHaveTextContent(/not bound to a role/i);
    expect(alert.textContent).not.toMatch(/Sign-in didn't complete/i);
    expect(
      screen.getByRole("button", { name: "Back to sign in" }),
    ).toBeInTheDocument();
  });

  // Nothing renders a credential: the ID token is handed to login() and never
  // reaches the DOM, and neither does the authorization code in the URL.
  it("never renders the ID token or the authorization code", async () => {
    stubFetch(true, undefined, 403);
    vi.mocked(oidc.completeLogin).mockResolvedValue({
      idToken: "ID.SUPER.SECRET",
      returnTo: "/agents",
    });
    renderCallback();
    await screen.findByRole("alert");
    expect(document.body.innerHTML).not.toContain("ID.SUPER.SECRET");
    expect(document.body.innerHTML).not.toContain("st4te");
  });
});
