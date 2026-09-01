import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ResourceLink, resourcePath } from "@/components/kit/resource-link";

// ResourceLink is the navigability seam (M22 / Theme 1): a resource name renders
// as a link to its detail — never dead-end text — with one place resolving routes.

describe("resourcePath", () => {
  it("maps each kind to its detail route (encoded)", () => {
    expect(resourcePath("agent", "default", "billing")).toBe("/agents/default/billing");
    expect(resourcePath("registry", "prod", "fleet")).toBe("/registries/prod/fleet");
    expect(resourcePath("route", "default", "anthropic-x")).toBe("/routes/default/anthropic-x");
    expect(resourcePath("secretbinding", "default", "anthropic")).toBe(
      "/secrets/default/anthropic",
    );
    // M151: a team is a destination now — the outline page at /teams/:ns/:name.
    expect(resourcePath("team", "team-a", "support-pod")).toBe(
      "/teams/team-a/support-pod",
    );
  });
});

describe("ResourceLink", () => {
  it("renders a link to the agent detail (the registry-member fix, U4)", () => {
    render(
      <MemoryRouter>
        <ResourceLink kind="agent" namespace="default" name="billing-agent" testId="m" />
      </MemoryRouter>,
    );
    const link = screen.getByTestId("m");
    expect(link.tagName).toBe("A");
    expect(link).toHaveAttribute("href", "/agents/default/billing-agent");
    expect(link).toHaveTextContent("billing-agent");
  });

  it("degrades to honest text (no dead link) when coordinates are missing", () => {
    render(
      <MemoryRouter>
        <ResourceLink kind="agent" namespace="" name="orphan" testId="m" />
      </MemoryRouter>,
    );
    const el = screen.getByTestId("m");
    expect(el.tagName).toBe("SPAN");
    expect(el).toHaveTextContent("orphan");
  });

  // M151 §2.3/§5.7: ONE link treatment console-wide — pine text over a resting rule that
  // firms to pine on hover. The resting rule is the promise of a destination, so the honest
  // non-link must not wear it. A resource name is machine-owned text, hence mono (§3.1).
  it("wears the one console link treatment: mono pine over a resting rule", () => {
    render(
      <MemoryRouter>
        <ResourceLink kind="route" namespace="default" name="anthropic-x" testId="m" />
      </MemoryRouter>,
    );
    const cls = screen.getByTestId("m").className;
    expect(cls).toContain("font-mono");
    expect(cls).toContain("text-primary");
    expect(cls).toContain("border-b");
    expect(cls).toContain("border-accent");
    expect(cls).toContain("hover:border-primary");
    expect(cls).not.toContain("hover:underline");
  });

  it("renders the non-link fallback as plain mono ink, explicitly not underlined", () => {
    render(
      <MemoryRouter>
        <ResourceLink kind="agent" namespace="" name="orphan" testId="m" />
      </MemoryRouter>,
    );
    const cls = screen.getByTestId("m").className;
    expect(cls).toContain("font-mono");
    expect(cls).toContain("no-underline");
    expect(cls).not.toContain("border-b");
    expect(cls).not.toContain("text-primary");
  });
});
