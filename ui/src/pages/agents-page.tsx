import { useCallback, useEffect, useRef, useState } from "react";
import { Boxes } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { api, ApiError, type AgentSummary } from "@/lib/api";
import { useNamespace } from "@/lib/namespace";

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

const PAGE_LIMIT = 50;

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
    className: "w-32",
    cell: (a) => (
      <Badge variant={a.ready ? "success" : "warning"}>
        {a.phase || (a.ready ? "Ready" : "Pending")}
      </Badge>
    ),
  },
];

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: AgentSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

export function AgentsPage() {
  const { namespace } = useNamespace();
  const [query, setQuery] = useState("");
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
        { limit: PAGE_LIMIT, cursor: cursor || undefined, q: query || undefined, namespace: namespace || undefined },
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

  return (
    <div className="mx-auto max-w-5xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Agents</h2>
        <p className="text-sm text-muted-foreground">
          AgentDeployments listed via the BFF (client-go, RBAC-scoped). The
          filter is windowed to the loaded page.
        </p>
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
        empty={{
          icon: Boxes,
          title: "No agents yet",
          description:
            namespace
              ? `No AgentDeployments in ${namespace}. Create one in the config builder or Playground.`
              : "No AgentDeployments visible. Create one in the config builder or Playground.",
        }}
      />
    </div>
  );
}
