import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Filter, KeyRound, Pencil, Plus, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuietNote,
  StatusBadge,
  UnknownValue,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type SecretBindingSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_SECRETS } from "@/lib/nav";

// SecretBindingsPage — the SecretBinding list, on archetype A1 (M151 spec §6.1:
// PageHeader → FilterChipRow → DataTable → ClosingNote) with the §4.4
// "resource list" column budget.
//
// ── SECURITY INVARIANT (unchanged, and load-bearing) ────────────────────────
// This page — and every SecretBinding surface — NEVER renders, requests, or
// transmits a secret value. The DTO carries only:
//   - secretRef.name  — which K8s Secret object holds it
//   - secretRef.key   — which key within that Secret
//   - backend         — where that Secret is sourced from
//   - phase/ready     — the controller's "Resolved" condition
// There is no value/data field in the DTO or in any form in this UI, and the
// redesign added no column, tooltip, drawer, or closing sentence that could
// carry one. The one column that touches the secret prints the REF and nothing
// else.
//
// ── THE PAGE'S ONE IDEA: SORT BY WHAT IS BLOCKING, NOT BY NAME ──────────────
// A binding matters to an operator for exactly one reason: it either resolves
// to a real Secret or it does not, and an agent whose route depends on it is
// down until it does. So the list is ordered by what needs a person —
// `nextStepRank` is the PRIMARY key (identically on all 43 pages, which is the
// point of the shared helper), the §6.1 attention order breaks the tie, and the
// name breaks that. The last column says what to DO, verb-first (§7.2).
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ────────────────────────────────────
// `SecretBindingSummary` carries the pointer and the verdict on it. It carries
// no usage and no rotation history, so the page renders no such column and says
// why in one QuietNote. An absent backend is never a zero.
//
// ── COUNTS ARE FACTS, OR THEY ARE ABSENT ───────────────────────────────────
// The list is cursor-paged, so a count of the rows in hand describes the loaded
// WINDOW, not the namespace. The chips therefore carry numbers only when the
// window provably is the whole set (one page, no cursor before or after it),
// and the closing line says "on this page" out loud whenever it is not.
//
// The list contract is unchanged: GET /api/secretbindings?limit&cursor&q&
// namespace → { items, nextCursor }; "more pages" keys off the CURSOR, never
// row count. RBAC-aware: create/update/delete affordances are gated on the
// caller's capabilities (display-only chrome — the API is the real gate, ADR
// 0011).
//
// data-testid contract:
//   create-secret-button    — the header's primary action (absent for a viewer)
//   namespace-{name}        — the entity cell's namespace line
//   backend-{name}          — the binding's source backend
//   secret-ref-{name}       — the K8s Secret / key REF (never a value)
//   next-step-{name}        — the row's Next step cell
//   row-actions-{name} / edit-{name} / delete-{name}

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

function bindingPath(s: SecretBindingSummary, suffix = ""): string {
  return `/secrets/${encodeURIComponent(s.namespace)}/${encodeURIComponent(s.name)}${suffix}`;
}

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Triaged {
  binding: SecretBindingSummary;
  tone: StatusTone;
  next: NextStep;
}

/**
 * One binding → (tone, next step). Everything the page sorts by and renders is
 * decided here once, so a cell can never disagree with the key the row was
 * ordered by.
 */
function triage(s: SecretBindingSummary): Triaged {
  const { tone } = resolveStatus(s.ready, s.phase);
  const to = bindingPath(s);

  let next: NextStep;
  if (tone === "failed") {
    // The controller could not resolve the reference — every route that names
    // this binding is down until someone changes it. Crit is allowed here
    // (§2.3) because the target genuinely is a failure.
    next = { label: "Fix the binding", tone: "crit", to };
  } else if (tone === "waiting") {
    next = { label: "Review the hold", tone: "default", to };
  } else {
    // Resolved, or still resolving. Either way nothing is asked of a person,
    // and saying so is more useful than inventing an errand.
    next = { tone: "none" };
  }

  return { binding: s, tone, next };
}

// ── The chip views (§5.28): one question, one answer at a time ─────────────

