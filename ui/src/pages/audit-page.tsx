import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ScrollText } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type AuditEvent, type AuditListParams } from "@/lib/api";

// AuditPage — the compliance audit trail viewer (m63.5, ADR 0056 §4).
//
// Backend: GET /api/audit?namespace=&actor=&action=&kind=&limit=&cursor=
//   • All filters are SERVER-SIDE (the store filters by exact match); there is NO
//     page-windowed client substring — the audit_log is Postgres-backed, so unlike
//     Runs every filter narrows the whole table, not just the loaded page.
//   • namespace comes from the GLOBAL namespace selector ("" = cluster-wide, which
//     only the operator persona can read). cursor is the opaque keyset token.
//
// 501-calm / 403-forbidden / 502-error discipline (mirrors runs-page):
//   • 501 (audit store not configured — control-plane DSN absent): listAudit()
//     returns null → calm "not enabled" empty state, NEVER an error toast.
//   • 403 (caller lacks the operator `auditlogs` persona): ApiError.isForbidden →
//     the DataTable's forbidden variant, never a fake empty list.
//   • 500 (DB read failed) + other non-2xx: a visible, retryable error state.
//
// RBAC: OPERATOR-ONLY. The nav item is hidden from developer/viewer chrome (gated
// on `list auditlogs`, nav.ts); a non-operator who deep-links gets an honest 403.
// The page is READ-only — the audit trail is append-only, never edited here.
//
// data-testid contract:
//   audit-page          — root container
//   audit-table         — the DataTable (via aria-label="Audit events")
//   audit-filter-bar    — the filter inputs container
//   audit-unavailable   — the 501 "not enabled" state
//   audit-row-{id}      — each table row (via rowKey)

const PAGE_LIMIT = 50;

// AuditFilterBar holds the three server-side exact-match filters: actor, action,
// resource kind. (Namespace is the global selector; date range is a fast-follow.)
function AuditFilterBar({
  actor,
  action,
  kind,
  onActor,
  onAction,
  onKind,
}: {
  actor: string;
  action: string;
  kind: string;
  onActor: (v: string) => void;
  onAction: (v: string) => void;
  onKind: (v: string) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-3" data-testid="audit-filter-bar">
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-actor" className="text-xs font-medium text-muted-foreground">
          Actor
        </label>
        <Input
          id="audit-filter-actor"
          value={actor}
          onChange={(e) => onActor(e.target.value)}
          placeholder="username"
          className="h-8 w-44 text-sm"
          aria-label="Filter by actor"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-action" className="text-xs font-medium text-muted-foreground">
          Action
        </label>
        <Input
          id="audit-filter-action"
          value={action}
          onChange={(e) => onAction(e.target.value)}
          placeholder="connect, grant.create…"
          className="h-8 w-44 text-sm"
          aria-label="Filter by action"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-kind" className="text-xs font-medium text-muted-foreground">
          Resource kind
        </label>
        <Input
          id="audit-filter-kind"
          value={kind}
          onChange={(e) => onKind(e.target.value)}
          placeholder="Provider, MCPGrant…"
          className="h-8 w-44 text-sm"
          aria-label="Filter by resource kind"
        />
      </div>
    </div>
  );
}

function fmtTimestamp(ts: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

// outcomeVariant maps an audit outcome to a Badge variant: success is calm, a
// denial warns (a refused action — compliance-relevant), an error is destructive.
function outcomeVariant(outcome: string): "success" | "warning" | "destructive" | "secondary" {
  switch (outcome) {
    case "success":
      return "success";
    case "denied":
      return "warning";
    case "error":
      return "destructive";
    default:
      return "secondary";
  }
}

// renderDetail flattens the non-secret detail map to compact `k=v` text. The store
// never held secret material (tokens live only in grant Secrets), so this is safe.
function renderDetail(detail?: Record<string, unknown>): string {
  if (!detail) return "";
  return Object.entries(detail)
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" · ");
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: AuditEvent[]; nextCursor: string }
  | { kind: "unavailable" } // 501 — audit store not configured
  | { kind: "error"; message: string; forbidden: boolean };

