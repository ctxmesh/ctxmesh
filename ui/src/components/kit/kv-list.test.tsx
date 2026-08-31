import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { render, screen } from "@testing-library/react";

import { KV_ABSENT_DEFAULT, KeyValueList } from "./kv-list";

// kv-list.test.tsx — the two rules that make the detail rail trustworthy.
//
// 1. A KNOWN ZERO IS A ZERO. `0 runs` is a measurement; printing "not yet
//    known" over it hides a real answer.
// 2. AN UNKNOWN IS NEVER BLANK AND NEVER A ZERO. A blank cell is
//    indistinguishable from a rendering bug, and a zero is a claim the backend
//    never made.
//
// A truthiness check on the value breaks (1); an unguarded render breaks (2).
// Both are one careless line away, so both are pinned here.

describe("KeyValueList — zero and unknown are different things", () => {
  it("renders a known zero as a real 0", () => {
    render(<KeyValueList items={[{ key: "Runs", value: 0 }]} />);
    expect(screen.getByText("0")).toBeInTheDocument();
    expect(screen.queryByText(KV_ABSENT_DEFAULT)).not.toBeInTheDocument();
  });

  it("renders an absent value as words with a title, never blank", () => {
    render(<KeyValueList items={[{ key: "Route" }]} />);
    const value = screen.getByText(KV_ABSENT_DEFAULT);
    expect(value).toBeInTheDocument();
    expect(value).toHaveAttribute("title", expect.stringContaining("not zero"));
  });

  it("honours the §7.1 vocabulary the caller chooses for an absence", () => {
    render(
      <KeyValueList
        items={[{ key: "Secret", absent: "not attached", title: "No SecretBinding is bound." }]}
      />,
    );
    const value = screen.getByText("not attached");
    expect(value).toHaveAttribute("title", "No SecretBinding is bound.");
  });

  it("never leaves a row visually empty, even for values React would drop", () => {
    // `false` / `true` render as nothing at all in React — the silent-blank
    // trap. They are treated as absent so the row still says something.
    render(<KeyValueList items={[{ key: "Streaming", value: false }]} />);
    expect(screen.getByText(KV_ABSENT_DEFAULT)).toBeInTheDocument();
  });

  it("pairs every key with its value as a definition list", () => {
    const { container } = render(
      <KeyValueList
        items={[
          { key: "Namespace", value: "acme" },
          { key: "Runs", value: 0 },
        ]}
      />,
    );
    expect(container.querySelectorAll("dt")).toHaveLength(2);
    expect(container.querySelectorAll("dd")).toHaveLength(2);
  });
});

// Structural a11y (M100 UI99-7): a definition list whose dt/dd pairs are wrapped
// in divs is only valid if the wrappers are the sanctioned kind — axe checks it.
describe("KeyValueList — structural a11y", () => {
  it("has no axe violations", async () => {
    const { container } = render(
      <KeyValueList items={[{ key: "Namespace", value: "acme" }, { key: "Route" }]} />,
    );
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });
});
