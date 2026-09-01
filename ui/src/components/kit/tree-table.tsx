import * as React from "react";

import {
  cellNum,
  type Column,
  type DataTableError,
  PRIORITY_CLASS,
  columnPriority,
} from "@/components/kit/data-table";
import { EmptyState, type EmptyStateProps } from "@/components/kit/empty-state";
import { ErrorState } from "@/components/kit/error-state";
import { Skeleton } from "@/components/kit/skeleton";
import { cn } from "@/lib/utils";
import { UNKNOWN_GLYPH, UNKNOWN_TITLE, ZERO_CLASS } from "./quantity";

// TreeTable — the Option-C outline primitive (spec §5.22, archetype A3). It is
// DataTable semantics bent around a hierarchy: same frame, same column budget
// (§4.4), same numeric register (§4.8), same honest degraded states (§7) — plus
// a gutter that draws ancestry and a disclosure contract that survives 1,024
// roles.
//
// ── THE SIZE-BLIND CONTRACT (the reason this component exists) ──────────────
// A team page must read the same at 2 agents and at 1,024. Two mechanisms, and
// neither of them is "render everything and hope":
//
//   1. THE SERVER WINDOWS THE TREE, NOT US. Roles collapse by default; expanding
//      one asks the backend for the leaves that NEED A PERSON, and the backend
//      also says how many it left behind. That remainder arrives as a `summary`
//      row carrying `childCount` (and optionally `needsPerson`) and renders as
//      "252 more, none need you". THE COMPONENT NEVER COMPUTES THAT NUMBER.
//      There is deliberately no `rows.length`-derived count anywhere below: a
//      client-side subtraction would be a guess dressed as a fact the moment the
//      backend pages, filters, or races. Everything printed came from the caller.
//
//   2. THE VIEWPORT WINDOWS THE DOM. Above `virtualizeThreshold` rows, the body
//      renders only the visible slice plus an overscan, with exact-height spacer
//      rows above and below. This is HONEST windowing, not truncation: the
//      scroll extent equals the full list's height, every row is reachable by
//      scrolling or by ↑/↓ (the focused row is always forced into the rendered
//      slice), and `aria-rowcount` reports the TRUE total so assistive tech is
//      never told the list is shorter than it is. Nothing is dropped and nothing
//      claims to be complete when it is not.
//
// ── ANCESTRY IS DRAWN, NOT IMPLIED ─────────────────────────────────────────
// Plain padding leaves a reader counting pixels to answer "whose child is this?".
// Instead each level gets a 20px gutter column holding a 1px `border-strong`
// rule: a full-height vertical for every ancestor that still has rows below it,
// and at the row's own level an elbow (vertical to mid-height, then an 11px
// horizontal). Last children get the half-height stub, so the rule visibly ENDS.
// The gutter is `aria-hidden` — it is a picture of the `aria-level` a screen
// reader already gets. Depth is unlimited; the gutter is not: past
// TREE_GUTTER_MAX_LEVELS it caps at 160px and the row prefixes a `d9 ·` chip, so
// a 30-deep tree never squeezes names into ellipsis-only (§5.22, §4.5).
//
// ── ARIA: role="treegrid", row-focus flavour ───────────────────────────────
// The table is a `treegrid`; rows carry `aria-level`, `aria-expanded` (only when
// expandable), `aria-setsize`/`aria-posinset`, and `aria-rowindex` against the
// table's `aria-rowcount`. Focus lives on the ROW (roving tabindex), which is
// the APG treegrid pattern that survives windowing: ↑/↓ rove, →/← expand/collapse
// or step to child/parent, Home/End jump, Enter follows the row's next step.
// `aria-setsize` is only ever the SERVER's `childCount` or the count of rows we
// actually rendered — when the parent knows a bigger set than we show, we report
// the true setsize and omit posinset rather than claim a false position.
//
// This component is CONTROLLED, exactly like DataTable: the parent owns
// expansion, the flattened row list, and the fetching. TreeTable is presentation
// plus affordances.

