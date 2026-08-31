import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Boxes, Filter, Pencil, Sparkles, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  LifecycleTrack,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  StatusBadge,
  UNKNOWN,
  humanizeStatusReason,
  nextStepRank,
  resolveStatus,
  resourcePath,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type LifecycleStage,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { api, ApiError, type AgentSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_AGENTS } from "@/lib/nav";

// AgentsPage — the fleet list, and the console's flagship (M151, spec §6.1
// archetype A1: PageHeader → FilterChipRow → DataTable → ClosingNote). Every
// other resource list copies this page, so what it decides here it decides 20
// times.
//
// ── THE PAGE'S ONE IDEA: SORT BY WHAT IS BLOCKING, NOT BY NAME ───────────────
// An alphabetical fleet list makes the reader do the triage. This one does it
// for them: `nextStepRank` is the PRIMARY key (anything that needs a person
// sorts above everything that does not, and "Nothing needed" sorts last —
// identically on all 43 pages, which is the whole point of the shared helper),
// and the §6.1 attention order (halted → failing → held → progressing →
// serving → draft) breaks the tie inside each half. The last column then says
// what to DO about it, verb-first and in the user's voice.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `AgentSummary` carries identity + readiness and nothing else. It has no run
// counts and no spend, so those two budgeted columns (§4.4) render the honest
// dash with ONE QuietNote above the table saying why — never a zero, never an
// estimate. Likewise the LifecycleTrack only ever reads Build / Govern / Ship,
// because "Improve" would be a position claim about evaluation data this
// endpoint does not return. A guessed position is the one thing that component
// must not draw.
//
// ── COUNTS ARE FACTS, OR THEY ARE ABSENT (kit FilterChipRow contract) ───────
// The list is cursor-paged, so counting the rows in hand and printing that as
// a fleet total would be confidently wrong in the direction that HIDES work.
// The chips therefore carry numbers only when the loaded window provably IS
// the whole fleet — one page, no cursor before or after it. Otherwise they
// carry none, and the closing line says "on this page" out loud.
//
// ── CURSOR PAGINATION (parent owns the page stack) ──────────────────────────
// The parent tracks a stack of the cursors it has fetched: pageStack[i] is the
// cursor for page i ("" = the first page). hasPrev = we're past page 0; hasNext
// = the LAST response's nextCursor is non-empty (⇐ the BFF, NEVER items.length —
// a page-windowed `q` filter can empty a page while later pages still match).
//
// ── q IS A WINDOWED FILTER (labelled so) ────────────────────────────────────
// `q` filters the CURRENT page window server-side (K8s has no substring search);
// the box is labelled "Filter…" and changing it resets to page 0. The empty-
// filtered-window-with-more-pages state is rendered by the DataTable for free.
//
// ── NAMESPACE SCOPE ─────────────────────────────────────────────────────────
// The shell's namespace picker scopes the list ("" = all the caller can see).
// Changing it resets pagination. A 403 (RBAC can't-list in this scope) renders
// the DataTable's forbidden variant (ErrorState forbidden — the ForbiddenInline
// family), NOT a fake empty list.
//
// ── RBAC-AWARE ROW AFFORDANCES (m15.11) ─────────────────────────────────────
// Edit + Delete row actions are rendered only when the caller has
// agentdeployments/update + agentdeployments/delete respectively. A viewer sees
// neither. The row-click → detail page remains available to viewers.

const PAGE_LIMIT = 50;

// ── Triage: the state model this page sorts and speaks from ─────────────────

/**
 * The §6.1 attention order, as a comparator key. `halted` outranks every tone
 * (0), so a stopped agent is the first row on the page.
 */
const ATTENTION: Record<StatusTone, number> = {
  failed: 1,
  waiting: 2,
  progressing: 3,
  ready: 4,
  draft: 5,
};

/**
 * A stop reaches this page only as free-form condition text — `spec.suspend` is
 * not projected into `AgentSummary` — so it is read the same way `resolveStatus`
 * reads a phase: from the words the backend actually sent. No match, no claim.
 */
