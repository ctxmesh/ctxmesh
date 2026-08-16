/**
 * ToolsClient tests — parity with `sdk/python/tests/test_tools.py`.
 *
 * Covers:
 *   - list() discovery (sidecar hit, then tools.json fallback, then empty on no-file)
 *   - Tool.fromDict inputSchema / description parsing
 *   - Full 4-step MCP handshake (initialize → notifications/initialized → tools/list → tools/call)
 *   - 307 redirect followed correctly (same-origin)
 *   - Cross-origin redirect refused
 *   - Session-Id carried on subsequent POSTs
 *   - Catalog name vs MCP name resolution (normalized, sole-tool, unresolvable)
 *   - SSE-framed response parsed
 *   - delegate() gating + request shape + capability relay
 *   - handoff() request shape + capability relay
 *   - knowledge_search dispatched locally
 *   - Synthetic tools (delegate_to / handoff_to / knowledge_search) in list()
 */

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as apprMod from "../src/_approval.js";
import * as capMod from "../src/_capability.js";
import * as recordMod from "../src/_record.js";
import { ApprovalRequiredError, ConfigError, EndpointError } from "../src/errors.js";
import { Tool, ToolsClient, DELEGATE_TOOL_NAME, HANDOFF_TOOL_NAME, KNOWLEDGE_SEARCH_TOOL_NAME } from "../src/tools.js";
import { DiscoveryStub, startPlane, type MockPlane, type StubResponse } from "./plane.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
  vi.restoreAllMocks();
  // Clean up env vars mutated within tests.
  delete process.env["DELEGATE_ENABLED"];
  delete process.env["DELEGATE_ROSTER"];
  delete process.env["KNOWLEDGE_BASE_ENABLED"];
  delete process.env["KNOWLEDGE_BASES"];
});

// ── Tool.fromDict ─────────────────────────────────────────────────────────────

describe("Tool.fromDict", () => {
  const base = {
    name: "word-count",
    mode: "remote",
    endpoint: "http://wc.svc/mcp",
    transport: "streamable-http",
  };

  it("parses a manifest entry with inputSchema verbatim", () => {
    const schema = { type: "object", properties: { text: { type: "string" } }, required: ["text"] };
    const tool = Tool.fromDict({ ...base, inputSchema: schema });
    expect(tool.inputSchema).toEqual(schema);
  });

  it("returns null inputSchema when absent", () => {
    expect(Tool.fromDict(base).inputSchema).toBeNull();
  });

  it("returns null inputSchema when inputSchema is null", () => {
    expect(Tool.fromDict({ ...base, inputSchema: null }).inputSchema).toBeNull();
  });

  it("returns null inputSchema when inputSchema is a non-object", () => {
    expect(Tool.fromDict({ ...base, inputSchema: "not-an-object" }).inputSchema).toBeNull();
    expect(Tool.fromDict({ ...base, inputSchema: 123 }).inputSchema).toBeNull();
  });

  it("parses description from manifest", () => {
    expect(Tool.fromDict({ ...base, description: "Count words." }).description).toBe("Count words.");
  });

  it('returns empty string description when absent', () => {
    expect(Tool.fromDict(base).description).toBe("");
  });

  it('returns empty string description when null or non-string', () => {
    expect(Tool.fromDict({ ...base, description: null }).description).toBe("");
    expect(Tool.fromDict({ ...base, description: 123 }).description).toBe("");
  });
});

// ── list() — discovery ────────────────────────────────────────────────────────

