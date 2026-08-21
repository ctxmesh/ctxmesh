import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Share2 } from "lucide-react";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api, ApiError, type MySharesItem } from "@/lib/api";

// MySharesPage — the caller's share links across all runs (V13).
//
// Backend:
//   GET  /api/my/shares                          — list caller's shares
//   DELETE /api/runs/{runId}/shares/{shareId}    — revoke a live share (existing endpoint)
//
// The page is caller-scoped: the BFF returns ONLY the shares created by the
// authenticated caller. There is no token/hash in the response (the backend
// never returns the token after creation) — the ID is an opaque DB id, not a
// secret, and is shown only for reference (never labelled as a secret).
//
// Status badge variants (matches PRD §shares):
//   live    → success  (green/active)
//   revoked → muted    (secondary)
//   expired → warning  (amber)
//
// data-testid contract:
//   my-shares-page       — root container
//   my-shares-table      — the DataTable (via aria-label="My Shares")
//   revoke-{id}          — the Revoke button for a live share

function fmtTimestamp(ts: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

// StatusBadge renders the share status with consistent color semantics.
function StatusBadge({ status }: { status: MySharesItem["status"] }) {
  if (status === "live")
    return <Badge variant="success" className="capitalize">Live</Badge>;
  if (status === "expired")
    return <Badge variant="warning" className="capitalize">Expired</Badge>;
  // revoked — muted
  return (
    <Badge variant="secondary" className="capitalize">
      Revoked
    </Badge>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: MySharesItem[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function MySharesPage() {
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  // Track which share IDs are in the process of being revoked (optimistic disable).
  const [revoking, setRevoking] = useState<Set<string>>(new Set());
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    api
      .listMyShares(controller.signal)
      .then((items) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", items });
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

  // handleRevoke calls the existing per-run revoke endpoint and refreshes the list.
  function handleRevoke(item: MySharesItem) {
    setRevoking((prev) => new Set(prev).add(item.id));
    api
      .revokeRunShare(item.runId, item.id)
      .then(() => {
        load();
      })
      .catch(() => {
        // Re-enable the button on failure so the user can retry.
        setRevoking((prev) => {
          const next = new Set(prev);
          next.delete(item.id);
          return next;
        });
      });
  }

  const items = loadState.kind === "ready" ? loadState.items : [];

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "shares",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<MySharesItem>[] = [
    {
      id: "run",
      header: "Run",
      cell: (s) => (
        // Link to the per-run detail page (/runs/:id, added in m112.4) — keyed by the
        // real run.ID, which is exactly what a share's runId is (shared_runs.run_id is
        // always run.ID, never the traceId, so this must NOT point at /traces/:id).
        <Link
          to={`/runs/${encodeURIComponent(s.runId)}`}
          onClick={(e) => e.stopPropagation()}
          className="font-mono text-xs text-primary hover:underline"
          data-testid={`run-link-${s.id}`}
        >
          {s.runId.length > 16
            ? `${s.runId.slice(0, 8)}…${s.runId.slice(-4)}`
            : s.runId}
        </Link>
      ),
    },
    {
      id: "namespace",
      header: "Namespace",
      hideOnMobile: true,
      cell: (s) => (
        <span className="text-sm text-muted-foreground">{s.namespace || "—"}</span>
      ),
    },
    {
      id: "status",
      header: "Status",
      cell: (s) => <StatusBadge status={s.status} />,
    },
    {
      id: "includeContent",
      header: "Content",
      hideOnMobile: true,
      cell: (s) => (
        <span className="text-sm text-muted-foreground">
          {s.includeContent ? "Yes" : "No"}
        </span>
      ),
    },
    {
      id: "createdAt",
      header: "Created",
      hideOnMobile: true,
      cell: (s) => (
        <span className="text-sm text-muted-foreground">{fmtTimestamp(s.createdAt)}</span>
      ),
    },
    {
      id: "expiresAt",
      header: "Expires",
      hideOnMobile: true,
      cell: (s) => (
        <span className="text-sm text-muted-foreground">{fmtTimestamp(s.expiresAt)}</span>
      ),
    },
    {
      id: "actions",
      header: "",
      cell: (s) =>
        s.status === "live" ? (
          <Button
            variant="outline"
            size="sm"
            disabled={revoking.has(s.id)}
            onClick={(e) => {
              e.stopPropagation();
              handleRevoke(s);
            }}
            data-testid={`revoke-${s.id}`}
          >
            {revoking.has(s.id) ? "Revoking…" : "Revoke"}
          </Button>
        ) : null,
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="my-shares-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">My Shares</h2>
        <p className="text-sm text-muted-foreground">
          Share links you have created across all runs. Revoke a live share to
          immediately invalidate it.
        </p>
      </div>

      <DataTable<MySharesItem>
        columns={columns}
        rows={items}
        rowKey={(s) => s.id}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="My Shares"
        empty={{
          icon: Share2,
          title: "You have no active shares",
          description:
            "Share links you create from run traces will appear here. You can revoke live ones from this page.",
        }}
      />
    </div>
  );
}
