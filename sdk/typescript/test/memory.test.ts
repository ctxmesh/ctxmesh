/**
 * MemoryClient tests — parity with `sdk/python/tests/test_memory.py`.
 *
 * Exercises the :2998 wire contract (get/put/append/search) plus the long-term
 * (agent-scope, ADR 0045) paths. Tests use the mock plane from plane.ts so no
 * live launcher is required.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as capMod from "../src/_capability.js";
import { ConfigError, EndpointError } from "../src/errors.js";
import { MemoryClient } from "../src/memory.js";
import { startPlane, type MockPlane } from "./plane.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
  vi.restoreAllMocks();
});

// ── conversation memory (get/put/append/search) ───────────────────────────────

describe("MemoryClient.get", () => {
  it("GET /memory/{id} returns an empty list when nothing stored", async () => {
    const client = new MemoryClient(plane.config);
    const result = await client.get("conv-1");
    expect(result).toEqual([]);

    const req = plane.memory.requests[0];
    expect(req?.method).toBe("GET");
    expect(req?.path).toBe("/memory/conv-1");
  });

  it("returns the stored entries after put", async () => {
    const client = new MemoryClient(plane.config);
    await client.put([{ role: "user", content: "hello" }], "conv-2");
    const result = await client.get("conv-2");
    expect(result).toEqual([{ role: "user", content: "hello" }]);
  });

  it("uses the client's default conversationId when none passed", async () => {
    const client = new MemoryClient(plane.config, "default-conv");
    await client.put([{ x: 1 }]);
    const result = await client.get();
    expect(result).toEqual([{ x: 1 }]);
  });

  it("throws ConfigError when memory is not wired", async () => {
    const client = new MemoryClient(
      { ...plane.config, memoryWired: false } as typeof plane.config,
    );
    await expect(client.get("conv-1")).rejects.toBeInstanceOf(ConfigError);
  });
});

describe("MemoryClient.put", () => {
  it("PUT /memory/{id} responds 204 and replaces the store", async () => {
    const client = new MemoryClient(plane.config);
    await client.put([{ role: "assistant", content: "hi" }], "conv-3");

    const req = plane.memory.requests[0];
    expect(req?.method).toBe("PUT");
    expect(req?.path).toBe("/memory/conv-3");
    expect(req?.json()).toEqual([{ role: "assistant", content: "hi" }]);
  });

  it("throws ConfigError if entries is not an array", async () => {
    const client = new MemoryClient(plane.config);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    await expect(client.put("not-an-array" as any, "conv-1")).rejects.toBeInstanceOf(ConfigError);
  });
});

describe("MemoryClient.append", () => {
  it("POST /memory/{id}/append responds 204", async () => {
    const client = new MemoryClient(plane.config);
    await client.append({ role: "user", content: "world" }, "conv-4");

    const req = plane.memory.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/memory/conv-4/append");
    expect(req?.json()).toEqual({ role: "user", content: "world" });
    // No X-Message-Id header when none passed.
    expect(req?.headers["x-message-id"]).toBeUndefined();
  });

  it("relays X-Message-Id header when messageId is provided", async () => {
    const client = new MemoryClient(plane.config);
    await client.append({ x: 1 }, "conv-5", "msg-abc");

    const req = plane.memory.requests[0];
    expect(req?.headers["x-message-id"]).toBe("msg-abc");
  });
});

describe("MemoryClient session memory forwards the run capability (M98, EU1a)", () => {
  it("get/put/append/search all relay X-Ctxmesh-Run-Capability when a cap is bound", async () => {
    // The launcher user-scopes per-user session memory off the forwarded run capability (ADR 0080);
    // when perUser is off it strips it, so the relay is always safe.
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-session-token");
    const client = new MemoryClient(plane.config);

    await client.get("c1");
    await client.put([{ role: "user", content: "a" }], "c1");
    await client.append({ role: "user", content: "b" }, "c1");
    await client.search("b", "c1");

    expect(plane.memory.requests.length).toBe(4);
    for (const req of plane.memory.requests) {
      expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-session-token");
    }
  });

  it("omits the capability header when none is bound (async/eventing turn)", async () => {
    const client = new MemoryClient(plane.config);
    await client.append({ role: "user", content: "x" }, "c2");
    const req = plane.memory.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBeUndefined();
  });
});

describe("MemoryClient.search", () => {
  it("GET /memory/{id}/search?q= returns matching entries", async () => {
    const client = new MemoryClient(plane.config);
    await client.put(
      [{ text: "needle in a haystack" }, { text: "nothing" }],
      "conv-6",
    );
    const results = await client.search("needle", "conv-6");
    expect(results).toHaveLength(1);
    expect((results[0] as Record<string, string>).text).toBe("needle in a haystack");

    const req = plane.memory.requests[1]; // second request is the search
    expect(req?.method).toBe("GET");
    expect(req?.query.get("q")).toBe("needle");
  });

  it("empty query returns all entries", async () => {
    const client = new MemoryClient(plane.config);
    await client.put([{ a: 1 }, { b: 2 }], "conv-7");
    const results = await client.search("", "conv-7");
    expect(results).toHaveLength(2);
  });
});

describe("MemoryClient conversationId validation", () => {
  it("throws ConfigError for an empty conversationId", async () => {
    const client = new MemoryClient(plane.config);
    await expect(client.get("")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId that is too long", async () => {
    const client = new MemoryClient(plane.config);
    const longId = "x".repeat(129);
    await expect(client.get(longId)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with a slash", async () => {
    const client = new MemoryClient(plane.config);
    await expect(client.get("bad/id")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with a colon", async () => {
    const client = new MemoryClient(plane.config);
    await expect(client.get("bad:id")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with whitespace", async () => {
    const client = new MemoryClient(plane.config);
    await expect(client.get("bad id")).rejects.toBeInstanceOf(ConfigError);
  });
});

// ── long-term memory (ADR 0045) ──────────────────────────────────────────────

describe("MemoryClient long-term memory", () => {
  it("throws ConfigError when longtermWired is false", async () => {
    const client = new MemoryClient(plane.config); // forTest defaults longtermWired=false
    await expect(client.remember("some content")).rejects.toBeInstanceOf(ConfigError);
    await expect(client.searchAgent("query")).rejects.toBeInstanceOf(ConfigError);
  });

  it("remember requires non-empty content", async () => {
    // ConfigError is raised before the HTTP call, so no stub routes needed.
    const longtermPlane = await startPlane();
    try {
      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      await expect(client.remember("")).rejects.toBeInstanceOf(ConfigError);
      await expect(client.remember("   ")).rejects.toBeInstanceOf(ConfigError);
    } finally {
      await longtermPlane.stop();
    }
  });

  it("remember POST /memory/agent/remember with 202", async () => {
    const longtermPlane = await startPlane();
    try {
      const stub = longtermPlane.memory;
      // normalisePath maps /memory/agent/remember → /memory/{id}/remember
      stub["routes"].set("POST /memory/{id}/remember", () => ({ status: 202 }));

      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      await client.remember("Important fact", { topic: "finance" });

      const req = stub.requests[0];
      expect(req?.method).toBe("POST");
      expect(req?.path).toBe("/memory/agent/remember");
      expect(req?.json()).toMatchObject({ content: "Important fact", tags: { topic: "finance" } });
    } finally {
      await longtermPlane.stop();
    }
  });

  it("searchAgent POST /memory/agent/search returns results", async () => {
    const longtermPlane = await startPlane();
    try {
      const stub = longtermPlane.memory;
      // normalisePath maps /memory/agent/search → /memory/{id}/search
      stub["routes"].set("POST /memory/{id}/search", () => ({
        status: 200,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ results: [{ content: "fact", score: 0.85 }] }),
      }));

      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      const results = await client.searchAgent("finance facts", 3, 0.5);

      expect(results).toEqual([{ content: "fact", score: 0.85 }]);
      const req = stub.requests[0];
      expect(req?.json()).toMatchObject({ query: "finance facts", topK: 3, threshold: 0.5 });
    } finally {
      await longtermPlane.stop();
    }
  });

  it("remember relays X-Ctxmesh-Run-Capability when currentCapability returns a value", async () => {
    // Inject a mock capability value to verify the relay plumbing is present.
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-token-xyz");

    const longtermPlane = await startPlane();
    try {
      const stub = longtermPlane.memory;
      stub["routes"].set("POST /memory/{id}/remember", () => ({ status: 202 }));

      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      await client.remember("fact to relay");

      const req = stub.requests[0];
      expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-token-xyz");
    } finally {
      await longtermPlane.stop();
    }
  });

  it("searchAgent relays X-Ctxmesh-Run-Capability when injected", async () => {
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-search-token");

    const longtermPlane = await startPlane();
    try {
      const stub = longtermPlane.memory;
      stub["routes"].set("POST /memory/{id}/search", () => ({
        status: 200,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ results: [] }),
      }));

      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      await client.searchAgent("anything");

      const req = stub.requests[0];
      expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-search-token");
    } finally {
      await longtermPlane.stop();
    }
  });

  it("does NOT relay capability header when currentCapability returns undefined", async () => {
    // Default stub returns undefined — no header should appear.
    const longtermPlane = await startPlane();
    try {
      const stub = longtermPlane.memory;
      stub["routes"].set("POST /memory/{id}/remember", () => ({ status: 202 }));

      const config = {
        ...longtermPlane.config,
        longtermWired: true,
        memoryBaseUrl: longtermPlane.memory.baseUrl,
      } as typeof longtermPlane.config;

      const client = new MemoryClient(config);
      await client.remember("no cap test");

      const req = stub.requests[0];
      expect(req?.headers["x-ctxmesh-run-capability"]).toBeUndefined();
    } finally {
      await longtermPlane.stop();
    }
  });
});

// ── EndpointError on non-2xx ──────────────────────────────────────────────────

describe("MemoryClient error handling", () => {
  it("throws EndpointError when the stub returns a non-200 on get", async () => {
    // Patch the route to return 500.
    plane.memory["routes"].set("GET /memory/{id}", () => ({ status: 500, body: "oops" }));
    const client = new MemoryClient(plane.config);
    const err = await client.get("conv-err").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(500);
  });
});
