import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Boxes, Pencil, Sparkles, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type AgentSummary } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_AGENTS } from "@/lib/nav";

// AgentsPage — the FIRST DataTable consumer (ui-foundation §4/§6). Re-housed from
// the M12 card list into the console's ONE table, backed by the list contract:
//   GET /api/agents?limit&cursor&q&namespace → { items, nextCursor }
// It reads `items` (NOT the legacy `agents` key) and pages by the opaque cursor.
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

// RowActions renders per-row edit + delete affordances, RBAC-aware.
// Hidden entirely for viewers (capabilities-driven, display-only — the API is
// the real gate, ADR 0011). We prevent the row-click from propagating on the
// action buttons so they don't also trigger navigation.
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
      className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${agent.name}`}
    >
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
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
          className="h-7 w-7 text-destructive hover:text-destructive"
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
  const canEdit = can(RES_AGENTS, "update");
  const canDelete = can(RES_AGENTS, "delete");

  const [query, setQuery] = useState("");
  const [includeDrafts, setIncludeDrafts] = useState(false);
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
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // Columns are rebuilt when RBAC capabilities change so the actions column
  // only renders when the caller can act.
  const columns: Column<AgentSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (a) => <span className="font-medium">{a.name}</span>,
    },
    {
      id: "namespace",
      header: "Namespace",
      cell: (a) => <span className="text-muted-foreground">{a.namespace}</span>,
    },
    {
      id: "image",
      header: "Image",
      hideOnMobile: true,
      className: "font-mono text-xs text-muted-foreground",
      cell: (a) => a.image || "—",
    },
    {
      id: "phase",
      header: "Status",
      className: "w-44",
      cell: (a) => (
        <div className="flex flex-wrap items-center gap-1.5">
          <StatusBadge ready={a.ready} phase={a.phase} />
          {/* Inline the NotReady reason (m23.7b) so a user sees WHY (e.g.
              "RevisionFailed") without clicking into the detail page. Only shown
              when not ready and a reason is present; the full message is the
              hover title. */}
          {!a.ready && a.reason && (
            <span
              className="text-xs text-muted-foreground"
              data-testid={`agent-reason-${a.name}`}
              title={a.message || a.reason}
            >
              {a.reason}
            </span>
          )}
          {/* Fleet-health badges (m18.12) from the m18.11 flags. */}
          {a.drift && (
            <Badge
              variant="warning"
              data-testid={`drift-${a.name}`}
              title="The live spec has diverged from the console config (ADR 0017)."
            >
              drift
            </Badge>
          )}
          {a.managedOutsideUI && !a.drift && (
            <Badge
              variant="secondary"
              data-testid={`external-${a.name}`}
              title="Created outside the console (e.g. kubectl) — edits are limited."
            >
              external
            </Badge>
          )}
          {a.isDraft && (
            <Badge variant="secondary" data-testid={`draft-${a.name}`}>
              draft
            </Badge>
          )}
        </div>
      ),
    },
    // The actions column only appears when the caller can edit or delete.
    // A viewer sees a clean 4-column table (no dead/greyed buttons).
    ...(canEdit || canDelete
      ? [
          {
            id: "actions" as const,
            header: "",
            className: "w-20 text-right",
            cell: (a: AgentSummary) => (
              <RowActions
                agent={a}
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
            ),
          },
        ]
      : []),
  ];

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Agents</h2>
          <p className="text-sm text-muted-foreground">
            Your agents, RBAC-scoped to what you can see. The filter is windowed
            to the loaded page.
          </p>
        </div>
        {/* New agent is a page action, not a nav item (m25 S8): the primary
            create entry point lives with the list it creates into. Hidden from a
            viewer's chrome — gated on create agentdeployments. */}
        {can(RES_AGENTS, "create") && (
          <Button
            onClick={() => navigate("/agents/new")}
            data-testid="new-agent-button"
            className="shrink-0"
          >
            <Sparkles className="mr-1.5 h-4 w-4" />
            New agent
          </Button>
        )}
        <Button
          variant={includeDrafts ? "secondary" : "outline"}
          onClick={() => { setIncludeDrafts((v) => !v); resetPaging(); }}
          data-testid="drafts-toggle"
          size="sm"
        >
          {includeDrafts ? "Hide drafts" : "Show drafts"}
        </Button>
      </div>

      <DataTable<AgentSummary>
        columns={columns}
        rows={items}
        rowKey={(a) => `${a.namespace}/${a.name}`}
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
        // Row-click → the agent LANDING page (m14.11): the detail/status/logs/run
        // surface. Keyed on namespace/name (the same key the row uses).
        onRowClick={(a) =>
          navigate(`/agents/${encodeURIComponent(a.namespace)}/${encodeURIComponent(a.name)}`)
        }
        empty={{
          icon: Boxes,
          title: "No agents yet",
          description:
            namespace
              ? `No agents in ${namespace}. Use “New agent” above to create one.`
              : "No agents visible. Use “New agent” above to create one.",
        }}
      />
    </div>
  );
}