/** Group / root rows: the §4.1 table rhythm. */
export const TREE_ROW_HEIGHT = 44;
/** Leaf and summary rows: the 40px `sub` band (§4.1, §5.22). */
export const TREE_SUBROW_HEIGHT = 40;
/** One gutter column per level (§5.22). */
export const TREE_INDENT_PX = 20;
/**
 * The gutter stops growing here (8 × 20px = 160px). Deeper rows keep their real
 * `aria-level` and gain a `d{n} ·` chip — unlimited depth, bounded indent.
 */
// Parity with DataTable: if the table has to scroll, the one column the design
// calls never-dropped is the one that must stay visible. TreeTable lacked this,
// so a wide roster pushed "Next step" out of sight at rest — the same defect the
// fit gate was taught to see on the flat table.
const PINNED_CELL = "sticky right-0 z-10 bg-inherit border-l border-border-soft";
const PINNED_HEAD = "sticky right-0 z-20 bg-card border-l border-border-soft";

function isPinnedCol<T>(col: TreeColumn<T>, index: number, total: number): boolean {
  return index === total - 1 && columnPriority(col) === 1 && !col.mobileOnly;
}

export const TREE_GUTTER_MAX_LEVELS = 8;

/**
 * A KNOWN zero (§4.5 / §5.22 / §7.1: "known zero is always a real 0, mono,
 * text-ghost when unremarkable"). Isolated as a constant because it sits on a
 * real tension: the token doctrine says `ghost` is decoration and must never
 * carry information, while §5.22 asks for it here. The spec wins for now because
 * the GLYPH carries the whole meaning — `0` versus the `—` an unknown gets — so
 * nothing is lost if the colour is not read. Flip this one line to `text-faint`
 * if the doctrine is tightened.
 */
// ZERO_CLASS now comes from ./quantity — see the note there on why a
// known zero is the one sanctioned use of the ghost register.
export { ZERO_CLASS };

/**
 * priority → responsive visibility (§4.4). Mirrors DataTable's private map; the
 * literal strings must stay literal so Tailwind's scanner emits them. Kept in
 * step with data-table.tsx by hand (it does not export the map).
 */

/** DataTable's bridge, repeated: explicit priority wins, else the legacy alias. */

export type TreeRowKind = "root" | "group" | "leaf" | "summary";

/**
 * One row of an ALREADY-FLATTENED tree (§5.22). The parent flattens because the
 * parent owns expansion: a collapsed group simply contributes no descendant rows.
 */
export interface TreeRow<T> {
  /** The caller's payload. Column cells receive the whole TreeRow, not this. */
  row: T;
  /** 0-based. Unlimited (the gutter caps, the level does not). */
  depth: number;
  kind: TreeRowKind;
  /**
   * Expansion state. `undefined` means NOT EXPANDABLE — that is the only signal
   * the component uses, so a leaf never grows a dead chevron and a group with
   * unloaded children still gets one.
   */
  expanded?: boolean;
  /**
   * SERVER-supplied. On a group: how many children exist in total (drives an
   * honest `aria-setsize` when we render fewer). On a `summary` row: how many
   * rows this summary stands for. Never derived here.
   */
  childCount?: number;
  /** SERVER-supplied count of those children that need a person. Never derived. */
  needsPerson?: number;
}

/** Name tint for a row that carries holds or failures (§5.22). */
export type TreeNameTone = "hold" | "failed";

/**
 * A tree column is a DataTable `Column` over `TreeRow<T>` — same `priority`,
 * `numeric`, `className`, `cell` contract — plus the §4.4 merge slot.
 *
 * `sortable` is accepted for type compatibility but ignored: a hierarchy has
 * exactly one correct order, and re-sorting it would orphan every child.
 */
export interface TreeColumn<T> extends Column<TreeRow<T>> {
  /**
   * The §4.4 MERGE slot. The tree-outline budget does not merely drop columns at
   * 768 — it MERGES Delegations + Held into one flow cell (`38,120 · 4 held`).
   * A column marked `mobileOnly` renders only BELOW `md`; pair it with the two
   * `priority: 2` columns it replaces (which hide at the same breakpoint) and the
   * swap is exact, with no width ever counted twice.
   */
  mobileOnly?: boolean;
}

