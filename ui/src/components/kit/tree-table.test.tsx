import { UNKNOWN_TITLE } from "./quantity";
import * as React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { axe } from "vitest-axe";

import {
  CellCount,
  TreeTable,
  TREE_GUTTER_MAX_LEVELS,
  TREE_ROW_HEIGHT,
  TREE_SUBROW_HEIGHT,
  type TreeColumn,
  type TreeRow,
} from "./tree-table";

// tree-table.test.tsx — the size-blind contract, pinned (spec §5.22 / §6.1 A3).
//
// The team page was redesigned around ONE component, so these tests are about
// the promises that made it the chosen design, not about markup:
//
//   • it reads the same at 2 agents and at 1,024 nodes;
//   • the remainder count is the SERVER's, never a client-side subtraction;
//   • ancestry is drawn (rules + elbows), so depth is legible without counting;
//   • unknown and zero never share a glyph;
//   • the whole tree is operable from the keyboard and walkable by a screen
//     reader — including the rows windowing has not put in the DOM.

type Node = { flow: number; held: number; failed: number | null };

const node = (flow: number, held: number, failed: number | null = 0): Node => ({
  flow,
  held,
  failed,
});

/** The small team: everything visible, nothing behind a chevron (§6.1 A3). */
function twoAgentTree(): TreeRow<Node>[] {
  return [
    { row: node(120, 0), depth: 0, kind: "root", expanded: true, childCount: 2 },
    { row: node(80, 0), depth: 1, kind: "leaf" },
    { row: node(40, 0), depth: 1, kind: "leaf" },
  ];
}

/**
 * The big team: a collapsed role, an expanded role showing only the leaves that
 * need a person, and the server's summary row behind them.
 */
function bigTeamTree(): TreeRow<Node>[] {
  return [
    { row: node(38120, 4), depth: 0, kind: "root", expanded: true, childCount: 3 },
    { row: node(9000, 0), depth: 1, kind: "group", expanded: true, childCount: 2 },
    { row: node(4000, 0), depth: 2, kind: "leaf" },
    { row: node(5000, 0), depth: 2, kind: "leaf" },
    {
      row: node(29000, 4),
      depth: 1,
      kind: "group",
      expanded: true,
      childCount: 255,
      needsPerson: 3,
    },
    { row: node(10, 2), depth: 2, kind: "leaf" },
    { row: node(11, 1), depth: 2, kind: "leaf" },
    { row: node(12, 1), depth: 2, kind: "leaf" },
    // The server's own remainder — the component must print it verbatim.
    { row: node(0, 0), depth: 2, kind: "summary", childCount: 252, needsPerson: 0 },
    { row: node(120, 0), depth: 1, kind: "group", expanded: false, childCount: 1 },
  ];
}

const NAMES = [
  "payments",
  "tier 1",
  "collector",
  "reconciler",
  "tier 2",
  "worker-a",
  "worker-b",
  "worker-c",
  "",
  "tools",
];

/** The §4.4 tree-outline budget, expressed exactly as a page would. */
function budgetColumns(): TreeColumn<Node>[] {
  return [
    { id: "kind", header: "Kind", priority: 4, cell: () => "agent" },
    { id: "state", header: "State", priority: 1, cell: () => "Serving" },
    {
      id: "delegations",
      header: "Delegations",
      priority: 2,
      numeric: true,
      cell: (r) => <CellCount value={r.row.flow} />,
    },
    {
      id: "held",
      header: "Held",
      priority: 2,
      numeric: true,
      cell: (r) => <CellCount value={r.row.held} tone="hold" />,
    },
    {
      id: "failed",
      header: "Failed",
      priority: 3,
      numeric: true,
      cell: (r) => <CellCount value={r.row.failed} tone="failed" />,
    },
    { id: "median", header: "Median", priority: 4, numeric: true, cell: () => "1.9s" },
    {
      id: "flow",
      header: "Flow",
      mobileOnly: true,
      cell: (r) => `${r.row.flow.toLocaleString()} · ${r.row.held} held`,
    },
    { id: "next", header: "Next step", priority: 1, cell: () => "Review 4 holds →" },
  ];
}

