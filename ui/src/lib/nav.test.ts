import { describe, expect, it } from "vitest";

import {
  NAV_SECTIONS,
  NAV_ITEMS,
  navRoute,
  FIRST_RUN_CHECKLIST,
} from "@/lib/nav";

// The intent-shaped IA (m20.8): the object-shaped CRD surfaces (Model routes,
// Secret bindings, Registries) are demoted OUT of the primary nav into an "Advanced"
// section, so a first-run user sees intent (Providers → Agents → …), not Kubernetes
// nouns. Providers stays primary (the model home).

const sectionOf = (label: string) =>
  NAV_SECTIONS.find((s) => s.items.some((i) => i.label === label))?.heading;

describe("intent-shaped nav (m20.8)", () => {
  it("demotes Model routes / Secret bindings / Registries into Advanced", () => {
    expect(sectionOf("Model routes")).toBe("Advanced");
    expect(sectionOf("Secret bindings")).toBe("Advanced");
    expect(sectionOf("Registries")).toBe("Advanced");
  });

  it("keeps Providers primary (not Advanced)", () => {
    const providers = sectionOf("Providers");
    expect(providers).toBeDefined();
    expect(providers).not.toBe("Advanced");
  });

  it("Advanced sits at the bottom of the nav", () => {
    expect(NAV_SECTIONS[NAV_SECTIONS.length - 1].heading).toBe("Advanced");
  });

  it("does not surface a CRD noun in the primary Platform section", () => {
    const platform = NAV_SECTIONS.find((s) => s.heading === "Platform");
    const labels = platform?.items.map((i) => i.label) ?? [];
    expect(labels).not.toContain("Model routes");
    expect(labels).not.toContain("Secret bindings");
  });
});

// The m23.7 consolidation (audit B3/B5): ONE primary create entry ("New agent"),
// with the raw hand-authoring "Config builder" demoted to Advanced; and the three
// MCP/tool surfaces grouped into a dedicated "Tools" section out of "Build".
describe("nav consolidation (m23.7)", () => {
  const buildLabels = () =>
    NAV_SECTIONS.find((s) => s.heading === "Build")?.items.map((i) => i.label) ??
    [];

  it("does NOT put 'New agent' in the nav (m25 S8 — it's a page action); Config builder is Advanced", () => {
    // New agent moved to a top-right button ON the Agents page, so it is no longer a
    // sidebar item (in Build or anywhere).
    expect(buildLabels()).not.toContain("New agent");
    expect(NAV_ITEMS.map((i) => i.label)).not.toContain("New agent");
    expect(buildLabels()).not.toContain("Config builder");
    expect(sectionOf("Config builder")).toBe("Advanced");
  });

  it("groups the MCP/tool surfaces into a dedicated Tools section (not Build)", () => {
    // m25 S10: the MCP entry is a list page "MCP Servers" (Add is in-page), not
    // "Add MCP server".
    expect(sectionOf("MCP Servers")).toBe("Tools");
    expect(sectionOf("MCP approvals")).toBe("Tools");
    expect(sectionOf("Tool catalog")).toBe("Tools");
    expect(buildLabels()).not.toContain("MCP Servers");
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
    expect(NAV_ITEMS.find((i) => i.id === "mcp-servers")).toBeDefined();
    expect(navRoute("mcp-servers")).toBe("/tools/mcp-servers");
  });

  it("tool-catalog nav item stays (bind-time picker, not discovery)", () => {
    expect(NAV_ITEMS.find((i) => i.id === "tool-catalog")).toBeDefined();
    expect(navRoute("tool-catalog")).toBe("/tools/catalog");
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
