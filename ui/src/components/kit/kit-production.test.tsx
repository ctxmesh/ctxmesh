import * as React from "react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";

import {
  CommandPalette,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  ErrorState,
  SkeletonTable,
  ToastProvider,
  Wizard,
  useToast,
  type Column,
  type WizardStep,
} from "@/components/kit";

// Production-kit behavior proofs (m13.4). These go beyond the m13.1 skeleton
// render proofs: controlled discipline (parent owns state), each visual state,
// the DataTable cursor-vs-q rule EXPLICITLY, ConfirmDialog typed-name gating
// (incl. the Enter keyboard path), Wizard canProceed gating + dirty-Esc guard,
// the CommandPalette keyboard flow, the Toast system (provider/hook/undo/
// timeout), and a11y basics (roles, focus-return, aria-current/-selected).

interface Row {
  id: string;
  name: string;
  model: string;
}

const cols: Column<Row>[] = [
  { id: "name", header: "Name", sortable: true, cell: (r) => r.name },
  { id: "model", header: "Model", cell: (r) => r.model },
];

function mkRows(n: number): Row[] {
  return Array.from({ length: n }, (_, i) => ({
    id: String(i),
    name: `agent-${i}`,
    model: "gpt-4o",
  }));
}

// ───────────────────────────── DataTable ──────────────────────────────────

