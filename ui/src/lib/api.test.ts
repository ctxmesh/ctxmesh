import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  api,
  ApiError,
  formatRunStep,
  openLogStream,
  openRunStream,
  setSessionExpiredHandler,
  setTokenProvider,
  whoami,
  type LogEventType,
  type RunEventKind,
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

  it("agentDetail reads GET /api/agents/{ns}/{name}", async () => {
    const fetchMock = mockFetch({
      name: "billing", namespace: "prod", image: "img:1", executionModel: "serving",
      role: "", scaling: { min: 0, max: 3 }, phase: "Ready", ready: true, url: "http://x",
      latestVersion: "v2", conditions: [], bindings: [], versions: ["v1", "v2"],
    });
    vi.stubGlobal("fetch", fetchMock);
    const res = await api.agentDetail("prod", "billing");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/agents/prod/billing");
    expect(res.ready).toBe(true);
    expect(res.versions).toEqual(["v1", "v2"]);
  });

  it("traceDetail reads GET /api/traces/{id}/detail (flat spans)", async () => {
    const fetchMock = mockFetch({
      rollup: { traceId: "t1", name: "run", timestamp: "", costUSD: 0.006, tokens: 1240, latencyMs: 1840, spanCount: 2 },
      spans: [{ id: "s0", parentId: "", type: "SPAN", name: "run", startMs: 0, durationMs: 1840, model: "", tokensIn: 0, tokensOut: 0, costUSD: 0, level: "", status: "ok", input: "", output: "", inputRedacted: false, outputRedacted: false }],
    });
    vi.stubGlobal("fetch", fetchMock);
    const res = await api.traceDetail("t1");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/traces/t1/detail");
    expect(res.spans).toHaveLength(1);
    expect(res.rollup.costUSD).toBeCloseTo(0.006);
  });
});

// --- SSE log tail (fetch-stream reader) -------------------------------------
// A fetch-stream Response whose body yields the given chunks in order, so the
// reader parses `event:`/`data:` frames off a byte stream exactly as the BFF
// writes them. It also records the (url, init) so a test can assert the Bearer.
function sseResponse(chunks: string[], ok = true, status = 200, errorBody?: unknown) {
  const enc = new TextEncoder();
  let i = 0;
  const body = {
    getReader() {
      return {
        read() {
          if (i < chunks.length) {
            const value = enc.encode(chunks[i++]);
            return Promise.resolve({ value, done: false });
          }
          return Promise.resolve({ value: undefined, done: true });
        },
        releaseLock() {},
      };
    },
  };
  return {
    ok,
    status,
    body: ok ? body : null,
    json: async () => errorBody ?? {},
    text: async () => (errorBody ? JSON.stringify(errorBody) : ""),
  } as unknown as Response;
}

// collect drains an openLogStream into a promise that resolves once a terminal
// signal (end / error / forbidden / onError) fires, so tests can await it.
function collect(
  ns: string,
  name: string,
  makeFetch: (capture: RequestInit | undefined) => void,
  opts?: Parameters<typeof openLogStream>[3],
) {
  type Errored = { message: string; status?: number } | null;
  const events: { type: LogEventType; data: string }[] = [];
  let forbidden: string | null = null;
  let errored: Errored = null;
  return new Promise<{
    events: typeof events;
    forbidden: string | null;
    errored: Errored;
    cancel: () => void;
  }>((resolve) => {
    const done = (cancel: () => void) =>
      setTimeout(() => resolve({ events, forbidden, errored, cancel }), 0);
    const cancel = openLogStream(
      ns,
      name,
      {
        onEvent: (type, data) => {
          events.push({ type, data });
          if (type === "end" || type === "error") done(cancel);
        },
        onForbidden: (m) => {
          forbidden = m;
          done(cancel);
        },
        onError: (m, s) => {
          errored = { message: m, status: s };
          done(cancel);
        },
      },
      opts,
    );
    // Give the async fetch a beat to record the request headers.
    setTimeout(() => makeFetch(lastInit), 0);
  });
}

let lastInit: RequestInit | undefined;

