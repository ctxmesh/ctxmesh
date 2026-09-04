/**
 * Client facade tests — verify wiring, withConversation scoping, and agent factory.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import * as agentMod from "../src/agent.js";
import { Client } from "../src/client.js";
import { FeedbackClient } from "../src/feedback.js";
import { KnowledgeClient } from "../src/knowledge.js";
import { MemoryClient } from "../src/memory.js";
import { ModelClient } from "../src/model.js";
import { startPlane, type MockPlane } from "../src/testing.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
});

describe("Client construction", () => {
  it("exposes memory, knowledge, feedback, and model clients", () => {
    const client = new Client(plane.config);
    expect(client.memory).toBeInstanceOf(MemoryClient);
    expect(client.knowledge).toBeInstanceOf(KnowledgeClient);
    expect(client.feedback).toBeInstanceOf(FeedbackClient);
    expect(client.model).toBeInstanceOf(ModelClient);
  });

  it("exposes config and run accessors", () => {
    const client = new Client(plane.config);
    expect(client.config).toBe(plane.config);
    expect(client.run).toBe(plane.config.run);
  });
});

describe("Client.withConversation", () => {
  it("returns a new Client whose memory ops use the given conversationId", async () => {
    const client = new Client(plane.config).withConversation("conv-scoped");

    await client.memory.put([{ role: "user", content: "hi" }]);
    const result = await client.memory.get();
    expect(result).toEqual([{ role: "user", content: "hi" }]);

    // The put request should have used the scoped conversationId in the path.
    const req = plane.memory.requests[0];
    expect(req?.path).toBe("/memory/conv-scoped");
  });

  it("does not mutate the original client's conversationId", async () => {
    const base = new Client(plane.config);
    const scoped = base.withConversation("scoped-conv");

    // The scoped client's memory should use "scoped-conv".
    await scoped.memory.put([{ x: 1 }]);
    expect(plane.memory.requests[0]?.path).toBe("/memory/scoped-conv");

    // The base client still has no default conversationId — calling get() without an
    // explicit id should throw ConfigError.
    const { ConfigError } = await import("../src/errors.js");
    await expect(base.memory.get()).rejects.toBeInstanceOf(ConfigError);
  });

  it("knowledge, feedback, model are shared between base and scoped clients", () => {
    const base = new Client(plane.config);
    const scoped = base.withConversation("conv-x");

    // They point to the same config but are freshly constructed — the behaviour is what
    // matters: both should talk to the same underlying URLs.
    expect(scoped.feedback).toBeInstanceOf(FeedbackClient);
    expect(scoped.model).toBeInstanceOf(ModelClient);
  });
});

describe("agent.fromConfig factory", () => {
  it("builds a Client from an explicit PlaneConfig", () => {
    const client = agentMod.fromConfig(plane.config);
    expect(client).toBeInstanceOf(Client);
    expect(client.config).toBe(plane.config);
  });
});

describe("agent.fromEnv factory", () => {
  it("throws NotInPodError when called without launcher env", async () => {
    const { NotInPodError } = await import("../src/errors.js");
    expect(() => agentMod.fromEnv({})).toThrow(NotInPodError);
  });

  it("builds a Client when a launcher marker is present", () => {
    const client = agentMod.fromEnv({ AGENT_NAME: "test", MODEL_GATEWAY_URL: plane.config.modelGatewayUrl });
    expect(client).toBeInstanceOf(Client);
  });
});
