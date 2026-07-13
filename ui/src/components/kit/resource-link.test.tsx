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
});