export interface TreeTableProps<T> {
  /** The flattened, already-expanded row list. */
  rows: TreeRow<T>[];
  /** Trailing columns; the tree column is column 0 and is built from `name`. */
  columns: TreeColumn<T>[];
  /** Stable row key. */
  rowKey: (row: TreeRow<T>) => string;

  /** The tree cell's name (mono; leaves render at weight 400, groups 600). */
  name: (row: TreeRow<T>) => React.ReactNode;
  /** Full value for the name's `title` when `name` is not a plain string (§4.5). */
  nameTitle?: (row: TreeRow<T>) => string | undefined;
  /** Role/kind suffix rendered mono `text-2xs text-faint` — `· tier 2`, `· tool`. */
  suffix?: (row: TreeRow<T>) => React.ReactNode;
  /** Tint a role row that carries holds or failures. */
  nameTone?: (row: TreeRow<T>) => TreeNameTone | undefined;

  /** Head of the tree column. */
  treeHeader?: React.ReactNode;
  /** Tree column classes; the §5.22 `min-w-[280px]` is the default. */
  treeColumnClassName?: string;

  /** Disclosure. Fired by the chevron, by →/←, and by clicking an expandable row. */
  onToggle?: (row: TreeRow<T>, next: boolean) => void;
  /** Enter / row click on a non-expandable row — follows the row's next step. */
  onActivate?: (row: TreeRow<T>) => void;
  /** A `summary` row's "Show all →" — pages the remainder in via the cursor. */
  onShowAll?: (row: TreeRow<T>) => void;
  /**
   * Overrides the summary copy entirely. The default formats ONLY the numbers
   * the caller put on the row (`childCount`, `needsPerson`) and says so plainly
   * when they are absent — it never subtracts, estimates, or invents a total.
   */
  summaryLabel?: (row: TreeRow<T>) => React.ReactNode;
  showAllLabel?: string;

  loading?: boolean;
  error?: DataTableError | null;
  /** Teaching empty state when the tree is genuinely empty (§7 A3). */
  empty?: EmptyStateProps;

  /** Accessible name for the treegrid. */
  ariaLabel?: string;
  /**
   * Above this many rows the body windows to the visible slice. Pass `Infinity`
   * to render every row (tests, print, tiny trees).
   */
  virtualizeThreshold?: number;
  /** `min-w-*` for the `<table>` from the §4.4 budget totals. */
  tableClassName?: string;
  className?: string;
}

function heightForKind(kind: TreeRowKind): number {
  return kind === "leaf" || kind === "summary"
    ? TREE_SUBROW_HEIGHT
    : TREE_ROW_HEIGHT;
}

interface RowMeta {
  /** Per ancestor level: does that ancestor still have rows below it? */
  ancestors: boolean[];
  /** True when no later sibling shares this depth — the elbow becomes a stub. */
  last: boolean;
  /** Index of the parent row in the flat list, or -1 at the root. */
  parentIndex: number;
  /** 1-based position among RENDERED siblings (0 for summary rows). */
  posInSet: number;
  /** How many siblings we actually rendered under this parent. */
  siblingCount: number;
  height: number;
}

/**
 * One O(n) pass pair over the flat list producing everything the gutter, the
 * keyboard, the ARIA and the windowing need. Done once per row-list identity, so
 * a 1,024-row tree costs one linear sweep, not a scan per rendered row.
 */
