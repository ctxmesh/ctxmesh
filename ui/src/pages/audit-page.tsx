import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ScrollText } from "lucide-react";

import { DataTable, DetailDrawer, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type AuditEvent, type AuditListParams } from "@/lib/api";

// AuditPage — the compliance audit trail viewer (m63.5, ADR 0056 §4).
//
// Backend: GET /api/audit?namespace=&actor=&action=&kind=&from=&to=&limit=&cursor=
//   • All filters are SERVER-SIDE (the store filters by exact match); there is NO
//     page-windowed client substring — the audit_log is Postgres-backed, so unlike
//     Runs every filter narrows the whole table, not just the loaded page.
//   • namespace comes from the GLOBAL namespace selector ("" = cluster-wide, which
//     only the operator persona can read). cursor is the opaque keyset token.
//   • action and kind are SELECT dropdowns (closed vocabularies) — a free-text
//     typo would silently produce zero rows; a dropdown prevents that (m76.5 H1).
//   • from/to are datetime-local inputs (m76.5 H2) matching the Runs filter bar.
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
//   audit-detail-drawer — the detail drawer (row-click)

const PAGE_LIMIT = 50;

// AUDIT_ACTIONS — the closed vocabulary for the `action` field. Enumerated directly
// from the Go source that writes each audit row (do NOT add values not present there;
// a non-existent value silently produces zero rows — the exact bug H1 was built to kill):
//   "connect"         — internal/bff/audit_events.go:auditActionConnect
//   "grant.create"    — internal/bff/audit_events.go:auditActionGrantCreate
//   "grant.revoke"    — internal/bff/audit_events.go:auditActionGrantRevoke
//   "share.create"    — internal/bff/shares.go:auditActionShareCreate
//   "share.revoke"    — internal/bff/shares.go:auditActionShareRevoke
//   "guardrail.block" — internal/bff/guardrail_event_handler.go:auditActionGuardrailBlock
//   "create"          — internal/audit/audit.go:VerbCreate (controller CRD mutations)
//   "update"          — internal/audit/audit.go:VerbUpdate (controller CRD mutations)
//   "delete"          — internal/audit/audit.go:VerbDelete (controller CRD mutations)
// NOTE: "connect.denied" was REMOVED — denial is outcome="denied" on action="connect",
// not a separate action string. No audit row ever carries action="connect.denied".
const AUDIT_ACTIONS = [
  "connect",
  "grant.create",
  "grant.revoke",
  "share.create",
  "share.revoke",
  "guardrail.block",
  "create",
  "update",
  "delete",
] as const;

// AUDIT_KINDS — the closed vocabulary for the `resourceKind` field. Enumerated from
// the Go source that writes ResourceKind in each audit row:
//   BFF rows (internal/bff/):
//     "Provider"       — providers.go:resourceKindProvider
//     "MCPGrant"       — mcp_grant_handlers.go:resourceKindMCPGrant
//     "GuardrailPolicy" — guardrail_event_handler.go (literal "GuardrailPolicy")
//     "SharedRun"      — shares.go:auditKindSharedRun
//   Controller rows (internal/audit/auditor.go:auditedTypes — the scheme-resolved Kind):
//     "AgentDeployment", "AgentVersion", "ModelRoute", "SecretBinding",
//     "MCPToolBinding", "MemoryBinding", "AgentRegistry", "AgentScalingPolicy",
//     "EvalSuite"
// NOTE: "PromptVersion"/"ToolRegistry" were retired to Postgres (ADR 0044) and are
// no longer CRDs — they are NOT in auditedTypes() and produce no controller rows.
// "AgentTeam"/"Workflow"/"KnowledgeBase" were REMOVED — not in auditedTypes() and
// have no BFF audit rows.
const AUDIT_KINDS = [
  "Provider",
  "MCPGrant",
  "GuardrailPolicy",
  "SharedRun",
  "AgentDeployment",
  "AgentVersion",
  "ModelRoute",
  "SecretBinding",
  "MCPToolBinding",
  "MemoryBinding",
  "AgentRegistry",
  "AgentScalingPolicy",
  "EvalSuite",
] as const;

// toRFC3339 converts a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time, no seconds/timezone) into a UTC RFC3339 string the BFF's audit
// filter accepts. Returns "" for empty/unparseable input so the caller omits the
// param rather than sending a malformed one. Mirrors runs-page.tsx (m76.5 H2).
function toRFC3339(dtLocal: string): string {
  if (!dtLocal) return "";
  const d = new Date(dtLocal);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString();
}

