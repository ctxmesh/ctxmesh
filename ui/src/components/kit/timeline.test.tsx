import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { render, screen, within } from "@testing-library/react";

import { Timeline, type TimelineStep } from "./timeline";

// timeline.test.tsx — governance and work are ONE story.
//
// The defect this file exists to prevent is architectural, not cosmetic: the
// moment guardrails, approvals and stops get their own lane (a tab, a panel, a
// second list), a reader has to correlate two clocks to answer "why did this
// run stop here?". So the test asserts that a governance step is an item in the
// SAME list, in its recorded position — and that its meaning survives for a
// reader who cannot see the hue that marks it.

const steps: TimelineStep[] = [
  { id: "1", time: "16:35:01", title: "The agent called claude-sonnet.", tone: "done", meta: "1.2s" },
  {
    id: "2",
    time: "16:35:03",
    title: "A guardrail removed an email address.",
    tone: "governance",
    detail: "Matched the pii-email rule before the tool call was sent.",
  },
  { id: "3", time: "16:35:04", title: "Waiting for someone to approve the $42.00 spend.", tone: "hold" },
  { id: "4", time: "16:35:40", title: "The tool call failed.", tone: "failed" },
];

describe("Timeline — one sequence, not two systems", () => {
  it("renders governance steps inline, in order, among the model and tool calls", () => {
    render(<Timeline steps={steps} label="Steps in run 4f2c" />);
    const items = within(
      screen.getByRole("list", { name: "Steps in run 4f2c" }),
    ).getAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(items[1]).toHaveTextContent("A guardrail removed an email address.");
  });

  it("names what a coloured step MEANS, so the hue is never the only carrier", () => {
    render(<Timeline steps={steps} />);
    // A reader who cannot see the violet wash still hears that a person is the
    // blocker — the single most important thing this timeline says.
    expect(screen.getByText(/Waiting on a person:/)).toBeInTheDocument();
    expect(screen.getByText(/Governance step:/)).toBeInTheDocument();
    expect(screen.getByText(/Failed:/)).toBeInTheDocument();
  });

  it("titles are sentences, and the machine words live in the detail line", () => {
    render(<Timeline steps={steps} />);
    expect(
      screen.getByText("A guardrail removed an email address."),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Matched the pii-email rule/),
    ).toBeInTheDocument();
  });

  it("loads as a spine of steps, announced once", () => {
    render(<Timeline steps={[]} loading />);
    const region = screen.getByRole("status", { name: "Loading timeline" });
    expect(region).toHaveAttribute("aria-busy", "true");
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });
});

describe("Timeline — structural a11y", () => {
  it("has no axe violations", async () => {
    const { container } = render(<Timeline steps={steps} label="Run steps" />);
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });
});