function renderTree(
  rows: TreeRow<Node>[],
  props: Partial<React.ComponentProps<typeof TreeTable<Node>>> = {},
) {
  const keyed = new Map(rows.map((r, i) => [r, i]));
  return render(
    <TreeTable<Node>
      rows={rows}
      columns={budgetColumns()}
      rowKey={(r) => `r${keyed.get(r)}`}
      name={(r) => NAMES[keyed.get(r) ?? 0] ?? "node"}
      ariaLabel="Team payments"
      {...props}
    />,
  );
}

const domRows = (c: HTMLElement) =>
  Array.from(c.querySelectorAll<HTMLTableRowElement>("tr[data-row-index]"));
const rowAt = (c: HTMLElement, i: number) =>
  c.querySelector<HTMLTableRowElement>(`[data-row-index="${i}"]`);

// ─────────────────────────────────────────────────────────────────────────────

describe("the size-blind contract — 2 agents and 1,024 read the same", () => {
  it("renders a two-agent team fully expanded, with nothing behind a chevron", () => {
    const { container } = renderTree(twoAgentTree());
    expect(domRows(container)).toHaveLength(3);
    // Only the root is expandable; the two members are leaves, so they carry no
    // aria-expanded at all (absent ≠ collapsed).
    expect(rowAt(container, 0)).toHaveAttribute("aria-expanded", "true");
    expect(rowAt(container, 1)).not.toHaveAttribute("aria-expanded");
    expect(rowAt(container, 2)).not.toHaveAttribute("aria-expanded");
  });

  it("prints the server's remainder verbatim and never derives it", () => {
    const { rerender } = renderTree(bigTeamTree());
    expect(screen.getByText("252 more, none need you")).toBeInTheDocument();

    // Show TWO more leaves under the same role. A component that computed the
    // remainder from what it can see would now say 250. This one must not move:
    // the number is the server's, and only the server can change it.
    const grown = bigTeamTree();
    grown.splice(8, 0, { row: node(13, 0), depth: 2, kind: "leaf" });
    grown.splice(9, 0, { row: node(14, 0), depth: 2, kind: "leaf" });
    const keyed = new Map(grown.map((r, i) => [r, i]));
    rerender(
      <TreeTable<Node>
        rows={grown}
        columns={budgetColumns()}
        rowKey={(r) => `g${keyed.get(r)}`}
        name={() => "n"}
        ariaLabel="Team payments"
      />,
    );
    expect(screen.getByText("252 more, none need you")).toBeInTheDocument();
    expect(screen.queryByText(/250 more/)).not.toBeInTheDocument();
  });

  it("says nothing numeric when the server reported no count", () => {
    const rows: TreeRow<Node>[] = [
      { row: node(1, 0), depth: 0, kind: "root", expanded: true, childCount: 2 },
      { row: node(1, 0), depth: 1, kind: "leaf" },
      { row: node(0, 0), depth: 1, kind: "summary" },
    ];
    renderTree(rows);
    expect(
      screen.getByText("More below — the count was not reported"),
    ).toBeInTheDocument();
  });

  it("names the holders when the remainder does need a person", () => {
    const rows: TreeRow<Node>[] = [
      { row: node(1, 0), depth: 0, kind: "root", expanded: true, childCount: 9 },
      {
        row: node(0, 0),
        depth: 1,
        kind: "summary",
        childCount: 1024,
        needsPerson: 7,
      },
    ];
    renderTree(rows);
    expect(
      screen.getByText(`${(1024).toLocaleString()} more, 7 need you`),
    ).toBeInTheDocument();
  });

  it("lets the caller replace the summary copy entirely", () => {
    renderTree(bigTeamTree(), {
      summaryLabel: () => "and the rest are fine",
    });
    expect(screen.getByText("and the rest are fine")).toBeInTheDocument();
  });
});