describe("ToolsClient.list — discovery sidecar", () => {
  it("returns manifest tools from the sidecar", async () => {
    const client = new ToolsClient(plane.config);
    const tools = await client.list();

    expect(tools).toHaveLength(1);
    expect(tools[0]?.name).toBe(DiscoveryStub.CATALOG_NAME);
    expect(tools[0]?.mode).toBe("remote");
    expect(tools[0]?.transport).toBe("streamable-http");
    expect(tools[0]?.endpoint).toContain("/mcp");

    // Sidecar was hit.
    const listReq = plane.discovery.requests.find((r) => r.path === "/tools");
    expect(listReq?.method).toBe("GET");
  });

  it("surfaces inputSchema and description from manifest when present", async () => {
    const schema = { type: "object", properties: { n: { type: "integer" } } };
    // Patch the manifest inline.
    plane.discovery["routes"].set("GET /tools", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        version: "x",
        tools: [
          {
            name: "word-count",
            mode: "remote",
            endpoint: `${plane.discovery.baseUrl}/mcp`,
            transport: "streamable-http",
            inputSchema: schema,
            description: "Count whitespace-separated words.",
          },
        ],
      }),
    }));

    const client = new ToolsClient(plane.config);
    const tools = await client.list();
    expect(tools[0]?.inputSchema).toEqual(schema);
    expect(tools[0]?.description).toBe("Count whitespace-separated words.");
  });
});

describe("ToolsClient.list — tools.json fallback", () => {
  it("falls back to tools.json when the sidecar is unreachable", async () => {
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "tools-test-"));
    const toolsJsonPath = path.join(tmpDir, "tools.json");
    fs.writeFileSync(
      toolsJsonPath,
      JSON.stringify({
        version: "f",
        tools: [
          {
            name: "local",
            mode: "remote",
            endpoint: "http://x/mcp",
            transport: "streamable-http",
          },
        ],
      }),
    );

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({
      discoveryBaseUrl: "http://127.0.0.1:1", // dead port → unreachable
      toolsJsonPath,
    });
    const client = new ToolsClient(config);
    const tools = await client.list();
    expect(tools).toHaveLength(1);
    expect(tools[0]?.name).toBe("local");

    fs.unlinkSync(toolsJsonPath);
    fs.rmdirSync(tmpDir);
  });

  it("returns empty list when neither sidecar nor tools.json is present", async () => {
    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({
      discoveryBaseUrl: "http://127.0.0.1:1", // dead port
      toolsJsonPath: "/tmp/agent-brain-test-nonexistent-tools.json",
    });
    const client = new ToolsClient(config);
    const tools = await client.list();
    expect(tools).toEqual([]);
  });

  it("throws EndpointError when tools.json exists but is malformed JSON", async () => {
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "tools-test-"));
    const toolsJsonPath = path.join(tmpDir, "tools.json");
    fs.writeFileSync(toolsJsonPath, "{ this is not valid json");

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({
      discoveryBaseUrl: "http://127.0.0.1:1", // dead port
      toolsJsonPath,
    });
    const client = new ToolsClient(config);
    await expect(client.list()).rejects.toBeInstanceOf(EndpointError);

    fs.unlinkSync(toolsJsonPath);
    fs.rmdirSync(tmpDir);
  });
});

// ── call() — full MCP handshake ───────────────────────────────────────────────

