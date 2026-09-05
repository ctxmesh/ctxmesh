import { describe, expect, it } from "vitest";

import { BUCKETS, bucketOf } from "./lifecycle";

describe("bucketOf", () => {
  it("calls a stopped agent halted, not failing — it did not break", () => {
    expect(bucketOf({ ready: false, phase: "Stopped", reason: "OperatorHalted" })).toBe("halted");
    expect(bucketOf({ ready: true, phase: "Suspended" })).toBe("halted");
  });

  it("puts a human gate above ready — someone is still blocked", () => {
    expect(bucketOf({ ready: true, phase: "Ready", reason: "AwaitingHumanPromotion" })).toBe(
      "held",
    );
  });

  it("calls a failing draft failing — draft is only reached with nothing louder to say", () => {
    expect(bucketOf({ ready: false, phase: "NotReady", reason: "ImagePullBackOff", isDraft: true }))
      .toBe("failing");
    expect(bucketOf({ ready: false, phase: "Pending", isDraft: true })).toBe("draft");
  });

  it("treats a converging phase as coming up, and a terminal one as failing", () => {
    expect(bucketOf({ ready: false, phase: "Provisioning" })).toBe("coming-up");
    expect(bucketOf({ ready: false, phase: "ProvisioningTimeout" })).toBe("failing");
  });

  it("puts a ready agent in serving", () => {
    expect(bucketOf({ ready: true, phase: "Ready" })).toBe("serving");
  });

  it("gives every bucket exactly one entry of render metadata", () => {
    const ids = BUCKETS.map((b) => b.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const b of BUCKETS) expect(b.view).not.toBe("");
  });
});
