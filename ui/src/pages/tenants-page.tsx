import { useCallback, useEffect, useRef, useState } from "react";
import { Building2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable, type Column, type DataTableError } from "@/components/kit";
import {
  api,
  ApiError,
  type TenantSummary,
  type TenantDetail,
  type TenantUsage,
} from "@/lib/api";

// TenantsPage (M47, ADR 0046) — a read-only list of cluster-scoped Tenants (namespace groupings + compute +
// model quotas). Row-click opens an inline detail panel (member namespaces + quota/model caps + conflicts).
// RBAC-aware: the whole surface degrades to the DataTable's forbidden state on a 403 (viewers/developers read;
// operators manage — enforced at the API server, ADR 0011). No end-user PII (a Tenant is config only).

type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: TenantSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

// The inline detail panel's own load state, so a row-click gives immediate feedback and a failed fetch is
// surfaced (never silently opens nothing).
type Detail =
  | { kind: "none" }
  | { kind: "loading"; name: string }
  | { kind: "ready"; detail: TenantDetail }
  | { kind: "error"; name: string; message: string };

function quotaSummary(t: TenantDetail): string {
  if (!t.quota) return "—";
  const parts = [
    t.quota.cpu && `cpu ${t.quota.cpu}`,
    t.quota.memory && `mem ${t.quota.memory}`,
    t.quota.pods ? `pods ${t.quota.pods}` : undefined,
  ].filter(Boolean);
  return parts.length ? parts.join(" · ") : "—";
}

function modelSummary(t: TenantDetail): string {
  if (!t.model) return "—";
  const parts = [
    t.model.budgetUSD && `$${t.model.budgetUSD} budget`,
    t.model.rpm ? `${t.model.rpm} req/min` : undefined,
    t.model.maxConcurrent ? `${t.model.maxConcurrent} concurrent` : undefined,
  ].filter(Boolean);
  return parts.length ? parts.join(" · ") : "—";
}

// usageSummary renders the tenant's LIVE consumption vs its caps (M49 — "who's about to be throttled?").
function usageSummary(tenant: TenantDetail, u: TenantUsage): string {
  const parts = [
    tenant.model?.budgetUSD
      ? `$${u.spendUSD.toFixed(2)} / $${tenant.model.budgetUSD} spent`
      : `$${u.spendUSD.toFixed(2)} spent`,
    tenant.model?.rpm ? `${u.rpm} / ${tenant.model.rpm} req/min` : `${u.rpm} req/min`,
    tenant.model?.maxConcurrent
      ? `${u.inFlight} / ${tenant.model.maxConcurrent} in-flight`
      : `${u.inFlight} in-flight`,
  ];
  return parts.join(" · ");
}

function TenantDetailPanel({
  tenant,
  onClose,
}: {
  tenant: TenantDetail;
  onClose: () => void;
}) {
  const conflict = tenant.conditions.find(
    (c) => c.type === "NamespaceConflict" && c.status === "True",
  );
  const [usage, setUsage] = useState<TenantUsage | null>(null);
  useEffect(() => {
    let live = true;
    api
      .tenantUsage(tenant.name)
      .then((u) => live && setUsage(u))
      .catch(() => live && setUsage(null)); // 501 (no state-layer) or 403 → hide the usage line
    return () => {
      live = false;
    };
  }, [tenant.name]);
  return (
    <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="tenant-detail">
      <div className="mb-3 flex items-center justify-between">
        <h3 className="text-sm font-medium">{tenant.name}</h3>
        <Button variant="ghost" size="sm" onClick={onClose} data-testid="tenant-detail-close">
          Close
        </Button>
      </div>
      <dl className="space-y-3 text-sm">
        <div>
          <dt className="text-xs text-muted-foreground">Member namespaces</dt>
          <dd className="mt-1 flex flex-wrap gap-1" data-testid="tenant-namespaces">
            {tenant.namespaces.length === 0 ? (
              <span className="text-muted-foreground">none</span>
            ) : (
              tenant.namespaces.map((ns) => (
                <Badge key={ns} variant="secondary" className="text-[10px]">
                  {ns}
                </Badge>
              ))
            )}
          </dd>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <dt className="text-xs text-muted-foreground">Compute quota</dt>
            <dd className="mt-1">{quotaSummary(tenant)}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Model quota</dt>
            <dd className="mt-1">{modelSummary(tenant)}</dd>
          </div>
        </div>
        {usage && (
          <div data-testid="tenant-usage">
            <dt className="text-xs text-muted-foreground">Live usage (this period)</dt>
            <dd className="mt-1">{usageSummary(tenant, usage)}</dd>
          </div>
        )}
      </dl>
      {conflict && (
        <div
          className="mt-3 rounded-md border border-destructive/40 bg-destructive/5 p-2.5"
          data-testid="tenant-conflict"
        >
          <p className="text-xs font-medium text-destructive">Namespace conflict</p>
          <p className="mt-1 text-xs text-destructive/90">{conflict.message}</p>
          <p className="mt-1.5 text-xs text-muted-foreground">
            A namespace belongs to at most one tenant. An operator must remove the contested
            namespace from this tenant&rsquo;s <code>spec.namespaces</code> (or from the tenant that
            already owns it) — then this tenant re-reconciles clean.
          </p>
        </div>
      )}
    </div>
  );
}

