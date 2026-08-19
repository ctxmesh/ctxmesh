import { useCallback, useEffect, useRef, useState } from "react";
import { Shield } from "lucide-react";

import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { api, ApiError, type GuardrailPolicySummary } from "@/lib/api";

// GuardrailPoliciesPage — the GuardrailPolicy content-governance policies (m66.10, ADR 0059).
//
// Read-only (caller-scoped, ADR 0011): each row is a policy — its PII, deny-list, judge, fail-mode,
// rate-limit summary, validated status, and the blast-radius referencing agents. A policy is authored
// via YAML/kubectl; the console surfaces it for visibility and operator awareness.
// A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// data-testid contract:
//   guardrail-policies-page  — root container
//   guardrail-policies-table — the DataTable (aria-label="Guardrail policies")

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; policies: GuardrailPolicySummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function GuardrailPoliciesPage() {
  const [query, setQuery] = useState("");
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listGuardrailPolicies(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", policies: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const all = loadState.kind === "ready" ? loadState.policies : [];
  const q = query.trim().toLowerCase();
  const policies = q ? all.filter((p) => p.name.toLowerCase().includes(q)) : all;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<GuardrailPolicySummary>[] = [
    {
      id: "name",
      header: "Policy",
      cell: (p) => <span className="font-medium">{p.name}</span>,
    },
    {
      id: "detectors",
      header: "Detectors",
      hideOnMobile: true,
      cell: (p) => {
        const parts: string[] = [];
        if (p.piiEnabled) parts.push("PII");
        if (p.denylistCount > 0) parts.push(`${p.denylistCount} deny rules`);
        if (p.judgeEnabled) parts.push("judge");
        if (p.userRateLimited) parts.push("rate-limited");
        return (
          <span className="text-sm text-muted-foreground">
            {parts.length > 0 ? parts.join(", ") : "—"}
          </span>
        );
      },
    },
    {
      id: "failMode",
      header: "Fail mode",
      hideOnMobile: true,
      cell: (p) => (
        <Badge variant={p.failMode === "closed" ? "success" : "warning"} className="text-xs">
          {p.failMode}
        </Badge>
      ),
    },
    {
      id: "agents",
      header: "Agents",
      hideOnMobile: true,
      cell: (p) => (
        <span className="text-sm text-muted-foreground">
          {p.referencingAgents.length === 0
            ? "—"
            : p.referencingAgents.length === 1
              ? p.referencingAgents[0]
              : `${p.referencingAgents.length} agents`}
        </span>
      ),
    },
    {
      id: "status",
      header: "Status",
      cell: (p) =>
        <StatusBadge ready={p.validated} phase={p.validated ? undefined : p.reason} />,
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="guardrail-policies-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Guardrail Policies</h2>
        <p className="text-sm text-muted-foreground">
          Namespace-scoped content-governance policies: PII scanning, pattern deny-lists, optional
          LLM-judge, and per-user rate limits. Applies at inference time.
        </p>
      </div>

      <DataTable<GuardrailPolicySummary>
        columns={columns}
        rows={policies}
        rowKey={(p) => `${p.namespace}/${p.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter policies by name…"
        ariaLabel="Guardrail policies"
        empty={{
          icon: Shield,
          title: "No guardrail policies",
          description:
            "No guardrail policies yet. A guardrail policy applies content governance — PII scanning, deny-lists, an optional LLM-judge, and per-user rate limits — to your agents.",
        }}
      />
    </div>
  );
}
