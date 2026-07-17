import { describe, expect, it } from "vitest";

import { NAV_SECTIONS, NAV_ITEMS } from "@/lib/nav";

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
