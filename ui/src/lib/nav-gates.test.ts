import { describe, expect, it } from "vitest";

import census from "@/test/contract/capabilities.json";
import * as nav from "@/lib/nav";
import { NAV_ITEMS, navCountGate, type NavItem } from "@/lib/nav";

// The capability census: every cell GET /api/capabilities answers, written by the BFF's own
// test (TestCapabilityCensus) from the probe it really runs. The console reads a cell the BFF
// never answered as allowed, so a gate naming one hides from nobody — which is how the
// Approvals and Alerts gates went on showing to callers who could read neither.
const PROBED: Record<string, string[]> = census;

/** Gated nav items whose gate names a cell the census does not hold. */
function unprobedGates(items: NavItem[], probed: Record<string, string[]>): string[] {
  return items
    .filter((i) => i.requiresCapability)
    .filter((i) => !(probed[i.requiresCapability!.resource] ?? []).includes(i.requiresCapability!.verb))
    .map((i) => `${i.id} (${i.requiresCapability!.verb} ${i.requiresCapability!.resource})`);
}

describe("nav gates name only capabilities the BFF probes", () => {
  it("holds for every gated item in the IA", () => {
    expect(NAV_ITEMS.some((i) => i.requiresCapability)).toBe(true);
    expect(unprobedGates(NAV_ITEMS, PROBED)).toEqual([]);
  });

  it("gates the Approvals count on the probed cell its item uses", () => {
    const gate = navCountGate("approvals");
    expect(gate).toEqual({ resource: nav.RES_WORKFLOWS, verb: "list" });
    expect(PROBED[gate!.resource]).toContain(gate!.verb);
  });

  it("names a probed resource in every exported RES_ constant", () => {
    const constants = Object.entries(nav).filter(([name]) => name.startsWith("RES_"));
    expect(constants.length).toBeGreaterThan(0);
    for (const [name, resource] of constants) {
      expect(Object.keys(PROBED), `${name} = "${String(resource)}"`).toContain(resource);
    }
  });

  // The other direction: the census check must catch a gate on a cell nobody probes, and a
  // probe the BFF stops running while a gate still names it.
  it("flags a gate on a cell the BFF does not probe", () => {
    const stray: NavItem = {
      ...NAV_ITEMS[0],
      id: "guardrails-gated",
      requiresCapability: { resource: "guardrailpolicies", verb: "list" },
    };
    expect(unprobedGates([...NAV_ITEMS, stray], PROBED)).toEqual([
      "guardrails-gated (list guardrailpolicies)",
    ]);
  });

  it("flags a gate whose probe the BFF dropped", () => {
    const withoutWorkflows = Object.fromEntries(
      Object.entries(PROBED).filter(([resource]) => resource !== nav.RES_WORKFLOWS),
    );
    expect(unprobedGates(NAV_ITEMS, withoutWorkflows)).toEqual(["approvals (list workflows)"]);
  });
});
