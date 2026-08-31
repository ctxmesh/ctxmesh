import * as React from "react";
import {
  ArrowDown,
  ArrowUp,
  ChevronLeft,
  ChevronRight,
  Search,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { EmptyState, type EmptyStateProps } from "@/components/kit/empty-state";
import { ErrorState } from "@/components/kit/error-state";
import { SkeletonTable } from "@/components/kit/skeleton";
import { cn } from "@/lib/utils";

// DataTable — the ONE table for the whole console arc (kit, m13.1 → real in
// m13.4). Cursor-paginated, column-sortable, q-filterable; renders the honest
// loading / empty / error / forbidden states from the same primitives every
// surface uses. Backed by the list contract (spec §4): `?limit&cursor&q&
// namespace` in → `{ items, nextCursor }` out. This is a CONTROLLED component —
// the parent owns data + cursor + query + sort and re-fetches on change; the
// table is pure presentation + affordances (so it composes with the BFF
// cleanly). Props are intentionally verbose: five milestones (agents, routes,
// secrets, registries, runs, traces, tools, feedback) render through this one
// API.
//
// ── THE CURSOR-vs-q RULE (m13.2 reviewer's contract, baked in here) ──────────
// "More pages" keys off the CURSOR, NEVER off row count. The BFF's `q` is a
// page-WINDOWED substring filter (K8s has no server-side substring search), so a
// filtered page can legitimately return ZERO items while `nextCursor` is still
// non-empty — the matches may live on a later page. Therefore:
//   • The Next/"load more" affordance is driven by `hasNext` (⇐ nextCursor
//     presence, computed by the parent), independent of `rows.length`. It stays
//     LIVE on an empty filtered window when more pages exist.
//   • When the filtered window is empty AND `hasNext`, we render the honest
//     state: "No matches in this page window — more pages exist. Load the next
//     page or clear the filter" — NOT a terminal "no results".
//   • Only when the filtered window is empty AND there are NO more pages is it a
//     true "no matches anywhere we can see" (still page-windowed, stated
//     honestly). The filter is labelled "filter", never "search".
//
// ── Virtualization ──────────────────────────────────────────────────────────
// A page window can be up to `?limit`=200 rows. Above `virtualizeThreshold`
// (default 60) the body windows the rendered rows to the visible slice + an
// overscan, so a 200-row page stays smooth. Off by default for small pages
// (keeps the DOM simple + fully present for tests/AT on typical windows).
//
// ── FIT: the page body never scrolls sideways (M151 §4.6) ───────────────────
// Every list in the console renders through this component, so the table is
// where "does the page fit?" is won or lost. Three mechanisms, all mechanical:
//   1. COLUMN BUDGET (§4.4). `Column.priority` (1–4) declares how essential a
//      column is; the table maps it to a `hidden <bp>:table-cell` class. Below
//      1280 the p4 columns leave, below 1024 the p3s, below 768 the p2s. p1
//      never leaves. Dropped ≠ lost — the row still opens its drawer/detail.
//   2. OWN-CONTAINER SCROLLING (§4.6). Residual width scrolls INSIDE the
//      table's bordered frame (`overflow-x-auto`), never on `document.body`.
//      The frame was `overflow-hidden` before, which silently CLIPPED wide
//      cells — data the user could not reach. Auto scrolls instead of hiding.
//   3. UNCONDITIONAL SHRINKABILITY. The root and the frame carry `min-w-0
//      max-w-full` so a flex/grid parent can never be widened by table content
//      (a flex item defaults to `min-width:auto` = its content's min-content
//      width — that single default is the classic source of body overflow).
// Acceptance (§4.6): at 360px, `document.body.scrollWidth === innerWidth`.

/**
 * Column budget priority (§4.4). 1 = never dropped (the entity, its state, and
 * the Next step always survive); 4 = the first to go. Dropping is mechanical —
 * see PRIORITY_CLASS.
 */
export type ColumnPriority = 1 | 2 | 3 | 4;

/**
 * priority → responsive visibility (§4.4). Written as literal class strings so
 * Tailwind's content scanner sees them.
 */
/**
 * Priority -> responsive class. Exported because TreeTable applies the SAME
 * budget: two tables that drop columns at different widths would be two design
 * systems wearing one name, and the duplication was already drifting.
 */
export const PRIORITY_CLASS: Record<ColumnPriority, string> = {
  1: "", //                        always visible
  2: "hidden md:table-cell", //    hidden below 768
  3: "hidden lg:table-cell", //    hidden below 1024
  4: "hidden xl:table-cell", //    hidden below 1280
};

/**
 * `cell-num` (§4.8 / §5.10) — the numeric cell register. Every digit the
 * machine owns is mono and tabular so columns of numbers align on their
 * decimal; `whitespace-nowrap` is what keeps MONEY whole (§4.5: money is never
 * truncated, wrapped, or elided). Applied automatically to `numeric` columns;
 * exported for cells that render numbers outside a numeric column.
 */
export const cellNum =
  "font-mono tabular-nums text-right whitespace-nowrap text-sm text-secondary-foreground";

/** Longest id that renders whole; above this it middle-truncates (§4.5). */
const ID_WHOLE_MAX = 16;
const ID_HEAD = 8;
const ID_TAIL = 4;

/**
 * Middle-truncate a run/trace id or UUID (§4.5): head 8 + … + tail 4
 * (`P1euQVd4…I3f2`). Ids are NEVER end-ellipsised — the tail is what
 * disambiguates two ids that share a prefix.
 */
export function truncateId(id: string): string {
  if (id.length <= ID_WHOLE_MAX) return id;
  return `${id.slice(0, ID_HEAD)}…${id.slice(-ID_TAIL)}`;
}

/**
 * `cell-id` — a run/trace id in a table cell (§4.5). Mono, never wrapped,
 * middle-truncated when long, with the full id in `title` so it stays
 * recoverable by hover and by the accessibility tree.
 */
export function CellId({
  id,
  className,
}: {
  id: string;
  className?: string;
}) {
  const shown = truncateId(id);
  return (
    <span
      className={cn("whitespace-nowrap font-mono text-xs", className)}
      title={shown === id ? undefined : id}
    >
      {shown}
    </span>
  );
}

/**
 * `cell-entity` (§5.10) — the two-line entity cell: the resource name over its
 * namespace. The namespace never shares the name's line (§4.5). The name
 * truncates with an end-ellipsis and keeps the full string in `title`; it is
 * never `break-all`-ed. Give the column a `max-w-*` in `Column.className` for
 * the truncation to bite (an auto-layout table otherwise widens to fit).
 */
export function CellEntity({
  name,
  namespace,
  title,
  className,
}: {
  name: React.ReactNode;
  namespace?: React.ReactNode;
  /** Full value for the tooltip; defaults to `name` when it is a string. */
  title?: string;
  className?: string;
}) {
  const nameTitle = title ?? (typeof name === "string" ? name : undefined);
  const nsTitle = typeof namespace === "string" ? namespace : undefined;
  return (
    <div className={cn("min-w-0", className)}>
      <div className="truncate text-sm font-semibold" title={nameTitle}>
        {name}
      </div>
      {namespace !== undefined && namespace !== null && namespace !== "" && (
        <div className="truncate font-mono text-xs text-faint" title={nsTitle}>
          {namespace}
        </div>
      )}
    </div>
  );
}

export interface Column<T> {
  /** Stable key — also the sort key sent to the BFF when `sortable`. */
  id: string;
  header: React.ReactNode;
  /** Cell renderer. Keep it presentational; row-click owns navigation. */
  cell: (row: T) => React.ReactNode;
  sortable?: boolean;
  /** Tailwind width/alignment classes for the column (e.g. "w-40 max-w-[16rem]"). */
  className?: string;
  /**
   * Column budget (§4.4). 1 = never dropped · 2 = hidden below `md` (768) ·
   * 3 = below `lg` (1024) · 4 = below `xl` (1280). Defaults to 1.
   * Dropped ≠ lost: the row's drawer/detail still renders every field.
   */
  priority?: ColumnPriority;
  /**
   * Numeric column (§4.8): cells render in the `cell-num` register (mono,
   * tabular, right-aligned, never wrapped) and the column head right-aligns to
   * match. Use for counts, durations, and money.
   */
  numeric?: boolean;
  /**
   * @deprecated Migration alias for `priority: 2` (§4.4). Kept so the ~20
   * pages that still pass `hideOnMobile` keep rendering identically while they
   * migrate column-by-column. REMOVE this field — and the `columnPriority`
   * fallback below — once no caller references it
   * (`grep -rn hideOnMobile ui/src` returns only this file).
   */
  hideOnMobile?: boolean;
}

/**
 * The one place the deprecated alias is bridged: an explicit `priority` always
 * wins; otherwise `hideOnMobile` means priority 2 (its old meaning, "hidden
 * below md"); otherwise the column is priority 1 and never drops.
 */
export function columnPriority<T>(col: Column<T>): ColumnPriority {
  if (col.priority !== undefined) return col.priority;
  return col.hideOnMobile ? 2 : 1;
}

export type SortDir = "asc" | "desc";
export interface SortState {
  columnId: string;
  dir: SortDir;
}

export interface DataTableError {
  message: string;
  onRetry?: () => void;
  /** Render as the 403 forbidden variant (RBAC-aware; spec §3). */
  forbidden?: boolean;
  /**
   * The resource denied, e.g. "agents" (M100 UI99-403). On a forbidden error it drives the friendly
   * "You don't have permission to view <resource>" message instead of the raw BFF RBAC string (which
   * is never surfaced on a 403). Optional — omitted → the generic friendly permission message.
   */
  resource?: string;
}

export interface DataTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  /** Stable row key — namespace/name for K8s objects. */
  rowKey: (row: T) => string;
  /** Loading / error toggles — the parent flips these around its fetch. */
  loading?: boolean;
  error?: DataTableError | null;

  /** Filter bar (the list contract's `q`). Omit to hide the filter box. */
  query?: string;
  onQueryChange?: (q: string) => void;
  queryPlaceholder?: string;

  /** Column sort (server-side). */
  sort?: SortState | null;
  onSortChange?: (sort: SortState) => void;

  /**
   * Cursor pagination. Prev is client-tracked by the parent; Next is LIVE iff
   * `hasNext` — which the parent derives from a non-empty `nextCursor`, NEVER
   * from `rows.length` (see the cursor-vs-q rule above).
   */
  hasPrev?: boolean;
  hasNext?: boolean;
  onPrev?: () => void;
  onNext?: () => void;
  /** e.g. "1–50" — the parent computes the window from its page stack. */
  rangeLabel?: string;

  /** Row interaction — opens a DetailDrawer or navigates. */
  onRowClick?: (row: T) => void;
  /** Trailing per-row actions (RBAC-gated by the parent: pass none for viewers). */
  rowActions?: (row: T) => React.ReactNode;
  /**
   * Marks a row whose entity is stopped/halted (§5.10): the row takes the
   * destructive tint at rest so a dead agent is legible at a glance in a long
   * list, and exposes `data-halted` for tests and visual regression.
   */
  rowHalted?: (row: T) => boolean;

  /** Teaching empty state when the unfiltered list is genuinely empty. */
  empty?: EmptyStateProps;
  /** A left-aligned toolbar slot (namespace picker, extra filters, "New"). */
  toolbar?: React.ReactNode;
  /** Accessible name for the table (defaults to "Data table"). */
  ariaLabel?: string;
  /** Above this row count the body virtualizes the visible window. */
  virtualizeThreshold?: number;
  /** Estimated row height (px) for virtualization math. */
  rowHeight?: number;
  /**
   * Classes for the `<table>` itself — in practice the archetype's `min-w-*`
   * from the §4.4 budget totals (e.g. `min-w-[52rem]`), which makes a wide
   * table scroll in its own frame rather than squash its columns. The frame
   * always clips to the page: this can never widen `document.body`.
   */
  tableClassName?: string;
  className?: string;
}

