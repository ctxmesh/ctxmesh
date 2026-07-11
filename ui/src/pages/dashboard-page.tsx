import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Coins, Network, PlugZap, RefreshCw } from "lucide-react";

import { Button, buttonVariants } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { EmptyState } from "@/components/kit";
import { TopologyGraph } from "@/components/dashboard/topology-graph";
import { CostPanel } from "@/components/dashboard/cost-panel";
import { RecentRuns } from "@/components/dashboard/recent-runs";
import {
  api,
  type CostResponse,
  type ProviderListResponse,
  type RunListResponse,
  type TopologyResponse,
} from "@/lib/api";

// DashboardPage — the operator's landing surface (m12.5). It composes three
// native, on-theme views over the Go BFF (creds server-side):
//   1. Live topology  — a React Flow graph from /api/topology
//   2. Cost / usage   — native cards + charts from /api/cost; links to /cost
//   3. Recent runs    — from /api/runs (Langfuse), each links to /traces/:id;
//                       "View all runs" links to /runs
// The Langfuse embedded iframe (formerly section 4) was demoted in m16.11.
// The ONE sanctioned Langfuse door is the forensics link-out on /traces/:id.

type Loadable<T> =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ready"; data: T };

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : "request failed";
}

export function DashboardPage() {
  const navigate = useNavigate();
  const [topology, setTopology] = useState<Loadable<TopologyResponse>>({
    kind: "loading",
  });
  const [cost, setCost] = useState<Loadable<CostResponse>>({ kind: "loading" });
  const [runs, setRuns] = useState<Loadable<RunListResponse>>({
    kind: "loading",
  });
  // Providers drive the FIRST-RUN teaching CTA: an empty list ⇒ "Connect a
  // provider to run your first agent" (the aha entry point, spec §5). A load
  // failure (incl. the kill-switch 404) is NOT treated as "empty" — we simply
  // don't show the CTA (an honest "no false invitation").
  const [providers, setProviders] = useState<Loadable<ProviderListResponse>>({
    kind: "loading",
  });
  const load = useCallback((signal?: AbortSignal) => {
    setTopology({ kind: "loading" });
    setCost({ kind: "loading" });
    setRuns({ kind: "loading" });
    setProviders({ kind: "loading" });

    api
      .listProviders(signal)
      .then((data) => setProviders({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setProviders({ kind: "error", message: messageOf(err) });
      });
    api
      .topology(undefined, signal)
      .then((data) => setTopology({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setTopology({ kind: "error", message: messageOf(err) });
      });
    api
      .cost(signal)
      .then((data) => setCost({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setCost({ kind: "error", message: messageOf(err) });
      });
    api
      .runs(signal)
      .then((data) => setRuns({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setRuns({ kind: "error", message: messageOf(err) });
      });
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const loading =
    topology.kind === "loading" ||
    cost.kind === "loading" ||
    runs.kind === "loading";

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Dashboard</h2>
          <p className="text-sm text-muted-foreground">
            Live topology, cost/usage, and traced runs over the BFF (creds
            server-side).
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => load()}
          disabled={loading}
        >
          <RefreshCw className="h-4 w-4" />
          Refresh
        </Button>
      </div>

      {/* First-run teaching CTA — the aha entry point. When no providers are
          connected the dashboard leads with "Connect a provider to run your
          first agent" → the connect-provider wizard (spec §5). Shown ONLY on a
          confirmed-empty list (a load error is not a false invitation). */}
      {providers.kind === "ready" && providers.data.providers.length === 0 && (
        <EmptyState
          icon={PlugZap}
          title="Connect a provider to run your first agent"
          description="No model provider is connected yet. Paste a key once — it's validated and stored server-side — then describe an agent and run it. No YAML, no kubectl."
          action={{
            label: "Connect a provider",
            icon: PlugZap,
            onClick: () => navigate("/providers/connect"),
          }}
        />
      )}

      {/* 1. Live topology */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Network className="h-4 w-4 text-primary" />
            Live topology
          </CardTitle>
        </CardHeader>
        <CardContent>
          <div className="h-[26rem] overflow-hidden rounded-md border">
            {topology.kind === "loading" && (
              <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
                Loading topology…
              </div>
            )}
            {topology.kind === "error" && (
              <div className="flex h-full items-center justify-center text-sm text-destructive">
                Failed to load topology: {topology.message}
              </div>
            )}
            {topology.kind === "ready" && (
              <TopologyGraph topology={topology.data} />
            )}
          </div>
        </CardContent>
      </Card>

      {/* 2. Cost / usage — headline stats + charts; "View cost details" links
          to the native /cost page (m16.10) for the full cost explorer. */}
      {cost.kind === "loading" && (
        <Card>
          <CardContent className="py-10 text-center text-sm text-muted-foreground">
            Loading cost &amp; usage…
          </CardContent>
        </Card>
      )}
      {cost.kind === "error" && (
        <Card>
          <CardContent className="py-10 text-center text-sm text-destructive">
            Failed to load cost: {cost.message}
          </CardContent>
        </Card>
      )}
      {cost.kind === "ready" && (
        <div className="space-y-3">
          <CostPanel cost={cost.data} />
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Coins className="h-3.5 w-3.5" />
              Full cost breakdown and historical trends on the Cost page.
            </div>
            <Link
              to="/cost"
              data-testid="view-cost-details"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              View cost details
            </Link>
          </div>
        </div>
      )}

      {/* 3. Recent runs — each row navigates to /traces/:id (native trace
          explorer, m16.7); "View all runs" links to /runs (m16.8).
          The embedded Langfuse iframe (formerly section 4) was demoted in
          m16.11 — use the forensics link-out on /traces/:id instead. */}
      <div className="space-y-2">
        <h3 className="text-sm font-medium">Recent runs</h3>
        {runs.kind === "loading" && (
          <Card>
            <CardContent className="py-8 text-center text-sm text-muted-foreground">
              Loading runs…
            </CardContent>
          </Card>
        )}
        {runs.kind === "error" && (
          <Card>
            <CardContent className="py-8 text-center text-sm text-destructive">
              Failed to load runs: {runs.message}
            </CardContent>
          </Card>
        )}
        {runs.kind === "ready" && (
          <RecentRuns runs={runs.data.runs} />
        )}
      </div>
    </div>
  );
}
