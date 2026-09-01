import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Filter, GitBranch, Pencil, Plus, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  StatusBadge,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type ModelRouteSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_ROUTES } from "@/lib/nav";

// ModelRoutesPage — archetype A1 (spec §6.1: PageHeader → FilterChipRow →
// DataTable → ClosingNote) on the §4.4 "resource list" column budget.
//
// ── THE PAGE'S ONE IDEA ─────────────────────────────────────────────────────
// A route matters to an operator for exactly one reason: a model name either
// resolves to a provider or it does not. So the list is sorted by what is
// BLOCKING (§6.1) — `nextStepRank` first, so anything needing a person sits
// above everything that does not and "Nothing needed" sinks to the bottom;
// status tone breaks the tie; name breaks that. The last column then says what
// to DO, verb-first and in the user's voice (§7.2).
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `ModelRouteSummary` carries what a route DECLARES (its providers, in priority
// order) and the controller's verdict on it. It carries no traffic and no
// spend, so the page does not render a usage column at all and says why in one
// QuietNote — an absent backend is never a zero.
//
// ── COUNTS ARE FACTS, OR THEY ARE ABSENT ────────────────────────────────────
// The list is cursor-paged, so a count of the rows in hand describes the loaded
// WINDOW, not the namespace. The chips therefore carry numbers only when the
// window provably is the whole set (one page, no cursor before or after it),
// and the closing line says "on this page" out loud whenever it is not.
//
// The list contract is unchanged: GET /api/modelroutes?limit&cursor&q&namespace
// → { items, nextCursor }; "more pages" keys off the CURSOR, never row count.
// RBAC-aware: create/update/delete affordances are gated on the caller's
// capabilities (display-only chrome — the API is the real gate, ADR 0011).

const PAGE_LIMIT = 50;

/** The §6.1 attention order, as a comparator key. */
const ATTENTION: Record<StatusTone, number> = {
  failed: 1,
  waiting: 2,
  progressing: 3,
  ready: 4,
  draft: 5,
};

/** ≤26 chars renders whole; deeper namespaces middle-truncate (§4.5). */
const NS_WHOLE_MAX = 26;

/**
 * Middle-truncate a deep namespace: first segment + `…` + the last two (§4.5).
 * An end-ellipsis would throw away the tail, which is the half that
 * disambiguates `…-team-d-shared-ingest` from `…-team-c-shared-ingest`. The
 * full value always rides along in `title`.
 */
function shortNamespace(ns: string): string {
  if (ns.length <= NS_WHOLE_MAX) return ns;
  const sep = ns.includes("/") ? "/" : "-";
  const parts = ns.split(sep).filter(Boolean);
  if (parts.length < 4) return ns; // nothing to elide without losing a whole word
  return `${parts[0]}…${parts.slice(-2).join(sep)}`;
}

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Triaged {
  route: ModelRouteSummary;
  tone: StatusTone;
  /** The priority-1 provider — the one a call actually lands on first. */
  primary?: ModelRouteSummary["providers"][number];
  fallbacks: number;
  next: NextStep;
}

function routePath(r: ModelRouteSummary, suffix = ""): string {
  return `/routes/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}${suffix}`;
}

/**
 * One route → (tone, provider order, next step). Everything the page sorts by
 * and renders is decided here once, so a cell can never disagree with the key
 * the row was ordered by.
 */
function triage(r: ModelRouteSummary): Triaged {
  const { tone } = resolveStatus(r.ready, r.phase);
  const ordered = [...r.providers].sort((a, b) => a.priority - b.priority);
  const to = routePath(r);

  let next: NextStep;
  if (r.providers.length === 0) {
    // A route with no provider resolves nothing. That is a setup gap, not a
    // failure, so it stays pine — crit is reserved for a failure or a stop.
    next = { label: "Add a provider", tone: "default", to: routePath(r, "?edit=1") };
  } else if (tone === "failed") {
    next = { label: "Fix the route", tone: "crit", to };
  } else if (tone === "waiting") {
    next = { label: "Review the route", tone: "default", to };
  } else {
    // Serving, or converging on its own. Either way nothing is asked of a
    // person, and saying so is more useful than inventing an errand.
    next = { tone: "none" };
  }

  return {
    route: r,
    tone,
    primary: ordered[0],
    fallbacks: Math.max(0, ordered.length - 1),
    next,
  };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "serving" | "all";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (t) => t.next.tone !== "none" },
  {
    id: "serving",
    label: "Serving",
    match: (t) => t.tone === "ready" && t.next.tone === "none",
  },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "Every route in view resolves, or is still coming up on its own. Show everything to see them.",
  },
  serving: {
    title: "Nothing is serving yet",
    description:
      "No route in view has reached Ready. Show everything to see what is still coming up.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are not the whole set.
 */
export function closingLine(rows: Triaged[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const needs = rows.filter((t) => nextStepRank(t.next.tone) === 0).length;
  const quiet = total - needs;
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More pages follow.";
  if (needs === 0) {
    return `None of the ${total} routes${where} needs a person. Every one of them resolves.${more}`;
  }
  if (quiet === 0) {
    return `Every one of the ${total} routes${where} needs a person.${more}`;
  }
  return `${needs} of the ${total} routes${where} need${needs === 1 ? "s" : ""} a person. The other ${quiet} resolve and need nothing from you.${more}`;
}

// RowActions renders per-row edit + delete affordances, RBAC-aware — hidden
// entirely for viewers (display-only chrome; the API is the real gate, ADR
// 0011). They render at REST rather than on row-hover: the DataTable's `<tr>`
// carries no `group` class, so the old `group-hover:opacity-100` could never
// fire and both buttons were invisible at every width except while focused. A
// control unreachable by mouse is a defect, not restraint — so they are quiet
// (faint icons that firm up on hover) instead of hidden.
function RowActions({
  route,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  route: ModelRouteSummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (r: ModelRouteSummary) => void;
  onDelete: (r: ModelRouteSummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center justify-end gap-1"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${route.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-foreground"
          aria-label={`Edit ${route.name}`}
          data-testid={`edit-${route.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(route);
          }}
        >
          <Pencil className="h-3.5 w-3.5" />
        </Button>
      )}
      {canDelete && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-destructive"
          aria-label={`Delete ${route.name}`}
          data-testid={`delete-${route.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(route);
          }}
        >
          <Trash2 className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  );
}

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: ModelRouteSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function ModelRoutesPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_ROUTES, "create");
  const canEdit = can(RES_ROUTES, "update");
  const canDelete = can(RES_ROUTES, "delete");

  const [query, setQuery] = useState("");
  const [view, setView] = useState<ViewId>("all");
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [state, setState] = useState<Load>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listModelRoutes(
        {
          limit: PAGE_LIMIT,
          cursor: cursor || undefined,
          q: query || undefined,
          namespace: namespace || undefined,
        },
        controller.signal,
      )
      .then((res) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", items: res.items, nextCursor: res.nextCursor });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const forbidden = err instanceof ApiError && err.isForbidden;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden,
        });
      });
  }, [cursor, query, namespace]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const resetPaging = useCallback(() => setPageStack([""]), []);

  function onQueryChange(q: string) {
    setQuery(q);
    resetPaging();
  }

  const prevNs = useRef(namespace);
  useEffect(() => {
    if (prevNs.current !== namespace) {
      prevNs.current = namespace;
      resetPaging();
    }
  }, [namespace, resetPaging]);

  const items = state.kind === "ready" ? state.items : [];
  const nextCursor = state.kind === "ready" ? state.nextCursor : "";
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;

  // The loaded window IS the whole set only when no page precedes it and none
  // follows it. That is the one condition under which counting the rows in hand
  // is a FACT rather than a windowed guess.
  const listComplete = state.kind === "ready" && !hasNext && !hasPrev;

  // Triage once, sort once. Attention-first (§6.1).
  const sorted = useMemo(() => {
    const rows = items.map(triage);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        ATTENTION[x.tone] - ATTENTION[y.tone] ||
        x.route.name.localeCompare(y.route.name),
    );
    return rows;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    // No number unless it is the whole set's number (kit FilterChipRow
    // contract). A count that describes one page while looking like a total is
    // the failure mode that hides work.
    count: listComplete ? sorted.filter(v.match).length : undefined,
  }));

  function onNext() {
    if (!hasNext) return;
    setPageStack((s) => [...s, nextCursor]);
  }
  function onPrev() {
    if (!hasPrev) return;
    setPageStack((s) => s.slice(0, -1));
  }

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          resource: "model routes",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 "resource list" budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 1 never. Route, State
  // and Next step survive every width — the row's identity, its condition, and
  // what to do about it.
  const columns: Column<Triaged>[] = [
    {
      id: "route",
      header: "Route",
      priority: 1,
      cell: ({ route: r }) => (
        <CellEntity
          // The cap is what makes `truncate` bite: it clamps the cell's
          // max-content contribution, so a 63-character name can no longer set
          // the width of the table (and through it, of the document).
          className="max-w-[18rem]"
          name={r.name}
          title={r.name}
          namespace={
            <span title={r.namespace} data-testid={`namespace-${r.name}`}>
              {shortNamespace(r.namespace)}
            </span>
          }
        />
      ),
    },
    {
      id: "primary",
      header: "Primary",
      priority: 3,
      className: "max-w-[14rem]",
      cell: (t) =>
        t.primary ? (
          // Machine-owned identifiers render mono, on one line, with the full
          // value in `title` (§4.5).
          <span
            className="block truncate font-mono text-xs text-secondary-foreground"
            title={`${t.primary.provider}/${t.primary.model}`}
          >
            {`${t.primary.provider}/${t.primary.model}`}
          </span>
        ) : (
          // Declared but never wired up — the `open` tag (§2.5). Not a zero,
          // not an error; the row's Next step says what to do about it.
          <Badge variant="open" data-testid={`no-provider-${t.route.name}`}>
            no provider
          </Badge>
        ),
    },
    {
      id: "fallbacks",
      header: "Fallbacks",
      priority: 4,
      numeric: true,
      // A known zero is a real `0` in the ghost register; it is never the dash.
      // "This route has no fallback" and "we do not know its fallbacks" are
      // different facts and never share a glyph (§7.1).
      cell: (t) => <QuantityValue value={t.fallbacks} />,
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[8rem]",
      cell: ({ route: r }) => <StatusBadge ready={r.ready} phase={r.phase} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[10rem]",
      cell: (t) => (
        <NextStepLink
          label={t.next.label}
          to={t.next.to}
          tone={t.next.tone}
          ariaLabel={t.next.label ? `${t.next.label} — ${t.route.name}` : undefined}
          testId={`next-step-${t.route.name}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching an operator with eight routes how to make a first.
  const chipEmptied = items.length > 0 && visible.length === 0;
  const empty: EmptyStateProps = chipEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: listComplete ? items.length : undefined,
        countNoun: "routes",
      }
    : {
        icon: GitBranch,
        title: "No model routes yet",
        description: namespace
          ? `No ModelRoutes in ${namespace}. A route is the name your agents ask for — it decides which provider and model the call lands on. Create one to route AI calls to a provider.`
          : "A route is the name your agents ask for — it decides which provider and model the call lands on. Create one to route AI calls to a provider.",
        action: canCreate
          ? { label: "New route", icon: Plus, onClick: () => navigate("/routes/new") }
          : undefined,
      };

  const closing = closingLine(sorted, listComplete);
  const showChips = state.kind !== "error";
  const showNote = state.kind === "ready" && items.length > 0;
  const metaLine =
    state.kind === "ready"
      ? listComplete
        ? `${items.length} route${items.length === 1 ? "" : "s"}`
        : `${items.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Model routes"
        meta={metaLine}
        lede="How a model name resolves to a provider. Sorted by what is blocking — anything that cannot resolve sits at the top."
        // Through `actionsSlot` rather than the structured `actions` list:
        // `PageHeaderAction` carries no `testId`, and the suite asserts on
        // `create-route-button` being absent for a viewer — an assertion that
        // would silently pass forever if the id disappeared.
        actionsSlot={
          canCreate ? (
            <Button asChild size="sm" className="text-sm" data-testid="create-route-button">
              <Link to="/routes/new">
                <Plus className="h-4 w-4" />
                New route
              </Link>
            </Button>
          ) : undefined
        }
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter model routes"
          className="min-w-0"
        />
      )}

      {showNote && (
        <QuietNote title="Traffic and spend aren’t in the route list.">
          This list reads what each route <em>declares</em> — its providers, in
          priority order — and the controller’s verdict on it. How much traffic a
          route carried, and what it cost, come from the trace backend and are not
          projected here. Nothing is estimated — those figures are simply absent.
        </QuietNote>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={(t) => `${t.route.namespace}/${t.route.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter routes on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Model routes"
        onRowClick={(t) => navigate(routePath(t.route))}
        // The actions column only appears when the caller can edit or delete.
        // A viewer sees a clean table with no dead or greyed-out buttons.
        rowActions={
          canEdit || canDelete
            ? (t) => (
                <RowActions
                  route={t.route}
                  canEdit={canEdit}
                  canDelete={canDelete}
                  onEdit={(r) => navigate(routePath(r, "?edit=1"))}
                  onDelete={(r) => navigate(routePath(r, "?delete=1"))}
                />
              )
            : undefined
        }
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}
    </div>
  );
}
