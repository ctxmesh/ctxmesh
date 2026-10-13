/**
 * KnowledgeClient tests — parity with `sdk/python/tests/test_knowledge.py`.
 *
 * Exercises the /knowledge/search wire contract and gating logic against the fake generated from
 * sdk/launcher-routes.json, so a body the launcher would not accept fails here.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { PlaneConfig } from "../src/config.js";
import { ConfigError, EndpointError } from "../src/errors.js";
import { KnowledgeClient } from "../src/knowledge.js";
import { FakeLauncher } from "./generated/fake-launcher.js";

const ROUTE = "POST /knowledge/search";

let fake: FakeLauncher;

beforeEach(async () => {
  fake = await FakeLauncher.start();
});

afterEach(async () => {
  await fake.stop();
  fake.assertConforms();
});

function enabled(): KnowledgeClient {
  return new KnowledgeClient(PlaneConfig.forTest({ memoryBaseUrl: fake.baseUrl, knowledgeEnabled: true }));
}

describe("KnowledgeClient.available", () => {
  it("returns empty list when KNOWLEDGE_BASES is not set", () => {
    const client = new KnowledgeClient(PlaneConfig.forTest());
    expect(client.available()).toEqual([]);
  });

  it("does not require knowledgeEnabled to call available()", () => {
    // knowledgeEnabled=false (forTest default), but available() still works.
    const client = new KnowledgeClient(PlaneConfig.forTest());
    expect(() => client.available()).not.toThrow();
  });
});

describe("KnowledgeClient.search gating", () => {
  it("throws ConfigError when knowledgeEnabled is false", async () => {
    const client = new KnowledgeClient(PlaneConfig.forTest({ memoryBaseUrl: fake.baseUrl }));
    await expect(client.search("hello")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for an empty query", async () => {
    const client = enabled();
    await expect(client.search("")).rejects.toBeInstanceOf(ConfigError);
    await expect(client.search("   ")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError when knowledgeBase is undefined and roster is empty", async () => {
    // KNOWLEDGE_BASES unset → empty roster → ConfigError.
    await expect(enabled().search("anything")).rejects.toBeInstanceOf(ConfigError);
  });

  it("makes no request when it refuses locally", async () => {
    await enabled().search("").catch(() => undefined);
    expect(fake.received).toHaveLength(0);
  });
});

describe("KnowledgeClient.search HTTP contract", () => {
  it("POST /knowledge/search with the correct body and returns the launcher's results", async () => {
    const results = await enabled().search("find this", "kb-alpha", 5, 0.5);

    const golden = fake.golden(ROUTE).response.body as { results: unknown[] };
    expect(results).toEqual(golden.results);

    const req = fake.received[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/knowledge/search");
    expect(req?.json()).toEqual({ knowledgeBase: "kb-alpha", query: "find this", topK: 5, threshold: 0.5 });
  });

  it("returns an empty array when the launcher finds nothing", async () => {
    fake.respond(ROUTE, 200, { results: [] });
    expect(await enabled().search("q", "kb-beta")).toEqual([]);
  });

  it("throws EndpointError carrying the status of a refusal", async () => {
    fake.useGolden(ROUTE, "refuses-a-knowledge-base-not-granted");
    const err = await enabled().search("q", "payroll").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(403);
  });

  it("throws EndpointError when the token service fails", async () => {
    fake.respond(ROUTE, 502, "knowledge search failed\n");
    const err = await enabled().search("q", "kb-gamma").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(502);
  });

  it("uses default topK=10 and threshold=0.0", async () => {
    await enabled().search("q", "kb-delta");
    expect(fake.received[0]?.json()).toMatchObject({ topK: 10, threshold: 0.0 });
  });
});