export function AuditPage() {
  const { namespace } = useNamespace();

  // Server-side exact-match filters.
  const [actorFilter, setActorFilter] = useState("");
  const [actionFilter, setActionFilter] = useState("");
  const [kindFilter, setKindFilter] = useState("");

  // Cursor pagination: a stack of keyset cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    const params: AuditListParams = {
      limit: PAGE_LIMIT,
      ...(namespace ? { namespace } : {}),
      ...(cursor ? { cursor } : {}),
      ...(actorFilter ? { actor: actorFilter } : {}),
      ...(actionFilter ? { action: actionFilter } : {}),
      ...(kindFilter ? { kind: kindFilter } : {}),
    };

    api
      .listAudit(params, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (audit store not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        setLoadState({
          kind: "ready",
          items: res.items,
          nextCursor: res.nextCursor ?? "",
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 403 (no operator persona) + 500 (DB) surface as a real error; 403 renders
        // the forbidden variant (never a fake empty list).
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [namespace, cursor, actorFilter, actionFilter, kindFilter]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Reset to page 0 whenever a filter (or the global namespace) changes.
  const resetPaging = useCallback(() => setPageStack([""]), []);
  function onActorChange(v: string) {
    setActorFilter(v);
    resetPaging();
  }
  function onActionChange(v: string) {
    setActionFilter(v);
    resetPaging();
  }
  function onKindChange(v: string) {
    setKindFilter(v);
    resetPaging();
  }

  const items = loadState.kind === "ready" ? loadState.items : [];
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR, never off items.length (keyset contract).
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
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<AuditEvent>[] = [
    {
      id: "occurredAt",
      header: "When",
      cell: (e) => (
        <span className="text-sm text-muted-foreground">{fmtTimestamp(e.occurredAt)}</span>
      ),
    },
    {
      id: "actor",
      header: "Actor",
      cell: (e) => (
        <span className="font-medium">
          {e.actor || "—"}
          {e.actorKind && e.actorKind !== "user" ? (
            <span className="ml-1 text-xs font-normal text-muted-foreground">({e.actorKind})</span>
          ) : null}
        </span>
      ),
    },
    {
      id: "action",
      header: "Action",
      cell: (e) => <span className="font-mono text-xs">{e.action}</span>,
    },
    {
      id: "resource",
      header: "Resource",
      hideOnMobile: true,
      cell: (e) =>
        e.resourceKind || e.resourceName ? (
          <span className="text-sm">
            <span className="text-muted-foreground">{e.resourceKind}</span>
            {e.resourceName ? <span className="font-medium">/{e.resourceName}</span> : null}
          </span>
        ) : (
          <span className="text-sm text-muted-foreground">—</span>
        ),
    },
    {
      id: "namespace",
      header: "Namespace",
      hideOnMobile: true,
      cell: (e) => (
        <span className="text-sm text-muted-foreground">{e.namespace || "—"}</span>
      ),
    },
    {
      id: "outcome",
      header: "Outcome",
      cell: (e) => <Badge variant={outcomeVariant(e.outcome)}>{e.outcome}</Badge>,
    },
    {
      id: "source",
      header: "Source",
      hideOnMobile: true,
      cell: (e) => <span className="text-xs text-muted-foreground">{e.source}</span>,
    },
    {
      id: "detail",
      header: "Detail",
      hideOnMobile: true,
      cell: (e) => {
        const detail = renderDetail(e.detail);
        return (
          <div className="flex items-center gap-2">
            {detail ? (
              <span className="max-w-[22rem] truncate font-mono text-xs text-muted-foreground" title={detail}>
                {detail}
              </span>
            ) : null}
            {e.traceId ? (
              // A row's trace_id links to the native run/trace view — the "invoke
              // story" is reached via the run, not re-recorded in the audit table.
              <Link
                to={`/traces/${encodeURIComponent(e.traceId)}`}
                data-testid={`audit-trace-link-${e.id}`}
                onClick={(ev) => ev.stopPropagation()}
                className="text-xs font-medium text-primary hover:underline"
              >
                View run
              </Link>
            ) : null}
          </div>
        );
      },
    },
  ];

  // 501 calm state — the audit store is not configured (no control-plane DSN).
  if (loadState.kind === "unavailable") {
    return (
      <div className="mx-auto max-w-6xl space-y-6" data-testid="audit-page">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Audit</h2>
          <p className="text-sm text-muted-foreground">
            The compliance trail — who connected, consented, and invoked what.
          </p>
        </div>
        <div
          className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
          data-testid="audit-unavailable"
        >
          Audit unavailable — the audit store is not configured (control-plane database not wired).
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="audit-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Audit</h2>
        <p className="text-sm text-muted-foreground">
          The compliance trail — who connected, consented, and invoked what. Every filter narrows
          server-side; switch the namespace scope with the global namespace selector.
        </p>
      </div>

      <DataTable<AuditEvent>
        columns={columns}
        rows={items}
        rowKey={(e) => String(e.id)}
        loading={loadState.kind === "loading"}
        error={error}
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Audit events"
        toolbar={
          <AuditFilterBar
            actor={actorFilter}
            action={actionFilter}
            kind={kindFilter}
            onActor={onActorChange}
            onAction={onActionChange}
            onKind={onKindChange}
          />
        }
        empty={{
          icon: ScrollText,
          title: "No audit events",
          description:
            actorFilter || actionFilter || kindFilter
              ? "No events match these filters. Try clearing the actor, action, or resource-kind filter."
              : "No audit events recorded yet in this scope. Connect a provider or grant an MCP server to see events here.",
        }}
      />
    </div>
  );
}