describe("ToolsClient.call — full MCP session", () => {
  it("completes initialize → initialized → tools/list → tools/call and returns parsed result", async () => {
    const client = new ToolsClient(plane.config);
    const result = await client.call(DiscoveryStub.CATALOG_NAME, { text: "a b c" });

    expect(result).toEqual({ count: 3, server_version: "v1" });

    // 4 MCP POSTs landed at /mcp/ (after the 307 redirect from /mcp).
    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    const methods = mcpReqs.map((r) => (r.json() as { method?: string }).method);
    expect(methods).toEqual([
      "initialize",
      "notifications/initialized",
      "tools/list",
      "tools/call",
    ]);

    // tools/list was consulted (name resolution).
    expect(plane.discovery.listCalls).toBe(1);
  });

  it("follows the 307 redirect from /mcp to /mcp/", async () => {
    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "hello" });

    // The endpoint in the manifest is /mcp (no trailing slash). The first POST
    // (initialize) hits /mcp, gets a 307 to /mcp/, follows it. Subsequent POSTs
    // reuse the resolved URL /mcp/ directly (the TS SDK caches the resolved endpoint
    // after the first redirect — mirrors the Python behavior with urllib, which also
    // follows redirects per-request, but the TS test correctly captures the
    // post-redirect endpoint and reuses it for all subsequent calls).
    // Net: exactly 1 redirect trigger (the initialize POST) and 4 calls to /mcp/.
    const redirectTriggers = plane.discovery.requests.filter((r) => r.path === "/mcp");
    const redirected = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    expect(redirectTriggers).toHaveLength(1); // initialize hits /mcp first
    expect(redirected).toHaveLength(4);       // all 4 MCP calls land at /mcp/
  });

  it("resolves catalog name (word-count) to MCP name (word_count)", async () => {
    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "hi" });

    // The accepted tools/call carried the underscore MCP name, not the hyphenated catalog name.
    expect(plane.discovery.mcpCalls).toHaveLength(1);
    expect(plane.discovery.mcpCalls[0]).toMatchObject({
      name: DiscoveryStub.MCP_TOOL_NAME,
      arguments: { text: "hi" },
    });
    expect(DiscoveryStub.MCP_TOOL_NAME).not.toBe(DiscoveryStub.CATALOG_NAME);
  });

  it("carries the Mcp-Session-Id on subsequent POSTs", async () => {
    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "x" });

    // After the 307, all MCP POSTs land at /mcp/.
    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    // The notifications/initialized (index 1) must carry the session-id returned by initialize.
    const initializedReq = mcpReqs[1];
    expect(initializedReq?.headers["mcp-session-id"]).toBe("sess-1");
  });

  it("handles SSE-framed (text/event-stream) response from tools/call", async () => {
    // The DiscoveryStub already replies with SSE for tools/call — this just asserts
    // the result is correctly parsed from the SSE data: frame.
    const client = new ToolsClient(plane.config);
    const result = await client.call(DiscoveryStub.CATALOG_NAME, { text: "a b c" });
    expect(result).toEqual({ count: 3, server_version: "v1" });
  });

  it("throws ConfigError for an unknown tool name", async () => {
    const client = new ToolsClient(plane.config);
    await expect(client.call("does-not-exist")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws EndpointError when the MCP server returns a JSON-RPC error for unknown tool name", async () => {
    // Force the tools/call to use a server name the stub will reject.
    // We do this by patching the server tools to advertise only alpha+beta so the catalog
    // name can't resolve, triggering a ConfigError (name resolution fails before the call).
    // For a server-side JSON-RPC error, we patch the stub response instead.
    const discovery2 = new DiscoveryStub({ count: 0 });
    discovery2["routes"].set("POST /mcp/", (_s, req): StubResponse => {
      const msg = req.json() as { method?: string; id?: unknown };
      if (msg.method === "initialize") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json", "Mcp-Session-Id": "sess-x" },
          body: JSON.stringify({
            jsonrpc: "2.0",
            id: msg.id,
            result: { protocolVersion: "2025-03-26", capabilities: {} },
          }),
        };
      }
      if (msg.method === "notifications/initialized") return { status: 202 };
      if (msg.method === "tools/list") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            jsonrpc: "2.0",
            id: msg.id,
            result: { tools: [{ name: "word_count", inputSchema: {} }] },
          }),
        };
      }
      if (msg.method === "tools/call") {
        // Return a JSON-RPC error.
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            jsonrpc: "2.0",
            id: msg.id,
            error: { code: -32602, message: "something went wrong on server" },
          }),
        };
      }
      return { status: 400 };
    });
    // Also route GET /tools and POST /mcp (redirect).
    discovery2["routes"].set("GET /tools", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        version: "x",
        tools: [
          { name: "word-count", mode: "remote", endpoint: `${discovery2.baseUrl}/mcp/`, transport: "streamable-http" },
        ],
      }),
    }));
    await discovery2.start();

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({ discoveryBaseUrl: discovery2.baseUrl });
    const client = new ToolsClient(config);
    await expect(client.call("word-count", { text: "x" })).rejects.toBeInstanceOf(EndpointError);

    await discovery2.stop();
  });

  it("throws ConfigError when the resolved tool list is ambiguous", async () => {
    // Patch the discovery stub's serverTools to return two tools, neither matching catalog name.
    const discovery2 = new DiscoveryStub();
    discovery2["routes"].set("POST /mcp/", (_s, req): StubResponse => {
      const msg = req.json() as { method?: string; id?: unknown };
      if (msg.method === "initialize") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json", "Mcp-Session-Id": "sess-a" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-03-26", capabilities: {} } }),
        };
      }
      if (msg.method === "notifications/initialized") return { status: 202 };
      if (msg.method === "tools/list") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { tools: [{ name: "alpha" }, { name: "beta" }] } }),
        };
      }
      return { status: 400 };
    });
    discovery2["routes"].set("GET /tools", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ version: "x", tools: [{ name: "word-count", mode: "remote", endpoint: `${discovery2.baseUrl}/mcp/`, transport: "streamable-http" }] }),
    }));
    await discovery2.start();

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({ discoveryBaseUrl: discovery2.baseUrl });
    const client = new ToolsClient(config);
    const err = await client.call("word-count", { text: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ConfigError);
    expect(String(err)).toContain("alpha");
    expect(String(err)).toContain("beta");

    await discovery2.stop();
  });

  it("uses sole-tool fallback when the server exposes exactly one tool", async () => {
    // Point the endpoint at a stub with a differently-named sole tool.
    const discovery2 = new DiscoveryStub({ count: 99, server_version: "v2" });
    discovery2["routes"].set("POST /mcp/", (_s, req): StubResponse => {
      const msg = req.json() as { method?: string; id?: unknown; params?: Record<string, unknown> };
      if (msg.method === "initialize") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json", "Mcp-Session-Id": "sess-s" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-03-26", capabilities: {} } }),
        };
      }
      if (msg.method === "notifications/initialized") return { status: 202 };
      if (msg.method === "tools/list") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { tools: [{ name: "only_thing", inputSchema: {} }] } }),
        };
      }
      if (msg.method === "tools/call") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: JSON.stringify({ count: 99 }) }] } }),
        };
      }
      return { status: 400 };
    });
    discovery2["routes"].set("GET /tools", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ version: "x", tools: [{ name: "word-count", mode: "remote", endpoint: `${discovery2.baseUrl}/mcp/`, transport: "streamable-http" }] }),
    }));
    await discovery2.start();

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({ discoveryBaseUrl: discovery2.baseUrl });
    const client = new ToolsClient(config);
    const result = await client.call("word-count", { text: "x" });
    expect(result).toEqual({ count: 99 });

    await discovery2.stop();
  });

  it("refuses a cross-origin 307 redirect", async () => {
    // Override the POST /mcp route to return a cross-origin Location.
    plane.discovery["routes"].set("POST /mcp", () => ({
      status: 307,
      headers: { Location: "http://evil.example.com:9999/mcp/" },
    }));

    const client = new ToolsClient(plane.config);
    const err = await client.call(DiscoveryStub.CATALOG_NAME, { text: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect(String(err).toLowerCase()).toContain("cross-origin");
  });

  it("relays X-Ctxmesh-Run-Capability on MCP calls when set", async () => {
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-tool-token");

    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "y" });

    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    for (const req of mcpReqs) {
      expect(req.headers["x-ctxmesh-run-capability"]).toBe("cap-tool-token");
    }
  });

  it("relays X-Ctxmesh-Record on MCP tool calls when the run is being recorded (M78)", async () => {
    // A recorded run relays the record toggle on every tool-call egress so the egress sidecar
    // captures the tool I/O into the run's replay fixture (TOOL channel).
    vi.spyOn(recordMod, "currentRecordRunId").mockReturnValue("run-tool-rec");

    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "y" });

    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    expect(mcpReqs.length).toBeGreaterThan(0);
    for (const req of mcpReqs) {
      expect(req.headers["x-ctxmesh-record"]).toBe("run-tool-rec");
    }
  });

  it("does NOT relay X-Ctxmesh-Record for a non-recorded run (M78)", async () => {
    // Default (no recordScope active) ⇒ undefined ⇒ no capture header ⇒ the sidecar captures nothing.
    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "y" });

    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    for (const req of mcpReqs) {
      expect(req.headers["x-ctxmesh-record"]).toBeUndefined();
    }
  });

  it("relays X-Ctxmesh-Approval on MCP tool calls when a voucher is bound (m82.4)", async () => {
    // A resumed run with a granted require-approval tool relays the voucher on every tool-call egress
    // so the egress sidecar forwards the approved tool.
    vi.spyOn(apprMod, "currentApprovalVoucher").mockReturnValue("voucher-tool-token");

    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "y" });

    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    expect(mcpReqs.length).toBeGreaterThan(0);
    for (const req of mcpReqs) {
      expect(req.headers["x-ctxmesh-approval"]).toBe("voucher-tool-token");
    }
  });

  it("does NOT relay X-Ctxmesh-Approval when no voucher is bound (m82.4)", async () => {
    // Default (no voucherScope active) ⇒ undefined ⇒ no header ⇒ a require-approval tool 403s.
    const client = new ToolsClient(plane.config);
    await client.call(DiscoveryStub.CATALOG_NAME, { text: "y" });

    const mcpReqs = plane.discovery.requests.filter((r) => r.path === "/mcp/");
    for (const req of mcpReqs) {
      expect(req.headers["x-ctxmesh-approval"]).toBeUndefined();
    }
  });

  it("maps a 403 approval_required from the wire to ApprovalRequiredError (m82.4)", async () => {
    // The egress sidecar answers a require-approval tool (no voucher) with a typed 403
    // approval_required; the SDK must surface it as ApprovalRequiredError so the managed loop pauses
    // (and a custom loop that ignores it is denied — the floor). The key mirrors tool:<name>.
    const discovery2 = new DiscoveryStub();
    discovery2["routes"].set("POST /mcp/", (_s, req): StubResponse => {
      const msg = req.json() as { method?: string; id?: unknown };
      if (msg.method === "initialize") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json", "Mcp-Session-Id": "sess-appr" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-03-26", capabilities: {} } }),
        };
      }
      if (msg.method === "notifications/initialized") return { status: 202 };
      if (msg.method === "tools/list") {
        return {
          status: 200,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { tools: [{ name: DiscoveryStub.MCP_TOOL_NAME }] } }),
        };
      }
      // tools/call → the sidecar's typed 403 approval_required.
      return {
        status: 403,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ error: "approval_required", tool: DiscoveryStub.MCP_TOOL_NAME, run: "run-1" }),
      };
    });
    discovery2["routes"].set("GET /tools", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ version: "x", tools: [{ name: DiscoveryStub.CATALOG_NAME, mode: "remote", endpoint: `${discovery2.baseUrl}/mcp/`, transport: "streamable-http" }] }),
    }));
    await discovery2.start();

    const { PlaneConfig } = await import("../src/config.js");
    const config = PlaneConfig.forTest({ discoveryBaseUrl: discovery2.baseUrl });
    const client = new ToolsClient(config);
    const err = await client.call(DiscoveryStub.CATALOG_NAME, { text: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApprovalRequiredError);
    expect((err as ApprovalRequiredError).key).toBe(`tool:${DiscoveryStub.MCP_TOOL_NAME}`);

    await discovery2.stop();
  });
});

