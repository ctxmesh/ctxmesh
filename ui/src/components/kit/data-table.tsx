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
// loading / empty / error states from the same primitives every surface uses.
// Backed by the list contract (spec §4): `?limit&cursor&q&namespace` in →
// `{ items, nextCursor }` out. This is a CONTROLLED component — the parent owns
// data + cursor + query + sort and re-fetches on change; the table is pure
// presentation + affordances (so it composes with the BFF cleanly at m13.4).
//
// Props are intentionally verbose: five milestones (agents, routes, secrets,
// registries, runs, traces, tools, feedback) all render through this one API.

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

export interface DataTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  /** Stable row key — namespace/name for K8s objects. */
  rowKey: (row: T) => string;
  /** Loading / error toggles — the parent flips these around its fetch. */
  loading?: boolean;
  error?: { message: string; onRetry?: () => void } | null;

  /** Filter bar (the list contract's `q`). Omit to hide the search box. */
  query?: string;
  onQueryChange?: (q: string) => void;
  queryPlaceholder?: string;

  /** Column sort (server-side). */
  sort?: SortState | null;
  onSortChange?: (sort: SortState) => void;

  /** Cursor pagination — Prev is client-tracked; Next uses the BFF cursor. */
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
  className?: string;
}

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
  className,
}: DataTableProps<T>) {
  const showSearch = onQueryChange !== undefined;
  const isFiltered = !!query && query.length > 0;

  function toggleSort(col: Column<T>) {
    if (!col.sortable || !onSortChange) return;
    const dir: SortDir =
      sort?.columnId === col.id && sort.dir === "asc" ? "desc" : "asc";
    onSortChange({ columnId: col.id, dir });
  }

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

      <div className="overflow-hidden rounded-lg border bg-card shadow-card">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b bg-surface-2/60">
              {columns.map((col) => {
                const active = sort?.columnId === col.id;
                return (
                  <th
                    key={col.id}
                    scope="col"
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
                <td colSpan={columns.length + (rowActions ? 1 : 0)} className="p-4">
                  <SkeletonTable rows={6} cols={columns.length} />
                </td>
              </tr>
            )}

            {!loading && error && (
              <tr>
                <td colSpan={columns.length + (rowActions ? 1 : 0)} className="p-6">
                  <ErrorState
                    description={error.message}
                    onRetry={error.onRetry}
                  />
                </td>
              </tr>
            )}

            {!loading && !error && rows.length === 0 && (
              <tr>
                <td colSpan={columns.length + (rowActions ? 1 : 0)} className="p-6">
                  {isFiltered ? (
                    <EmptyState
                      intent="filtered"
                      icon={Search}
                      title="No matches"
                      description="Nothing in this page matched your filter. Filtering is windowed to the loaded page."
                      action={{
                        label: "Clear filter",
                        variant: "outline",
                        onClick: () => onQueryChange?.(""),
                      }}
                    />
                  ) : (
                    empty && <EmptyState {...empty} />
                  )}
                </td>
              </tr>
            )}

            {!loading &&
              !error &&
              rows.map((row) => (
                <tr
                  key={rowKey(row)}
                  onClick={onRowClick ? () => onRowClick(row) : undefined}
                  className={cn(
                    "border-b border-border/60 last:border-0 transition-colors",
                    onRowClick && "cursor-pointer hover:bg-surface-2/70",
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
              ))}
          </tbody>
        </table>
      </div>

      {/* Pagination — always shown (scale-first affordance), even on one page. */}
      {(onPrev || onNext || rangeLabel) && (
        <div className="flex items-center justify-between px-1 text-xs text-muted-foreground">
          <span>{rangeLabel ?? `${rows.length} shown`}</span>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={!hasPrev}
              onClick={onPrev}
            >
              <ChevronLeft className="h-4 w-4" />
              Prev
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!hasNext}
              onClick={onNext}
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