describe("openLogStream (fetch-stream SSE tail)", () => {
  beforeEach(() => {
    lastInit = undefined;
  });

  it("attaches the caller's Bearer to the SSE request (EventSource can't)", async () => {
    setTokenProvider(() => "tok-abc");
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      lastInit = init;
      return Promise.resolve(sseResponse(["event: end\ndata: done\n\n"]));
    });
    vi.stubGlobal("fetch", fetchMock);
    await collect("prod", "billing", () => {});
    expect(new Headers(lastInit?.headers).get("Authorization")).toBe("Bearer tok-abc");
    // The Bearer rides the /logs request specifically.
    expect(String(fetchMock.mock.calls[0][0])).toContain("/api/agents/prod/billing/logs");
  });

  it("renders log frames in order and ends on the end frame", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          sseResponse([
            "event: log\ndata: line one\n\nevent: log\ndata: line two\n\n",
            "event: log\ndata: line three\n\nevent: end\ndata: bye\n\n",
          ]),
        ),
      ),
    );
    const { events } = await collect("prod", "billing", () => {});
    const logs = events.filter((e) => e.type === "log").map((e) => e.data);
    expect(logs).toEqual(["line one", "line two", "line three"]);
    expect(events[events.length - 1].type).toBe("end");
  });

  it("surfaces a `waiting` frame (no pod yet), distinct from an error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(sseResponse(["event: waiting\ndata: starting\n\nevent: end\ndata: x\n\n"]))),
    );
    const { events, errored, forbidden } = await collect("prod", "billing", () => {});
    expect(events.some((e) => e.type === "waiting")).toBe(true);
    expect(errored).toBeNull();
    expect(forbidden).toBeNull();
  });

  it("surfaces an IN-STREAM error frame (mid-stream break)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(sseResponse(["event: log\ndata: a\n\nevent: error\ndata: pod died\n\n"]))),
    );
    const { events } = await collect("prod", "billing", () => {});
    const err = events.find((e) => e.type === "error");
    expect(err?.data).toBe("pod died");
  });

  it("distinguishes a PRE-STREAM 403 (HTTP status, no frames) from an in-stream error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(sseResponse([], false, 403, { error: "forbidden: not allowed to read pods" })),
      ),
    );
    const { events, forbidden, errored } = await collect("prod", "billing", () => {});
    // No SSE frames — this is a forbidden STATE, not an in-stream `error` event.
    expect(events).toHaveLength(0);
    expect(forbidden).toContain("forbidden");
    expect(errored).toBeNull();
  });

  it("cancel() aborts the request (no leak on unmount)", async () => {
    let signal: AbortSignal | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init?: RequestInit) => {
        signal = init?.signal ?? undefined;
        return Promise.resolve(sseResponse(["event: log\ndata: a\n\n"]));
      }),
    );
    const cancel = openLogStream("prod", "billing", {
      onEvent: () => {},
    });
    cancel();
    await new Promise((r) => setTimeout(r, 0));
    expect(signal?.aborted).toBe(true);
  });
});

// collectRun drains an openRunStream into a promise resolving once the stream closes
// (clean close, an error, or a pre-stream forbidden).
function collectRun(runId: string) {
  type RunEv = { kind: RunEventKind; data: string; seq: number };
  type Errored = { message: string; status?: number } | null;
  const events: RunEv[] = [];
  let errored: Errored = null;
  let forbidden: string | null = null;
  return new Promise<{
    events: RunEv[];
    errored: Errored;
    forbidden: string | null;
  }>((resolve) => {
    const done = () => setTimeout(() => resolve({ events, errored, forbidden }), 0);
    openRunStream(runId, {
      onEvent: (kind, data, seq) => events.push({ kind, data, seq }),
      onClose: done,
      onForbidden: (m) => {
        forbidden = m;
        done();
      },
      onError: (message, status) => {
        errored = { message, status };
        done();
      },
    });
  });
}

describe("openRunStream (run event SSE, m32.8)", () => {
  it("parses id/event/data frames into typed run events in order", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          sseResponse([
            "id:1\nevent:token\ndata:Hel\n\n",
            "id:2\nevent:token\ndata:lo\n\n",
            "id:3\nevent:message\ndata:Hello\n\n",
            "id:4\nevent:state\ndata:succeeded\n\n",
          ]),
        ),
      ),
    );
    const { events, errored } = await collectRun("run-1");
    expect(errored).toBeNull();
    expect(events).toEqual([
      { kind: "token", data: "Hel", seq: 1 },
      { kind: "token", data: "lo", seq: 2 },
      { kind: "message", data: "Hello", seq: 3 },
      { kind: "state", data: "succeeded", seq: 4 },
    ]);
  });

  it("attaches the caller's Bearer + Accept: text/event-stream", async () => {
    setTokenProvider(() => "tok-run");
    let init: RequestInit | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, i?: RequestInit) => {
        init = i;
        return Promise.resolve(sseResponse(["event:state\ndata:succeeded\n\n"]));
      }),
    );
    await collectRun("run-9");
    const headers = new Headers(init?.headers);
    expect(headers.get("Authorization")).toBe("Bearer tok-run");
    expect(headers.get("Accept")).toBe("text/event-stream");
  });

  it("a pre-stream 403 reports onForbidden, emits no events", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(sseResponse([], false, 403, { error: "denied" }))));
    const { events, forbidden } = await collectRun("run-x");
    expect(events).toEqual([]);
    expect(forbidden).not.toBeNull();
  });

  it("parses `step` metadata frames as typed step events (M78)", async () => {
    const stepJSON = '{"step":1,"kind":"model","tokens":{"prompt":11,"completion":7},"ref":null}';
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          sseResponse([
            `id:1\nevent:step\ndata:${stepJSON}\n\n`,
            "id:2\nevent:step\ndata:plan-approved\n\n", // legacy plain-label form
            "id:3\nevent:state\ndata:succeeded\n\n",
          ]),
        ),
      ),
    );
    const { events } = await collectRun("run-step");
    expect(events).toEqual([
      { kind: "step", data: stepJSON, seq: 1 },
      { kind: "step", data: "plan-approved", seq: 2 },
      { kind: "state", data: "succeeded", seq: 3 },
    ]);
  });
});

