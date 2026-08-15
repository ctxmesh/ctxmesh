/**
 * FeedbackClient tests — parity with `sdk/python/tests/test_feedback.py`.
 *
 * Exercises the :2995 POST /feedback contract: 202 on valid, 400/502 → EndpointError.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { ConfigError, EndpointError } from "../src/errors.js";
import { FeedbackClient } from "../src/feedback.js";
import { startPlane, FeedbackStub, type MockPlane } from "./plane.js";

let plane: MockPlane;

beforeEach(async () => {
  plane = await startPlane();
});

afterEach(async () => {
  await plane.stop();
});

describe("FeedbackClient.score", () => {
  it("POST /feedback returns 202 on a valid score", async () => {
    const client = new FeedbackClient(plane.config);
    await client.score("trace-abc", "quality", 0.8, "looks good");

    const req = plane.feedback.requests[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/feedback");
    expect(req?.json()).toMatchObject({
      traceId: "trace-abc",
      name: "quality",
      value: 0.8,
      comment: "looks good",
    });
  });

  it("omits the comment field when not provided", async () => {
    const client = new FeedbackClient(plane.config);
    await client.score("trace-xyz", "accuracy", 1.0);

    const body = plane.feedback.requests[0]?.json() as Record<string, unknown>;
    expect(body.comment).toBeUndefined();
  });

  it("throws ConfigError when feedbackWired is false", async () => {
    const config = { ...plane.config, feedbackWired: false } as typeof plane.config;
    const client = new FeedbackClient(config);
    await expect(client.score("t", "n", 1)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for an empty traceId", async () => {
    const client = new FeedbackClient(plane.config);
    await expect(client.score("", "n", 1)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a non-numeric value (string)", async () => {
    const client = new FeedbackClient(plane.config);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    await expect(client.score("t", "n", "bad" as any)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for NaN value", async () => {
    const client = new FeedbackClient(plane.config);
    await expect(client.score("t", "n", NaN)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws EndpointError with status 400 when the stub returns 400", async () => {
    // Use a stub that forces 400.
    const feedbackStub400 = new FeedbackStub({ forceStatus: 400 });
    await feedbackStub400.start();

    const config = {
      ...plane.config,
      feedbackBaseUrl: feedbackStub400.baseUrl,
    } as typeof plane.config;

    const client = new FeedbackClient(config);
    const err = await client.score("trace-400", "name", 1).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(400);

    await feedbackStub400.stop();
  });

  it("throws EndpointError with status 502 when Langfuse is down", async () => {
    const feedbackStub502 = new FeedbackStub({ forceStatus: 502 });
    await feedbackStub502.start();

    const config = {
      ...plane.config,
      feedbackBaseUrl: feedbackStub502.baseUrl,
    } as typeof plane.config;

    const client = new FeedbackClient(config);
    const err = await client.score("trace-502", "name", 1).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(502);

    await feedbackStub502.stop();
  });

  it("accepts integer score values", async () => {
    const client = new FeedbackClient(plane.config);
    await expect(client.score("t", "n", 5)).resolves.toBeUndefined();
  });

  it("accepts negative score values", async () => {
    const client = new FeedbackClient(plane.config);
    await expect(client.score("t", "n", -1)).resolves.toBeUndefined();
  });
});
