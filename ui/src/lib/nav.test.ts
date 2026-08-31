import { describe, expect, it } from "vitest";

import {
  NAV_SECTIONS,
  NAV_ITEMS,
  navRoute,
  navSectionOf,
  FIRST_RUN_CHECKLIST,
} from "@/lib/nav";

// The M151 IA: six sections — Home / Agents / Library / Govern / Activity /
// Admin — replacing Overview / Build / Tools / Observe / Platform / Advanced.
//
// These tests were rewritten, not deleted. Each older assertion is carried
// forward as the same INTENT expressed against the new shape (the m20.8
// CRD-noun demotion, the m23.7 consolidation, the m25 S8 "New agent is a page
// action" rule, the m76.1 catalog taxonomy, the m54.4 checklist derivation),
// plus the two properties the new IA is judged on: nothing was lost, and Govern
// is the fattest section.

const sectionOf = (label: string) =>
  NAV_SECTIONS.find((s) => s.items.some((i) => i.label === label))?.heading;

const labelsOf = (heading: string) =>
  NAV_SECTIONS.find((s) => s.heading === heading)?.items.map((i) => i.label) ?? [];

describe("the six-section IA (M151 §4.2)", () => {
  it("is exactly Home / Agents / Library / Govern / Activity / Admin, in that order", () => {
    expect(NAV_SECTIONS.map((s) => s.heading)).toEqual([
      "Home",
      "Agents",
      "Library",
      "Govern",
      "Activity",
      "Admin",
    ]);
  });

  it("makes Govern the fattest section — governance is the thesis, not a footnote", () => {
    const govern = NAV_SECTIONS.find((s) => s.heading === "Govern")!;
    for (const section of NAV_SECTIONS) {
      if (section.heading === "Govern") continue;
      expect(govern.items.length).toBeGreaterThan(section.items.length);
    }
  });

  it("gives every item a unique id and every routed item a unique route", () => {
    const ids = NAV_ITEMS.map((i) => i.id);
    expect(new Set(ids).size).toBe(ids.length);
    const routes = NAV_ITEMS.map((i) => i.route).filter(Boolean);
    expect(new Set(routes).size).toBe(routes.length);
  });

  it("gives every item an icon (the 1024–1279 rail is icons only)", () => {
    for (const item of NAV_ITEMS) expect(item.icon).toBeTruthy();
  });

  it("navSectionOf resolves the owning section (the breadcrumb's middle crumb)", () => {
    expect(navSectionOf("approvals")).toBe("Govern");
    expect(navSectionOf("runs")).toBe("Activity");
    expect(navSectionOf("does-not-exist")).toBeUndefined();
  });
});

// The re-house is only correct if NOTHING was dropped on the way. Every
// destination the pre-M151 nav carried is listed here by id; each must still be
// reachable from some section.
describe("every pre-M151 destination still lands somewhere", () => {
  const LEGACY_IDS = [
    "topology",
    "providers",
    "agents",
    "teams",
    "gallery",
    "playground",
    "prompts",
    "evals",
    "mcp-servers",
    "mcp-approvals",
    "tool-catalog",
    "runs",
    "my-shares",
    "approvals",
    "cost",
    "datasets",
    "audit",
    "alerts",
    "tenants",
    "guardrails",
    "workflows",
    "knowledgebases",
    "config",
    "registries",
    "routes",
    "secrets",
  ];

  it.each(LEGACY_IDS)("keeps %s", (id) => {
    const item = NAV_ITEMS.find((i) => i.id === id);
    expect(item, `nav id "${id}" disappeared in the M151 re-house`).toBeDefined();
    expect(item?.route).toBeTruthy();
  });

  it("renames the index from Dashboard to Home without moving its route", () => {
    // The page is the same surface (spec §6.1 A11); only the name changed.
    expect(NAV_ITEMS.find((i) => i.id === "dashboard")).toBeUndefined();
    expect(navRoute("home")).toBe("/");
  });
});

// m20.8's intent, restated: a first-run user meets INTENT (agents, tools,
// governance), never Kubernetes nouns. The nouns now live in Admin — last, and
// out of the daily sections.
describe("intent-shaped nav (m20.8, carried forward)", () => {
  it("keeps the raw CRD surfaces in Admin, at the bottom", () => {
    expect(sectionOf("Model routes")).toBe("Admin");
    expect(sectionOf("Secret bindings")).toBe("Admin");
    expect(NAV_SECTIONS[NAV_SECTIONS.length - 1].heading).toBe("Admin");
  });

  it("keeps CRD nouns out of the sections daily work happens in", () => {
    for (const heading of ["Home", "Agents", "Library", "Activity"]) {
      expect(labelsOf(heading)).not.toContain("Model routes");
      expect(labelsOf(heading)).not.toContain("Secret bindings");
    }
  });

  it("moves Registries to Govern — an allow-list is a control, not plumbing", () => {
    expect(sectionOf("Registries")).toBe("Govern");
  });

  it("keeps Providers first in Admin and first in the run-up to a first agent", () => {
    // m49.2's onboarding-order fix now lives in the Home checklist rather than in
    // nav position: step one links straight at the connect wizard.
    expect(labelsOf("Admin")[0]).toBe("Providers");
    expect(FIRST_RUN_CHECKLIST[0].to).toBe(`${navRoute("providers")}/connect`);
  });
});

