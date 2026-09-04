/**
 * ModelClient tests — parity with `sdk/python/tests/test_model.py`.
 *
 * Exercises the /chat/completions wire contract: text/usage/toolCalls parsing,
 * capability relay, and error paths (402 budget, 403 guardrail, 502 upstream).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as capMod from "../src/_capability.js";
import * as recordMod from "../src/_record.js";
import { ConfigError, EndpointError, GuardrailBlockedError } from "../src/errors.js";
import { ChatResponse, ModelClient } from "../src/model.js";
import { GatewayStub, startPlane, type MockPlane } from "../src/testing.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
  vi.restoreAllMocks();
});

describe("ModelClient.chat — happy path", () => {
  it("returns a ChatResponse with text, usage, and model", async () => {
    const client = new ModelClient(plane.config);
    const resp = await client.chat("gpt-4o-mini", [
      { role: "user", content: "hello" },
    ]);

    expect(resp).toBeInstanceOf(ChatResponse);
    expect(resp.text).toBe("the answer is 42");
    expect(resp.model).toBe("gpt-4o-mini");
    expect(resp.usage.promptTokens).toBe(11);
    expect(resp.usage.completionTokens).toBe(5);
    expect(resp.usage.totalTokens).toBe(16);
    expect(resp.hasToolCalls).toBe(false);
    expect(resp.toolCalls).toEqual([]);
  });

  it("sends the correct body and Authorization header", async () => {
    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", [{ role: "user", content: "hi" }], {
      temperature: 0.5,
    });

    const req = plane.gateway.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/chat/completions");
    expect(req?.json()).toMatchObject({
      model: "gpt-4o-mini",
      messages: [{ role: "user", content: "hi" }],
      temperature: 0.5,
    });
    // Authorization header must be present with Bearer scheme.
    expect(req?.headers.authorization).toMatch(/^Bearer /);
  });

  it("message returns choices[0].message", async () => {
    const client = new ModelClient(plane.config);
    const resp = await client.chat("gpt-4o-mini", []);
    expect(resp.message).toMatchObject({ role: "assistant", content: "the answer is 42" });
  });

  it("toString() returns the text", async () => {
    const client = new ModelClient(plane.config);
    const resp = await client.chat("gpt-4o-mini", []);
    expect(String(resp)).toBe("the answer is 42");
  });
});

describe("ModelClient.chat — tool calls", () => {
  it("parses tool_calls from the assistant turn", async () => {
    const toolCallBody = {
      id: "chatcmpl-tool",
      model: "gpt-4o-mini",
      choices: [
        {
          index: 0,
          message: {
            role: "assistant",
            content: null,
            tool_calls: [
              {
                id: "call-1",
                type: "function",
                function: { name: "search", arguments: '{"q":"weather"}' },
              },
            ],
          },
        },
      ],
      usage: { prompt_tokens: 10, completion_tokens: 20, total_tokens: 30 },
    };

    // Use a custom gateway stub for this test.
    const gw = new GatewayStub({ content: undefined });
    // Patch the route to return tool call response.
    gw["routes"].set("POST /chat/completions", () => ({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(toolCallBody),
    }));
    await gw.start();

    const config = {
      ...plane.config,
      modelGatewayUrl: gw.baseUrl,
    } as typeof plane.config;

    const client = new ModelClient(config);
    const resp = await client.chat("gpt-4o-mini", []);

    expect(resp.text).toBe("");
    expect(resp.hasToolCalls).toBe(true);
    expect(resp.toolCalls).toHaveLength(1);
    expect(resp.toolCalls[0]).toMatchObject({
      id: "call-1",
      type: "function",
      function: { name: "search", arguments: '{"q":"weather"}' },
    });
    // message includes tool_calls for re-sending to the model.
    expect(resp.message).toMatchObject({ role: "assistant", content: null });

    await gw.stop();
  });
});

describe("ModelClient.chat — capability relay", () => {
  it("relays X-Ctxmesh-Run-Capability when currentCapability returns a value", async () => {
    vi.spyOn(capMod, "currentCapability").mockReturnValue("cap-model-token");

    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", []);

    const req = plane.gateway.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBe("cap-model-token");
  });

  it("does NOT relay capability header when currentCapability returns undefined", async () => {
    // Default stub returns undefined.
    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", []);

    const req = plane.gateway.requests[0];
    expect(req?.headers["x-ctxmesh-run-capability"]).toBeUndefined();
  });
});

describe("ModelClient.chat — record-mode relay (M78, ADR 0071)", () => {
  it("relays X-Ctxmesh-Record when the run is being recorded", async () => {
    vi.spyOn(recordMod, "currentRecordRunId").mockReturnValue("run-rec-42");

    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", []);

    const req = plane.gateway.requests[0];
    expect(req?.headers["x-ctxmesh-record"]).toBe("run-rec-42");
  });

  it("does NOT relay the record header for a non-recorded run", async () => {
    // Default (no recordScope active) ⇒ undefined.
    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", []);

    const req = plane.gateway.requests[0];
    expect(req?.headers["x-ctxmesh-record"]).toBeUndefined();
  });
});

describe("ModelClient.chat — error paths", () => {
  it("throws ConfigError when modelGatewayUrl is empty", async () => {
    const config = { ...plane.config, modelGatewayUrl: "" } as typeof plane.config;
    const client = new ModelClient(config);
    await expect(client.chat("gpt-4o-mini", [])).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError when messages is not an array", async () => {
    const client = new ModelClient(plane.config);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    await expect(client.chat("gpt-4o-mini", "not an array" as any)).rejects.toBeInstanceOf(
      ConfigError,
    );
  });

  it("throws EndpointError with status 402 for a budget-exceeded response", async () => {
    const gw = new GatewayStub({ forceStatus: 402 });
    await gw.start();

    const config = {
      ...plane.config,
      modelGatewayUrl: gw.baseUrl,
    } as typeof plane.config;

    const client = new ModelClient(config);
    const err = await client.chat("gpt-4o-mini", []).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(402);

    await gw.stop();
  });

  it("throws EndpointError with status 502 for an upstream error", async () => {
    const gw = new GatewayStub({ forceStatus: 502 });
    await gw.start();

    const config = {
      ...plane.config,
      modelGatewayUrl: gw.baseUrl,
    } as typeof plane.config;

    const client = new ModelClient(config);
    const err = await client.chat("gpt-4o-mini", []).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(502);

    await gw.stop();
  });

  it("throws GuardrailBlockedError for a guardrail-blocked 403", async () => {
    const gw = new GatewayStub();
    gw["routes"].set("POST /chat/completions", () => ({
      status: 403,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        error: {
          type: "guardrail_blocked",
          detector: "pii-detector",
          scan_point: "input",
        },
      }),
    }));
    await gw.start();

    const config = {
      ...plane.config,
      modelGatewayUrl: gw.baseUrl,
    } as typeof plane.config;

    const client = new ModelClient(config);
    const err = await client.chat("gpt-4o-mini", []).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(GuardrailBlockedError);
    expect((err as GuardrailBlockedError).detector).toBe("pii-detector");
    expect((err as GuardrailBlockedError).scanPoint).toBe("input");
    expect((err as GuardrailBlockedError).status).toBe(403);

    await gw.stop();
  });

  it("throws a plain EndpointError for a non-guardrail 403", async () => {
    const gw = new GatewayStub();
    gw["routes"].set("POST /chat/completions", () => ({
      status: 403,
      body: "forbidden",
    }));
    await gw.start();

    const config = {
      ...plane.config,
      modelGatewayUrl: gw.baseUrl,
    } as typeof plane.config;

    const client = new ModelClient(config);
    const err = await client.chat("gpt-4o-mini", []).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect(err).not.toBeInstanceOf(GuardrailBlockedError);

    await gw.stop();
  });

  it("does not leak timeout into the request body", async () => {
    const client = new ModelClient(plane.config);
    await client.chat("gpt-4o-mini", [], { timeout: 5000, temperature: 0.7 });

    const body = plane.gateway.requests[0]?.json() as Record<string, unknown>;
    expect(body.timeout).toBeUndefined();
    expect(body.temperature).toBe(0.7);
  });
});
