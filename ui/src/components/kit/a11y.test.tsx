import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { axe } from "vitest-axe";
import { Boxes } from "lucide-react";

import {
  DataTable,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  StatusBadge,
  type Column,
} from "@/components/kit";

// WCAG 2.1 AA structural a11y gate (M100 UI99-7, ADR-locked target). The kit primitives render on
// nearly every console page, so gating THEM guards the whole surface. axe under jsdom covers the
// structural rules — accessible names, roles, aria, landmarks, list/table structure — but NOT
// color-contrast (no layout engine in jsdom); contrast + focus-visible are verified on the live
// visual loop (carded, m52.UI99-layout). A violation here fails the suite, so regressions can't land.

async function expectNoAxeViolations(ui: ReactElement) {
  const { container } = render(<MemoryRouter>{ui}</MemoryRouter>);
  // color-contrast needs a real layout engine (jsdom has no canvas) — disable it here and verify
  // contrast on the live visual loop. Everything else (names/roles/aria/landmarks) runs.
  expect(
    await axe(container, { rules: { "color-contrast": { enabled: false } } }),
  ).toHaveNoViolations();
}

type Row = { id: string; name: string };
const cols: Column<Row>[] = [
  { id: "name", header: "Name", cell: (r) => <span>{r.name}</span> },
];

describe("kit primitives — WCAG 2.1 AA structural a11y (M100 UI99-7)", () => {
  it("ErrorState (error variant, with retry) has no violations", async () => {
    await expectNoAxeViolations(
      <ErrorState description="Something failed." onRetry={() => {}} />,
    );
  });

  it("ErrorState (forbidden variant, resource-named) has no violations", async () => {
    await expectNoAxeViolations(
      <ErrorState variant="forbidden" resource="agents" />,
    );
  });

  it("ForbiddenInline has no violations", async () => {
    await expectNoAxeViolations(<ForbiddenInline resource="model routes" />);
  });

  it("EmptyState (icon + title + CTA) has no violations", async () => {
    await expectNoAxeViolations(
      <EmptyState
        icon={Boxes}
        title="No agents yet"
        description="Create one to get started."
        action={{ label: "New agent", onClick: () => {} }}
      />,
    );
  });

  it("StatusBadge (ready + pending) has no violations", async () => {
    await expectNoAxeViolations(
      <div>
        <StatusBadge ready phase="Ready" />
        <StatusBadge ready={false} phase="Pending" />
      </div>,
    );
  });

  it("DataTable (with rows) has no violations", async () => {
    await expectNoAxeViolations(
      <DataTable<Row>
        columns={cols}
        rows={[
          { id: "a", name: "alpha" },
          { id: "b", name: "beta" },
        ]}
        rowKey={(r) => r.id}
        ariaLabel="Things"
      />,
    );
  });

  it("DataTable (empty state) has no violations", async () => {
    await expectNoAxeViolations(
      <DataTable<Row>
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        ariaLabel="Things"
        empty={{ icon: Boxes, title: "Nothing here", description: "Add one." }}
      />,
    );
  });

  it("DataTable (forbidden error) has no violations", async () => {
    await expectNoAxeViolations(
      <DataTable<Row>
        columns={cols}
        rows={[]}
        rowKey={(r) => r.id}
        ariaLabel="Things"
        error={{ message: "denied", forbidden: true, resource: "agents" }}
      />,
    );
  });
});
