import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { render, screen } from "@testing-library/react";

import { QuietNote, UNKNOWN_VALUE_TITLE, UnknownValue } from "./quiet-note";

// quiet-note.test.tsx — "we cannot answer this" must not read as "this broke".
//
// The console has five degraded states and they are five DIFFERENT truths (§7).
// The one this component carries — the install has no backend for this value —
// is the one most often mis-rendered as an error or as a zero. Both mistakes
// send an operator to debug a healthy system, or worse, let them believe a
// number nobody measured.

describe("QuietNote — the calm register", () => {
  it("is an aside, not an alert", () => {
    render(
      <QuietNote title="Per-trace cost isn't configured.">
        Nothing here is estimated — the breakdown is simply absent.
      </QuietNote>,
    );
    expect(screen.getByRole("note")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("carries no status hue — it is not ok, warn, hold or crit", () => {
    const { container } = render(<QuietNote>No cap is set.</QuietNote>);
    const cls = container.firstElementChild?.className ?? "";
    for (const hue of ["success", "warning", "destructive", "hold", "info"]) {
      expect(cls).not.toContain(hue);
    }
  });
});

describe("UnknownValue — the one glyph for 'we do not know'", () => {
  it("is a dash with an explanation, and never a zero", () => {
    render(<UnknownValue />);
    const dash = screen.getByTitle(UNKNOWN_VALUE_TITLE);
    expect(dash).toHaveTextContent("—");
    expect(dash).not.toHaveTextContent("0");
  });

  it("lets the caller explain the specific absence", () => {
    render(<UnknownValue title="Attributed when the run closes." />);
    expect(
      screen.getByTitle("Attributed when the run closes."),
    ).toBeInTheDocument();
  });
});

describe("QuietNote — structural a11y", () => {
  it("has no axe violations", async () => {
    const { container } = render(
      <QuietNote title="Per-trace cost isn't configured.">
        Nothing here is estimated — the breakdown is simply absent.{" "}
        <UnknownValue />
      </QuietNote>,
    );
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });
});