describe("formatRunStep (M78 live step-visibility, ADR 0071 §4)", () => {
  it("renders the new JSON step metadata into a compact label", () => {
    expect(
      formatRunStep('{"step":2,"kind":"model","tokens":{"prompt":11,"completion":7},"ref":null}'),
    ).toBe("Step 2 · model · ↑11 ↓7");
  });

  it("tolerates the SSE-envelope `type` key the SDK/BFF carry verbatim", () => {
    // The real EventStep Data is the SDK's SSE frame payload, which carries "type":"step".
    // formatRunStep ignores it and renders only the visible metadata.
    expect(
      formatRunStep('{"type":"step","step":3,"kind":"model","tokens":{"prompt":5,"completion":4}}'),
    ).toBe("Step 3 · model · ↑5 ↓4");
  });

  it("includes the tool name for a tool step and omits zero token counts", () => {
    expect(
      formatRunStep('{"step":1,"kind":"tool","tool":"echo_tool","tokens":{"prompt":0,"completion":0}}'),
    ).toBe("Step 1 · tool · echo_tool");
  });

  it("returns a LEGACY plain-string label verbatim (backward-compat)", () => {
    // The workflow plan-approval EventStep Data is a bare label, not JSON.
    expect(formatRunStep("plan-approved")).toBe("plan-approved");
    expect(formatRunStep("plan-rejected")).toBe("plan-rejected");
  });

  it("renders a PLATFORM step frame's human label (M143 structured-output re-ask)", () => {
    // Without the label branch this frame would hit the raw-text fallback below and the user would
    // be shown `{"kind":"output_schema_reask",…}` in the step indicator.
    expect(
      formatRunStep('{"kind":"output_schema_reask","label":"Re-asking — the answer didn\'t match the required format"}'),
    ).toBe("Re-asking — the answer didn't match the required format");
  });

  it("ignores a blank label rather than blanking the step indicator", () => {
    expect(formatRunStep('{"label":"   "}')).toBe('{"label":"   "}');
  });

  it("falls back to the raw text for a malformed / non-step JSON object, never throws", () => {
    expect(formatRunStep('{"unexpected":true}')).toBe('{"unexpected":true}');
    expect(formatRunStep("{not json")).toBe("{not json");
    expect(formatRunStep("")).toBe("");
    expect(formatRunStep("   ")).toBe("");
  });
});

describe("run mutations (m32.8/m32.9)", () => {
  it("createRun POSTs /api/runs and returns the handle", async () => {
    const fetchMock = mockFetch({ id: "run-1", status: "queued" }, true, 202);
    vi.stubGlobal("fetch", fetchMock);
    const handle = await api.createRun({ agent: "echo", namespace: "prod", input: { input: "hi" } });
    expect(handle).toEqual({ id: "run-1", status: "queued" });
    expect(String(fetchMock.mock.calls[0][0])).toContain("/api/runs");
  });

  it("resumeRun sends the decision to /resume", async () => {
    const fetchMock = mockFetch({ id: "run-1", status: "cancelled" });
    vi.stubGlobal("fetch", fetchMock);
    const handle = await api.resumeRun("run-1", "deny");
    expect(handle.status).toBe("cancelled");
    expect(String(fetchMock.mock.calls[0][0])).toContain("/api/runs/run-1/resume");
    const body = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string);
    expect(body).toEqual({ decision: "deny" });
  });

  it("cancelRun POSTs /cancel", async () => {
    const fetchMock = mockFetch({ id: "run-1", status: "cancelled" });
    vi.stubGlobal("fetch", fetchMock);
    const handle = await api.cancelRun("run-1");
    expect(handle.status).toBe("cancelled");
    expect(String(fetchMock.mock.calls[0][0])).toContain("/api/runs/run-1/cancel");
  });
});
