import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  getSession,
  getToken,
  login,
  logout,
  restore,
  subscribe,
} from "@/lib/session";

// The session module (ADR 0012). These tests pin the security-critical
// invariants: the token lives in sessionStorage (never localStorage), it is
// never logged, login validates via whoami (a non-200 → no session persisted),
// and a persisted token survives a "refresh" (restore()).

const TOKEN_KEY = "ctxmesh.session.token";

// okWhoami / badWhoami are fetch stubs standing in for GET /api/whoami.
function okWhoami(username = "alex.dev", groups = ["dev-team"]) {
  return vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: async () => ({ username, groups }),
    text: async () => JSON.stringify({ username, groups }),
  } as Response);
}
function badWhoami(status = 401) {
  return vi.fn().mockResolvedValue({
    ok: false,
    status,
    json: async () => ({ error: "invalid bearer token" }),
    text: async () => "invalid bearer token",
  } as Response);
}

beforeEach(() => {
  logout(); // reset in-memory + storage
  sessionStorage.clear();
  localStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  logout();
  sessionStorage.clear();
});

describe("session module", () => {
  it("login validates via whoami and creates a session (happy path)", async () => {
    vi.stubGlobal("fetch", okWhoami("alex.dev", ["dev-team"]));

    const result = await login("good-token");
    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.session.user.username).toBe("alex.dev");
      expect(result.session.token).toBe("good-token");
    }
    expect(getSession()?.user.username).toBe("alex.dev");
    expect(getToken()).toBe("good-token");
  });

  it("persists the token to sessionStorage, NEVER localStorage", async () => {
    vi.stubGlobal("fetch", okWhoami());
    await login("secret-token");

    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("secret-token");
    // The token must never land in localStorage (persists across tabs/forever).
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(localStorage.length).toBe(0);
  });

  it("a wrong/expired token (whoami 401) yields a typed failure and NO session", async () => {
    vi.stubGlobal("fetch", badWhoami(401));

    const result = await login("expired-token");
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.kind).toBe("bad-token");
      expect(result.status).toBe(401);
    }
    // No session created, nothing persisted.
    expect(getSession()).toBeNull();
    expect(getToken()).toBeNull();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("an empty token is rejected without a network call", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const result = await login("   ");
    expect(result.ok).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("logout clears the session and the persisted token", async () => {
    vi.stubGlobal("fetch", okWhoami());
    await login("tok");
    expect(getSession()).not.toBeNull();

    logout();
    expect(getSession()).toBeNull();
    expect(getToken()).toBeNull();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("restore() re-validates a persisted token (refresh survival)", async () => {
    // Simulate a prior tab having persisted a token, then a page refresh (memory
    // cleared, storage intact).
    sessionStorage.setItem(TOKEN_KEY, "persisted-token");
    vi.stubGlobal("fetch", okWhoami("casey.viewer", ["viewers"]));

    const session = await restore();
    expect(session?.user.username).toBe("casey.viewer");
    expect(getToken()).toBe("persisted-token");
  });

  it("restore() clears a now-invalid persisted token", async () => {
    sessionStorage.setItem(TOKEN_KEY, "stale-token");
    vi.stubGlobal("fetch", badWhoami(401));

    const session = await restore();
    expect(session).toBeNull();
    expect(getSession()).toBeNull();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("notifies subscribers on login and logout", async () => {
    vi.stubGlobal("fetch", okWhoami());
    const listener = vi.fn();
    const unsub = subscribe(listener);

    await login("tok");
    expect(listener).toHaveBeenCalled();
    const afterLogin = listener.mock.calls.length;

    logout();
    expect(listener.mock.calls.length).toBeGreaterThan(afterLogin);
    unsub();
  });

  it("NEVER logs the token — no console output contains it", async () => {
    const logSpy = vi.spyOn(console, "log").mockImplementation(() => {});
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});
    const debugSpy = vi.spyOn(console, "debug").mockImplementation(() => {});

    const SECRET = "eyJhbGciOiJSUzI1Nisuper-secret-token-value";

    // Exercise the full lifecycle: a happy login, a logout, and a rejected login.
    vi.stubGlobal("fetch", okWhoami());
    await login(SECRET);
    logout();
    vi.stubGlobal("fetch", badWhoami(401));
    await login(SECRET);
    sessionStorage.setItem(TOKEN_KEY, SECRET);
    await restore().catch(() => {});

    const allOutput = [logSpy, errSpy, warnSpy, infoSpy, debugSpy]
      .flatMap((spy) => spy.mock.calls)
      .flat()
      .map((arg) => (typeof arg === "string" ? arg : JSON.stringify(arg)))
      .join(" ");
    expect(allOutput).not.toContain(SECRET);
  });
});