const HALTED = /(^|[^a-z])(suspend(ed)?|stopped|halted|killed)([^a-z]|$)/i;

/** Reasons that name a promotion gate, which gets the more specific next step. */
const PROMOTION = /promot/i;

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Triaged {
  agent: AgentSummary;
  tone: StatusTone;
  halted: boolean;
  stage: LifecycleStage;
  next: NextStep;
}

/**
 * One agent → (tone, halted, lifecycle stage, next step). Everything the page
 * renders and sorts by is decided here, once, so the column cells stay
 * presentational and the sort can never disagree with the link it ordered by.
 */
function triage(a: AgentSummary): Triaged {
  const words = `${a.phase ?? ""} ${a.reason ?? ""}`;
  const { tone } = resolveStatus(a.ready, a.phase, a.reason);
  const halted = HALTED.test(words);
  const to = resourcePath("agent", a.namespace, a.name) ?? undefined;

  // Build / Govern / Ship only. An agent that is serving is at Ship, not
  // Improve: this endpoint returns no evaluation or feedback data, and drawing
  // the fourth segment would be a position claim nothing here supports (§5.20).
  const stage: LifecycleStage = a.isDraft
    ? "Build"
    : tone === "waiting"
      ? "Govern"
      : "Ship";

  let next: NextStep;
  if (halted) {
    next = { label: "Review the stop", tone: "crit", to };
  } else if (tone === "failed") {
    next = { label: "Open the failure", tone: "crit", to };
  } else if (tone === "waiting") {
    next = {
      label: PROMOTION.test(words) ? "Promote to 100%" : "Review the hold",
      tone: "default",
      to,
    };
  } else if (a.isDraft) {
    next = { label: "Finish setup", tone: "default", to: to && `${to}?edit=1` };
  } else if (a.drift) {
    next = { label: "Review the drift", tone: "default", to };
  } else {
    // Converging on its own, or serving. Either way nothing is asked of a
    // person, and saying so is more useful than inventing an errand.
    next = { tone: "none" };
  }

  return { agent: a, tone, halted, stage, next };
}

function attention(t: Triaged): number {
  return t.halted ? 0 : ATTENTION[t.tone];
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "failing" | "serving" | "all";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (t) => t.next.tone !== "none" },
  { id: "failing", label: "Failing", match: (t) => t.halted || t.tone === "failed" },
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
      "Every agent in view is either serving or coming up on its own. Show everything to see the fleet.",
  },
  failing: {
    title: "Nothing is failing",
    description:
      "No agent in view is stopped or refusing to converge. Show everything to see the fleet.",
  },
  serving: {
    title: "Nothing is serving yet",
    description:
      "No agent in view has reached Ready. Show everything to see what is still coming up.",
  },
};

/** ≤26 chars renders whole; deeper paths middle-truncate (§4.5). */
const NS_WHOLE_MAX = 26;

/**
 * Middle-truncate a deep namespace: first segment + `…` + the last two (§4.5).
 * The tail is what disambiguates `…-team-d-shared-ingest` from
 * `…-team-c-shared-ingest`, so an end-ellipsis would throw away the half that
 * carries the meaning. The full value always rides along in `title`.
 *
 * Namespaces are flattened paths here — a real one cannot contain `/`, so the
 * separator is `-` unless the caller genuinely handed us a path.
 */
export function shortNamespace(ns: string): string {
  if (ns.length <= NS_WHOLE_MAX) return ns;
  const sep = ns.includes("/") ? "/" : "-";
  const parts = ns.split(sep).filter(Boolean);
  if (parts.length < 4) return ns; // nothing to elide without losing a whole word
  return `${parts[0]}…${parts.slice(-2).join(sep)}`;
}

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number in it is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are not the whole fleet.
 */
