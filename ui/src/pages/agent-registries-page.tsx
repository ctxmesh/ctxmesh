import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Pencil, Trash2, Users } from "lucide-react";

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
  StatusBadge,
  UNKNOWN,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type AgentRegistrySummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_REGISTRIES } from "@/lib/nav";

// AgentRegistriesPage — archetype A1 (index/table), resource-list column budget
// (M151 spec §6.1/§4.4).
//
// NO egress/allowlist field anywhere in this UI — the egress NetworkPolicy is
// controller-managed and cannot be altered through the console.
//
// ── WHAT THE PAGE IS SORTED BY ──────────────────────────────────────────────
// Not the name. A registry index answers "which of these is holding something
// up?", so the rows are ordered by what is BLOCKING (§6.1 A1 sort doctrine):
// every row that needs a person first, "Nothing needed" last, and within each
// group by status attention (failing → held → converging → serving → draft).
// `nextStepRank` is the primary key so this column sorts identically on all
// ~20 list pages.
//
// ── THE ONE UNKNOWN THIS PAGE RENDERS ───────────────────────────────────────
// `guards` is optional on the wire: a registry may simply never have declared a
// delegation bound. That is NOT zero — an undeclared hop budget is unbounded
// delegation, the opposite of nought — so the cells render the §7.1 unknown
// glyph with its own reason, and the row's next step is to declare them.

const PAGE_LIMIT = 50;

/** Attention order inside a next-step group (§6.1 A1): failing first, off last. */
const TONE_RANK: Record<StatusTone, number> = {
  failed: 0,
  waiting: 1,
  progressing: 2,
  ready: 3,
  draft: 4,
};

/**
 * The closing line's copy (§5.18) — a SIGHTED FLOURISH that restates the table's
 * ratio and never carries a fact alone. It is built from counts of the rows the
 * response actually contained, and says "on this page" whenever the cursor tells
 * us those rows are a window onto a larger set.
 */
function closingNote(total: number, needing: number, windowed: boolean): string {
  const scope = windowed ? " on this page" : "";
  const noun = total === 1 ? "registry" : "registries";
  if (total === 1)
    return needing > 0
      ? `The one registry${scope} needs a person.`
      : `The one registry${scope} is settled — nothing here needs a person.`;
  if (needing === 0)
    return `Nothing${scope} needs a person — all ${total} ${noun} are settled.`;
  if (needing === total)
    return `Every one of the ${total} ${noun}${scope} needs a person.`;
  return `${needing} of the ${total} ${noun}${scope} need a person. The other ${total - needing} are settled.`;
}

/** The §7.1 reason for an absent guard — "not declared", never "zero". */
const GUARDS_ABSENT_TITLE =
  "No delegation bound is declared on this registry — unknown, not zero.";

interface NextStep {
  label?: string;
  tone: NextStepTone;
  to?: string;
}

/**
 * The user's next action on one registry (§7.2) — verb-first, ≤22 chars, and
 * never a restatement of the system's state. A converging registry deliberately
 * says "Nothing needed": the controller is doing its own work and asks nothing
 * of a person (§2.5). It still sorts above a serving one, via TONE_RANK.
 */
function nextStep(
  r: AgentRegistrySummary,
  tone: StatusTone,
  canEdit: boolean,
): NextStep {
  const detail = `/registries/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}`;
  if (tone === "failed") return { label: "Fix the registry", tone: "crit", to: detail };
  if (tone === "waiting") return { label: "Review the hold", tone: "default", to: detail };
  if (!r.guards)
    return {
      label: "Set the guards",
      tone: "default",
      to: canEdit ? `${detail}?edit=1` : detail,
    };
  return { tone: "none" };
}