// 44px = 12px pad + 20px line + 12px pad (§4.1). LOAD-BEARING: the
// virtualization math (spacer heights, visible window) is computed from it, and
// the editorial redesign keeps it deliberately. Do not change without redoing
// the windowing math and re-measuring the row rhythm.
const DEFAULT_ROW_HEIGHT = 44;
const OVERSCAN = 8;

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  loading,
  error,
  query,
  onQueryChange,
  queryPlaceholder = "Filter…",
  sort,
  onSortChange,
  hasPrev,
  hasNext,
  onPrev,
  onNext,
  rangeLabel,
  onRowClick,
  rowActions,
  rowHalted,
  empty,
  toolbar,
  ariaLabel = "Data table",
  virtualizeThreshold = 60,
  rowHeight = DEFAULT_ROW_HEIGHT,
  tableClassName,
  className,
}: DataTableProps<T>) {
  const showSearch = onQueryChange !== undefined;
  const isFiltered = !!query && query.length > 0;
  const colSpan = columns.length + (rowActions ? 1 : 0);
  // Show the pager only when there is actually another page to reach — a disabled
  // Prev/Next under a single-page table is dead chrome (M144.2). A rangeLabel alone
  // (a "1–N of M" count) may still show.
  const showPagination =
    ((hasPrev || hasNext) && (onPrev !== undefined || onNext !== undefined)) ||
    rangeLabel !== undefined;

  // Roving keyboard focus over rows (Up/Down move, Enter/Space activate).
  const [focusedRow, setFocusedRow] = React.useState(0);
  React.useEffect(() => {
    // Reset the roving index when the row set changes (page/filter/sort).
    setFocusedRow(0);
  }, [rows]);

  function toggleSort(col: Column<T>) {
    if (!col.sortable || !onSortChange) return;
    const dir: SortDir =
      sort?.columnId === col.id && sort.dir === "asc" ? "desc" : "asc";
    onSortChange({ columnId: col.id, dir });
  }

  function onRowKeyDown(e: React.KeyboardEvent, index: number, row: T) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setFocusedRow(Math.min(index + 1, rows.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setFocusedRow(Math.max(index - 1, 0));
    } else if ((e.key === "Enter" || e.key === " ") && onRowClick) {
      e.preventDefault();
      onRowClick(row);
    }
  }

  // ── Virtualization ────────────────────────────────────────────────────────
  const scrollRef = React.useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = React.useState(0);
  const [viewportH, setViewportH] = React.useState(0);
  const virtualize =
    !loading && !error && rows.length > virtualizeThreshold;

  React.useEffect(() => {
    if (!virtualize) return;
    const el = scrollRef.current;
    if (!el) return;
    setViewportH(el.clientHeight || 480);
  }, [virtualize]);

  const total = rows.length;
  let startIndex = 0;
  let endIndex = total;
  if (virtualize) {
    const vh = viewportH || 480;
    startIndex = Math.max(0, Math.floor(scrollTop / rowHeight) - OVERSCAN);
    endIndex = Math.min(
      total,
      Math.ceil((scrollTop + vh) / rowHeight) + OVERSCAN,
    );
  }
  const visibleRows = virtualize ? rows.slice(startIndex, endIndex) : rows;
  const padTop = virtualize ? startIndex * rowHeight : 0;
  const padBottom = virtualize ? (total - endIndex) * rowHeight : 0;

  const dataRows = (offset: number) =>
    visibleRows.map((row, i) => {
      const index = offset + i;
      const focused = index === focusedRow;
      const halted = rowHalted?.(row) ?? false;
      return (
        <tr
          key={rowKey(row)}
          data-halted={halted ? "true" : undefined}
          tabIndex={onRowClick ? (focused ? 0 : -1) : undefined}
          aria-selected={onRowClick ? focused : undefined}
          onFocus={() => setFocusedRow(index)}
          onKeyDown={
            onRowClick ? (e) => onRowKeyDown(e, index, row) : undefined
          }
          onClick={onRowClick ? () => onRowClick(row) : undefined}
          style={virtualize ? { height: rowHeight } : undefined}
          className={cn(
            // Rows are separated by the SOFT rule, not the panel frame: the
            // table reads as one object with internal divisions (§2.7 —
            // elevation is rules, not shadows).
            "border-b border-border-soft transition-colors outline-none last:border-0",
            halted && "bg-destructive-surface",
            onRowClick &&
              "cursor-pointer focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
            onRowClick &&
              (halted
                ? "hover:bg-destructive-surface/70 focus-visible:bg-destructive-surface/70"
                : "hover:bg-surface-2/50 focus-visible:bg-surface-2"),
          )}
        >
          {columns.map((col) => (
            <td
              key={col.id}
              className={cn(
                "px-4 py-3 align-middle text-sm",
                PRIORITY_CLASS[columnPriority(col)],
                col.numeric && cellNum,
                col.className,
              )}
            >
              {col.cell(row)}
            </td>
          ))}
          {rowActions && (
            <td
              className="px-4 py-3 text-right"
              onClick={(e) => e.stopPropagation()}
            >
              {rowActions(row)}
            </td>
          )}
        </tr>
      );
    });

  // Genuinely-empty list (no items at all, not a filtered window, no more pages):
  // hide the column headers so the teaching empty state doesn't sit under ghost
  // chrome (M144.2). The filter toolbar + the empty-state row still render.
  const genuinelyEmpty = !loading && !error && rows.length === 0 && !isFiltered && !hasNext;

  return (
    // min-w-0: a flex/grid parent must be able to shrink this below its content
    // (§4.6). max-w-full: and it never exceeds that parent.
    <div className={cn("min-w-0 max-w-full space-y-3", className)}>
      {/* Toolbar: filter bar (q) + parent-supplied controls. */}
      {(showSearch || toolbar) && (
        <div className="flex min-w-0 flex-wrap items-center gap-3">
          {showSearch && (
            // Prefers 16rem but SHRINKS below it (flex-basis + min-w-0) — a hard
            // `min-w-[16rem]` is itself a fit failure on a 360px viewport.
            <div className="relative min-w-0 flex-[1_1_16rem]">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query ?? ""}
                onChange={(e) => onQueryChange?.(e.target.value)}
                placeholder={queryPlaceholder}
                className="pl-9"
                aria-label="Filter list"
              />
            </div>
          )}
          {toolbar}
        </div>
      )}

      <div
        ref={scrollRef}
        onScroll={
          virtualize
            ? (e) => setScrollTop((e.target as HTMLDivElement).scrollTop)
            : undefined
        }
        className={cn(
          // The frame: a bordered card, no shadow (--shadow-card is `none`;
          // elevation on-page is rules, §2.7). overflow-x-auto — NOT
          // overflow-hidden: wide tables scroll HERE, inside their own
          // container, and are never clipped away or pushed onto the body
          // (§4.6). min-w-0/max-w-full make that promise unconditional.
          "min-w-0 max-w-full overflow-x-auto rounded-lg border bg-card shadow-card",
          virtualize && "max-h-[32rem] overflow-y-auto",
        )}
      >
        <table
          className={cn("w-full text-left text-sm", tableClassName)}
          aria-label={ariaLabel}
        >
          {!genuinelyEmpty && (
          <thead>
            {/* Header sits on the card, separated by the frame rule — no fill
                band (§5.10). */}
            <tr className="border-b border-border bg-card">
              {columns.map((col) => {
                const active = sort?.columnId === col.id;
                return (
                  <th
                    key={col.id}
                    scope="col"
                    aria-sort={
                      col.sortable
                        ? active
                          ? sort?.dir === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                        : undefined
                    }
                    className={cn(
                      // The uppercase-mono register (§3.2): 10px, tracked open,
                      // faint — a label, not a heading competing with the data.
                      "whitespace-nowrap px-4 py-2.5 font-mono text-2xs font-medium uppercase tracking-wide text-faint",
                      // Numeric heads right-align onto their digit column (§4.8).
                      col.numeric && "text-right",
                      active && "text-foreground",
                      PRIORITY_CLASS[columnPriority(col)],
                      col.className,
                    )}
                  >
                    {col.sortable ? (
                      <button
                        type="button"
                        onClick={() => toggleSort(col)}
                        className="inline-flex items-center gap-1 hover:text-foreground"
                      >
                        {col.header}
                        {active &&
                          (sort?.dir === "asc" ? (
                            <ArrowUp className="h-3 w-3" />
                          ) : (
                            <ArrowDown className="h-3 w-3" />
                          ))}
                      </button>
                    ) : (
                      // An empty visual header (e.g. an actions column) still needs an accessible
                      // name for screen readers + WCAG (axe empty-table-header, M100 UI99-7): fall
                      // back to an sr-only label derived from the column id.
                      col.header || <span className="sr-only">{col.id}</span>
                    )}
                  </th>
                );
              })}
              {rowActions && (
                <th className="w-10 px-4 py-2.5">
                  <span className="sr-only">Actions</span>
                </th>
              )}
            </tr>
          </thead>
          )}
          <tbody>
            {loading && (
              <tr>
                <td colSpan={colSpan} className="p-4">
                  <SkeletonTable rows={6} cols={columns.length} />
                </td>
              </tr>
            )}

            {!loading && error && (
              <tr>
                <td colSpan={colSpan} className="p-6">
                  <ErrorState
                    variant={error.forbidden ? "forbidden" : "error"}
                    // A forbidden error shows the friendly, resource-named permission message (M100
                    // UI99-403) — NEVER the raw BFF RBAC string. Only the generic `error` variant
                    // surfaces the real message (a 502/500 where the reason helps).
                    description={error.forbidden ? undefined : error.message}
                    resource={error.forbidden ? error.resource : undefined}
                    onRetry={error.forbidden ? undefined : error.onRetry}
                  />
                </td>
              </tr>
            )}

            {!loading && !error && rows.length === 0 && (
              <tr>
                <td colSpan={colSpan} className="p-6">
                  {isFiltered ? (
                    <EmptyState
                      intent="filtered"
                      icon={Search}
                      title={hasNext ? "No matches in this page" : "No matches"}
                      // The cursor-vs-q rule, surfaced to the user: an empty
                      // filtered window with more pages is NOT a dead end.
                      description={
                        hasNext
                          ? "Nothing on the loaded page matched your filter, but more pages exist — filtering is windowed to the loaded page. Load the next page or clear the filter."
                          : "Nothing matched your filter on the loaded pages. Filtering is windowed to the loaded page."
                      }
                      action={{
                        label: "Clear filter",
                        variant: "outline",
                        onClick: () => onQueryChange?.(""),
                      }}
                      secondaryAction={
                        hasNext && onNext
                          ? { label: "Load next page", onClick: onNext }
                          : undefined
                      }
                    />
                  ) : (
                    empty && <EmptyState {...empty} />
                  )}
                </td>
              </tr>
            )}

            {/* Virtualization top spacer. */}
            {virtualize && padTop > 0 && (
              <tr aria-hidden="true" style={{ height: padTop }}>
                <td colSpan={colSpan} className="p-0" />
              </tr>
            )}

            {!loading && !error && dataRows(virtualize ? startIndex : 0)}

            {/* Virtualization bottom spacer. */}
            {virtualize && padBottom > 0 && (
              <tr aria-hidden="true" style={{ height: padBottom }}>
                <td colSpan={colSpan} className="p-0" />
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {/* Pagination — always shown when wired (scale-first affordance), even on
          one page. Next is driven by `hasNext` (⇐ nextCursor), so it stays live
          on an empty filtered window when more pages exist. */}
      {showPagination && (
        <div className="flex min-w-0 flex-wrap items-center justify-between gap-2 px-1 text-xs text-muted-foreground">
          <span className="min-w-0 truncate font-mono tabular-nums">
            {rangeLabel ?? `${rows.length} shown`}
          </span>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={!hasPrev}
              onClick={onPrev}
              aria-label="Previous page"
            >
              <ChevronLeft className="h-4 w-4" />
              Prev
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!hasNext}
              onClick={onNext}
              aria-label="Next page"
            >
              Next
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