describe("1,024 nodes — the requirement the design exists to satisfy", () => {
  /** root + 32 roles × 32 members = exactly 1,024 leaf nodes, 1,057 rows. */
  function hugeTree(): TreeRow<Node>[] {
    const rows: TreeRow<Node>[] = [
      { row: node(38120, 4), depth: 0, kind: "root", expanded: true, childCount: 32 },
    ];
    for (let g = 0; g < 32; g++) {
      rows.push({
        row: node(1000 + g, g % 3),
        depth: 1,
        kind: "group",
        expanded: true,
        childCount: 32,
      });
      for (let l = 0; l < 32; l++) {
        rows.push({ row: node(l, 0), depth: 2, kind: "leaf" });
      }
    }
    return rows;
  }

  it("windows a 1,024-node tree instead of exploding, and tells AT the true size", () => {
    const rows = hugeTree();
    expect(rows).toHaveLength(1057);

    const started = Date.now();
    const { container } = renderTree(rows, {
      rowKey: (r) => `h${rows.indexOf(r)}`,
    });
    const elapsed = Date.now() - started;

    // Windowed: only the visible slice + overscan is in the DOM.
    const rendered = domRows(container);
    expect(rendered.length).toBeGreaterThan(0);
    expect(rendered.length).toBeLessThan(rows.length / 4);

    // …but the tree never CLAIMS to be shorter than it is. aria-rowcount is the
    // true total (+1 for the header row), which is what makes the windowing
    // honest rather than a silent truncation.
    const grid = container.querySelector('[role="treegrid"]');
    expect(grid).toHaveAttribute("aria-rowcount", String(rows.length + 1));

    // And the spacers reserve the full scroll extent, so the scrollbar tells the
    // truth about how much tree is below.
    const spacers = Array.from(
      container.querySelectorAll<HTMLTableRowElement>("tr[aria-hidden='true']"),
    );
    const reserved = spacers.reduce(
      (sum, tr) => sum + parseInt(tr.style.height || "0", 10),
      0,
    );
    const bodyHeight = rendered.reduce(
      (sum, tr) => sum + parseInt(tr.style.height || "0", 10),
      0,
    );
    const expected =
      TREE_ROW_HEIGHT * 33 + TREE_SUBROW_HEIGHT * 1024; // 33 group-rhythm rows
    expect(reserved + bodyHeight).toBe(expected);

    expect(elapsed).toBeLessThan(4000);
  }, 20_000);

  it("renders every one of the 1,057 rows when windowing is switched off", () => {
    const rows = hugeTree();
    const { container } = renderTree(rows, {
      rowKey: (r) => `h${rows.indexOf(r)}`,
      virtualizeThreshold: Infinity,
    });
    // No spacers, no windowing — the full tree, correct at depth 3.
    expect(domRows(container)).toHaveLength(1057);
    expect(container.querySelectorAll("tr[aria-hidden='true']")).toHaveLength(0);
    expect(rowAt(container, 1056)).toHaveAttribute("aria-level", "3");
  }, 30_000);

  it("keeps every windowed row reachable from the keyboard", () => {
    const rows = hugeTree();
    const { container } = renderTree(rows, {
      rowKey: (r) => `h${rows.indexOf(r)}`,
    });
    // The last row starts far outside the rendered window…
    expect(rowAt(container, 1056)).toBeNull();

    const first = rowAt(container, 0)!;
    act(() => first.focus());
    fireEvent.keyDown(first, { key: "End" });

    // …and End pulls it into the DOM and onto the focus, rather than dead-ending
    // navigation at the edge of the window.
    const last = rowAt(container, 1056);
    expect(last).not.toBeNull();
    expect(document.activeElement).toBe(last);
    expect(last).toHaveAttribute("aria-rowindex", "1058");
  }, 20_000);

  it("reveals later rows on scroll", () => {
    const rows = hugeTree();
    const { container } = renderTree(rows, {
      rowKey: (r) => `h${rows.indexOf(r)}`,
    });
    const frame = container.querySelector<HTMLDivElement>(".overflow-x-auto")!;
    // jsdom has no layout, so scrollTop is inert — define it, then fire.
    Object.defineProperty(frame, "scrollTop", { value: 20000, writable: true });
    fireEvent.scroll(frame);

    const rendered = domRows(container).map((tr) =>
      Number(tr.getAttribute("data-row-index")),
    );
    expect(Math.min(...rendered)).toBeGreaterThan(400);
    expect(rendered.length).toBeLessThan(rows.length / 4);
  }, 20_000);
});