// ── delegate() ───────────────────────────────────────────────────────────────

describe("ToolsClient.delegate", () => {
  it("throws ConfigError when delegateEnabled is false", async () => {
    // plane.config has delegateEnabled=false by default.
    const client = new ToolsClient(plane.config);
    await expect(client.delegate("researcher", "find it")).rejects.toBeInstanceOf(ConfigError);
  });

  it("POSTs to /delegate with the correct body and returns the launcher response", async () => {
    const config = { ...plane.config, delegateEnabled: true } as typeof plane.config;
    const client = new ToolsClient(config);

    const result = await client.delegate("researcher", "find it", "step-2", "call-9");

    expect(result).toMatchObject({ ok: true, answer: "sub-answer" });

    const req = plane.delegate.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/delegate");
    expect(req?.json()).toMatchObject({
      subAgent: "researcher",
      input: "find it",
      step: "step-2",
      callId: "call-9",
    });
  });

  it("relays X-Ctxmesh-Run-Capability when set", async () => {
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-delegate-token");

    const config = { ...plane.config, delegateEnabled: true } as typeof plane.config;
    const client = new ToolsClient(config);
    await client.delegate("researcher", "task");

    const req = plane.delegate.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-delegate-token");
  });

  it("does NOT relay capability when currentCapability returns undefined", async () => {
    const config = { ...plane.config, delegateEnabled: true } as typeof plane.config;
    const client = new ToolsClient(config);
    await client.delegate("researcher", "task");

    const req = plane.delegate.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBeUndefined();
  });
});

