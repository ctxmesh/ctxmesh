import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { CheckCircle2, Circle, Coins, Network, RefreshCw } from "lucide-react";

import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { TopologyGraph } from "@/components/dashboard/topology-graph";
import { CostPanel } from "@/components/dashboard/cost-panel";
import { RecentRuns } from "@/components/dashboard/recent-runs";
import {
  api,
  ApiError,
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
  // "unavailable" = the backing observability adapter (Langfuse) isn't configured
  // (a 501, not an error). Cost/runs render a calm "not configured" state, not a
  // red "Failed to load" — an unwired optional integration is not a failure.
  | { kind: "unavailable" }
  | { kind: "ready"; data: T };

// isNotConfigured is true for a 501 (the adapter is deliberately not wired) — the
// signal to degrade calmly rather than surface a destructive error.
function isNotConfigured(err: unknown): boolean {
  return err instanceof ApiError && err.status === 501;
}

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
        setCost(
          isNotConfigured(err)
            ? { kind: "unavailable" }
            : { kind: "error", message: messageOf(err) },
        );
      });
    api
      .runs(signal)
      .then((data) => setRuns({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setRuns(
          isNotConfigured(err)
            ? { kind: "unavailable" }
            : { kind: "error", message: messageOf(err) },
        );
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

      {/* First-run checklist (m18.10) — the guided aha path. Rendered only when all
          three signals are loaded (accurate checkmarks) AND the setup is incomplete
          (a fully set-up or errored cluster shows nothing — no false invitation). */}
      {providers.kind === "ready" &&
        topology.kind === "ready" &&
        runs.kind === "ready" &&
        (() => {
          const hasProvider = providers.data.providers.length > 0;
          const hasAgent = topology.data.nodes.some((n) => n.kind === "agent");
          const hasRun = runs.data.runs.length > 0;
          if (hasProvider && hasAgent && hasRun) return null; // fully set up
          const steps = [
            {
              label: "Connect a provider",
              done: hasProvider,
              to: "/providers/connect",
            },
            { label: "Create an agent", done: hasAgent, to: "/agents/new" },
            { label: "Run your agent", done: hasRun, to: "/agents" },
          ];
          const next = steps.find((s) => !s.done);
          return (
            <div
              className="rounded-lg border bg-card p-5 shadow-card"
              data-testid="first-run-checklist"
            >
              <p className="text-sm font-medium">Get started</p>
              <p className="mt-1 text-xs text-muted-foreground">
                Three steps to your first running agent — paste a key once,
                describe an agent, run it. No YAML, no kubectl.
              </p>
              <ol className="mt-3 space-y-2">
                {steps.map((s, i) => (
                  <li
                    key={s.label}
                    className="flex items-center gap-2 text-sm"
                    data-testid={`first-run-step-${i}`}
                  >
                    {s.done ? (
                      <CheckCircle2 className="h-4 w-4 shrink-0 text-success" />
                    ) : (
                      <Circle className="h-4 w-4 shrink-0 text-muted-foreground" />
                    )}
                    <span
                      className={
                        s.done ? "text-muted-foreground line-through" : ""
                      }
                    >
                      {s.label}
                    </span>
                  </li>
                ))}
              </ol>
              {next && (
                <Button
                  size="sm"
                  className="mt-4"
                  onClick={() => navigate(next.to)}
                  data-testid="first-run-cta"
                >
                  {next.label}
                </Button>
              )}
            </div>
          );
        })()}

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
      {cost.kind === "unavailable" && (
        <Card>
          <CardContent
            className="py-10 text-center text-sm text-muted-foreground"
            data-testid="cost-unavailable"
          >
            Cost &amp; usage isn&apos;t configured — connect an observability
            backend (Langfuse) to see spend here. Everything else works without
            it.
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
        {runs.kind === "unavailable" && (
          <Card>
            <CardContent
              className="py-8 text-center text-sm text-muted-foreground"
              data-testid="runs-unavailable"
            >
              Run history isn&apos;t configured — connect an observability
              backend (Langfuse) to see recent runs here.
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
        {runs.kind === "ready" && <RecentRuns runs={runs.data.runs} />}
      </div>
    </div>
  );
}