describe("ancestry is drawn, not implied", () => {
  it("gives every level a gutter column and closes the last child's rule", () => {
    const { container } = renderTree(bigTeamTree());

    // Root: depth 0, so no gutter at all.
    expect(
      within(rowAt(container, 0)!).queryByTestId("tree-gutter"),
    ).toBeNull();

    // A depth-2 leaf: two gutter columns (one ancestor rule + its own elbow).
    const leaf = within(rowAt(container, 2)!).getByTestId("tree-gutter");
    expect(leaf).toHaveAttribute("data-levels", "2");
    // Its role has a later sibling ("tier 2"), so the ancestor rule runs through.
    expect(leaf.querySelectorAll("[data-rule='ancestor']")).toHaveLength(1);
    // It is not the last member of its role, so its own rule runs through too.
    expect(leaf.querySelector("[data-rule='elbow-through']")).not.toBeNull();
    expect(leaf.querySelector("[data-rule='elbow-arm']")).not.toBeNull();

    // The LAST member of that role gets the half-height stub — the branch
    // visibly ends instead of running into the next role's rows.
    const lastLeaf = within(rowAt(container, 3)!).getByTestId("tree-gutter");
    expect(lastLeaf.querySelector("[data-rule='elbow-stub']")).not.toBeNull();
    expect(lastLeaf.querySelector("[data-rule='elbow-through']")).toBeNull();
  });

  it("drops the ancestor rule once that ancestor has no rows left below it", () => {
    // A rule is drawn for an ancestor ONLY while that ancestor still has rows
    // below it. Here the single role is the last one, so nothing continues past.
    const rows: TreeRow<Node>[] = [
      { row: node(1, 0), depth: 0, kind: "root", expanded: true, childCount: 1 },
      { row: node(1, 0), depth: 1, kind: "group", expanded: true, childCount: 1 },
      { row: node(1, 0), depth: 2, kind: "leaf" },
    ];
    const { container } = renderTree(rows);
    const gut = within(rowAt(container, 2)!).getByTestId("tree-gutter");
    expect(gut).toHaveAttribute("data-levels", "2");
    expect(gut.querySelectorAll("[data-rule='ancestor']")).toHaveLength(0);
    // …and the leaf's own elbow is a stub, because it is the last child too.
    expect(gut.querySelector("[data-rule='elbow-stub']")).not.toBeNull();
  });

  it("caps the gutter past 8 levels and states the real depth instead", () => {
    const rows: TreeRow<Node>[] = Array.from({ length: 14 }, (_, d) => ({
      row: node(0, 0),
      depth: d,
      kind: d === 0 ? ("root" as const) : ("group" as const),
      expanded: d < 13 ? true : undefined,
      childCount: 1,
    }));
    const { container } = renderTree(rows);

    const deep = within(rowAt(container, 13)!).getByTestId("tree-gutter");
    // Indent stops growing…
    expect(deep).toHaveAttribute("data-levels", String(TREE_GUTTER_MAX_LEVELS));
    // …while the row still says exactly how deep it is, in text and in ARIA.
    expect(within(rowAt(container, 13)!).getByText("d14 ·")).toBeInTheDocument();
    expect(rowAt(container, 13)).toHaveAttribute("aria-level", "14");
    // A row inside the cap is drawn at its true level.
    expect(
      within(rowAt(container, 5)!).getByTestId("tree-gutter"),
    ).toHaveAttribute("data-levels", "5");
  });
});

