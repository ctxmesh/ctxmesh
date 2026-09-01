import { describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import { fireEvent, render, screen } from "@testing-library/react";

import { FilterChipRow, type FilterChip } from "./filter-chips";

// filter-chips.test.tsx — the counts are the feature, and they are not ours.
//
// The row is the first thing above every table, so its numbers are the first
// numbers anyone reads. Two rules keep them honest: a count the backend did not
// send shows NOTHING (never a client-side tally of the rows on this page, never
// a placeholder zero), and a backend zero shows `0` because that is a real
// answer. The third rule is structural: these are VIEWS, so exactly one is
// selected and the row is one radiogroup with one tab stop.

const chips: FilterChip[] = [
  { id: "attention", label: "Needs a person", count: 4 },
  { id: "failing", label: "Failing", count: 0 },
  { id: "all", label: "Everything" }, // the backend did not answer this one
];

describe("FilterChipRow — honest counts", () => {
  it("shows a backend zero as 0 — a real answer", () => {
    render(<FilterChipRow chips={chips} value="attention" onChange={vi.fn()} />);
    expect(
      screen.getByRole("radio", { name: "Failing 0" }),
    ).toBeInTheDocument();
  });

  it("shows NO number for a count the backend did not send", () => {
    render(<FilterChipRow chips={chips} value="attention" onChange={vi.fn()} />);
    const chip = screen.getByRole("radio", { name: "Everything" });
    expect(chip.textContent).toBe("Everything");
  });
});

describe("FilterChipRow — one view at a time", () => {
  it("is a radiogroup with exactly one checked chip and one tab stop", () => {
    render(
      <FilterChipRow
        chips={chips}
        value="failing"
        onChange={vi.fn()}
        label="Filter agents"
      />,
    );
    const group = screen.getByRole("radiogroup", { name: "Filter agents" });
    expect(group).toBeInTheDocument();
    const checked = screen
      .getAllByRole("radio")
      .filter((r) => r.getAttribute("aria-checked") === "true");
    expect(checked).toHaveLength(1);
    const tabbable = screen
      .getAllByRole("radio")
      .filter((r) => r.getAttribute("tabindex") === "0");
    expect(tabbable).toHaveLength(1);
  });

  it("arrow keys move the selection, as a radiogroup must", () => {
    const onChange = vi.fn();
    render(
      <FilterChipRow chips={chips} value="attention" onChange={onChange} />,
    );
    fireEvent.keyDown(screen.getByRole("radio", { name: "Needs a person 4" }), {
      key: "ArrowRight",
    });
    expect(onChange).toHaveBeenCalledWith("failing");
  });

  it("clicking a chip selects it", () => {
    const onChange = vi.fn();
    render(
      <FilterChipRow chips={chips} value="attention" onChange={onChange} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: "Everything" }));
    expect(onChange).toHaveBeenCalledWith("all");
  });
});

describe("FilterChipRow — structural a11y", () => {
  it("has no axe violations", async () => {
    const { container } = render(
      <FilterChipRow chips={chips} value="attention" onChange={vi.fn()} label="Filter agents" />,
    );
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });
});