export function TenantsPage() {
  const [query, setQuery] = useState("");
  const [state, setState] = useState<Load>({ kind: "loading" });
  const [detail, setDetail] = useState<Detail>({ kind: "none" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    api
      .listTenants(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load tenants",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const openDetail = useCallback((name: string) => {
    setDetail({ kind: "loading", name });
    api
      .tenantDetail(name)
      .then((d) => setDetail({ kind: "ready", detail: d }))
      .catch((err: unknown) =>
        setDetail({
          kind: "error",
          name,
          message: err instanceof Error ? err.message : "couldn't load tenant detail",
        }),
      );
  }, []);

  const items = state.kind === "ready" ? state.items : [];
  const filtered = query
    ? items.filter((t) => {
        const q = query.toLowerCase();
        // Match the tenant name OR any of its namespaces — "which tenant owns X?" (M47 review).
        return (
          t.name.toLowerCase().includes(q) ||
          t.namespaces.some((ns) => ns.toLowerCase().includes(q))
        );
      })
    : items;

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<TenantSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (t) => <span className="font-medium">{t.name}</span>,
    },
    {
      id: "namespaces",
      header: "Namespaces",
      cell: (t) => <span className="text-muted-foreground">{t.memberNamespaces}</span>,
    },
    {
      id: "status",
      header: "Status",
      className: "w-32",
      cell: (t) => (
        <Badge variant={t.ready ? "success" : "warning"}>{t.ready ? "Ready" : "Pending"}</Badge>
      ),
    },
  ];

  return (
    <div className="mx-auto max-w-5xl space-y-6" data-testid="tenants-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Tenants</h2>
        <p className="text-sm text-muted-foreground">
          Cluster-scoped groupings of namespaces with compute + model quotas. Read-only here; operators
          manage tenants (the RBAC split is enforced by the API server).
        </p>
      </div>

      <DataTable<TenantSummary>
        columns={columns}
        rows={filtered}
        rowKey={(t) => t.name}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter by name or namespace…"
        ariaLabel="Tenants"
        onRowClick={(t) => openDetail(t.name)}
        empty={{
          icon: Building2,
          title: "No tenants yet",
          description:
            "A Tenant groups namespaces and caps their compute + model usage. An operator creates one to enforce multi-tenant quotas.",
        }}
      />

      {detail.kind === "loading" && (
        <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="tenant-detail">
          <p className="text-sm text-muted-foreground" data-testid="tenant-detail-loading">
            Loading {detail.name}…
          </p>
        </div>
      )}
      {detail.kind === "error" && (
        <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="tenant-detail">
          <div className="mb-2 flex items-center justify-between">
            <h3 className="text-sm font-medium">{detail.name}</h3>
            <Button variant="ghost" size="sm" onClick={() => setDetail({ kind: "none" })}>
              Close
            </Button>
          </div>
          <p className="text-sm text-destructive" data-testid="tenant-detail-error">
            {detail.message}
          </p>
        </div>
      )}
      {detail.kind === "ready" && (
        <TenantDetailPanel tenant={detail.detail} onClose={() => setDetail({ kind: "none" })} />
      )}
    </div>
  );
}
