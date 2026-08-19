import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { VisibilityBadge } from "@/components/kit";

// VisibilityBadge is the ONE visibility chip (H5) unifying the gallery + mcp-servers near-duplicates.
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
});