function RowActions({
  registry,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  registry: AgentRegistrySummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (r: AgentRegistrySummary) => void;
  onDelete: (r: AgentRegistrySummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${registry.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label={`Edit ${registry.name}`}
          data-testid={`edit-${registry.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(registry);
          }}
        >
          <Pencil className="h-3.5 w-3.5" />
        </Button>
      )}
      {canDelete && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-destructive hover:text-destructive"
          aria-label={`Delete ${registry.name}`}
          data-testid={`delete-${registry.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(registry);
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
  | { kind: "ready"; items: AgentRegistrySummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

type View = "all" | "attention";

export function AgentRegistriesPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_REGISTRIES, "create");
  const canEdit = can(RES_REGISTRIES, "update");
  const canDelete = can(RES_REGISTRIES, "delete");

  const [query, setQuery] = useState("");
  const [view, setView] = useState<View>("all");
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
      .listAgentRegistries(
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

  const items = useMemo(
    () => (state.kind === "ready" ? state.items : []),
    [state],
  );
  const nextCursor = state.kind === "ready" ? state.nextCursor : "";
  // hasNext keys off the CURSOR (BFF), never items.length — an empty filtered
  // window with more pages must keep Next live (the cursor-vs-q rule).
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;
  // The loaded rows are one WINDOW of a larger set whenever a page exists on
  // either side. Every count this page states is scoped to that window in
  // words, because the console cannot see past it.
  const windowed = hasPrev || hasNext;

  // Decorate once: status, next step and sort key all come from the same
  // resolve, so a row can never disagree with itself.
  const decorated = useMemo(
    () =>
      items
        .map((r) => {
          const status = resolveStatus(r.ready, r.phase);
          return { row: r, tone: status.tone, step: nextStep(r, status.tone, canEdit) };
        })
        .sort(
          (a, b) =>
            nextStepRank(a.step.tone) - nextStepRank(b.step.tone) ||
            TONE_RANK[a.tone] - TONE_RANK[b.tone] ||
            a.row.name.localeCompare(b.row.name),
        ),
    [items, canEdit],
  );

  const needing = decorated.filter((d) => d.step.tone !== "none").length;
  const visible =
    view === "attention" ? decorated.filter((d) => d.step.tone !== "none") : decorated;
  const rows = visible.map((d) => d.row);
  const stepFor = new Map(
    visible.map((d) => [`${d.row.namespace}/${d.row.name}`, d.step] as const),
  );

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
          resource: "agent registries",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  const detailPath = (r: AgentRegistrySummary) =>
    `/registries/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}`;

  // §4.4 resource-list budget. Entity / State / Next step are priority 1 and
  // survive every width; the id takes the p4 (first-to-drop) slot and the roles
  // + the two guard numerics take p3, so 768 renders exactly the three columns
  // the archetype promises. Dropped ≠ lost — every row opens its detail page.
  const columns: Column<AgentRegistrySummary>[] = [
    {
      id: "name",
      header: "Registry",
      className: "max-w-[18rem]",
      cell: (r) => <CellEntity name={r.name} namespace={r.namespace} />,
    },
    {
      id: "registryId",
      header: "Registry ID",
      priority: 4,
      className: "max-w-[14rem]",
      cell: (r) => (
        <span className="block truncate font-mono text-xs text-faint" title={r.registryId}>
          {r.registryId}
        </span>
      ),
    },
    {
      id: "roles",
      header: "Roles",
      priority: 3,
      className: "max-w-[14rem]",
      cell: (r) =>
        r.roles.length > 0 ? (
          <span
            className="block truncate text-sm text-muted-foreground"
            title={r.roles.join(", ")}
          >
            {r.roles.join(", ")}
          </span>
        ) : (
          // "Declared but never exercised" is a Tag, not a dash (§2.5): a
          // registry with no roles is a real, readable state, not a missing
          // measurement.
          <Badge variant="open">no roles</Badge>
        ),
    },
    {
      id: "maxDepth",
      header: "Max depth",
      priority: 3,
      numeric: true,
      cell: (r) => (
        <QuantityValue
          value={r.guards?.maxDepth ?? UNKNOWN}
          title={GUARDS_ABSENT_TITLE}
        />
      ),
    },
    {
      id: "hopBudget",
      header: "Hop budget",
      priority: 3,
      numeric: true,
      cell: (r) => (
        <QuantityValue
          value={r.guards?.hopBudget ?? UNKNOWN}
          title={GUARDS_ABSENT_TITLE}
        />
      ),
    },
    {
      id: "phase",
      header: "State",
      className: "w-[7rem]",
      cell: (r) => <StatusBadge ready={r.ready} phase={r.phase} />,
    },
    {
      id: "nextStep",
      header: "Next step",
      className: "w-[10rem]",
      cell: (r) => {
        const step = stepFor.get(`${r.namespace}/${r.name}`);
        return (
          <NextStepLink
            label={step?.label}
            to={step?.to}
            tone={step?.tone ?? "none"}
            testId={`next-step-${r.name}`}
          />
        );
      },
    },
  ];

  // The empties are different truths (§7). A chip that matched nothing is NOT a
  // first run: the rows exist, this view excluded them — so it says so, offers
  // the way back, and never re-teaches what the surface is for.
  const emptyState =
    view === "attention"
      ? {
          icon: Users,
          intent: "filtered" as const,
          title: "Nothing here needs you",
          description:
            "No registry on this page is failing, held, or missing its delegation guards.",
          totalCount: decorated.length > 0 ? decorated.length : undefined,
          countNoun: "registries",
          action: {
            label: "Show everything",
            variant: "outline" as const,
            onClick: () => setView("all"),
          },
        }
      : {
          icon: Users,
          title: "No agent registries yet",
          description: namespace
            ? `No AgentRegistries in ${namespace}.`
            : "No AgentRegistries visible. Create one to group agents into a named registry.",
        };

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Agent registries"
        lede="Registries group your agents and decide who may join a team. Whatever needs a person is at the top."
        actions={
          canCreate
            ? [{ id: "new", label: "New registry", to: "/registries/new", primary: true }]
            : undefined
        }
      />

      {/* Views, not filters — one question, one answer (§5.28). No counts: this
          list is a cursor-paged window, so a number counted here would be a
          claim about a set the console cannot see. */}
      {decorated.length > 0 && (
        <FilterChipRow
          label="Filter registries"
          value={view}
          onChange={(id) => setView(id as View)}
          chips={[
            { id: "all", label: "Everything" },
            { id: "attention", label: "Needs attention" },
          ]}
        />
      )}

      <DataTable<AgentRegistrySummary>
        columns={columns}
        rows={rows}
        rowKey={(r) => `${r.namespace}/${r.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter registries on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Agent registries"
        onRowClick={(r) => navigate(detailPath(r))}
        rowActions={
          canEdit || canDelete
            ? (r) => (
                <RowActions
                  registry={r}
                  canEdit={canEdit}
                  canDelete={canDelete}
                  onEdit={(reg) => navigate(`${detailPath(reg)}?edit=1`)}
                  onDelete={(reg) => navigate(`${detailPath(reg)}?delete=1`)}
                />
              )
            : undefined
        }
        empty={emptyState}
      />

      {/* The honest ratio, restated — never the only place a fact appears
          (§5.18), and always scoped to the window the console can actually see. */}
      {state.kind === "ready" && decorated.length > 0 && (
        <ClosingNote>{closingNote(decorated.length, needing, windowed)}</ClosingNote>
      )}
    </div>
  );
}