function computeMeta<T>(rows: TreeRow<T>[]): {
  meta: RowMeta[];
  offsets: number[];
} {
  const n = rows.length;
  const depthAt = (i: number) => {
    const d = Math.floor(rows[i].depth);
    return Number.isFinite(d) && d > 0 ? d : 0;
  };

  // Backward: does a row at the same depth follow, with nothing shallower
  // between? That is exactly "has a next sibling", and it decides whether the
  // vertical rule continues past this row.
  const hasNext = new Array<boolean>(n).fill(false);
  const openAt: boolean[] = [];
  for (let i = n - 1; i >= 0; i--) {
    const d = depthAt(i);
    hasNext[i] = openAt[d] === true;
    openAt.length = d + 1; // everything deeper belonged to this row's subtree
    openAt[d] = true;
  }

  // Forward: carry the ancestors' flags and identities down the spine.
  const meta = new Array<RowMeta>(n);
  const flagStack: boolean[] = [];
  const parentStack: number[] = [];
  const tally = new Map<number, number>();
  for (let i = 0; i < n; i++) {
    const d = depthAt(i);
    const ancestors = flagStack.slice(0, d);
    while (ancestors.length < d) ancestors.push(false); // tolerate a depth jump
    const parentIndex = d > 0 ? (parentStack[d - 1] ?? -1) : -1;
    let posInSet = 0;
    if (rows[i].kind !== "summary") {
      posInSet = (tally.get(parentIndex) ?? 0) + 1;
      tally.set(parentIndex, posInSet);
    }
    meta[i] = {
      ancestors,
      last: !hasNext[i],
      parentIndex,
      posInSet,
      siblingCount: 0,
      height: heightForKind(rows[i].kind),
    };
    flagStack.length = d;
    flagStack[d] = hasNext[i];
    parentStack.length = d;
    parentStack[d] = i;
  }
  for (let i = 0; i < n; i++) {
    meta[i].siblingCount = tally.get(meta[i].parentIndex) ?? 0;
  }

  const offsets = new Array<number>(n + 1);
  offsets[0] = 0;
  for (let i = 0; i < n; i++) offsets[i + 1] = offsets[i] + meta[i].height;

  return { meta, offsets };
}

/** Largest row index whose top offset is at or above `y`. */
function indexAtOffset(offsets: number[], y: number): number {
  let lo = 0;
  let hi = offsets.length - 2;
  if (hi < 0) return 0;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (offsets[mid] <= y) lo = mid;
    else hi = mid - 1;
  }
  return lo;
}

/**
 * The ancestry drawing (§5.22). `aria-hidden` throughout — it restates
 * `aria-level`, and a screen reader reading "vertical line, vertical line,
 * elbow" would be worse than silence.
 */
function TreeGutter({
  ancestors,
  last,
  levels,
}: {
  ancestors: boolean[];
  last: boolean;
  levels: number;
}) {
  if (levels <= 0) return null;
  const cells: React.ReactNode[] = [];
  for (let l = 0; l < levels - 1; l++) {
    cells.push(
      <span key={l} className="relative block w-5 shrink-0 self-stretch">
        {ancestors[l] === true && (
          <span
            data-rule="ancestor"
            className="absolute inset-y-0 left-0 block w-px bg-border-strong"
          />
        )}
      </span>,
    );
  }
  cells.push(
    <span key="elbow" className="relative block w-5 shrink-0 self-stretch">
      <span
        data-rule={last ? "elbow-stub" : "elbow-through"}
        className={cn(
          "absolute left-0 top-0 block w-px bg-border-strong",
          // A last child's rule STOPS at the elbow — that stub is how the eye
          // knows the branch ended without counting rows.
          last ? "h-1/2" : "h-full",
        )}
      />
      <span
        data-rule="elbow-arm"
        className="absolute left-0 top-1/2 block h-px w-[11px] bg-border-strong"
      />
    </span>,
  );
  return (
    <span
      aria-hidden="true"
      data-testid="tree-gutter"
      data-levels={levels}
      className="flex shrink-0 items-stretch self-stretch"
    >
      {cells}
    </span>
  );
}

/**
 * A count in a tree cell (§4.5, §4.8, §7.1). Unknown and zero never share a
 * glyph: a known zero is a real `0`, an unknown is `—` with a `title` saying so.
 * Nonzero held/failed counts take their hue and weight (§5.22).
 *
 * Pass `undefined`/`null` for "the backend cannot answer" — never 0.
 */