describe("DataTable — controlled + states", () => {
  it("is controlled: sort clicks emit toggles, parent owns the sort state", () => {
    const onSortChange = vi.fn();
    const { rerender } = render(
      <DataTable
        columns={cols}
        rows={mkRows(2)}
        rowKey={(r) => r.id}
        sort={null}
        onSortChange={onSortChange}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Name" }));
    expect(onSortChange).toHaveBeenCalledWith({ columnId: "name", dir: "asc" });
    // Parent flips the sort → asc; the SAME header now toggles to desc.
    rerender(
      <DataTable
        columns={cols}
        rows={mkRows(2)}
        rowKey={(r) => r.id}
        sort={{ columnId: "name", dir: "asc" }}
        onSortChange={onSortChange}
      />,
    );
    // aria-sort reflects the controlled state.
    expect(
      screen.getByRole("columnheader", { name: /Name/ }),
    ).toHaveAttribute("aria-sort", "ascending");
    fireEvent.click(screen.getByRole("button", { name: "Name" }));
    expect(onSortChange).toHaveBeenLastCalledWith({
      columnId: "name",
      dir: "desc",
    });
  });

  it("renders the loading skeleton state", () => {
    render(
      <DataTable columns={cols} rows={[]} rowKey={(r) => r.id} loading />,
    );
    expect(screen.getByRole("status", { name: /Loading table/ })).toBeInTheDocument();
  });

  it("renders the teaching-empty state (unfiltered + empty)", () => {
    render(
      <DataTable
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        empty={{ title: "No agents yet", description: "Create one." }}
      />,
    );
    expect(screen.getByText("No agents yet")).toBeInTheDocument();
  });

  it("renders the generic error state with a retry", () => {
    const onRetry = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        error={{ message: "boom", onRetry }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Retry/ }));
    expect(onRetry).toHaveBeenCalledOnce();
  });

  it("renders the forbidden (403) error variant from the error prop", () => {
    render(
      <DataTable
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        error={{ message: "no list on agents", forbidden: true }}
      />,
    );
    expect(screen.getByText("You don't have access")).toBeInTheDocument();
  });

  // ── THE CURSOR-vs-q RULE ──────────────────────────────────────────────────
  it("cursor-vs-q: empty filtered window + hasNext → the more-affordance stays LIVE", () => {
    const onNext = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={[]} // empty filtered window
        rowKey={(r) => r.id}
        query="zzz"
        onQueryChange={() => {}}
        hasNext // ⇐ non-empty nextCursor: more pages exist
        onNext={onNext}
        rangeLabel="0 on this page"
      />,
    );
    // The honest "more pages exist" copy, NOT a terminal "no results".
    expect(screen.getByText("No matches in this page")).toBeInTheDocument();
    expect(screen.getByText(/more pages exist/i)).toBeInTheDocument();
    // The Next affordance is enabled even though rows.length === 0.
    const next = screen.getByRole("button", { name: "Next page" });
    expect(next).not.toBeDisabled();
    fireEvent.click(next);
    expect(onNext).toHaveBeenCalledOnce();
    // And an in-empty-state "Load next page" secondary action is also live.
    fireEvent.click(screen.getByRole("button", { name: "Load next page" }));
    expect(onNext).toHaveBeenCalledTimes(2);
  });

  it("cursor-vs-q: empty filtered window + NO more pages → honest terminal no-match", () => {
    render(
      <DataTable
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        query="zzz"
        onQueryChange={() => {}}
        hasNext={false}
      />,
    );
    expect(screen.getByText("No matches")).toBeInTheDocument();
    expect(screen.queryByText(/more pages exist/i)).not.toBeInTheDocument();
  });

  it("cursor-vs-q: Next is driven by hasNext, never by a full/empty row count", () => {
    // A FULL page (many rows) but no cursor → Next disabled.
    render(
      <DataTable
        columns={cols}
        rows={mkRows(50)}
        rowKey={(r) => r.id}
        hasNext={false}
        onNext={() => {}}
        rangeLabel="1–50"
      />,
    );
    expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
  });

  it("keyboard row navigation: roving focus + Enter activates the row", () => {
    const onRowClick = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={mkRows(3)}
        rowKey={(r) => r.id}
        onRowClick={onRowClick}
      />,
    );
    const rows = screen
      .getAllByRole("row")
      .filter((r) => r.getAttribute("tabindex") !== null);
    expect(rows).toHaveLength(3);
    // First row is the roving-tabindex entry (tabindex 0), rest are -1.
    expect(rows[0]).toHaveAttribute("tabindex", "0");
    expect(rows[1]).toHaveAttribute("tabindex", "-1");
    // ArrowDown moves the roving focus; Enter activates.
    fireEvent.keyDown(rows[0], { key: "ArrowDown" });
    fireEvent.keyDown(screen.getByText("agent-1").closest("tr")!, {
      key: "Enter",
    });
    expect(onRowClick).toHaveBeenCalledWith(
      expect.objectContaining({ name: "agent-1" }),
    );
  });

  it("virtualizes a large window: not all 200 rows are in the DOM", () => {
    render(
      <DataTable
        columns={cols}
        rows={mkRows(200)}
        rowKey={(r) => r.id}
        virtualizeThreshold={60}
        rowHeight={44}
      />,
    );
    // With a windowed render, the full 200 data rows are never all mounted.
    const dataRows = screen
      .getAllByRole("row")
      .filter((r) => within(r).queryByText(/agent-\d+/));
    expect(dataRows.length).toBeGreaterThan(0);
    expect(dataRows.length).toBeLessThan(200);
    // The first row is present (top of the window).
    expect(screen.getByText("agent-0")).toBeInTheDocument();
  });

  it("does not virtualize a small window (all rows present)", () => {
    render(
      <DataTable
        columns={cols}
        rows={mkRows(10)}
        rowKey={(r) => r.id}
        virtualizeThreshold={60}
      />,
    );
    expect(screen.getByText("agent-9")).toBeInTheDocument();
  });
});

// ───────────────────────────── Wizard ─────────────────────────────────────

const steps: WizardStep[] = [
  { id: "a", title: "Step A", content: <p>body A</p> },
  { id: "b", title: "Step B", review: true, content: <p>body B</p> },
];