// ── handoff() ────────────────────────────────────────────────────────────────

describe("ToolsClient.handoff", () => {
  it("POSTs to /handoff with the correct body and returns the launcher response", async () => {
    const client = new ToolsClient(plane.config);
    const result = await client.handoff("billing", "refund needed");

    expect(result).toMatchObject({ ok: true, runId: "hand-1", handedOffTo: "billing" });

    const req = plane.delegate.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/handoff");
    // includeHistory defaults true (replay B's full history — today's behavior, m83.6).
    expect(req?.json()).toMatchObject({
      targetAgent: "billing",
      message: "refund needed",
      includeHistory: true,
    });
  });

  it("uses empty string for message when not provided", async () => {
    const client = new ToolsClient(plane.config);
    await client.handoff("billing");

    const req = plane.delegate.requests[0];
    expect((req?.json() as { message: string }).message).toBe("");
  });

  it("relays includeHistory=false so B skips the transfer-turn history replay (m83.6)", async () => {
    const client = new ToolsClient(plane.config);
    await client.handoff("billing", "here is a summary…", false);

    const req = plane.delegate.requests[0];
    expect(req?.json()).toMatchObject({
      targetAgent: "billing",
      message: "here is a summary…",
      includeHistory: false,
    });
  });

  it("relays X-Ctxmesh-Run-Capability when set", async () => {
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-handoff-token");

    const client = new ToolsClient(plane.config);
    await client.handoff("billing", "msg");

    const req = plane.delegate.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-handoff-token");
  });
});