describe("disclosure — roles collapse by default, expansion is the caller's", () => {
  it("toggles from the chevron without touching its own state", () => {
    const onToggle = vi.fn();
    const rows = bigTeamTree();
    const { container } = renderTree(rows, { onToggle });

    const collapsed = rowAt(container, 9)!; // "tools", expanded: false
    expect(collapsed).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(within(collapsed).getByTestId("tree-chevron"));
    expect(onToggle).toHaveBeenCalledWith(rows[9], true);
    // Controlled: the row does NOT flip itself.
    expect(rowAt(container, 9)).toHaveAttribute("aria-expanded", "false");
  });

  it("expands and collapses from the keyboard alone", () => {
    const onToggle = vi.fn();
    const rows = bigTeamTree();
    const { container } = renderTree(rows, { onToggle });

    const collapsed = rowAt(container, 9)!;
    act(() => collapsed.focus());
    fireEvent.keyDown(collapsed, { key: "ArrowRight" });
    expect(onToggle).toHaveBeenLastCalledWith(rows[9], true);

    const expanded = rowAt(container, 1)!;
    act(() => expanded.focus());
    fireEvent.keyDown(expanded, { key: "ArrowLeft" });
    expect(onToggle).toHaveBeenLastCalledWith(rows[1], false);
  });

  it("steps into the first child and back out to the parent", () => {
    const { container } = renderTree(bigTeamTree());
    const role = rowAt(container, 1)!; // expanded "tier 1"
    act(() => role.focus());
    fireEvent.keyDown(role, { key: "ArrowRight" }); // already open → first child
    expect(document.activeElement).toBe(rowAt(container, 2));

    fireEvent.keyDown(rowAt(container, 2)!, { key: "ArrowLeft" }); // leaf → parent
    expect(document.activeElement).toBe(rowAt(container, 1));
  });

  it("roves with the arrow keys and jumps with Home/End", () => {
    const { container } = renderTree(bigTeamTree());
    const first = rowAt(container, 0)!;
    act(() => first.focus());
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(document.activeElement).toBe(rowAt(container, 1));
    fireEvent.keyDown(rowAt(container, 1)!, { key: "ArrowUp" });
    expect(document.activeElement).toBe(rowAt(container, 0));
    fireEvent.keyDown(rowAt(container, 0)!, { key: "End" });
    expect(document.activeElement).toBe(rowAt(container, 9));
    fireEvent.keyDown(rowAt(container, 9)!, { key: "Home" });
    expect(document.activeElement).toBe(rowAt(container, 0));
  });

  it("keeps exactly one tab stop across the whole tree", () => {
    const { container } = renderTree(bigTeamTree());
    const tabbable = domRows(container).filter(
      (tr) => tr.getAttribute("tabindex") === "0",
    );
    expect(tabbable).toHaveLength(1);
    // The chevron is a mouse target, never a second tab stop per row.
    expect(container.querySelectorAll("[data-testid='tree-chevron'][tabindex]"))
      .toHaveLength(0);
  });

  it("follows the row's next step on Enter, and pages the remainder in on the summary", () => {
    const onActivate = vi.fn();
    const onShowAll = vi.fn();
    const rows = bigTeamTree();
    const { container } = renderTree(rows, { onActivate, onShowAll });

    fireEvent.keyDown(rowAt(container, 2)!, { key: "Enter" });
    expect(onActivate).toHaveBeenCalledWith(rows[2]);

    fireEvent.keyDown(rowAt(container, 8)!, { key: "Enter" });
    expect(onShowAll).toHaveBeenCalledWith(rows[8]);
    expect(onActivate).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Show all →" }));
    expect(onShowAll).toHaveBeenCalledTimes(2);
  });
});

