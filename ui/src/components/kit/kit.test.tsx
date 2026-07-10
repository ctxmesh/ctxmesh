import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import {
  CommandPalette,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  EmptyState,
  ErrorState,
  Skeleton,
  Toast,
  Wizard,
  type Column,
  type WizardStep,
} from "@/components/kit";

// Render + behavior proofs for the primitive-kit skeletons (m13.1). These are
// low-fi but real; the full vitest coverage lands with the production kit in
// m13.4. Each test pins the props/API the five later milestones will compose.

describe("Skeleton", () => {
  it("renders a busy status region", () => {
    render(<Skeleton className="h-4 w-20" />);
    expect(screen.getByRole("status")).toHaveAttribute("aria-busy", "true");
  });
});

describe("EmptyState", () => {
  it("teaches: title, description, and a CTA that fires", () => {
    const onClick = vi.fn();
    render(
      <EmptyState
        title="No providers connected"
        description="Connect one to get started."
        action={{ label: "Connect a provider", onClick }}
      />,
    );
    expect(screen.getByText("No providers connected")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Connect a provider/ }));
    expect(onClick).toHaveBeenCalledOnce();
  });
});

describe("ErrorState", () => {
  it("always offers a next action (retry)", () => {
    const onRetry = vi.fn();
    render(<ErrorState description="boom" onRetry={onRetry} />);
    fireEvent.click(screen.getByRole("button", { name: /Retry/ }));
    expect(onRetry).toHaveBeenCalledOnce();
  });

  it("has a forbidden (403) variant that explains, not blanks", () => {
    render(
      <ErrorState variant="forbidden" description="ask an admin for access" />,
    );
    expect(screen.getByText("You don't have access")).toBeInTheDocument();
  });
});

interface Row {
  id: string;
  name: string;
}
const cols: Column<Row>[] = [
  { id: "name", header: "Name", sortable: true, cell: (r) => r.name },
];

describe("DataTable", () => {
  it("renders rows and fires row-click", () => {
    const onRowClick = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={[{ id: "1", name: "alpha" }]}
        rowKey={(r) => r.id}
        onRowClick={onRowClick}
      />,
    );
    fireEvent.click(screen.getByText("alpha"));
    expect(onRowClick).toHaveBeenCalledOnce();
  });

  it("shows the teaching empty state when unfiltered and empty", () => {
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

  it("shows a filtered-empty state (with clear) when a query matches nothing", () => {
    const onQueryChange = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        query="zzz"
        onQueryChange={onQueryChange}
      />,
    );
    expect(screen.getByText("No matches")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Clear filter/ }));
    expect(onQueryChange).toHaveBeenCalledWith("");
  });

  it("emits a sort request on a sortable header click", () => {
    const onSortChange = vi.fn();
    render(
      <DataTable
        columns={cols}
        rows={[{ id: "1", name: "a" }]}
        rowKey={(r) => r.id}
        sort={null}
        onSortChange={onSortChange}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Name" }));
    expect(onSortChange).toHaveBeenCalledWith({ columnId: "name", dir: "asc" });
  });
});

describe("Wizard", () => {
  const steps: WizardStep[] = [
    { id: "a", title: "Step A", content: <p>body A</p> },
    { id: "b", title: "Step B", review: true, content: <p>body B</p> },
  ];

  it("renders the current step and advances via onStepChange", () => {
    const onStepChange = vi.fn();
    render(
      <Wizard steps={steps} current={0} onStepChange={onStepChange} />,
    );
    expect(screen.getByText("body A")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Continue/ }));
    expect(onStepChange).toHaveBeenCalledWith(1);
  });

  it("shows the finish action on the last step", () => {
    const onFinish = vi.fn();
    render(
      <Wizard
        steps={steps}
        current={1}
        onStepChange={() => {}}
        onFinish={onFinish}
        finishLabel="Create it"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Create it" }));
    expect(onFinish).toHaveBeenCalledOnce();
  });
});

describe("DetailDrawer", () => {
  it("renders when open and closes on the close button", () => {
    const onClose = vi.fn();
    render(
      <DetailDrawer open onClose={onClose} title="my-agent">
        <p>drawer body</p>
      </DetailDrawer>,
    );
    expect(screen.getByText("drawer body")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Close panel/ }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("renders nothing when closed", () => {
    const { container } = render(
      <DetailDrawer open={false} onClose={() => {}} title="x">
        <p>hidden</p>
      </DetailDrawer>,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

describe("ConfirmDialog", () => {
  it("gates the confirm button until the typed name matches", () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open
        onCancel={() => {}}
        onConfirm={onConfirm}
        title="Delete agent"
        confirmText="my-agent"
        confirmLabel="Delete"
      />,
    );
    const btn = screen.getByRole("button", { name: "Delete" });
    expect(btn).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/to confirm/), {
      target: { value: "my-agent" },
    });
    expect(btn).not.toBeDisabled();
    fireEvent.click(btn);
    expect(onConfirm).toHaveBeenCalledOnce();
  });
});

describe("CommandPalette", () => {
  it("filters commands and runs the chosen one on click", () => {
    const onRun = vi.fn();
    render(
      <CommandPalette
        open
        onClose={() => {}}
        commands={[
          { id: "a", label: "Go to Agents", group: "Navigate", onRun },
          { id: "b", label: "Connect a provider", group: "Actions", onRun: () => {} },
        ]}
      />,
    );
    fireEvent.change(screen.getByLabelText("Command"), {
      target: { value: "agents" },
    });
    expect(screen.queryByText("Connect a provider")).not.toBeInTheDocument();
    fireEvent.click(screen.getByText("Go to Agents"));
    expect(onRun).toHaveBeenCalledOnce();
  });
});

describe("Toast", () => {
  it("renders with an Undo affordance for reversible actions", () => {
    const onUndo = vi.fn();
    render(<Toast variant="success" title="Agent deleted" onUndo={onUndo} />);
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    expect(onUndo).toHaveBeenCalledOnce();
  });
});
