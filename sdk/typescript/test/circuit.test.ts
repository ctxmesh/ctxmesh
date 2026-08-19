/**
 * The per-run tool circuit breaker (O5) — parity with the Python SDK's `_CircuitBreaker` (m65.7).
 * These are internal exports (not on the package surface); the test imports them from the module.
 */

import { afterEach, describe, expect, it, vi } from "vitest";

import { CircuitBreaker, CircuitOpenError, makeBreaker } from "../src/managed.js";

afterEach(() => {
  vi.useRealTimers();
});

describe("makeBreaker (O5)", () => {
  it("returns null when unconfigured (the 'None → unchanged' contract)", () => {
    expect(makeBreaker(null)).toBeNull();
    expect(makeBreaker({})).toBeNull();
    expect(makeBreaker({ toolCall: {} })).toBeNull();
    // A non-positive threshold → disabled → no breaker.
    expect(makeBreaker({ toolCall: { circuitBreaker: { failureThreshold: 0 } } })).toBeNull();
  });

  it("builds a breaker from a valid config", () => {
    const b = makeBreaker({
      toolCall: { circuitBreaker: { failureThreshold: 3, cooldownSeconds: 60 } },
    });
    expect(b).toBeInstanceOf(CircuitBreaker);
  });
});

describe("CircuitBreaker state machine (O5)", () => {
  it("a non-positive threshold is disabled — always allows", () => {
    const b = new CircuitBreaker(0, 1000);
    b.recordFailure("x");
    b.recordFailure("x");
    expect(b.allow("x")).toBe(true);
  });

  it("opens after `threshold` consecutive failures, then short-circuits", () => {
    const b = new CircuitBreaker(3, 60_000);
    expect(b.allow("t")).toBe(true);
    b.recordFailure("t");
    b.recordFailure("t");
    expect(b.allow("t")).toBe(true); // still closed at 2/3
    b.recordFailure("t"); // 3rd → open
    expect(b.allow("t")).toBe(false);
  });

  it("a success resets the failure count (stays closed)", () => {
    const b = new CircuitBreaker(2, 60_000);
    b.recordFailure("t");
    b.recordSuccess("t"); // reset
    b.recordFailure("t");
    expect(b.allow("t")).toBe(true); // only 1 failure since the reset
  });

  it("half-open after cooldown: one probe admitted; a probe failure re-opens; a probe success closes", () => {
    vi.useFakeTimers();
    const b = new CircuitBreaker(1, 1000); // open on 1 failure, 1s cooldown
    b.recordFailure("t");
    expect(b.allow("t")).toBe(false); // open, still cooling down

    vi.advanceTimersByTime(1001);
    expect(b.allow("t")).toBe(true); // half-open: one probe admitted
    b.recordFailure("t"); // the probe FAILED → re-open with a fresh cooldown
    expect(b.allow("t")).toBe(false);

    vi.advanceTimersByTime(1001);
    expect(b.allow("t")).toBe(true); // another probe
    b.recordSuccess("t"); // the probe SUCCEEDED → closed
    expect(b.allow("t")).toBe(true);
  });

  it("isolates state per tool name", () => {
    const b = new CircuitBreaker(1, 60_000);
    b.recordFailure("a");
    expect(b.allow("a")).toBe(false); // a is open
    expect(b.allow("b")).toBe(true); // b is unaffected
  });
});

describe("CircuitOpenError (O5)", () => {
  it("carries the tool name", () => {
    const e = new CircuitOpenError("flaky");
    expect(e).toBeInstanceOf(Error);
    expect(e.toolName).toBe("flaky");
    expect(e.message).toContain("flaky");
  });
});