describe("ARIA — a screen reader can walk the whole tree", () => {
  it("is a treegrid whose rows carry level, expansion and row index", () => {
    const { container } = renderTree(bigTeamTree());
    const grid = screen.getByRole("treegrid", { name: "Team payments" });
    expect(grid).toHaveAttribute("aria-rowcount", "11");

    expect(rowAt(container, 0)).toHaveAttribute("aria-level", "1");
    expect(rowAt(container, 1)).toHaveAttribute("aria-level", "2");
    expect(rowAt(container, 2)).toHaveAttribute("aria-level", "3");
    expect(rowAt(container, 2)).toHaveAttribute("aria-rowindex", "4");
    expect(container.querySelectorAll("[role='gridcell']").length).toBeGreaterThan(0);
  });

  it("reports the server's set size and stays silent about position when the set is windowed", () => {
    const { container } = renderTree(bigTeamTree());
    // tier 1 shows both of its 2 children → position is knowable.
    expect(rowAt(container, 2)).toHaveAttribute("aria-setsize", "2");
    expect(rowAt(container, 2)).toHaveAttribute("aria-posinset", "1");

    // tier 2 shows 3 of 255 → the TRUE set size, and no invented position.
    expect(rowAt(container, 5)).toHaveAttribute("aria-setsize", "255");
    expect(rowAt(container, 5)).not.toHaveAttribute("aria-posinset");
  });

  it("passes axe with a collapsed role, an expanded role and a summary row", async () => {
    const { container } = renderTree(bigTeamTree());
    expect(
      await axe(container, { rules: { "color-contrast": { enabled: false } } }),
    ).toHaveNoViolations();
  });

  it("passes axe while windowed", async () => {
    const rows: TreeRow<Node>[] = [
      { row: node(1, 0), depth: 0, kind: "root", expanded: true, childCount: 200 },
      ...Array.from({ length: 200 }, () => ({
        row: node(1, 0),
        depth: 1,
        kind: "leaf" as const,
      })),
    ];
    const { container } = renderTree(rows, {
      rowKey: (r) => `w${rows.indexOf(r)}`,
    });
    expect(container.querySelectorAll("tr[aria-hidden='true']").length).toBeGreaterThan(0);
    expect(
      await axe(container, { rules: { "color-contrast": { enabled: false } } }),
    ).toHaveNoViolations();
  }, 20_000);

  it("passes axe in the loading, empty and error states", async () => {
    const opts = { rules: { "color-contrast": { enabled: false } } };
    const { container: a } = renderTree([], { loading: true });
    expect(await axe(a, opts)).toHaveNoViolations();

    const { container: b } = renderTree([], {
      empty: {
        title: "This team has no members yet.",
        description: "Add agents to its roster.",
      },
    });
    expect(await axe(b, opts)).toHaveNoViolations();

    const { container: c } = renderTree([], {
      error: { message: "The team API is unreachable.", onRetry: () => {} },
    });
    expect(await axe(c, opts)).toHaveNoViolations();
  }, 20_000);
});

describe("unknown and zero never share a glyph (§7.1)", () => {
  it("renders a known zero as a real 0 and an unknown as a titled dash", () => {
    render(
      <table>
        <tbody>
          <tr>
            <td>
              <CellCount value={0} />
            </td>
            <td>
              <CellCount value={null} unknownTitle="No trace backend." />
            </td>
            <td>
              <CellCount value={undefined} />
            </td>
          </tr>
        </tbody>
      </table>,
    );
    expect(screen.getByText("0")).toBeInTheDocument();
    const dashes = screen.getAllByText("—");
    expect(dashes).toHaveLength(2);
    expect(dashes[0]).toHaveAttribute("title", "No trace backend.");
    expect(dashes[1]).toHaveAttribute("title", UNKNOWN_TITLE);
    // An unknown is INFORMATION, so it may never land in the decoration ink.
    for (const d of dashes) expect(d.className).not.toContain("text-ghost");
  });

  it("tints a nonzero held/failed count and leaves a zero unremarkable", () => {
    render(
      <table>
        <tbody>
          <tr>
            <td>
              <CellCount value={4} tone="hold" />
            </td>
            <td>
              <CellCount value={2} tone="failed" />
            </td>
            <td>
              <CellCount value={0} tone="hold" />
            </td>
          </tr>
        </tbody>
      </table>,
    );
    expect(screen.getByText("4").className).toContain("text-hold");
    expect(screen.getByText("2").className).toContain("text-destructive");
    // Zero is not a hold — a zero-held role must not wear the hold hue.
    expect(screen.getByText("0").className).not.toContain("text-hold");
  });

  it("groups digits by locale and never truncates them", () => {
    render(
      <table>
        <tbody>
          <tr>
            <td>
              <CellCount value={38120} />
            </td>
          </tr>
        </tbody>
      </table>,
    );
    expect(screen.getByText((38120).toLocaleString())).toBeInTheDocument();
  });
});