// AuditFilterBar holds the server-side filters: actor (free-text, open cardinality),
// action + kind (Select dropdowns — closed vocabularies), and from/to date range.
function AuditFilterBar({
  actor,
  action,
  kind,
  from,
  to,
  onActor,
  onAction,
  onKind,
  onFrom,
  onTo,
}: {
  actor: string;
  action: string;
  kind: string;
  from: string;
  to: string;
  onActor: (v: string) => void;
  onAction: (v: string) => void;
  onKind: (v: string) => void;
  onFrom: (v: string) => void;
  onTo: (v: string) => void;
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
        <Select
          id="audit-filter-action"
          value={action}
          onChange={(e) => onAction(e.target.value)}
          className="h-8 w-44 text-sm"
          aria-label="Filter by action"
        >
          <option value="">Any</option>
          {AUDIT_ACTIONS.map((a) => (
            <option key={a} value={a}>{a}</option>
          ))}
        </Select>
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-kind" className="text-xs font-medium text-muted-foreground">
          Resource kind
        </label>
        <Select
          id="audit-filter-kind"
          value={kind}
          onChange={(e) => onKind(e.target.value)}
          className="h-8 w-44 text-sm"
          aria-label="Filter by resource kind"
        >
          <option value="">Any</option>
          {AUDIT_KINDS.map((k) => (
            <option key={k} value={k}>{k}</option>
          ))}
        </Select>
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-from" className="text-xs font-medium text-muted-foreground">
          From
        </label>
        <Input
          id="audit-filter-from"
          type="datetime-local"
          value={from}
          onChange={(e) => onFrom(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter from date"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-to" className="text-xs font-medium text-muted-foreground">
          To
        </label>
        <Input
          id="audit-filter-to"
          type="datetime-local"
          value={to}
          onChange={(e) => onTo(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter to date"
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

// AuditDetailDrawer — click-through detail for one audit event (m76.5 H3).
// Opens as a right-side DetailDrawer with the full detail k=v map as a definition
// list and the "View run" trace link as a first-class action. Fixes the a11y gap
// where the full detail was only in title= (invisible to touch/keyboard/SR).
function AuditDetailDrawer({
  event: e,
  onClose,
}: {
  event: AuditEvent | null;
  onClose: () => void;
}) {
  if (!e) return null;
  const detail = e.detail ? Object.entries(e.detail) : [];
  return (
    <DetailDrawer
      open={!!e}
      onClose={onClose}
      title={
        <span className="font-mono text-base">{e.action}</span>
      }
      subtitle={`${e.actor || "—"} · ${fmtTimestamp(e.occurredAt)}`}
      status={<Badge variant={outcomeVariant(e.outcome)}>{e.outcome}</Badge>}
      size="md"
      footer={
        e.traceId ? (
          <Link
            to={`/traces/${encodeURIComponent(e.traceId)}`}
            data-testid={`audit-trace-link-${e.id}`}
            className="text-sm font-medium text-primary hover:underline"
          >
            View run
          </Link>
        ) : undefined
      }
    >
      <div data-testid="audit-detail-drawer" className="space-y-6">
        {/* Resource + namespace */}
        <section>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Resource
          </h3>
          <dl className="space-y-1">
            {e.resourceKind && (
              <div className="flex gap-2 text-sm">
                <dt className="w-24 shrink-0 text-muted-foreground">Kind</dt>
                <dd className="font-medium">{e.resourceKind}</dd>
              </div>
            )}
            {e.resourceName && (
              <div className="flex gap-2 text-sm">
                <dt className="w-24 shrink-0 text-muted-foreground">Name</dt>
                <dd className="font-mono">{e.resourceName}</dd>
              </div>
            )}
            {e.namespace && (
              <div className="flex gap-2 text-sm">
                <dt className="w-24 shrink-0 text-muted-foreground">Namespace</dt>
                <dd className="font-mono">{e.namespace}</dd>
              </div>
            )}
          </dl>
        </section>

        {/* Actor */}
        <section>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Actor
          </h3>
          <dl className="space-y-1">
            <div className="flex gap-2 text-sm">
              <dt className="w-24 shrink-0 text-muted-foreground">Name</dt>
              <dd className="font-medium">{e.actor || "—"}</dd>
            </div>
            <div className="flex gap-2 text-sm">
              <dt className="w-24 shrink-0 text-muted-foreground">Kind</dt>
              <dd>{e.actorKind}</dd>
            </div>
            <div className="flex gap-2 text-sm">
              <dt className="w-24 shrink-0 text-muted-foreground">Source</dt>
              <dd className="text-xs text-muted-foreground">{e.source}</dd>
            </div>
          </dl>
        </section>

        {/* Detail context map */}
        {detail.length > 0 && (
          <section>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Detail
            </h3>
            <dl className="space-y-1">
              {detail.map(([k, v]) => (
                <div key={k} className="flex gap-2 text-sm">
                  <dt className="w-24 shrink-0 text-muted-foreground">{k}</dt>
                  <dd className="break-all font-mono text-xs">
                    {typeof v === "string" ? v : JSON.stringify(v)}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        )}

        {/* Event metadata */}
        <section>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Event
          </h3>
          <dl className="space-y-1">
            <div className="flex gap-2 text-sm">
              <dt className="w-24 shrink-0 text-muted-foreground">ID</dt>
              <dd className="font-mono text-xs">{e.id}</dd>
            </div>
            <div className="flex gap-2 text-sm">
              <dt className="w-24 shrink-0 text-muted-foreground">When</dt>
              <dd className="text-xs">{fmtTimestamp(e.occurredAt)}</dd>
            </div>
            {e.traceId && (
              <div className="flex gap-2 text-sm">
                <dt className="w-24 shrink-0 text-muted-foreground">Trace ID</dt>
                <dd className="break-all font-mono text-xs">{e.traceId}</dd>
              </div>
            )}
          </dl>
        </section>
      </div>
    </DetailDrawer>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: AuditEvent[]; nextCursor: string }
  | { kind: "unavailable" } // 501 — audit store not configured
  | { kind: "error"; message: string; forbidden: boolean };

export function AuditPage() {
  const { namespace } = useNamespace();

  // Server-side filters.
  const [actorFilter, setActorFilter] = useState("");
  const [actionFilter, setActionFilter] = useState("");
  const [kindFilter, setKindFilter] = useState("");
  const [fromFilter, setFromFilter] = useState("");
  const [toFilter, setToFilter] = useState("");

  // Cursor pagination: a stack of keyset cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  // Detail drawer — null when closed.
  const [drawerEvent, setDrawerEvent] = useState<AuditEvent | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    // The <input type="datetime-local"> value is "YYYY-MM-DDTHH:MM" in LOCAL time
    // (no seconds/zone). Convert to UTC RFC3339 before sending to the BFF.
    const fromRfc = toRFC3339(fromFilter);
    const toRfc = toRFC3339(toFilter);

    const params: AuditListParams = {
      limit: PAGE_LIMIT,
      ...(namespace ? { namespace } : {}),
      ...(cursor ? { cursor } : {}),
      ...(actorFilter ? { actor: actorFilter } : {}),
      ...(actionFilter ? { action: actionFilter } : {}),
      ...(kindFilter ? { kind: kindFilter } : {}),
      ...(fromRfc ? { from: fromRfc } : {}),
      ...(toRfc ? { to: toRfc } : {}),
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
  }, [namespace, cursor, actorFilter, actionFilter, kindFilter, fromFilter, toFilter]);

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
  function onFromChange(v: string) {
    setFromFilter(v);
    resetPaging();
  }
  function onToChange(v: string) {
    setToFilter(v);
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
        // Truncated preview — full detail is in the drawer (row-click, H3).
        return detail ? (
          <span className="max-w-[22rem] truncate font-mono text-xs text-muted-foreground">
            {detail}
          </span>
        ) : null;
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
        onRowClick={(e) => setDrawerEvent(e)}
        toolbar={
          <AuditFilterBar
            actor={actorFilter}
            action={actionFilter}
            kind={kindFilter}
            from={fromFilter}
            to={toFilter}
            onActor={onActorChange}
            onAction={onActionChange}
            onKind={onKindChange}
            onFrom={onFromChange}
            onTo={onToChange}
          />
        }
        empty={{
          icon: ScrollText,
          title: "No audit events",
          description:
            actorFilter || actionFilter || kindFilter || fromFilter || toFilter
              ? "No events match these filters. Try clearing the actor, action, resource-kind, or date-range filter."
              : "No audit events in this scope yet. Try widening the namespace scope (top bar), or check back after activity occurs.",
        }}
      />

      <AuditDetailDrawer event={drawerEvent} onClose={() => setDrawerEvent(null)} />
    </div>
  );
}
