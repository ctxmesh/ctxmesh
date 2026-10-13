/**
 * MemoryClient tests — parity with `sdk/python/tests/test_memory.py`.
 *
 * Exercises the :2998 wire contract (get/put/append/search) plus the long-term (agent-scope, ADR
 * 0045) paths, against the fake generated from sdk/launcher-routes.json. The fake answers with the
 * launcher's proven goldens and records every request that breaks a route's schema; afterEach
 * fails the test on any.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { CAPABILITY_HEADER, capabilityScope } from "../src/_capability.js";
import { PlaneConfig, makeRunContext } from "../src/config.js";
import { ConfigError, EndpointError } from "../src/errors.js";
import { MemoryClient } from "../src/memory.js";
import { FakeLauncher } from "./generated/fake-launcher.js";

const CAP = CAPABILITY_HEADER.toLowerCase();

let fake: FakeLauncher;

beforeEach(async () => {
  fake = await FakeLauncher.start();
});

afterEach(async () => {
  await fake.stop();
  fake.assertConforms();
});

function config(overrides: Parameters<typeof PlaneConfig.forTest>[0] = {}): PlaneConfig {
  return PlaneConfig.forTest({ memoryBaseUrl: fake.baseUrl, ...overrides });
}

function withCapability<T>(cap: string, fn: () => Promise<T>): Promise<T> {
  return capabilityScope({ [CAPABILITY_HEADER]: cap }, fn);
}

// ── conversation memory (get/put/append/search) ───────────────────────────────

describe("MemoryClient.get", () => {
  it("GET /memory/{id} returns the conversation the launcher sends", async () => {
    const result = await new MemoryClient(config()).get("conv-1");
    expect(result).toEqual(fake.golden("GET /memory/{conversationId}").response.body);

    const req = fake.received[0];
    expect(req?.method).toBe("GET");
    expect(req?.path).toBe("/memory/conv-1");
  });

  it("returns an empty list for a conversation never written", async () => {
    fake.useGolden("GET /memory/{conversationId}", "returns-an-empty-list-for-an-unknown-conversation");
    expect(await new MemoryClient(config()).get("never-seen")).toEqual([]);
  });

  it("uses the client's default conversationId when none passed", async () => {
    await new MemoryClient(config(), "default-conv").get();
    expect(fake.received[0]?.path).toBe("/memory/default-conv");
  });

  it("falls back to the run's conversationId", async () => {
    await new MemoryClient(config({ run: makeRunContext({ conversationId: "from-env" }) })).get();
    expect(fake.received[0]?.path).toBe("/memory/from-env");
  });

  it("throws ConfigError when memory is not wired", async () => {
    const client = new MemoryClient(config({ memoryWired: false }));
    await expect(client.get("conv-1")).rejects.toBeInstanceOf(ConfigError);
    expect(fake.received).toHaveLength(0);
  });
});

describe("MemoryClient.put", () => {
  it("PUT /memory/{id} sends the entries as one JSON array", async () => {
    await new MemoryClient(config()).put([{ role: "assistant", content: "hi" }], "conv-3");

    const req = fake.received[0];
    expect(req?.method).toBe("PUT");
    expect(req?.path).toBe("/memory/conv-3");
    expect(req?.json()).toEqual([{ role: "assistant", content: "hi" }]);
  });

  it("throws ConfigError if entries is not an array", async () => {
    const client = new MemoryClient(config());
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    await expect(client.put("not-an-array" as any, "conv-1")).rejects.toBeInstanceOf(ConfigError);
  });

  it("surfaces a stale If-Match refusal as EndpointError 412", async () => {
    fake.useGolden("PUT /memory/{conversationId}", "refuses-a-stale-write");
    const err = await new MemoryClient(config()).put([], "conv-1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(412);
  });
});

describe("MemoryClient.append", () => {
  it("POST /memory/{id}/append sends one JSON value", async () => {
    await new MemoryClient(config()).append({ role: "user", content: "world" }, "conv-4");

    const req = fake.received[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/memory/conv-4/append");
    expect(req?.json()).toEqual({ role: "user", content: "world" });
    // No X-Message-Id header when none passed: the launcher mints one.
    expect(req?.headers["x-message-id"]).toBeUndefined();
  });

  it("relays X-Message-Id header when messageId is provided", async () => {
    await new MemoryClient(config()).append({ x: 1 }, "conv-5", "msg-abc");
    expect(fake.received[0]?.headers["x-message-id"]).toBe("msg-abc");
  });
});

describe("MemoryClient session memory forwards the run capability (M98, EU1a)", () => {
  it("get/put/append/search all relay X-Ctxmesh-Run-Capability when a cap is bound", async () => {
    // The launcher user-scopes per-user session memory off the forwarded run capability (ADR 0080);
    // when perUser is off it strips it, so the relay is always safe.
    const client = new MemoryClient(config());
    await withCapability("cap-session-token", async () => {
      await client.get("c1");
      await client.put([{ role: "user", content: "a" }], "c1");
      await client.append({ role: "user", content: "b" }, "c1");
      await client.search("b", "c1");
    });

    expect(fake.received).toHaveLength(4);
    for (const req of fake.received) {
      expect(req.headers[CAP]).toBe("cap-session-token");
    }
  });

  it("omits the capability header when none is bound (async/eventing turn)", async () => {
    await new MemoryClient(config()).append({ role: "user", content: "x" }, "c2");
    expect(fake.received[0]?.headers[CAP]).toBeUndefined();
  });
});

describe("MemoryClient.search", () => {
  it("GET /memory/{id}/search?q= returns the entries the launcher matched", async () => {
    const results = await new MemoryClient(config()).search("metric", "conv-1");
    expect(results).toEqual(fake.golden("GET /memory/{conversationId}/search").response.body);

    const req = fake.received[0];
    expect(req?.method).toBe("GET");
    expect(req?.path).toBe("/memory/conv-1/search");
    expect(req?.query.get("q")).toBe("metric");
  });

  it("sends an empty q for an empty query, which the launcher treats as match-all", async () => {
    await new MemoryClient(config()).search("", "conv-7");
    expect(fake.received[0]?.query.get("q")).toBe("");
  });
});

describe("MemoryClient conversationId validation", () => {
  it("throws ConfigError for an empty conversationId", async () => {
    await expect(new MemoryClient(config()).get("")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId that is too long", async () => {
    await expect(new MemoryClient(config()).get("x".repeat(129))).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with a slash", async () => {
    await expect(new MemoryClient(config()).get("bad/id")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with a colon", async () => {
    await expect(new MemoryClient(config()).get("bad:id")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a conversationId with whitespace", async () => {
    await expect(new MemoryClient(config()).get("bad id")).rejects.toBeInstanceOf(ConfigError);
  });

  it("refuses locally before the launcher would, so nothing is sent", async () => {
    await new MemoryClient(config()).get("bad:id").catch(() => undefined);
    expect(fake.received).toHaveLength(0);
  });
});

// ── long-term memory (ADR 0045) ──────────────────────────────────────────────

describe("MemoryClient long-term memory", () => {
  const longterm = () => new MemoryClient(config({ longtermWired: true }));

  it("throws ConfigError when longtermWired is false", async () => {
    const client = new MemoryClient(config()); // forTest defaults longtermWired=false
    await expect(client.remember("some content")).rejects.toBeInstanceOf(ConfigError);
    await expect(client.searchAgent("query")).rejects.toBeInstanceOf(ConfigError);
  });

  it("remember requires non-empty content", async () => {
    await expect(longterm().remember("")).rejects.toBeInstanceOf(ConfigError);
    await expect(longterm().remember("   ")).rejects.toBeInstanceOf(ConfigError);
    expect(fake.received).toHaveLength(0);
  });

  it("remember POST /memory/agent/remember with 202", async () => {
    await longterm().remember("Important fact", { topic: "finance" });

    const req = fake.received[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/memory/agent/remember");
    expect(req?.json()).toEqual({ content: "Important fact", tags: { topic: "finance" } });
  });

  it("searchAgent POST /memory/agent/search returns the launcher's results", async () => {
    const results = await longterm().searchAgent("finance facts", 3, 0.5);

    const golden = fake.golden("POST /memory/agent/search").response.body as { results: unknown[] };
    expect(results).toEqual(golden.results);
    expect(fake.received[0]?.json()).toEqual({ query: "finance facts", topK: 3, threshold: 0.5 });
  });

  it("remember relays X-Ctxmesh-Run-Capability when a capability is bound", async () => {
    await withCapability("cap-token-xyz", () => longterm().remember("fact to relay"));
    expect(fake.received[0]?.headers[CAP]).toBe("cap-token-xyz");
  });

  it("searchAgent relays X-Ctxmesh-Run-Capability when a capability is bound", async () => {
    await withCapability("cap-search-token", () => longterm().searchAgent("anything"));
    expect(fake.received[0]?.headers[CAP]).toBe("cap-search-token");
  });

  it("does NOT relay a capability header when none is bound", async () => {
    await longterm().remember("no cap test");
    expect(fake.received[0]?.headers[CAP]).toBeUndefined();
  });

  it("surfaces a refused remember as EndpointError 400", async () => {
    fake.useGolden("POST /memory/agent/remember", "requires-content");
    const err = await longterm().remember("x").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(400);
  });
});

// ── EndpointError on non-2xx ──────────────────────────────────────────────────

describe("MemoryClient error handling", () => {
  it("throws EndpointError when the memory backend fails", async () => {
    fake.respond("GET /memory/{conversationId}", 502, { error: "memory backend: connection refused" });
    const err = await new MemoryClient(config()).get("conv-err").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(502);
  });
});
