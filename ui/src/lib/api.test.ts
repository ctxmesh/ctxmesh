import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  api,
  ApiError,
  setSessionExpiredHandler,
  setTokenProvider,
  whoami,
} from "@/lib/api";

// mockFetch returns a fetch stub resolving a Response-like object. It captures
// the (url, init) call so tests can assert on the attached headers.
function mockFetch(body: unknown, ok = true, status = 200) {
  return vi.fn().mockResolvedValue({
    ok,
    status,
    json: async () => body,
    text: async () => (typeof body === "string" ? body : JSON.stringify(body)),
  } as Response);
}

// authHeader pulls the Authorization value out of a captured fetch call's init,
// normalizing the Headers instance api.ts builds.
function authHeader(fetchMock: ReturnType<typeof vi.fn>, callIndex = 0): string | null {
  const init = fetchMock.mock.calls[callIndex]?.[1] as RequestInit | undefined;
  return new Headers(init?.headers).get("Authorization");
}

beforeEach(() => {
  // Reset the module-level seams to their anonymous defaults before each test.
  setTokenProvider(() => null);
  setSessionExpiredHandler(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("api client", () => {
  it("listAgents parses the BFF DTO", async () => {
    const payload = {
      agents: [
        {
          name: "echo",
          namespace: "default",
          image: "echo:latest",
          phase: "Ready",
          ready: true,
        },
      ],
    };
    vi.stubGlobal("fetch", mockFetch(payload));

    const res = await api.listAgents();
    expect(res.agents).toHaveLength(1);
    expect(res.agents[0].name).toBe("echo");
    expect(res.agents[0].ready).toBe(true);
  });

  it("health hits /api/health with the JSON Accept header", async () => {
    const fetchMock = mockFetch({ status: "ok", version: "dev" });
    vi.stubGlobal("fetch", fetchMock);

    const res = await api.health();
    expect(res.status).toBe("ok");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/health");
    expect(new Headers(fetchMock.mock.calls[0][1]?.headers).get("Accept")).toBe(
      "application/json",
    );
  });

  it("throws ApiError with the status on a non-2xx response", async () => {
    vi.stubGlobal("fetch", mockFetch({}, false, 403));

    await expect(api.listAgents()).rejects.toMatchObject({
      name: "ApiError",
      status: 403,
    });
    await expect(api.listAgents()).rejects.toBeInstanceOf(ApiError);
  });

  it("ApiError typing distinguishes 401 / 403 / other", () => {
    const forbidden = new ApiError("denied", 403);
    expect(forbidden.isForbidden).toBe(true);
    expect(forbidden.isUnauthorized).toBe(false);

    const unauth = new ApiError("expired", 401);
    expect(unauth.isUnauthorized).toBe(true);
    expect(unauth.isForbidden).toBe(false);

    const other = new ApiError("boom", 500);
    expect(other.isForbidden).toBe(false);
    expect(other.isUnauthorized).toBe(false);
  });

  it("attaches Authorization: Bearer for /api/* when a token is set", async () => {
    const fetchMock = mockFetch({ agents: [] });
    vi.stubGlobal("fetch", fetchMock);
    setTokenProvider(() => "tok-123");

    await api.listAgents();
    expect(authHeader(fetchMock)).toBe("Bearer tok-123");
  });

  it("attaches NO Authorization header when there is no session", async () => {
    const fetchMock = mockFetch({ agents: [] });
    vi.stubGlobal("fetch", fetchMock);
    setTokenProvider(() => null);

    await api.listAgents();
    expect(authHeader(fetchMock)).toBeNull();
  });

  it("attaches the bearer to POST /api/* requests too", async () => {
    const fetchMock = mockFetch({ created: [] });
    vi.stubGlobal("fetch", fetchMock);
    setTokenProvider(() => "tok-post");

    await api.createAgent("kind: Agent", "default");
    expect(authHeader(fetchMock)).toBe("Bearer tok-post");
  });

  it("fires the session-expired handler on a mid-session 401", async () => {
    vi.stubGlobal("fetch", mockFetch({ error: "expired" }, false, 401));
    const expired = vi.fn();
    setSessionExpiredHandler(expired);
    setTokenProvider(() => "tok-dead");

    await expect(api.listAgents()).rejects.toBeInstanceOf(ApiError);
    expect(expired).toHaveBeenCalledTimes(1);
  });

  it("does NOT fire the session-expired handler on a 403", async () => {
    vi.stubGlobal("fetch", mockFetch({ error: "denied" }, false, 403));
    const expired = vi.fn();
    setSessionExpiredHandler(expired);
    setTokenProvider(() => "tok");

    await expect(api.listAgents()).rejects.toMatchObject({ status: 403 });
    expect(expired).not.toHaveBeenCalled();
  });

  it("does NOT fire session-expired for a login-validation 401 (whoami login)", async () => {
    vi.stubGlobal("fetch", mockFetch({ error: "bad token" }, false, 401));
    const expired = vi.fn();
    setSessionExpiredHandler(expired);

    await expect(
      whoami({ token: "wrong", login: true }),
    ).rejects.toBeInstanceOf(ApiError);
    // The login page handles this 401 itself — it is NOT a session expiry.
    expect(expired).not.toHaveBeenCalled();
  });

  it("whoami sends the explicit token even without a global session", async () => {
    const fetchMock = mockFetch({ username: "alex", groups: ["dev"] });
    vi.stubGlobal("fetch", fetchMock);
    setTokenProvider(() => null); // no session yet

    const who = await whoami({ token: "paste-me", login: true });
    expect(who.username).toBe("alex");
    expect(authHeader(fetchMock)).toBe("Bearer paste-me");
  });

  it("listTools reads the merged catalog from GET /api/tools", async () => {
    const fetchMock = mockFetch({
      tools: [{ name: "get_order", source: "acme-mcp", approvalStatus: "approved" }],
    });
    vi.stubGlobal("fetch", fetchMock);
    const res = await api.listTools();
    expect(res.tools).toHaveLength(1);
    expect(res.tools[0].name).toBe("get_order");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/tools");
  });

  it("generateAgent returns the valid config on 200", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch({ agentYAML: "name: x\nruntime: managed\n", expanded: "kind: AgentDeployment\n", model: "m" }),
    );
    const res = await api.generateAgent({ description: "a bot" });
    expect(res.agentYAML).toContain("runtime: managed");
    expect(res.regenerate).toBeUndefined();
  });

  it("generateAgent does NOT throw on 422 — it returns the regenerate body (keyed on the flag)", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch({ reason: "invalid", agentYAML: "name: bad\n", regenerate: true }, false, 422),
    );
    // A 422 is the regenerate outcome, NOT an error — the raw YAML is preserved.
    const res = await api.generateAgent({ description: "x" });
    expect(res.regenerate).toBe(true);
    expect(res.reason).toBe("invalid");
    expect(res.agentYAML).toContain("bad");
  });

  it("generateAgent DOES throw on a genuine failure (403/500)", async () => {
    vi.stubGlobal("fetch", mockFetch({ error: "forbidden" }, false, 403));
    await expect(api.generateAgent({ description: "x" })).rejects.toBeInstanceOf(ApiError);
  });
});