// ── knowledge_search local dispatch ──────────────────────────────────────────

describe("ToolsClient.call — knowledge_search local dispatch", () => {
  it("dispatches knowledge_search locally to the KnowledgeClient (not over MCP)", async () => {
    process.env["KNOWLEDGE_BASE_ENABLED"] = "true";
    process.env["KNOWLEDGE_BASES"] = JSON.stringify([{ name: "kb-alpha" }]);

    // Add /knowledge/search to the memory stub.
    plane.memory["routes"].set("POST /knowledge/search", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        results: [
          {
            content: "relevant passage",
            documentRef: "doc-1",
            chunkIndex: 0,
            startOffset: 0,
            endOffset: 10,
            mimeType: "text/plain",
            score: 0.9,
          },
        ],
      }),
    }));

    const { KnowledgeClient } = await import("../src/knowledge.js");
    const config = { ...plane.config, knowledgeEnabled: true, memoryBaseUrl: plane.memory.baseUrl } as typeof plane.config;
    const kc = new KnowledgeClient(config);
    const client = new ToolsClient(config, kc);

    // The knowledge_search tool is in the list.
    const tools = await client.list();
    const ksTool = tools.find((t) => t.name === KNOWLEDGE_SEARCH_TOOL_NAME);
    expect(ksTool).toBeDefined();
    expect(ksTool?.transport).toBe("internal");

    // call() routes it locally.
    const results = await client.call(KNOWLEDGE_SEARCH_TOOL_NAME, { query: "climate", knowledge_base: "kb-alpha" });
    expect(Array.isArray(results)).toBe(true);
    // Confirm NO MCP call was made.
    expect(plane.discovery.mcpCalls).toHaveLength(0);

    // The /knowledge/search endpoint was hit (not the discovery MCP).
    const ksReq = plane.memory.requests.find((r) => r.path === "/knowledge/search");
    expect(ksReq).toBeDefined();
  });
});

