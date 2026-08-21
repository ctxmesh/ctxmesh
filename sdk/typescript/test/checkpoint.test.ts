/**
 * L7 checkpoint envelope round-trip + fail-safe verification (ADR 0091, m108.6).
 *
 * The SDK builds the opaque PAYLOAD; the BFF wraps it in a hashed ENVELOPE
 * (internal/run/checkpoint.go). These tests mirror that wrap and prove verifyAndExtract accepts a
 * good envelope and rejects — never throws on — every corrupt form. Parity with Python test_checkpoint.
 */

import { createHash } from "node:crypto";

import { describe, expect, it } from "vitest";

import {
  buildPayload,
  CHECKPOINT_MAX_BYTES,
  PAYLOAD_VERSION,
  verifyAndExtract,
} from "../src/_checkpoint.js";

/** Mirror the BFF's NewSupervisorCheckpoint envelope (internal/run/checkpoint.go). */
function wrap(payload: string): Record<string, unknown> {
  return {
    version: 1,
    kind: "supervisor-loop",
    sha256: createHash("sha256").update(payload, "utf8").digest("hex"),
    payload,
  };
}

function samplePayload(overrides: Record<string, unknown> = {}): string {
  return buildPayload({
    messages: [{ role: "user", content: "hi" }],
    step: 3,
    pending: [{ call_id: "c1", step: "3", sub_agent: "researcher", task: "find it" }],
    tools_called: ["delegate_to"],
    consent_required: [],
    spotlight_token: "tok-123",
    model_index: 2,
    tool_index: 1,
    ...overrides,
  });
}

describe("checkpoint — round-trip through the envelope", () => {
  it("a built payload verifies and restores its fields", () => {
    const fields = verifyAndExtract(wrap(samplePayload()));
    expect(fields).not.toBeNull();
    expect(fields!.v).toBe(PAYLOAD_VERSION);
    expect(fields!.step).toBe(3);
    expect(fields!.spotlight_token).toBe("tok-123");
    expect(fields!.pending[0]!.call_id).toBe("c1");
    expect(fields!.pending[0]!.task).toBe("find it");
    expect(fields!.model_index).toBe(2);
    expect(fields!.tool_index).toBe(1);
    expect(fields!.tools_called).toEqual(["delegate_to"]);
  });

  it("tolerates a stringified envelope (defensive)", () => {
    expect(verifyAndExtract(JSON.stringify(wrap(samplePayload())))).not.toBeNull();
  });
});

describe("checkpoint — fail-safe on every bad envelope", () => {
  const good = wrap(samplePayload());

  it("returns null (never throws) for absent / non-JSON / non-object", () => {
    expect(verifyAndExtract(null)).toBeNull();
    expect(verifyAndExtract(undefined)).toBeNull();
    expect(verifyAndExtract("not json at all")).toBeNull();
    expect(verifyAndExtract(42)).toBeNull();
  });

  it("rejects a wrong kind / version", () => {
    expect(verifyAndExtract({ ...good, kind: "workflow-cursor" })).toBeNull();
    expect(verifyAndExtract({ ...good, version: 2 })).toBeNull();
  });

  it("rejects a hash mismatch (corruption)", () => {
    expect(verifyAndExtract({ ...good, payload: (good["payload"] as string) + " tampered" })).toBeNull();
  });

  it("rejects a missing payload", () => {
    expect(verifyAndExtract({ version: 1, kind: "supervisor-loop", sha256: "x" })).toBeNull();
  });

  it("rejects an unknown payload version", () => {
    const payload = JSON.stringify({ v: 999, messages: [] });
    expect(verifyAndExtract(wrap(payload))).toBeNull();
  });
});

describe("checkpoint — size cap", () => {
  it("sits safely under the BFF's 4 MiB /invoke response limit", () => {
    expect(CHECKPOINT_MAX_BYTES).toBeGreaterThan(0);
    expect(CHECKPOINT_MAX_BYTES).toBeLessThan(4 * 1024 * 1024);
  });
});
