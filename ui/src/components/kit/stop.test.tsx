import { describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import {
  STOP_CONTRACT,
  STOP_NOTICE_CONTRACT,
  STOP_REASON_REQUIRED,
  StopControl,
  StopNotice,
  type StopScopeOption,
} from "./stop";

// stop.test.tsx — the kill switch, pinned.
//
// These assertions are about the CONTRACT, not the pixels. StopControl is on
// every page and StopNotice explains, to everyone whose work just stopped, what
// happened to it. The failure modes that matter are all semantic:
//   • the copy quietly softening until it no longer matches ADR 0126,
//   • the reason becoming optional (the audit line disappears),
//   • the cluster-wide gate becoming one click,
//   • an unreported count rendering as 0 (a claim the backend never made),
//   • a viewer being shown a Lift button they cannot use.
// Each has a test below, because each is a one-line diff away at all times.

const scopes: StopScopeOption[] = [
  {
    kind: "agent",
    name: "acme/summarizer",
    impact: { agents: 1, queued: 3, running: 1 },
  },
  { kind: "team", name: "acme/team-b", impact: { agents: 6, queued: 12 } },
  { kind: "fleet", impact: { agents: 41 } },
];

function openDialog() {
  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
}

describe("StopControl — what it promises", () => {
  it("states the contract verbatim: refuse, hold, stop at the next call, discard nothing", () => {
    render(<StopControl scopes={scopes} onStop={vi.fn()} />);
    openDialog();
    expect(screen.getByText(STOP_CONTRACT)).toBeInTheDocument();
  });

  it("says what stopping does NOT do — held work is kept, and a local busy loop is not interrupted", () => {
    render(<StopControl scopes={scopes} onStop={vi.fn()} />);
    openDialog();
    // ADR 0126's honest limit: the interrupt lands at model/tool boundaries.
    expect(
      screen.getByText(/not interrupted until it next calls a model or a tool/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/keep their place in the queue/),
    ).toBeInTheDocument();
  });

  it("states the blast radius in counts, and an unreported count is never a zero", () => {
    render(<StopControl scopes={scopes} onStop={vi.fn()} />);
    openDialog();
    // The agent scope is pre-selected: 1 agent, 3 queued, 1 in flight.
    expect(screen.getByText("3")).toBeInTheDocument();

    // Switch to the team scope, which reported no in-flight count at all.
    fireEvent.click(screen.getByRole("radio", { name: /This team/ }));
    expect(screen.getByText("12")).toBeInTheDocument();
    const unreported = screen.getByText("not reported");
    expect(unreported).toBeInTheDocument();
    expect(unreported).toHaveAttribute(
      "title",
      expect.stringContaining("unknown"),
    );
  });
});

describe("StopControl — the gates", () => {
  it("refuses to stop without a reason, and submits nothing", async () => {
    const onStop = vi.fn();
    render(<StopControl scopes={scopes} onStop={onStop} />);
    openDialog();
    fireEvent.click(screen.getByRole("button", { name: "Stop this agent" }));
    expect(await screen.findByText(STOP_REASON_REQUIRED)).toBeInTheDocument();
    expect(onStop).not.toHaveBeenCalled();
  });

  it("submits the selected scope and the trimmed reason", async () => {
    const onStop = vi.fn();
    render(<StopControl scopes={scopes} onStop={onStop} />);
    openDialog();
    fireEvent.click(screen.getByRole("radio", { name: /This team/ }));
    fireEvent.change(screen.getByLabelText(/Why are you stopping this/), {
      target: { value: "  runaway delegation loop  " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Stop this team" }));
    await waitFor(() =>
      expect(onStop).toHaveBeenCalledWith({
        scope: "team",
        name: "acme/team-b",
        reason: "runaway delegation loop",
      }),
    );
  });

  it("a cluster-wide stop needs the typed-name gate — never one click", async () => {
    const onStop = vi.fn();
    render(
      <StopControl scopes={scopes} clusterName="prod-eu" onStop={onStop} />,
    );
    openDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
    fireEvent.change(screen.getByLabelText(/Why are you stopping this/), {
      target: { value: "prompt injection incident" },
    });

    const confirm = screen.getByRole("button", { name: "Stop everything" });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(onStop).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText(/to confirm/), {
      target: { value: "prod-eu" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Stop everything" }));
    await waitFor(() => expect(onStop).toHaveBeenCalledOnce());
  });

  it("surfaces a failed stop instead of closing on a lie", async () => {
    const onStop = vi.fn().mockRejectedValue(new Error("503 from the BFF"));
    render(<StopControl scopes={scopes} onStop={onStop} />);
    openDialog();
    fireEvent.change(screen.getByLabelText(/Why are you stopping this/), {
      target: { value: "spend spike" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Stop this agent" }));
    expect(
      await screen.findByText(/The stop was not applied: 503 from the BFF/),
    ).toBeInTheDocument();
  });

  it("disables the control when there is nothing this caller may stop", () => {
    render(<StopControl scopes={[]} onStop={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled();
  });
});

describe("StopNotice — what the person whose work stopped reads", () => {
  it("names the scope, quotes the reason, and restates the contract", () => {
    render(
      <StopNotice
        scope="team"
        scopeName="acme/team-b"
        reason="runaway delegation loop"
        by="oncall@acme"
        impact={{ queued: 12, running: 1, agents: 6 }}
      />,
    );
    expect(screen.getByText("acme/team-b")).toBeInTheDocument();
    expect(screen.getByText(/runaway delegation loop/)).toBeInTheDocument();
    expect(screen.getByText(STOP_NOTICE_CONTRACT)).toBeInTheDocument();
    expect(screen.getByText("12")).toBeInTheDocument();
  });

  it("renders the unknown glyph, never a zero, when no counts were reported", () => {
    render(<StopNotice scope="fleet" reason="incident" />);
    const dash = screen.getByTitle(/unknown — not zero/i);
    expect(dash).toHaveTextContent("—");
    expect(screen.queryByText("0")).not.toBeInTheDocument();
  });

  it("shows no Lift button to a caller who cannot lift", () => {
    render(<StopNotice scope="workspace" scopeName="acme" reason="incident" />);
    expect(
      screen.queryByRole("button", { name: /Lift the stop/ }),
    ).not.toBeInTheDocument();
  });

  it("lifting is its own confirmed act, and says what is about to start", async () => {
    const onLift = vi.fn();
    render(
      <StopNotice
        scope="team"
        scopeName="acme/team-b"
        reason="incident"
        impact={{ queued: 12, agents: 6 }}
        onLift={onLift}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Lift the stop" }));
    expect(screen.getByText(/Held runs that will start/)).toBeInTheDocument();
    expect(onLift).not.toHaveBeenCalled();

    fireEvent.click(
      screen.getAllByRole("button", { name: "Lift the stop" })[1],
    );
    await waitFor(() => expect(onLift).toHaveBeenCalledWith("team", "acme/team-b"));
  });
});

describe("the kill switch — structural a11y", () => {
  it("the scope dialog has no axe violations", async () => {
    const { container } = render(
      <StopControl scopes={scopes} clusterName="prod-eu" onStop={vi.fn()} />,
    );
    openDialog();
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });

  it("the banner has no axe violations", async () => {
    const { container } = render(
      <StopNotice
        scope="team"
        scopeName="acme/team-b"
        reason="runaway delegation loop"
        by="oncall@acme"
        impact={{ queued: 12, running: 1, agents: 6 }}
        onLift={vi.fn()}
      />,
    );
    expect(await axe(container, { rules: { "color-contrast": { enabled: false } } })).toHaveNoViolations();
  });
});