// The m23.7 / m25 S8 consolidation: ONE primary create entry (a page action, not
// a nav item), and the MCP/tool surfaces grouped together — now inside Library,
// which is what the old "Tools" section became.
describe("nav consolidation (m23.7 / m25 S8, carried forward)", () => {
  it("does NOT put 'New agent' in the nav — it is the Agents page's action", () => {
    expect(NAV_ITEMS.map((i) => i.label)).not.toContain("New agent");
    expect(labelsOf("Agents")).not.toContain("New agent");
  });

  it("keeps the hand-authoring Config builder out of the agent-creation flow", () => {
    expect(labelsOf("Agents")).not.toContain("Config builder");
    expect(sectionOf("Config builder")).toBe("Library");
  });

  it("groups the MCP/tool surfaces in Library, with the approval queue in Govern", () => {
    expect(sectionOf("MCP servers")).toBe("Library");
    expect(sectionOf("Tool catalog")).toBe("Library");
    // The approval QUEUE is a human gate, so it sits with the other queues.
    expect(sectionOf("MCP approvals")).toBe("Govern");
    expect(NAV_ITEMS.map((i) => i.label)).not.toContain("Add MCP server");
  });
});

// m76.1 taxonomy: the standalone MCP Catalog nav item is retired (it duplicated the
// Gallery's MCP tab). The Gallery is the single discovery surface; MCP Servers is the
// owned list; Tool catalog is the bind-time picker.
describe("catalog taxonomy (m76.1)", () => {
  it("mcp-catalog nav item is removed (route retired — /tools/mcp-catalog redirects to /gallery)", () => {
    expect(NAV_ITEMS.find((i) => i.id === "mcp-catalog")).toBeUndefined();
  });

  it("gallery nav item exists as the single discovery surface", () => {
    expect(NAV_ITEMS.find((i) => i.id === "gallery")).toBeDefined();
    expect(navRoute("gallery")).toBe("/gallery");
  });

  it("mcp-servers nav item stays (owned list, not discovery)", () => {
    expect(navRoute("mcp-servers")).toBe("/tools/mcp-servers");
  });

  it("tool-catalog nav item stays (bind-time picker, not discovery)", () => {
    expect(navRoute("tool-catalog")).toBe("/tools/catalog");
  });
});

// Counts are DATA. The IA declares which backend answers each one and what the
// number means, because the meaning is what fixes the colour (§4.2).
describe("live counts are declared by the IA, not invented by the shell", () => {
  it("marks Approvals as a waiting-on-a-person count", () => {
    expect(NAV_ITEMS.find((i) => i.id === "approvals")?.count).toEqual({
      source: "approvals",
      tone: "waiting",
    });
  });

  it("marks Stops as an in-force count, in Govern", () => {
    const stops = NAV_ITEMS.find((i) => i.id === "stops");
    expect(stops?.count).toEqual({ source: "stops", tone: "stopped" });
    expect(navSectionOf("stops")).toBe("Govern");
  });

  it("gives no other item a count — nothing renders a number no backend answers", () => {
    expect(NAV_ITEMS.filter((i) => i.count).map((i) => i.id).sort()).toEqual([
      "approvals",
      "stops",
    ]);
  });

  it("leaves Stops routeless until its page ships (spec §6.2 gap 2, m151.7)", () => {
    // A routeless item walks to /soon/<id> — the honest "arrives in M151"
    // placeholder — rather than pretending a surface exists.
    expect(NAV_ITEMS.find((i) => i.id === "stops")?.route).toBeUndefined();
    expect(NAV_ITEMS.find((i) => i.id === "stops")?.milestone).toBe("M151");
  });
});

// The first-run checklist (m54.4) derives its routes from the nav IA, so a nav
// route change can't leave a stale hardcoded checklist path.
describe("first-run checklist derives from nav (m54.4)", () => {
  it("navRoute resolves a nav id to its route and throws on an unknown id", () => {
    expect(navRoute("providers")).toBe("/providers");
    expect(navRoute("playground")).toBe("/playground");
    expect(() => navRoute("does-not-exist")).toThrow(/no routed nav item/);
  });

  it("throws for a nav id that exists but has no route (a /soon placeholder)", () => {
    expect(() => navRoute("stops")).toThrow(/no routed nav item/);
  });

  it("each checklist step's route is anchored to its nav surface", () => {
    const byKey = Object.fromEntries(FIRST_RUN_CHECKLIST.map((s) => [s.doneKey, s]));
    // Base routes come from nav (the connect/new suffixes are the on-page actions).
    expect(byKey.provider.to).toBe(`${navRoute("providers")}/connect`);
    expect(byKey.agent.to).toBe(`${navRoute("agents")}/new`);
    expect(byKey.run.to).toBe(navRoute("playground"));
    // Three steps, each mapped to a distinct live signal.
    expect(FIRST_RUN_CHECKLIST.map((s) => s.doneKey)).toEqual(["provider", "agent", "run"]);
  });
});
