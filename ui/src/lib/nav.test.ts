import { describe, expect, it } from "vitest";

import { NAV_SECTIONS } from "@/lib/nav";

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
