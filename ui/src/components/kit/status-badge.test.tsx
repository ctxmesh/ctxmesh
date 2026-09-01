// status-badge.test.tsx — the colour doctrine, pinned (M151).
//
// This map is the most semantically load-bearing line in the console: it is
// where a state becomes a colour. It shipped for ~60 milestones with no test,
// which is how `waiting` — "a HUMAN must act", the single most important state
// the product has — spent all of them wearing the same amber as "you are near a
// quota". ADR 0128 fixed the colour; this file makes the fix permanent.
//
// These assertions are deliberately about MEANING, not appearance. They name
// variants, never class strings, so restyling a chip does not break them but
// re-pointing a state at the wrong hue does.

import { describe, expect, it } from "vitest";
import { resolveStatus } from "./status-badge";

describe("the tone a state resolves to", () => {
  it("routes anything needing a person to `waiting`, even when the resource is otherwise ready", () => {
    // The override matters: an agent can be Ready AND blocked on an approval.
    // If readiness won, the queue that exists to surface human gates would show
    // a green chip on the row that needs you most.
    for (const reason of [
      "awaiting approval",
      "GatePassedAwaitingApproval",
      "requires_action",
      "held",
      "hitl",
      "human review",
      "awaiting promotion",
    ]) {
      expect(resolveStatus(true, "Serving", reason).tone, reason).toBe("waiting");
    }
  });

  it("routes converging phases to `progressing`, not to a problem", () => {
    for (const phase of ["Pending", "Provisioning", "BuildingRevision", "Reconciling", "Queued"]) {
      expect(resolveStatus(false, phase).tone, phase).toBe("progressing");
    }
  });

  it("routes a named not-ready state that is not converging to `failed`", () => {
    for (const reason of ["RevisionFailed", "ImagePullBackOff", "InvalidPattern", "DanglingEdge"]) {
      expect(resolveStatus(false, "NotReady", reason).tone, reason).toBe("failed");
    }
  });

  it("treats no signal at all as converging rather than failed", () => {
    expect(resolveStatus(false).tone).toBe("progressing");
  });

  it("routes draft/disabled/paused to `draft` — off is not a problem", () => {
    for (const phase of ["Draft", "Disabled", "Paused"]) {
      expect(resolveStatus(false, phase).tone, phase).toBe("draft");
    }
  });
});

describe("the variant a tone renders as (ADR 0128)", () => {
  const cases: [string, ReturnType<typeof resolveStatus>["variant"]][] = [
    ["ready", "ok"],
    ["progressing", "progressing"],
    ["waiting", "hold"],
    ["failed", "crit"],
    ["draft", "muted"],
  ];

  it.each(cases)("%s renders as %s", (tone, variant) => {
    const probe: Record<string, ReturnType<typeof resolveStatus>> = {
      ready: resolveStatus(true, "Ready"),
      progressing: resolveStatus(false, "Pending"),
      waiting: resolveStatus(true, "Serving", "awaiting approval"),
      failed: resolveStatus(false, "NotReady", "RevisionFailed"),
      draft: resolveStatus(false, "Draft"),
    };
    expect(probe[tone].tone).toBe(tone);
    expect(probe[tone].variant).toBe(variant);
  });

  it("never renders a status in the brand variant", () => {
    // Pine means "you can act here, and this is us". A status is not an action.
    // The rule predates the re-brand — it was written for the old purple — and
    // survives it. `default` is the brand variant.
    const every = [
      resolveStatus(true, "Ready"),
      resolveStatus(false, "Pending"),
      resolveStatus(true, "Serving", "awaiting approval"),
      resolveStatus(false, "NotReady", "RevisionFailed"),
      resolveStatus(false, "Draft"),
      resolveStatus(false),
    ];
    for (const r of every) {
      expect(r.variant).not.toBe("default");
      expect(r.variant).not.toBe("primary");
    }
  });

  it("keeps `waiting` and `warn` distinct — a human gate is not a quota warning", () => {
    // The whole point of ADR 0128. If these ever collapse onto one hue again,
    // "act now" and "look into it eventually" become indistinguishable, and the
    // attention-first sort the console is built around stops meaning anything.
    const humanGate = resolveStatus(true, "Serving", "awaiting approval");
    expect(humanGate.variant).toBe("hold");
    expect(humanGate.variant).not.toBe("warn");
    expect(humanGate.variant).not.toBe("warning");
  });
});