describe("fit — the §4.4 budget and the §4.6 scroll container", () => {
  it("maps column priorities to the documented breakpoints", () => {
    const { container } = renderTree(bigTeamTree());
    const head = (id: string) =>
      Array.from(container.querySelectorAll("th")).find((th) =>
        th.textContent?.trim().toLowerCase().startsWith(id),
      )!;
    expect(head("kind").className).toContain("hidden xl:table-cell");
    expect(head("median").className).toContain("hidden xl:table-cell");
    expect(head("failed").className).toContain("hidden lg:table-cell");
    expect(head("delegations").className).toContain("hidden md:table-cell");
    expect(head("held").className).toContain("hidden md:table-cell");
    // Never dropped.
    expect(head("state").className).not.toContain("hidden");
    expect(head("next step").className).not.toContain("hidden");
  });

  it("swaps Delegations+Held for one flow cell below 768", () => {
    const { container } = renderTree(bigTeamTree());
    const flowHead = Array.from(container.querySelectorAll("th")).find(
      (th) => th.textContent?.trim() === "Flow",
    )!;
    // The merged cell is the mirror image of the two it replaces: present only
    // where they are absent, so no width is ever counted twice.
    expect(flowHead.className).toContain("md:hidden");
    expect(flowHead.className).not.toContain("hidden md:table-cell");
    expect(screen.getAllByText(/38,?120 · 4 held|38120 · 4 held/).length).toBeGreaterThan(0);
  });

  it("owns its horizontal scrolling and can always shrink", () => {
    const { container } = renderTree(bigTeamTree());
    const frame = container.querySelector<HTMLDivElement>(".overflow-x-auto")!;
    expect(frame.className).toContain("min-w-0");
    expect(frame.className).toContain("max-w-full");
    expect(frame.className).toContain("rounded-lg");
    expect(frame.className).not.toContain("overflow-hidden");
    // The tree column keeps its floor so depth never squeezes names away.
    expect(container.querySelector("th")!.className).toContain("min-w-[280px]");
  });

  it("puts numeric cells in the cell-num register", () => {
    const { container } = renderTree(bigTeamTree());
    const held = rowAt(container, 1)!.querySelectorAll("td")[4];
    expect(held.className).toContain("font-mono");
    expect(held.className).toContain("tabular-nums");
    expect(held.className).toContain("text-right");
    expect(held.className).toContain("whitespace-nowrap");
  });

  it("gives sub-rows the 40px band against the parent's 44px", () => {
    const { container } = renderTree(bigTeamTree());
    expect(rowAt(container, 0)!.style.height).toBe(`${TREE_ROW_HEIGHT}px`);
    expect(rowAt(container, 1)!.style.height).toBe(`${TREE_ROW_HEIGHT}px`);
    expect(rowAt(container, 2)!.style.height).toBe(`${TREE_SUBROW_HEIGHT}px`);
    expect(rowAt(container, 2)!.className).toContain("bg-surface-2");
    expect(rowAt(container, 8)!.style.height).toBe(`${TREE_SUBROW_HEIGHT}px`);
  });
});

describe("honest degraded states (§7 A3)", () => {
  it("shows tree-shaped skeletons while loading, not the grid", () => {
    const { container } = renderTree([], { loading: true });
    expect(screen.getByRole("status", { name: "Loading" })).toBeInTheDocument();
    expect(container.querySelector("[role='treegrid']")).toBeNull();
  });

  it("teaches on an empty roster", () => {
    renderTree([], {
      empty: {
        title: "This team has no members yet.",
        description: "Add agents to its roster.",
      },
    });
    expect(screen.getByText("This team has no members yet.")).toBeInTheDocument();
  });

  it("names the resource on a 403 and never leaks the RBAC string", () => {
    renderTree([], {
      error: {
        message: 'teams.ctxmesh.io "payments" is forbidden: User cannot get',
        forbidden: true,
        resource: "teams",
      },
    });
    expect(screen.queryByText(/is forbidden: User cannot get/)).toBeNull();
    expect(screen.getByText(/permission/i)).toBeInTheDocument();
  });
});
