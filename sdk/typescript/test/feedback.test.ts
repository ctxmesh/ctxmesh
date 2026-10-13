/**
 * FeedbackClient tests — parity with `sdk/python/tests/test_feedback.py`.
 *
 * Exercises the :2995 POST /feedback contract against the fake generated from
 * sdk/launcher-routes.json: 202 on a valid score, 400/502 → EndpointError.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { PlaneConfig } from "../src/config.js";
import { ConfigError, EndpointError } from "../src/errors.js";
import { FeedbackClient } from "../src/feedback.js";
import { FakeLauncher } from "./generated/fake-launcher.js";

const ROUTE = "ANY /feedback";

let fake: FakeLauncher;
let client: FeedbackClient;

beforeEach(async () => {
  fake = await FakeLauncher.start();
  client = new FeedbackClient(PlaneConfig.forTest({ feedbackBaseUrl: fake.baseUrl }));
});

afterEach(async () => {
  await fake.stop();
  fake.assertConforms();
});

describe("FeedbackClient.score", () => {
  it("POST /feedback returns on the launcher's 202", async () => {
    await client.score("trace-abc", "quality", 0.8, "looks good");

    const req = fake.received[0];
    expect(req?.method).toBe("POST");
    expect(req?.path).toBe("/feedback");
    expect(req?.json()).toEqual({ traceId: "trace-abc", name: "quality", value: 0.8, comment: "looks good" });
  });

  it("omits the comment field when not provided", async () => {
    await client.score("trace-xyz", "accuracy", 1.0);
    const body = fake.received[0]?.json() as Record<string, unknown>;
    expect(body.comment).toBeUndefined();
  });

  it("throws ConfigError when feedbackWired is false", async () => {
    const unwired = new FeedbackClient(PlaneConfig.forTest({ feedbackBaseUrl: fake.baseUrl, feedbackWired: false }));
    await expect(unwired.score("t", "n", 1)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for an empty traceId", async () => {
    await expect(client.score("", "n", 1)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for a non-numeric value (string)", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    await expect(client.score("t", "n", "bad" as any)).rejects.toBeInstanceOf(ConfigError);
  });

  it("throws ConfigError for NaN value", async () => {
    await expect(client.score("t", "n", NaN)).rejects.toBeInstanceOf(ConfigError);
    expect(fake.received).toHaveLength(0);
  });

  it("throws EndpointError with status 400 when the launcher refuses the score", async () => {
    fake.useGolden(ROUTE, "refuses-a-score-without-a-trace");
    const err = await client.score("trace-400", "name", 1).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(400);
  });

  it("throws EndpointError with status 502 when Langfuse is down", async () => {
    fake.respond(ROUTE, 502, "feedback relay error: Langfuse scores POST returned 503\n");
    const err = await client.score("trace-502", "name", 1).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EndpointError);
    expect((err as EndpointError).status).toBe(502);
  });

  it("accepts integer score values", async () => {
    await expect(client.score("t", "n", 5)).resolves.toBeUndefined();
  });

  it("accepts negative score values", async () => {
    await expect(client.score("t", "n", -1)).resolves.toBeUndefined();
  });
});