export function closingLine(rows: Triaged[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const needs = rows.filter((t) => nextStepRank(t.next.tone) === 0).length;
  const quiet = total - needs;
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More pages follow.";
  if (needs === 0) {
    return `None of the ${total} agents${where} needs a person. Every one of them is running itself.${more}`;
  }
  if (quiet === 0) {
    return `Every one of the ${total} agents${where} needs a person.${more}`;
  }
  return `${needs} of the ${total} agents${where} need${needs === 1 ? "s" : ""} a person. The other ${quiet} need nothing from you.${more}`;
}

// RowActions renders per-row edit + delete affordances, RBAC-aware.
// Hidden entirely for viewers (capabilities-driven, display-only — the API is
// the real gate, ADR 0011). We prevent the row-click from propagating on the
// action buttons so they don't also trigger navigation.
//
// They render at rest rather than on row-hover: the DataTable's `<tr>` carries
// no `group` class, so the old `group-hover:opacity-100` could never fire and
// the two buttons were invisible at every width except while focused. A control
// that is unreachable by mouse is not restraint, it is a defect; they are
// quiet instead (faint icons that firm up on hover).
function RowActions({
  agent,
  canEdit,
  canDelete,
  onEdit,
  onDelete,
}: {
  agent: AgentSummary;
  canEdit: boolean;
  canDelete: boolean;
  onEdit: (a: AgentSummary) => void;
  onDelete: (a: AgentSummary) => void;
}) {
  if (!canEdit && !canDelete) return null;
  return (
    <div
      className="flex items-center justify-end gap-1"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${agent.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-faint hover:text-foreground"
          aria-label={`Edit ${agent.name}`}
          data-testid={`edit-${agent.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onEdit(agent);
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
          aria-label={`Delete ${agent.name}`}
          data-testid={`delete-${agent.name}`}
          onClick={(e) => {
            e.stopPropagation();
            onDelete(agent);
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
  | { kind: "ready"; items: AgentSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function AgentsPage() {
  const navigate = useNavigate();
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const canCreate = can(RES_AGENTS, "create");
  const canEdit = can(RES_AGENTS, "update");
  const canDelete = can(RES_AGENTS, "delete");

  const [query, setQuery] = useState("");
  const [includeDrafts, setIncludeDrafts] = useState(false);
  // Fleet triage: the chip row is a set of VIEWS over the loaded window — one
  // question with one answer at a time (§5.28), never an AND of checkboxes.
  const [view, setView] = useState<ViewId>("all");
  // The page stack: the cursor used to fetch each page. [""] = we're on page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [state, setState] = useState<Load>({ kind: "loading" });

  // Keep the live request abortable so a rapid namespace/filter/page change
  // doesn't race a stale response into the UI.
  const abortRef = useRef<AbortController | null>(null);

  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listAgents(
        { limit: PAGE_LIMIT, cursor: cursor || undefined, q: query || undefined, namespace: namespace || undefined, includeDrafts: includeDrafts || undefined },
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
  }, [cursor, query, namespace, includeDrafts]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // A new filter or namespace scope resets to page 0 (a fresh cursor stack). The
  // effect above then refetches. Guard against resetting when already at page 0
  // with no change to avoid an extra render.
  const resetPaging = useCallback(() => setPageStack([""]), []);

  function onQueryChange(q: string) {
    setQuery(q);
    resetPaging();
  }

  // Reset paging whenever the namespace scope changes (the shell owns the value).
  const prevNs = useRef(namespace);
  useEffect(() => {
    if (prevNs.current !== namespace) {
      prevNs.current = namespace;
      resetPaging();
    }
  }, [namespace, resetPaging]);

  const items = state.kind === "ready" ? state.items : [];
  const nextCursor = state.kind === "ready" ? state.nextCursor : "";
  // hasNext keys off the CURSOR (BFF), never items.length — an empty filtered
  // window with more pages must keep Next live (the cursor-vs-q rule).
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;
  // The loaded window IS the whole fleet only when no cursor precedes or
  // follows it. That is the one condition under which a count of the rows in
  // hand is a FACT rather than a windowed guess — see the header note.
  const fleetComplete = state.kind === "ready" && !hasNext && !hasPrev;

  // Triage once, sort once. Attention-first (§6.1): nextStepRank is the primary
  // key so "Nothing needed" always sinks; the attention order breaks ties.
  const sorted = useMemo(() => {
    const rows = items.map(triage);
    rows.sort(
      (x, y) =>
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        attention(x) - attention(y) ||
        x.agent.name.localeCompare(y.agent.name),
    );
    return rows;
  }, [items]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(
    () => sorted.filter(activeView.match),
    [sorted, activeView],
  );

  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    // No number unless it is the whole fleet's number (kit FilterChipRow
    // contract). A count that describes one page while looking like a total is
    // the failure mode that hides work.
    count: fleetComplete ? sorted.filter(v.match).length : undefined,
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
          resource: "agents",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // Columns are the §4.4 "resource list" budget, in visual order. Priorities are
  // the whole responsive story: 4 leaves below 1280, 3 below 1024, 1 never.
  // Entity, State and Next step survive every width, because they are the row's
  // identity, its condition, and what to do about it.
  const columns: Column<Triaged>[] = [
    {
      id: "agent",
      header: "Agent",
      priority: 1,
      cell: ({ agent: a }) => (
        <CellEntity
          // The cap is what makes `truncate` bite: it clamps the cell's
          // max-content contribution, so a 63-character name can no longer set
          // the width of the table (and through it, of the document).
          className="max-w-[20rem]"
          title={a.name}
          name={
            <span className="flex min-w-0 items-center gap-1.5">
              {/* One line, end-ellipsis, full value in the cell's title — never
                  `break-all`, which turns a name into a five-line paragraph
                  (§4.5). */}
              <span className="truncate">{a.name}</span>
              {/* Provenance rides with the agent IDENTITY (M100 UI99-refs), not
                  in the STATUS lane: "external" and "draft" describe WHAT the
                  agent is, not how healthy it is. */}
              {a.managedOutsideUI && !a.drift && (
                <Badge
                  variant="muted"
                  data-testid={`external-${a.name}`}
                  title="Created outside the console (e.g. kubectl) — edits are limited."
                >
                  external
                </Badge>
              )}
              {a.isDraft && (
                <Badge variant="muted" data-testid={`draft-${a.name}`}>
                  draft
                </Badge>
              )}
            </span>
          }
          namespace={
            <span title={a.namespace} data-testid={`namespace-${a.name}`}>
              {shortNamespace(a.namespace)}
            </span>
          }
        />
      ),
    },
    {
      id: "lifecycle",
      header: "Lifecycle",
      priority: 4,
      className: "w-[11.5rem]",
      cell: (t) => <LifecycleTrack stage={t.stage} stopped={t.halted} />,
    },
    {
      id: "state",
      header: "State",
      priority: 1,
      className: "w-[13rem]",
      cell: ({ agent: a }) => (
        <div className="flex min-w-0 flex-col items-start gap-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <StatusBadge ready={a.ready} phase={a.phase} reason={a.reason} />
            {/* Only drift stays in the STATUS lane — it is a health/sync signal
                (the live spec diverged, ADR 0017), and a bound crossed while
                still serving is exactly what warn means (§2.2). */}
            {a.drift && (
              <Badge
                variant="warn"
                data-testid={`drift-${a.name}`}
                title="The live spec has diverged from the console config (ADR 0017)."
              >
                drift
              </Badge>
            )}
          </div>
          {/* The NotReady reason inline (m23.7b) so a reader sees WHY without
              clicking in. Truncated on one line with the controller's full
              message in `title`. */}
          {!a.ready && a.reason && (
            <span
              className="max-w-[12rem] truncate text-xs text-faint"
              data-testid={`agent-reason-${a.name}`}
              title={a.message || a.reason}
            >
              {humanizeStatusReason(a.reason)}
            </span>
          )}
        </div>
      ),
    },
    {
      id: "runs",
      header: "Runs 24h",
      priority: 3,
      numeric: true,
      cell: () => (
        <QuantityValue
          value={UNKNOWN}
          title="Runs are not recorded in the fleet list — unknown, not zero. See the note above the table."
        />
      ),
    },
    {
      id: "spend",
      header: "Spend 24h",
      priority: 3,
      numeric: true,
      cell: () => (
        <QuantityValue
          value={UNKNOWN}
          title="Spend is not recorded in the fleet list — unknown, not zero. See the note above the table."
        />
      ),
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
          ariaLabel={t.next.label ? `${t.next.label} — ${t.agent.name}` : undefined}
          testId={`next-step-${t.agent.name}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching a user with 13 agents how to make their first.
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
        totalCount: fleetComplete ? items.length : undefined,
        countNoun: "agents",
      }
    : {
        icon: Boxes,
        title: "No agents yet",
        description: namespace
          ? `No agents in ${namespace}. An agent is one deployed assistant — its model route, its tools, and the policy it runs under. Create the first one to see it here.`
          : "An agent is one deployed assistant — its model route, its tools, and the policy it runs under. Create the first one to see it here.",
        action: canCreate
          ? { label: "New agent", icon: Sparkles, onClick: () => navigate("/agents/new") }
          : undefined,
      };

  const closing = closingLine(sorted, fleetComplete);
  const showChips = state.kind !== "error";
  const showNote = state.kind === "ready" && items.length > 0;
  const metaLine =
    state.kind === "ready"
      ? fleetComplete
        ? `${items.length} agent${items.length === 1 ? "" : "s"}`
        : `${items.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Agents"
        meta={metaLine}
        lede="Sorted by what is blocking. Whatever is waiting on a person sits at the top; everything running itself sits below it."
        // The primary and the drafts toggle both go through `actionsSlot` rather
        // than the structured `actions` list: `PageHeaderAction` carries no
        // `testId`, and the black-box viewer suite asserts on `new-agent-button`
        // being absent for a viewer — an assertion that would silently pass
        // forever if the id disappeared.
        actionsSlot={
          <>
            <Button
              variant={includeDrafts ? "secondary" : "outline"}
              size="sm"
              className="text-sm"
              onClick={() => {
                setIncludeDrafts((v) => !v);
                resetPaging();
              }}
              data-testid="drafts-toggle"
            >
              {includeDrafts ? "Hide drafts" : "Show drafts"}
            </Button>
            {/* New agent is a page action, not a nav item (m25 S8): the create
                entry point lives with the list it creates into. Hidden from a
                viewer's chrome — gated on create agentdeployments. */}
            {canCreate && (
              <Button asChild size="sm" className="text-sm" data-testid="new-agent-button">
                <Link to="/agents/new">
                  <Sparkles className="h-4 w-4" />
                  New agent
                </Link>
              </Button>
            )}
          </>
        }
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter agents"
          className="min-w-0"
        />
      )}

      {showNote && (
        <QuietNote title="Runs and spend aren’t in the fleet list.">
          This list reads the agent registry, which records what each agent{" "}
          <em>is</em> — not what it has done. Per-agent run counts and spend come
          from the trace backend; until one is wired up, those two columns read
          “—”. Nothing here is estimated — the figures are simply absent.
        </QuietNote>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={(t) => `${t.agent.namespace}/${t.agent.name}`}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={onQueryChange}
        queryPlaceholder="Filter agents on this page…"
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Agents"
        // A stopped agent takes the crit tint at rest (§5.10) — it is the one
        // row a reader must not scroll past.
        rowHalted={(t) => t.halted}
        // Row-click → the agent LANDING page (m14.11): the detail/status/logs/run
        // surface. Keyed on namespace/name (the same key the row uses).
        onRowClick={(t) =>
          navigate(
            `/agents/${encodeURIComponent(t.agent.namespace)}/${encodeURIComponent(t.agent.name)}`,
          )
        }
        // The actions column only appears when the caller can edit or delete.
        // A viewer sees a clean table with no dead or greyed-out buttons.
        rowActions={
          canEdit || canDelete
            ? (t) => (
                <RowActions
                  agent={t.agent}
                  canEdit={canEdit}
                  canDelete={canDelete}
                  onEdit={(agent) =>
                    navigate(
                      `/agents/${encodeURIComponent(agent.namespace)}/${encodeURIComponent(agent.name)}?edit=1`,
                    )
                  }
                  onDelete={(agent) =>
                    navigate(
                      `/agents/${encodeURIComponent(agent.namespace)}/${encodeURIComponent(agent.name)}?delete=1`,
                    )
                  }
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
