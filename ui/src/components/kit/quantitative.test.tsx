import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import {
  LIFECYCLE_STAGES,
  LifecycleStrip,
  LifecycleTrack,
} from "./lifecycle";
import {
  Meter,
  QuantityValue,
  UNKNOWN,
  formatCount,
  isKnown,
  meterState,
  type Quantity,
} from "./meter";
import { UNKNOWN_TITLE } from "./quantity";
import { PressureStrip } from "./pressure-strip";

// The quantitative kit, pinned (M151 §5.20/§5.21/§5.24).
//
// These assertions are almost entirely about ONE rule — §7.1's unknown-vs-zero
// rule — because it is the rule these three components exist to protect and the
// only one whose violation is invisible in review: a full bar with no cap, a
// missing segment rendered as zero, and a `$0.0000` that means "we don't know"
// all LOOK fine. So every test below asks the same question in a different
// shape: when the backend didn't answer, does the console still claim a number?

const money = (n: number) =>
  n < 1 ? `$${n.toFixed(4)}` : `$${n.toFixed(2)}`;

describe("the quantity contract", () => {
  it("treats only finite numbers as known — zero included, NaN excluded", () => {
    expect(isKnown(0)).toBe(true);
    expect(isKnown(1024)).toBe(true);
    // NaN and Infinity are `number` to TypeScript but they are not answers, and
    // a NaN width renders as a zero-width segment — an unknown wearing a known
    // glyph, which is exactly what this gate exists to stop.
    const notAnswers: Quantity[] = [
      NaN,
      Infinity,
      -Infinity,
      null,
      undefined,
      UNKNOWN,
    ];
    for (const v of notAnswers) {
      expect(isKnown(v)).toBe(false);
    }
  });

  it("groups counts by locale", () => {
    expect(formatCount(1024)).toBe((1024).toLocaleString());
  });

  it("never lets an unknown and a zero share a glyph", () => {
    const { rerender } = render(<QuantityValue value={0} />);
    expect(screen.getByText("0")).toBeInTheDocument();
    expect(screen.queryByText("—")).not.toBeInTheDocument();

    rerender(<QuantityValue value={UNKNOWN} />);
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByText("0")).not.toBeInTheDocument();
    // The dash is explainable, not decorative.
    expect(screen.getByText("—")).toHaveAttribute(
      "title",
      UNKNOWN_TITLE,
    );
  });

  it("routes a money unknown to the dash, never to a formatted zero", () => {
    render(<QuantityValue value={UNKNOWN} format={money} />);
    expect(screen.queryByText("$0.0000")).not.toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});

describe("Meter", () => {
  it("draws no bar at all when the cap is unknown", () => {
    render(<Meter label="spend" used={0.19} cap={UNKNOWN} thing="workspace" format={money} />);
    // The whole point: no cap means no denominator, so a bar of ANY width would
    // be invented — including the full green one a naive implementation draws.
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
    expect(
      screen.getByText(/No cap is set for this workspace\./),
    ).toBeInTheDocument();
    // The figure the backend DOES know still shows.
    expect(screen.getByText("$0.1900")).toBeInTheDocument();
  });

  it("draws an empty track and says so when the usage is unknown", () => {
    render(<Meter label="spend" used={UNKNOWN} cap={0.5} format={money} />);
    // No value to announce ⇒ no meter role: an empty track claims nothing,
    // whereas a zero-width fill would claim nothing has been spent.
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
    expect(screen.queryByText("$0.0000")).not.toBeInTheDocument();
    expect(screen.getByText(/not recorded for this install/)).toBeInTheDocument();
  });

  it("exposes the numbers to a screen reader, not silence", () => {
    render(<Meter label="spend" used={0.19} cap={0.5} format={money} />);
    const meter = screen.getByRole("meter");
    expect(meter).toHaveAttribute("aria-label", "spend");
    expect(meter).toHaveAttribute("aria-valuenow", "0.19");
    expect(meter).toHaveAttribute("aria-valuemax", "0.5");
    expect(meter).toHaveAttribute("aria-valuetext", "$0.1900 of $0.5000");
  });

  it("routes the fill hue by MEANING: inside the bound, near it, past it", () => {
    expect(meterState(10, 100, 80)).toBe("under");
    expect(meterState(80, 100, 80)).toBe("warn");
    expect(meterState(91, 100, 80)).toBe("warn");
    expect(meterState(100, 100, 80)).toBe("over");
    expect(meterState(140, 100, 80)).toBe("over");
    // No threshold ⇒ amber never fires; amber means "a bound is near", and
    // without a threshold there is no such bound to be near (§2.2).
    expect(meterState(91, 100)).toBe("under");
    expect(meterState(91, 100, UNKNOWN)).toBe("under");
  });

  it("says in words what the tick means, and how much is left", () => {
    const { container } = render(
      <Meter label="spawn budget" used={69} cap={100} threshold={80} />,
    );
    expect(
      screen.getByText(/The tick is where the alert fires\. 31% of the cap is left\./),
    ).toBeInTheDocument();
    // One tick, positioned by the real threshold — not a decorative notch.
    const tick = container.querySelector<HTMLElement>(".bg-faint");
    expect(tick?.style.left).toBe("80%");
  });

  it("suppresses the foot when the caller passes null", () => {
    render(<Meter label="spawn budget" used={69} cap={100} threshold={80} foot={null} />);
    expect(screen.queryByText(/The tick is where/)).not.toBeInTheDocument();
  });

  it("caps the fill at the track when usage exceeds the cap", () => {
    const { container } = render(<Meter label="spend" used={140} cap={100} />);
    const fill = container.querySelector<HTMLElement>(".bg-destructive");
    expect(fill?.style.width).toBe("100%");
  });
});

describe("PressureStrip", () => {
  const full = { running: 812, queued: 156, held: 4, failed: 11, idle: 41 };

  it("carries the real counts in the legend and in its accessible name", () => {
    render(<PressureStrip {...full} running={1024} />);
    expect(screen.getByTestId("pressure-held")).toHaveTextContent("held4");
    expect(screen.getByTestId("pressure-running")).toHaveTextContent(
      `running${(1024).toLocaleString()}`,
    );
    expect(
      screen.getByRole("img", {
        name: /Agent pressure: .*4 held, 11 failed, 41 idle/,
      }),
    ).toBeInTheDocument();
  });

  it("keeps a sub-1% segment visible — 4 holds out of 1,024 must not vanish", () => {
    render(<PressureStrip {...full} running={1024} />);
    const bar = screen.getByRole("img", { name: /Agent pressure/ });
    const held = bar.querySelector<HTMLElement>(".bg-hold");
    expect(held).not.toBeNull();
    expect(held?.className).toContain("min-w-[3px]");
  });

  it("omits an unknown segment instead of drawing it as zero", () => {
    render(<PressureStrip {...full} held={UNKNOWN} />);
    // Three segments (running, queued, failed) — not four with one at zero.
    const bar = screen.getByRole("img", { name: /Agent pressure/ });
    expect(bar.children).toHaveLength(3);
    expect(bar.querySelector(".bg-hold")).toBeNull();
    // And the legend says "—", not "0".
    expect(screen.getByTestId("pressure-held")).toHaveTextContent("held—");
    expect(screen.getByRole("img", { name: /not recorded held/ })).toBeInTheDocument();
    expect(
      screen.getByText(/Held is not recorded for this install\./),
    ).toBeInTheDocument();
  });

  it("renders a KNOWN zero as a real zero, with no segment", () => {
    render(<PressureStrip {...full} failed={0} />);
    expect(screen.getByTestId("pressure-failed")).toHaveTextContent("failed0");
    expect(screen.getByTestId("pressure-failed")).not.toHaveTextContent("—");
    const bar = screen.getByRole("img", { name: /Agent pressure/ });
    expect(bar.querySelector(".bg-destructive")).toBeNull();
  });

  it("draws nothing at all when nothing is known", () => {
    render(
      <PressureStrip
        running={UNKNOWN}
        queued={UNKNOWN}
        held={UNKNOWN}
        failed={UNKNOWN}
        idle={UNKNOWN}
      />,
    );
    // An empty track here would read as "all idle" — a claim, not an absence.
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByText(/There is nothing to draw/)).toBeInTheDocument();
  });

  it("mini drops the legend but never the counts a reader needs", () => {
    render(<PressureStrip {...full} size="mini" />);
    expect(screen.queryByTestId("pressure-held")).not.toBeInTheDocument();
    expect(
      screen.getByRole("img", { name: /Agent pressure: 812 running/ }),
    ).toBeInTheDocument();
  });

  it("mini renders a dash, not an empty bar, when nothing is known", () => {
    render(
      <PressureStrip
        running={UNKNOWN}
        queued={UNKNOWN}
        held={UNKNOWN}
        failed={UNKNOWN}
        idle={UNKNOWN}
        size="mini"
      />,
    );
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});

describe("LifecycleStrip", () => {
  const stages = [
    { name: "Build" as const, fact: "3 agents" },
    { name: "Govern" as const, fact: "2 policies" },
    { name: "Ship" as const, fact: "1 canary", active: true },
    { name: "Improve" as const },
  ];

  it("is a strip, never navigation", () => {
    render(<LifecycleStrip stages={stages} />);
    // No links, no buttons, no nav landmark: the lifecycle says where a thing
    // IS, it is not a set of destinations.
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("navigation")).not.toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Lifecycle" })).toBeInTheDocument();
    expect(screen.getAllByRole("listitem")).toHaveLength(4);
  });

  it("marks the current stage where a screen reader can hear it", () => {
    render(<LifecycleStrip stages={stages} />);
    const current = screen
      .getAllByRole("listitem")
      .filter((li) => li.getAttribute("aria-current") === "step");
    expect(current).toHaveLength(1);
    expect(current[0]).toHaveTextContent("Ship");
  });

  it("renders the unknown copy for a fact the backend cannot answer", () => {
    render(<LifecycleStrip stages={stages} />);
    // Never blank, never invented (§5.20: "never invent one").
    const improve = screen
      .getAllByRole("listitem")
      .find((li) => li.textContent?.startsWith("Improve"));
    expect(improve).toHaveTextContent("not yet known");
  });
});

describe("LifecycleTrack", () => {
  it("names its position for a screen reader", () => {
    render(<LifecycleTrack stage="Ship" />);
    expect(
      screen.getByRole("img", { name: "Lifecycle: Ship, stage 3 of 4" }),
    ).toBeInTheDocument();
    expect(LIFECYCLE_STAGES).toHaveLength(4);
  });

  it("says 'stopped' in words as well as in crit", () => {
    render(<LifecycleTrack stage="Ship" stopped />);
    expect(
      screen.getByRole("img", { name: "Lifecycle: stopped at Ship" }),
    ).toBeInTheDocument();
    // Colour is never the only carrier of a state this consequential.
    expect(screen.getByText("stopped")).toBeInTheDocument();
  });

  it("draws no track when the stage is unknown — a position is a claim", () => {
    render(<LifecycleTrack stage={UNKNOWN} />);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByText("—")).toHaveAttribute(
      "title",
      UNKNOWN_TITLE,
    );
  });
});