export function CellCount({
  value,
  tone,
  unknownTitle,
  className,
}: {
  value: number | null | undefined;
  tone?: TreeNameTone;
  /** Why it is unknown, e.g. "No trace backend is configured." */
  unknownTitle?: string;
  className?: string;
}) {
  if (value === null || value === undefined || Number.isNaN(value)) {
    return (
      // text-faint, not ghost: an unknown is INFORMATION and has to be readable.
      <span
        className={cn("text-faint", className)}
        title={unknownTitle ?? UNKNOWN_TITLE}
      >
        {UNKNOWN_GLYPH}
      </span>
    );
  }
  const emphasis =
    value === 0
      ? ZERO_CLASS
      : tone === "hold"
        ? "text-hold font-semibold"
        : tone === "failed"
          ? "text-destructive font-semibold"
          : undefined;
  return (
    <span className={cn(emphasis, className)}>{value.toLocaleString()}</span>
  );
}

/**
 * The default summary copy. Every number here came off the row the caller built
 * from the server's answer; when the server said nothing, this says nothing
 * numeric rather than subtracting one list length from another.
 */
function defaultSummaryLabel<T>(row: TreeRow<T>): React.ReactNode {
  const total = row.childCount;
  if (total === null || total === undefined || Number.isNaN(total)) {
    return (
      <span title="The server did not report how many rows are behind this summary.">
        More below — the count was not reported
      </span>
    );
  }
  const count = total.toLocaleString();
  const need = row.needsPerson;
  if (need === null || need === undefined || Number.isNaN(need)) {
    return `${count} more`;
  }
  if (need === 0) return `${count} more, none need you`;
  return `${count} more, ${need.toLocaleString()} need you`;
}

/** §7 A3 loading: tree-shaped rows with staggered gutter indents. */
function TreeSkeleton() {
  const indents = [0, 1, 2, 2, 3, 1, 2, 2];
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading"
      className="divide-y divide-border-soft"
    >
      {indents.map((d, i) => (
        <div
          key={i}
          className={cn(
            "flex items-center gap-3 pl-4 pr-4",
            d === 0 ? "h-11" : "h-10",
          )}
        >
          <span aria-hidden="true" className="flex shrink-0">
            {Array.from({ length: d }).map((_, l) => (
              <span key={l} className="block w-5 shrink-0" />
            ))}
          </span>
          <Skeleton decorative className={cn("h-3.5", d === 0 ? "w-48" : "w-36")} />
          <Skeleton decorative className="ml-auto h-3.5 w-16" />
        </div>
      ))}
    </div>
  );
}