type ViewId = "needs-you" | "resolved" | "all";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (t) => t.next.tone !== "none" },
  {
    id: "resolved",
    label: "Resolved",
    match: (t) => t.tone === "ready" && t.next.tone === "none",
  },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "Every binding in view resolves, or is still resolving on its own. Show everything to see them all.",
  },
  resolved: {
    title: "Nothing has resolved yet",
    description:
      "No binding in view has reached Resolved. Show everything to see what is still coming up.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number is counted from the rows in hand, the sentence
 * says so whenever the rows in hand are not the whole set, and it is
 * grammatical at n=1.
 */
export function closingLine(
  total: number,
  needs: number,
  broken: number,
  complete: boolean,
): string | null {
  if (total === 0) return null;
  const quiet = total - needs;
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More pages follow.";
  if (total === 1) {
    if (needs === 0) return `The one binding${where} needs nothing from you.${more}`;
    return broken === 1
      ? `The one binding${where} won’t resolve until someone fixes it.${more}`
      : `The one binding${where} is waiting on a person.${more}`;
  }
  if (needs === 0) {
    return `None of the ${total} bindings${where} needs a person. Every one of them resolves, or is still resolving.${more}`;
  }
  // The trailing clauses count in WORDS at one, so the sentence never reads
  // "The other 1 need nothing" — the exact ungrammaticality M151 fixed on the
  // guardrail page. The leading ratio stays in numerals: it is the fact the
  // note exists to state.
  const brokenClause =
    broken === 0
      ? ""
      : broken === 1
        ? " One of them won’t resolve until it is fixed."
        : ` ${broken} of them won’t resolve until they are fixed.`;
  if (quiet === 0) {
    return `Every one of the ${total} bindings${where} needs a person.${brokenClause}${more}`;
  }
  const otherClause =
    quiet === 1
      ? " The other one needs nothing from you."
      : ` The other ${quiet} need nothing from you.`;
  return `${needs} of the ${total} bindings${where} need${needs === 1 ? "s" : ""} a person.${brokenClause}${otherClause}${more}`;
}

// RowActions renders per-row edit + delete affordances, RBAC-aware — hidden
// entirely for viewers (display-only chrome; the API is the real gate, ADR
// 0011). They render at REST rather than on row-hover: the DataTable's `<tr>`
// carries no `group` class, so the old `group-hover:opacity-100` could never
// fire and both buttons were invisible at every width except while focused. A
// control unreachable by mouse is a defect, not restraint — so they are quiet
// (faint icons that firm up on hover) instead of hidden.
function RowActions({
  binding,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  binding: SecretBindingSummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (s: SecretBindingSummary) => void;
  onDelete: (s: SecretBindingSummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center justify-end gap-1"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${binding.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-foreground"
          aria-label={`Edit ${binding.name}`}
          data-testid={`edit-${binding.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(binding);
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
          aria-label={`Delete ${binding.name}`}
          data-testid={`delete-${binding.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(binding);
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
  | { kind: "ready"; items: SecretBindingSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function SecretBindingsPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_SECRETS, "create");
  const canEdit = can(RES_SECRETS, "update");
  const canDelete = can(RES_SECRETS, "delete");

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
      .listSecretBindings(
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
  // hasNext keys off the CURSOR (BFF), never items.length — a page-windowed `q`
  // filter can empty a page while later pages still match.
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;

  // The loaded window IS the whole set only when no page precedes it and none
  // follows it. That is the one condition under which counting the rows in hand
  // is a FACT rather than a windowed guess.
  const listComplete = state.kind === "ready" && !hasNext && !hasPrev;

  // Triage once, sort once. Attention-first (§6.1): nextStepRank is the primary
  // key so "Nothing needed" always sinks; the attention order breaks ties.
  const sorted = useMemo(() => {
    const rows = items.map(triage);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        ATTENTION[x.tone] - ATTENTION[y.tone] ||
        x.binding.name.localeCompare(y.binding.name),
    );
    return rows;
  }, [items]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling.
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
          resource: "secret bindings",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 "resource list" budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 1 never. Binding,
  // State and Next step survive every width — the row's identity, its
  // condition, and what to do about it.
  const columns: Column<Triaged>[] = [
    {
      id: "binding",
      header: "Binding",
      priority: 1,
      cell: ({ binding: s }) => (
        <CellEntity
          // The cap is what makes `truncate` bite: it clamps the cell's
          // max-content contribution, so a 63-character name can no longer set
          // the width of the table (and through it, of the document).
          className="max-w-[18rem]"
          name={s.name}
          title={s.name}
          namespace={
            <span title={s.namespace} data-testid={`namespace-${s.name}`}>
              {shortNamespace(s.namespace)}
            </span>
          }
        />
      ),
    },
    {
      id: "backend",
      header: "Backend",
      priority: 4,
      cell: ({ binding: s }) =>
        s.backend ? (
          // Where the Secret is sourced from is a MODE, not a health state, so
          // it takes the muted tag — never ok/warn, and never pine (§2.2).
          <Badge variant="muted" data-testid={`backend-${s.name}`}>
            {s.backend}
          </Badge>
        ) : (
          // Absent, not "kubernetes". Unknown and a real answer never share a
          // glyph (§7.1).
          <UnknownValue title="The controller didn’t state a source backend — unknown, not a default." />
        ),
    },
    {
      id: "secretRef",
      header: "K8s Secret / Key",
      priority: 3,
      className: "max-w-[16rem]",
      cell: ({ binding: s }) => (
        // The REF — which Secret object, which key inside it. NEVER the value
        // it holds; there is no field in the DTO that could carry one. Mono,
        // one line, full ref in `title` (§4.5).
        <span
          className="block truncate font-mono text-xs text-secondary-foreground"
          title={`${s.secretRef.name}/${s.secretRef.key}`}
          data-testid={`secret-ref-${s.name}`}
        >
          {s.secretRef.name}/{s.secretRef.key}
        </span>
      ),
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[9rem]",
      cell: ({ binding: s }) => <StatusBadge ready={s.ready} phase={s.phase} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[11rem]",
      cell: (t) => (
        <NextStepLink
          label={t.next.label}
          to={t.next.to}
          tone={t.next.tone}
          ariaLabel={t.next.label ? `${t.next.label} — ${t.binding.name}` : undefined}
          testId={`next-step-${t.binding.name}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching an operator with ten bindings how to make a first.
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
        countNoun: "bindings",
      }
    : {
        icon: KeyRound,
        title: "No secret bindings yet",
        description: namespace
          ? `No SecretBindings in ${namespace}. A binding is how the platform points at a stored provider secret — which Kubernetes Secret, and which key inside it. Connecting a provider makes one for you.`
          : "A binding is how the platform points at a stored provider secret — which Kubernetes Secret, and which key inside it. Connecting a provider makes one for you.",
        action: canCreate
          ? { label: "New binding", icon: Plus, onClick: () => navigate("/secrets/new") }
          : undefined,
      };

  const needs = sorted.filter((t) => nextStepRank(t.next.tone) === 0).length;
  const broken = sorted.filter((t) => t.tone === "failed").length;
  const closing = closingLine(sorted.length, needs, broken, listComplete);
  const showChips = state.kind !== "error";
  const showNote = state.kind === "ready" && items.length > 0;
  const metaLine =
    state.kind === "ready"
      ? listComplete
        ? `${items.length} binding${items.length === 1 ? "" : "s"}`
        : `${items.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Secret bindings"
        meta={metaLine}
        lede="How the platform points at your stored provider secrets — a pointer to a Kubernetes Secret, never its contents. Sorted by what is blocking: a binding that will not resolve sits at the top."
        // Through `actionsSlot` rather than the structured `actions` list:
        // `PageHeaderAction` carries no `testId`, and the black-box viewer suite
        // asserts on `create-secret-button` being visible — an assertion that
        // would silently change meaning if the id disappeared.
        actionsSlot={
          canCreate ? (
            <Button asChild size="sm" className="text-sm" data-testid="create-secret-button">
              <Link to="/secrets/new">
                <Plus className="h-4 w-4" />
                New binding
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
          label="Filter secret bindings"
          className="min-w-0"
        />
      )}

      {/* The backend-cannot-answer state (§7 A1 / §7.1). Two things a reader
          reasonably expects on this page — who depends on a binding, and when
          it was last rotated — are not in this response, and the honest move is
          to say so once rather than render a column of dashes with no reason. */}
      {showNote && (
        <QuietNote title="Usage and rotation aren’t in the binding list.">
          This list reads the <em>pointer</em> each binding holds — which
          Kubernetes Secret, and which key inside it — plus the controller’s
          verdict on resolving it. It never reads the stored value, and no column
          here can. Which agents depend on a binding, and when it was last
          rotated, are not in this response. Nothing is estimated — those facts
          are simply absent.
        </QuietNote>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={(t) => `${t.binding.namespace}/${t.binding.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter bindings on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Secret bindings"
        onRowClick={(t) => navigate(bindingPath(t.binding))}
        // The actions column only appears when the caller can edit or delete.
        // A viewer sees a clean table with no dead or greyed-out buttons.
        rowActions={
          canEdit || canDelete
            ? (t) => (
                <RowActions
                  binding={t.binding}
                  canEdit={canEdit}
                  canDelete={canDelete}
                  onEdit={(s) => navigate(bindingPath(s, "?edit=1"))}
                  onDelete={(s) => navigate(bindingPath(s, "?delete=1"))}
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
