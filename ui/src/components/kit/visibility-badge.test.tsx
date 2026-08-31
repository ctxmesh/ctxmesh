import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { VisibilityBadge } from "@/components/kit";

// VisibilityBadge is the ONE visibility chip (H5) unifying the gallery + mcp-servers near-duplicates.
//
// M151 §5.6 re-points it onto Tag variants. Visibility is a DECLARED SCOPE, not a health
// status, so it may never carry a semantic hue: every known scope is the `muted` chip, and
// an unrecognised value is the `open` chip (dashed = "declared but never exercised"). The
// tests below assert that split through `border-dashed` — the one class that distinguishes
// `open` from every filled-tint variant — rather than pinning the whole recipe.
describe("VisibilityBadge", () => {
  it("renders nothing for an absent visibility", () => {
    const { container } = render(<VisibilityBadge visibility={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the visibility label", () => {
    render(<VisibilityBadge visibility="org" />);
    expect(screen.getByText("org")).toBeInTheDocument();
  });

  it("emits a stable visibility-<name> testid when name is given", () => {
    render(<VisibilityBadge visibility="team" name="acme" />);
    expect(screen.getByTestId("visibility-acme")).toHaveTextContent("team");
  });

  it("renders an unknown visibility verbatim (with a lock)", () => {
    render(<VisibilityBadge visibility="mystery" name="x" />);
    expect(screen.getByTestId("visibility-x")).toHaveTextContent("mystery");
  });

  it.each(["public", "org", "team"])(
    "renders a known scope (%s) as the muted chip, never a semantic hue",
    (visibility) => {
      render(<VisibilityBadge visibility={visibility} name="k" />);
      const el = screen.getByTestId("visibility-k");
      expect(el.className).not.toContain("border-dashed");
      // Scope is not health: no ok/warn/crit/hold tint, and never the pine brand.
      for (const hue of ["success", "warning", "destructive", "hold", "primary"]) {
        expect(el.className).not.toContain(hue);
      }
    },
  );

  it("renders an unknown scope as the open (dashed) chip", () => {
    render(<VisibilityBadge visibility="mystery" name="x" />);
    expect(screen.getByTestId("visibility-x").className).toContain("border-dashed");
  });

  it("lets the Tag own its size — no ad-hoc text-[10px] override (§5.6)", () => {
    render(<VisibilityBadge visibility="org" name="s" />);
    expect(screen.getByTestId("visibility-s").className).not.toContain("text-[10px]");
  });
});