// ── synthetic tools in list() ────────────────────────────────────────────────

describe("ToolsClient.list — synthetic tools", () => {
  it("does NOT include delegate_to or handoff_to when DELEGATE_ENABLED is unset", async () => {
    const client = new ToolsClient(plane.config);
    const tools = await client.list();
    const names = tools.map((t) => t.name);
    expect(names).not.toContain(DELEGATE_TOOL_NAME);
    expect(names).not.toContain(HANDOFF_TOOL_NAME);
  });

  it("includes delegate_to and handoff_to when DELEGATE_ENABLED=true", async () => {
    process.env["DELEGATE_ENABLED"] = "true";
    process.env["DELEGATE_ROSTER"] = JSON.stringify([
      { name: "researcher", description: "searches the web" },
      { name: "coder", description: "writes code" },
    ]);

    const client = new ToolsClient(plane.config);
    const tools = await client.list();
    const names = tools.map((t) => t.name);
    expect(names).toContain(DELEGATE_TOOL_NAME);
    expect(names).toContain(HANDOFF_TOOL_NAME);
    expect(names).toContain(DiscoveryStub.CATALOG_NAME); // MCP tools still present

    const dt = tools.find((t) => t.name === DELEGATE_TOOL_NAME)!;
    expect(dt.mode).toBe("delegate");
    const dtSchema = dt.inputSchema!;
    expect((dtSchema["properties"] as Record<string, unknown>)["sub_agent"]).toMatchObject({
      enum: ["researcher", "coder"],
    });
    expect(dtSchema["required"]).toEqual(["sub_agent", "task"]);
    expect(dt.description).toContain("researcher: searches the web");

    const ht = tools.find((t) => t.name === HANDOFF_TOOL_NAME)!;
    expect(ht.mode).toBe("handoff");
    const htSchema = ht.inputSchema!;
    expect((htSchema["properties"] as Record<string, unknown>)["target_agent"]).toMatchObject({
      enum: ["researcher", "coder"],
    });
    // include_history (m83.6) is exposed to the model as an optional boolean (default true).
    expect((htSchema["properties"] as Record<string, unknown>)["include_history"]).toMatchObject({
      type: "boolean",
    });
    expect(htSchema["required"]).toEqual(["target_agent"]);
  });

  it("does NOT include knowledge_search when KNOWLEDGE_BASE_ENABLED is unset", async () => {
    const client = new ToolsClient(plane.config);
    const tools = await client.list();
    expect(tools.map((t) => t.name)).not.toContain(KNOWLEDGE_SEARCH_TOOL_NAME);
  });

  it("includes knowledge_search when KNOWLEDGE_BASE_ENABLED=true and KNOWLEDGE_BASES non-empty", async () => {
    process.env["KNOWLEDGE_BASE_ENABLED"] = "true";
    process.env["KNOWLEDGE_BASES"] = JSON.stringify([
      { name: "docs" },
      { name: "wiki" },
    ]);

    const client = new ToolsClient(plane.config);
    const tools = await client.list();
    const ksTool = tools.find((t) => t.name === KNOWLEDGE_SEARCH_TOOL_NAME);
    expect(ksTool).toBeDefined();
    expect(ksTool?.transport).toBe("internal");
    expect(ksTool?.description).toContain("docs");
    expect(ksTool?.description).toContain("wiki");
  });
});

// ── Client.tools wiring ───────────────────────────────────────────────────────

describe("Client.tools wiring", () => {
  it("exposes tools on the Client facade", async () => {
    const { Client } = await import("../src/client.js");
    const client = new Client(plane.config);
    expect(client.tools).toBeInstanceOf(ToolsClient);
  });

  it("client.tools.list() works via the facade", async () => {
    const { Client } = await import("../src/client.js");
    const client = new Client(plane.config);
    const tools = await client.tools.list();
    expect(tools.length).toBeGreaterThan(0);
  });
});
