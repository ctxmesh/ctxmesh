/**
 * KnowledgeClient tests — parity with `sdk/python/tests/test_knowledge.py`.
 *
 * Exercises the /knowledge/search wire contract and gating logic.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { ConfigError, EndpointError } from "../src/errors.js";
import { KnowledgeClient } from "../src/knowledge.js";
import { startPlane, type MockPlane } from "../src/testing.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
});

// Add a /knowledge/search route to the memory stub (it listens on :2998).
function addKnowledgeRoute(
  stub: MockPlane["memory"],
  response: { status: number; body?: unknown },
): void {
  stub["routes"].set("POST /knowledge/search", () => {
    if (typeof response.body === "undefined") {
      return { status: response.status };
    }
    return {
      status: response.status,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(response.body),
    };
  });
}

describe("KnowledgeClient.available", () => {
  it("returns empty list when KNOWLEDGE_BASES is not set", () => {
    const client = new KnowledgeClient(plane.config);
    expect(client.available()).toEqual([]);
  });

  it("does not require knowledgeEnabled to call available()", () => {
    // knowledgeEnabled=false (forTest default), but available() still works.
    const client = new KnowledgeClient(plane.config);
    // No throw.
    expect(() => client.available()).not.toThrow();
  });
});

describe("KnowledgeClient.search gating", () => {
  it("throws ConfigError when knowledgeEnabled is false", async () => {
    // forTest defaults knowledgeEnabled=false
    const client = new KnowledgeClient(plane.config);
    await expect(client.search("hello")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for an empty query", async () => {
    const config = { ...plane.config, knowledgeEnabled: true } as typeof plane.config;
    const client = new KnowledgeClient(config);
    await expect(client.search("")).rejects.toBeInstanceOf(ConfigError);
    await expect(client.search("   ")).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError when knowledgeBase is undefined and roster is empty", async () => {
    const config = { ...plane.config, knowledgeEnabled: true } as typeof plane.config;
    const client = new KnowledgeClient(config);
    // KNOWLEDGE_BASES unset → empty roster → ConfigError.
    await expect(client.search("anything")).rejects.toBeInstanceOf(ConfigError);
  });
});

describe("KnowledgeClient.search HTTP contract", () => {
  it("POST /knowledge/search with the correct body and returns results", async () => {
    addKnowledgeRoute(plane.memory, {
      status: 200,
      body: {
        results: [
          {
            content: "chunk text",
            documentRef: "doc-1",
            chunkIndex: 0,
            startOffset: 0,
            endOffset: 10,
            mimeType: "text/plain",
            score: 0.92,
          },
        ],
      },
    });

    const config = {
      ...plane.config,
      knowledgeEnabled: true,
      memoryBaseUrl: plane.memory.baseUrl,
    } as typeof plane.config;

    const client = new KnowledgeClient(config);
    const results = await client.search("find this", "kb-alpha", 5, 0.5);

    expect(results).toHaveLength(1);
    expect(results[0]?.content).toBe("chunk text");
    expect(results[0]?.score).toBe(0.92);

    const req = plane.memory.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/knowledge/search");
    expect(req?.json()).toMatchObject({
      knowledgeBase: "kb-alpha",
      query: "find this",
      topK: 5,
      threshold: 0.5,
    });
  });

  it("returns empty array when results key is absent", async () => {
    addKnowledgeRoute(plane.memory, { status: 200, body: {} });

    const config = {
      ...plane.config,
      knowledgeEnabled: true,
      memoryBaseUrl: plane.memory.baseUrl,
    } as typeof plane.config;

    const client = new KnowledgeClient(config);
    const results = await client.search("q", "kb-beta");
    expect(results).toEqual([]);
  });

  it("throws EndpointError on non-200", async () => {
    addKnowledgeRoute(plane.memory, { status: 503, body: "unavailable" });

    const config = {
      ...plane.config,
      knowledgeEnabled: true,
      memoryBaseUrl: plane.memory.baseUrl,
    } as typeof plane.config;

    const client = new KnowledgeClient(config);
    const err = await client.search("q", "kb-gamma").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(503);
  });

  it("uses default topK=10 and threshold=0.0", async () => {
    addKnowledgeRoute(plane.memory, { status: 200, body: { results: [] } });

    const config = {
      ...plane.config,
      knowledgeEnabled: true,
      memoryBaseUrl: plane.memory.baseUrl,
    } as typeof plane.config;

    const client = new KnowledgeClient(config);
    await client.search("q", "kb-delta");

    const req = plane.memory.requests[0];
    expect(req?.json()).toMatchObject({ topK: 10, threshold: 0.0 });
  });
});