export function TreeTable<T>({
  rows,
  columns,
  rowKey,
  name,
  nameTitle,
  suffix,
  nameTone,
  treeHeader = "Name",
  treeColumnClassName = "min-w-[280px]",
  onToggle,
  onActivate,
  onShowAll,
  summaryLabel,
  showAllLabel = "Show all →",
  loading,
  error,
  empty,
  ariaLabel = "Tree outline",
  virtualizeThreshold = 60,
  tableClassName,
  className,
}: TreeTableProps<T>) {
  const colSpan = columns.length + 1;
  const { meta, offsets } = React.useMemo(() => computeMeta(rows), [rows]);

  // Roving focus (APG treegrid): exactly one row is tabbable at a time.
  const [focusIndex, setFocusIndex] = React.useState(0);
  const wantFocus = React.useRef(false);
  const bodyRef = React.useRef<HTMLTableSectionElement>(null);
  React.useEffect(() => {
    setFocusIndex(0);
  }, [rows]);

  React.useLayoutEffect(() => {
    if (!wantFocus.current) return;
    wantFocus.current = false;
    const el = bodyRef.current?.querySelector<HTMLTableRowElement>(
      `[data-row-index="${focusIndex}"]`,
    );
    if (!el) return;
    el.focus();
    el.scrollIntoView?.({ block: "nearest" });
  }, [focusIndex]);

  // ── Windowing ─────────────────────────────────────────────────────────────
  const scrollRef = React.useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = React.useState(0);
  const [viewportH, setViewportH] = React.useState(0);
  const virtualize =
    !loading && !error && rows.length > virtualizeThreshold && rows.length > 0;

  React.useEffect(() => {
    if (!virtualize) return;
    const el = scrollRef.current;
    if (!el) return;
    setViewportH(el.clientHeight || 480);
  }, [virtualize]);

  const total = rows.length;
  const OVERSCAN = 8;
  let startIndex = 0;
  let endIndex = total;
  if (virtualize) {
    const vh = viewportH || 480;
    startIndex = Math.max(0, indexAtOffset(offsets, scrollTop) - OVERSCAN);
    endIndex = Math.min(
      total,
      indexAtOffset(offsets, scrollTop + vh) + 1 + OVERSCAN,
    );
  }
  const padTop = virtualize ? offsets[startIndex] : 0;
  const padBottom = virtualize ? offsets[total] - offsets[endIndex] : 0;

  // Exactly ONE tab stop for the whole tree. Normally it is the roved row; when
  // the user has scrolled that row out of the window it falls back to the first
  // rendered row, so Tab can always re-enter the grid (a windowed treegrid with
  // no tabbable row is a keyboard trap in reverse — unreachable).
  const focusInWindow = focusIndex >= startIndex && focusIndex < endIndex;
  const tabStopIndex = focusInWindow ? focusIndex : startIndex;

  /**
   * Bring a row into the window by MOVING THE SCROLL, never by stretching the
   * rendered slice. Stretching is the tempting fix and it is the wrong one: with
   * focus parked on row 0 and the viewport at row 900, a slice that spans both
   * renders 900 rows — the explosion this component exists to avoid.
   */
  function ensureVisible(index: number) {
    if (!virtualize) return;
    const vh = viewportH || 480;
    const top = offsets[index];
    const bottom = offsets[index + 1];
    let next = scrollTop;
    if (top < scrollTop) next = top;
    else if (bottom > scrollTop + vh) next = bottom - vh;
    if (next === scrollTop) return;
    setScrollTop(next);
    const el = scrollRef.current;
    if (el) el.scrollTop = next;
  }

  function move(next: number) {
    const clamped = Math.max(0, Math.min(next, total - 1));
    if (clamped === focusIndex) {
      wantFocus.current = false;
      return;
    }
    ensureVisible(clamped);
    wantFocus.current = true;
    setFocusIndex(clamped);
  }

  function activate(row: TreeRow<T>) {
    if (row.kind === "summary") {
      if (onShowAll) onShowAll(row);
      return;
    }
    onActivate?.(row);
  }

  function onRowKeyDown(
    e: React.KeyboardEvent,
    index: number,
    row: TreeRow<T>,
  ) {
    const expandable = row.expanded !== undefined;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        move(index + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        move(index - 1);
        break;
      case "Home":
        e.preventDefault();
        move(0);
        break;
      case "End":
        e.preventDefault();
        move(total - 1);
        break;
      case "ArrowRight":
        e.preventDefault();
        if (expandable && !row.expanded) onToggle?.(row, true);
        else if (expandable && row.expanded) move(index + 1); // to first child
        break;
      case "ArrowLeft":
        e.preventDefault();
        if (expandable && row.expanded) onToggle?.(row, false);
        else if (meta[index].parentIndex >= 0) move(meta[index].parentIndex);
        break;
      case "Enter":
      case " ":
        e.preventDefault();
        activate(row);
        break;
      default:
        break;
    }
  }

  const headCell = (
    key: string,
    node: React.ReactNode,
    extra?: string,
    numeric?: boolean,
    // Read by the fit gate: a never-dropped column that is scrolled out of
    // sight has been dropped in every sense that matters to a person.
    priority?: number,
  ) => (
    <th
      key={key}
      role="columnheader"
      scope="col"
      data-col-priority={priority}
      className={cn(
        // The uppercase-mono column-head register (§3.2), identical to DataTable
        // so a tree and a list read as one kit.
        "whitespace-nowrap px-4 py-2.5 text-left font-mono text-2xs font-medium uppercase tracking-wide text-faint",
        numeric && "text-right",
        extra,
      )}
    >
      {node || <span className="sr-only">{key}</span>}
    </th>
  );

  function renderRow(index: number) {
    const row = rows[index];
    const m = meta[index];
    const rawDepth = Math.max(0, Math.floor(row.depth) || 0);
    const levels = Math.min(rawDepth, TREE_GUTTER_MAX_LEVELS);
    // When the gutter is capped, keep the NEAREST ancestors — those are the ones
    // whose branches the eye is actually tracking.
    const shown =
      levels > 1
        ? m.ancestors.slice(Math.max(0, m.ancestors.length - (levels - 1)))
        : [];
    const capped = rawDepth > TREE_GUTTER_MAX_LEVELS;
    const expandable = row.expanded !== undefined;
    const isSub = row.kind === "leaf" || row.kind === "summary";
    const focused = index === tabStopIndex;
    const tone = nameTone?.(row);
    const label = name(row);
    const title =
      nameTitle?.(row) ?? (typeof label === "string" ? label : undefined);

    // Honest set metrics: prefer the parent's SERVER-side childCount. When it is
    // bigger than what we render (the size-blind window), report the true size
    // and stay silent about position rather than claim "3 of 255".
    const parent = m.parentIndex >= 0 ? rows[m.parentIndex] : undefined;
    const declared = parent?.childCount;
    const partial =
      declared !== undefined &&
      Number.isFinite(declared) &&
      declared > m.siblingCount;
    const setSize = partial ? declared : m.siblingCount || undefined;
    const posInSet =
      row.kind === "summary" || partial || !m.posInSet ? undefined : m.posInSet;

    const treeCell = (
      <td
        role="gridcell"
        className={cn("p-0 align-middle", treeColumnClassName)}
        colSpan={row.kind === "summary" ? colSpan : undefined}
      >
        <div
          className={cn(
            "flex items-stretch pl-4 pr-4",
            isSub ? "h-10" : "h-11",
          )}
        >
          <TreeGutter ancestors={shown} last={m.last} levels={levels} />
          <div
            className={cn(
              "flex min-w-0 flex-1 items-center gap-1.5",
              levels > 0 && "pl-1",
            )}
          >
            {expandable ? (
              <span
                aria-hidden="true"
                onClick={(e) => {
                  e.stopPropagation();
                  onToggle?.(row, !row.expanded);
                }}
                data-testid="tree-chevron"
                className={cn(
                  // Mouse target only — deliberately NOT a tab stop. The row
                  // owns `aria-expanded` and →/← operate it, so adding a second
                  // focusable node per row would double the tab cost of a
                  // 1,024-row tree for no gain.
                  "inline-flex w-[11px] shrink-0 cursor-pointer select-none justify-center font-mono text-2xs leading-none text-faint transition-transform hover:text-foreground",
                  row.expanded && "rotate-90",
                )}
              >
                ▸
              </span>
            ) : (
              <span aria-hidden="true" className="inline-block w-[11px] shrink-0" />
            )}

            {capped && (
              <span
                className="shrink-0 font-mono text-2xs text-faint"
                title={`Level ${rawDepth + 1} of the tree`}
              >
                d{rawDepth + 1} ·
              </span>
            )}

            {row.kind === "summary" ? (
              <>
                <span className="truncate font-mono text-sm italic text-faint">
                  {(summaryLabel ?? defaultSummaryLabel)(row)}
                </span>
                {onShowAll && (
                  <button
                    type="button"
                    tabIndex={-1}
                    onClick={(e) => {
                      e.stopPropagation();
                      onShowAll(row);
                    }}
                    className="ml-3 shrink-0 whitespace-nowrap font-mono text-2xs text-primary underline underline-offset-2 hover:text-foreground"
                  >
                    {showAllLabel}
                  </button>
                )}
              </>
            ) : (
              <>
                <span
                  className={cn(
                    "truncate font-mono text-sm",
                    row.kind === "leaf" ? "font-normal" : "font-semibold",
                    tone === "hold" && "text-hold",
                    tone === "failed" && "text-destructive",
                  )}
                  title={title}
                >
                  {label}
                </span>
                {suffix && (
                  <span className="shrink-0 whitespace-nowrap font-mono text-2xs text-faint">
                    {suffix(row)}
                  </span>
                )}
              </>
            )}
          </div>
        </div>
      </td>
    );

    return (
      <tr
        key={rowKey(row)}
        role="row"
        data-row-index={index}
        data-depth={rawDepth}
        data-kind={row.kind}
        aria-level={rawDepth + 1}
        aria-expanded={expandable ? row.expanded === true : undefined}
        aria-setsize={setSize}
        aria-posinset={posInSet}
        aria-rowindex={index + 2}
        tabIndex={focused ? 0 : -1}
        onFocus={() => {
          // Guarded: the roving focus() we fire from the layout effect lands
          // here, and an unconditional setState would re-enter React for a value
          // that did not change.
          if (index !== focusIndex) setFocusIndex(index);
        }}
        onKeyDown={(e) => onRowKeyDown(e, index, row)}
        onClick={() => {
          if (expandable) onToggle?.(row, !row.expanded);
          else activate(row);
        }}
        style={{ height: m.height }}
        className={cn(
          "border-b border-border-soft outline-none transition-colors last:border-0",
          // The 40px sub band (§5.22): leaves and summaries sit on surface-2 so a
          // role's children read as one block under it.
          isSub && "bg-surface-2",
          "cursor-pointer hover:bg-accent/40",
          "focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
        )}
      >
        {treeCell}
        {row.kind !== "summary" &&
          columns.map((col, ci) => (
            <td
              key={col.id}
              role="gridcell"
              data-col-priority={columnPriority(col)}
              className={cn(
                "px-4 align-middle text-sm",
                isPinnedCol(col, ci, columns.length) && PINNED_CELL,
                col.mobileOnly
                  ? "md:hidden"
                  : PRIORITY_CLASS[columnPriority(col)],
                col.numeric && cellNum,
                col.className,
              )}
            >
              {col.cell(row)}
            </td>
          ))}
      </tr>
    );
  }

  const showTable = !loading && !error && rows.length > 0;

  return (
    // min-w-0 / max-w-full: a flex or grid parent must always be able to shrink
    // this. The body never scrolls sideways (§4.6) — residual width scrolls in
    // the frame below.
    <div className={cn("min-w-0 max-w-full space-y-3", className)}>
      <div
        ref={scrollRef}
        onScroll={
          virtualize
            ? (e) => setScrollTop((e.target as HTMLDivElement).scrollTop)
            : undefined
        }
        className={cn(
          "min-w-0 max-w-full overflow-x-auto rounded-lg border bg-card",
          virtualize && "max-h-[32rem] overflow-y-auto",
        )}
      >
        {loading && <TreeSkeleton />}

        {!loading && error && (
          <div className="p-6">
            <ErrorState
              variant={error.forbidden ? "forbidden" : "error"}
              description={error.forbidden ? undefined : error.message}
              resource={error.forbidden ? error.resource : undefined}
              onRetry={error.forbidden ? undefined : error.onRetry}
            />
          </div>
        )}

        {!loading && !error && rows.length === 0 && (
          <div className="p-6">{empty && <EmptyState {...empty} />}</div>
        )}

        {showTable && (
          <table
            role="treegrid"
            aria-label={ariaLabel}
            // The TRUE total, always — windowing changes what is in the DOM,
            // never what the tree claims to contain. +1 for the header row.
            aria-rowcount={total + 1}
            className={cn("w-full text-left text-sm", tableClassName)}
          >
            <thead role="rowgroup">
              <tr role="row" aria-rowindex={1} className="border-b border-border bg-card">
                {headCell("tree", treeHeader, treeColumnClassName)}
                {columns.map((col, ci) =>
                  headCell(
                    col.id,
                    col.header,
                    cn(
                      isPinnedCol(col, ci, columns.length) && PINNED_HEAD,
                      col.mobileOnly
                        ? "md:hidden"
                        : PRIORITY_CLASS[columnPriority(col)],
                      col.className,
                    ),
                    col.numeric,
                    columnPriority(col),
                  ),
                )}
              </tr>
            </thead>
            <tbody role="rowgroup" ref={bodyRef}>
              {virtualize && padTop > 0 && (
                <tr aria-hidden="true" style={{ height: padTop }}>
                  <td colSpan={colSpan} className="p-0" />
                </tr>
              )}

              {Array.from({ length: endIndex - startIndex }, (_, k) =>
                renderRow(startIndex + k),
              )}

              {virtualize && padBottom > 0 && (
                <tr aria-hidden="true" style={{ height: padBottom }}>
                  <td colSpan={colSpan} className="p-0" />
                </tr>
              )}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
