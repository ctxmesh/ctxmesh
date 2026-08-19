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

export interface Column<T> {
  /** Stable key — also the sort key sent to the BFF when `sortable`. */
  id: string;
  header: React.ReactNode;
  /** Cell renderer. Keep it presentational; row-click owns navigation. */
  cell: (row: T) => React.ReactNode;
  sortable?: boolean;
  /** Tailwind width/alignment classes for the column (e.g. "w-40 text-right"). */
  className?: string;
  /** Hide below md — non-essential columns collapse on narrow viewports. */
  hideOnMobile?: boolean;
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
  className?: string;
}

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
  empty,
  toolbar,
  ariaLabel = "Data table",
  virtualizeThreshold = 60,
  rowHeight = DEFAULT_ROW_HEIGHT,
  className,
}: DataTableProps<T>) {
  const showSearch = onQueryChange !== undefined;
  const isFiltered = !!query && query.length > 0;
  const colSpan = columns.length + (rowActions ? 1 : 0);
  const showPagination = onPrev !== undefined || onNext !== undefined || rangeLabel !== undefined;

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
      return (
        <tr
          key={rowKey(row)}
          tabIndex={onRowClick ? (focused ? 0 : -1) : undefined}
          aria-selected={onRowClick ? focused : undefined}
          onFocus={() => setFocusedRow(index)}
          onKeyDown={
            onRowClick ? (e) => onRowKeyDown(e, index, row) : undefined
          }
          onClick={onRowClick ? () => onRowClick(row) : undefined}
          style={virtualize ? { height: rowHeight } : undefined}
          className={cn(
            "border-b border-border/60 last:border-0 transition-colors outline-none",
            onRowClick &&
              "cursor-pointer hover:bg-surface-2/70 focus-visible:bg-surface-2 focus-visible:ring-2 focus-visible:ring-ring",
          )}
        >
          {columns.map((col) => (
            <td
              key={col.id}
              className={cn(
                "px-4 py-3 align-middle",
                col.hideOnMobile && "hidden md:table-cell",
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

  return (
    <div className={cn("space-y-3", className)}>
      {/* Toolbar: filter bar (q) + parent-supplied controls. */}
      {(showSearch || toolbar) && (
        <div className="flex flex-wrap items-center gap-3">
          {showSearch && (
            <div className="relative min-w-[16rem] flex-1">
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
          "overflow-hidden rounded-lg border bg-card shadow-card",
          virtualize && "max-h-[32rem] overflow-y-auto",
        )}
      >
        <table className="w-full text-left text-sm" aria-label={ariaLabel}>
          <thead>
            <tr className="border-b bg-surface-2/60">
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
                      "px-4 py-2.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground",
                      col.hideOnMobile && "hidden md:table-cell",
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
                      col.header
                    )}
                  </th>
                );
              })}
              {rowActions && <th className="w-10 px-4 py-2.5" />}
            </tr>
          </thead>
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
        <div className="flex items-center justify-between px-1 text-xs text-muted-foreground">
          <span>{rangeLabel ?? `${rows.length} shown`}</span>
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