describe("Wizard — gating + a11y + dirty guard", () => {
  it("canProceed=false disables Next; true enables it (controlled gate)", () => {
    const onStepChange = vi.fn();
    const { rerender } = render(
      <Wizard
        steps={steps}
        current={0}
        onStepChange={onStepChange}
        canProceed={false}
      />,
    );
    const next = screen.getByRole("button", { name: /Continue/ });
    expect(next).toBeDisabled();
    fireEvent.click(next);
    expect(onStepChange).not.toHaveBeenCalled();
    rerender(
      <Wizard steps={steps} current={0} onStepChange={onStepChange} canProceed />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    expect(onStepChange).toHaveBeenCalledWith(1);
  });

  it("busy shows a working spinner label and disables navigation", () => {
    render(
      <Wizard
        steps={steps}
        current={0}
        onStepChange={() => {}}
        busy
      />,
    );
    expect(screen.getByRole("button", { name: /Working/ })).toBeDisabled();
  });

  it("marks the active rail step with aria-current='step'", () => {
    render(<Wizard steps={steps} current={1} onStepChange={() => {}} />);
    const current = screen.getByRole("button", { current: "step" });
    expect(current).toHaveTextContent("Step B");
  });

  it("Esc when NOT dirty cancels straight out", () => {
    const onCancel = vi.fn();
    render(
      <Wizard steps={steps} current={0} onStepChange={() => {}} onCancel={onCancel} />,
    );
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("Esc when dirty routes through a discard-confirm (guards unsaved input)", () => {
    const onCancel = vi.fn();
    render(
      <Wizard
        steps={steps}
        current={0}
        onStepChange={() => {}}
        onCancel={onCancel}
        dirty
      />,
    );
    fireEvent.keyDown(window, { key: "Escape" });
    // Not cancelled yet — the guard is asking.
    expect(onCancel).not.toHaveBeenCalled();
    expect(screen.getByText("Discard your changes?")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(onCancel).toHaveBeenCalledOnce();
  });
});

// ─────────────────────────── ConfirmDialog ────────────────────────────────

describe("ConfirmDialog — typed-name gate", () => {
  it("gates confirm until the typed name matches (button + Enter both gated)", () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open
        onCancel={() => {}}
        onConfirm={onConfirm}
        title="Delete agent"
        confirmText="my-agent"
      />,
    );
    const btn = screen.getByRole("button", { name: "Delete" });
    const input = screen.getByLabelText(/to confirm/);
    expect(btn).toBeDisabled();
    // Enter is inert while the gate is unsatisfied (no keyboard bypass).
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onConfirm).not.toHaveBeenCalled();
    // Wrong text keeps it gated.
    fireEvent.change(input, { target: { value: "wrong" } });
    expect(btn).toBeDisabled();
    // Correct text opens the gate; Enter now submits.
    fireEvent.change(input, { target: { value: "my-agent" } });
    expect(btn).not.toBeDisabled();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("wires alertdialog to its heading + description (a11y)", () => {
    render(
      <ConfirmDialog
        open
        onCancel={() => {}}
        onConfirm={() => {}}
        title="Delete registry"
        description="This cannot be undone."
      />,
    );
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveAttribute("aria-labelledby");
    expect(dialog).toHaveAttribute("aria-describedby");
  });
});

// ─────────────────────────── DetailDrawer ─────────────────────────────────

describe("DetailDrawer — focus + a11y", () => {
  it("is a labelled modal dialog wired to its title", () => {
    render(
      <DetailDrawer open onClose={() => {}} title="my-agent">
        <p>drawer body</p>
      </DetailDrawer>,
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAttribute("aria-labelledby");
  });

  it("moves focus into the panel on open and restores it to the opener on close", async () => {
    function Harness() {
      const [open, setOpen] = React.useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Open drawer</button>
          <DetailDrawer open={open} onClose={() => setOpen(false)} title="x">
            <button>Inside</button>
          </DetailDrawer>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Open drawer" });
    opener.focus();
    fireEvent.click(opener);
    // Focus moved into the drawer (the first focusable — the Close button).
    await waitFor(() =>
      expect(document.activeElement).not.toBe(opener),
    );
    expect(screen.getByRole("dialog").contains(document.activeElement)).toBe(
      true,
    );
    // Esc closes; focus returns to the opener.
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(document.activeElement).toBe(opener));
  });

  it("supports size presets", () => {
    const { container } = render(
      <DetailDrawer open onClose={() => {}} title="x" size="lg">
        <p>b</p>
      </DetailDrawer>,
    );
    expect(container.querySelector("aside")?.className).toContain("sm:w-[44rem]");
  });
});

// ─────────────────────────── ErrorState ───────────────────────────────────

describe("ErrorState — always a next action", () => {
  it("forbidden with no props still explains the next step (no dead end)", () => {
    render(<ErrorState variant="forbidden" />);
    expect(screen.getByText("You don't have access")).toBeInTheDocument();
    expect(screen.getByText(/Ask an admin/i)).toBeInTheDocument();
  });

  it("generic error with no action falls back to an explanation", () => {
    render(<ErrorState />);
    expect(screen.getByText(/try again/i)).toBeInTheDocument();
  });
});

// ─────────────────────────── SkeletonTable ────────────────────────────────

describe("SkeletonTable — single busy region", () => {
  it("announces ONE busy region, not one per bar", () => {
    render(<SkeletonTable rows={4} cols={3} />);
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });
});

// ─────────────────────────── CommandPalette ───────────────────────────────

describe("CommandPalette — keyboard-first flow", () => {
  const commands = [
    { id: "agents", label: "Go to Agents", group: "Navigate", onRun: vi.fn() },
    { id: "routes", label: "Go to Routes", group: "Navigate", onRun: vi.fn() },
    { id: "connect", label: "Connect a provider", group: "Actions", onRun: vi.fn() },
  ];
  beforeEach(() => commands.forEach((c) => c.onRun.mockReset()));

  it("fuzzy-filters (subsequence) and groups results", () => {
    render(<CommandPalette open onClose={() => {}} commands={commands} />);
    // "gtr" is a subsequence of "Go To Routes".
    fireEvent.change(screen.getByLabelText("Command"), {
      target: { value: "gtr" },
    });
    expect(screen.getByText("Go to Routes")).toBeInTheDocument();
    expect(screen.queryByText("Connect a provider")).not.toBeInTheDocument();
  });

  it("↓/↑ move the active option and Enter runs it (aria-activedescendant tracks)", () => {
    render(<CommandPalette open onClose={() => {}} commands={commands} />);
    const input = screen.getByLabelText("Command");
    // First option active by default.
    expect(input).toHaveAttribute("aria-activedescendant", "cmd-agents");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input).toHaveAttribute("aria-activedescendant", "cmd-routes");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(commands[1].onRun).toHaveBeenCalledOnce();
  });

  it("wrapping: ArrowUp from the top wraps to the last option", () => {
    render(<CommandPalette open onClose={() => {}} commands={commands} />);
    const input = screen.getByLabelText("Command");
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input).toHaveAttribute("aria-activedescendant", "cmd-connect");
  });

  it("exposes a listbox of options with aria-selected on the active one", () => {
    render(<CommandPalette open onClose={() => {}} commands={commands} />);
    expect(screen.getByRole("listbox", { name: "Commands" })).toBeInTheDocument();
    const options = screen.getAllByRole("option");
    expect(options[0]).toHaveAttribute("aria-selected", "true");
    expect(options[1]).toHaveAttribute("aria-selected", "false");
  });
});

// ───────────────────────────── Toast system ───────────────────────────────

function ToastHarness({ onUndo }: { onUndo?: () => void }) {
  const { toast } = useToast();
  return (
    <button
      onClick={() =>
        toast({
          variant: "success",
          title: "Agent deleted",
          onUndo,
          duration: 1000,
        })
      }
    >
      Fire
    </button>
  );
}

describe("Toast system — provider + hook", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("useToast enqueues into the provider's live region and auto-dismisses", () => {
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    );
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Fire" }));
    });
    expect(screen.getByText("Agent deleted")).toBeInTheDocument();
    // After the duration elapses, it auto-dismisses.
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(screen.queryByText("Agent deleted")).not.toBeInTheDocument();
  });

  it("Undo runs the callback and dismisses immediately (grace window)", () => {
    const onUndo = vi.fn();
    render(
      <ToastProvider>
        <ToastHarness onUndo={onUndo} />
      </ToastProvider>,
    );
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Fire" }));
    });
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    });
    expect(onUndo).toHaveBeenCalledOnce();
    expect(screen.queryByText("Agent deleted")).not.toBeInTheDocument();
  });

  it("useToast throws outside a provider (fail-fast wiring bug)", () => {
    function Bare() {
      useToast();
      return null;
    }
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<Bare />)).toThrow(/ToastProvider/);
    spy.mockRestore();
  });
});
