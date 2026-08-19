import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { CheckCircle2, Circle, Coins, Network, RefreshCw } from "lucide-react";

import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { TopologySummary } from "@/components/dashboard/topology-summary";
import { DashboardStats } from "@/components/dashboard/dashboard-stats";
import { RecentRuns } from "@/components/dashboard/recent-runs";
import { useNamespace } from "@/lib/namespace";
import { FIRST_RUN_CHECKLIST } from "@/lib/nav";
import {
  api,
  ApiError,
  type ProviderListResponse,
  type RunListResponse,
  type TopologyResponse,
} from "@/lib/api";

// DashboardPage — the operator's landing surface (m12.5). It composes the
// native, on-theme views over the Go BFF (creds server-side):
//   1. Live topology  — a React Flow graph from /api/topology
//   2. Recent runs    — from /api/runs (Langfuse), each links to /traces/:id;
//                       "View all runs" links to /runs
// Cost is now TENANT-SCOPED (ADR 0077): /api/cost requires a ?tenant=, and the
// dashboard has no tenant context — so the landing page no longer fetches cost
// (a tenant-less call is a guaranteed 400). It shows a calm "cost is per-tenant"
// pointer to the /cost page, where a tenant is selected. This also retired the
// dashboard's "cost by model" panel — the durable per-tenant rollup carries no
// per-model detail (ADR 0077 consequence; restoration carded on the backlog).
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
  // The header namespace scope now filters the dashboard topology, not just the
  // agents list (m24.3) — "" = all namespaces the caller can see.
  const { namespace } = useNamespace();
  const [topology, setTopology] = useState<Loadable<TopologyResponse>>({
    kind: "loading",
  });
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
      .topology({ namespace: namespace || undefined }, signal)
      .then((data) => setTopology({ kind: "ready", data }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setTopology({ kind: "error", message: messageOf(err) });
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
    // Refetch when the header namespace scope changes (m24.3).
  }, [namespace]);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const loading =
    topology.kind === "loading" || runs.kind === "loading";

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Dashboard</h2>
          <p className="text-sm text-muted-foreground">
            Live topology and traced runs across your agents. Cost lives on the
            Cost page.
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

      {/* Headline metrics (m35 redesign): run volume, latency, fleet — sourced from the
          runs list + live topology. Spend/tokens are now TENANT-SCOPED (ADR 0077) and no
          longer live here (the dashboard has no tenant context); the stat cards for cost
          show a "per-tenant — see Cost page" pointer instead. Each stat degrades to a
          placeholder until its own feed loads. */}
      <DashboardStats
        runs={runs.kind === "ready" ? runs.data.runs : undefined}
        topology={topology.kind === "ready" ? topology.data : undefined}
      />

      {/* First-run checklist (m18.10) — the guided aha path. Rendered when the two
          signals that DRIVE onboarding (providers + topology) are loaded AND the setup
          is incomplete (a fully set-up or errored cluster shows nothing — no false
          invitation). The runs signal is NOT required (DX-4): on a minimal install
          observability is unwired (api.runs → 501 → "unavailable"), and gating on
          runs.kind === "ready" made the whole checklist vanish on exactly the empty
          cluster a new user needs it. Treat a non-ready runs signal as "no run yet" —
          the run step just stays unchecked. */}
      {providers.kind === "ready" &&
        topology.kind === "ready" &&
        (() => {
          const hasProvider = providers.data.providers.length > 0;
          const hasAgent = topology.data.nodes.some((n) => n.kind === "agent");
          // Only a "ready" runs feed can confirm a run happened; unavailable/loading/error
          // ⇒ unknown ⇒ treat as not-yet-run (show the checklist, run step unchecked).
          const hasRun = runs.kind === "ready" && runs.data.runs.length > 0;
          // Fully set up ⇒ nothing to show. A cluster with a provider + agent whose runs feed is
          // "unavailable" (observability off) is ALSO treated as set up (DX-4): we can't verify a
          // run, so don't nag it forever — only a "ready" feed showing zero runs keeps nudging.
          if (hasProvider && hasAgent && (hasRun || runs.kind === "unavailable")) return null;
          // The steps + routes are the shared FIRST_RUN_CHECKLIST (nav.ts, m54.4) so
          // they can't drift from the IA; only the live `done` signal is computed here.
          const done: Record<string, boolean> = {
            provider: hasProvider,
            agent: hasAgent,
            run: hasRun,
          };
          const steps = FIRST_RUN_CHECKLIST.map((s) => ({
            label: s.label,
            to: s.to,
            done: done[s.doneKey],
          }));
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

      {/* Cost pointer + live topology, side by side. Cost is now TENANT-SCOPED (ADR 0077)
          and has no home on the tenant-less dashboard — this card points to the /cost page
          where a tenant is selected (it replaced the old "cost by model" panel, whose
          per-model data the durable rollup no longer carries). Live topology is a
          scale-first SUMMARY (m22.6/U5) — the full interactive graph is the /topology page. */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="h-full" data-testid="cost-pointer-card">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <Coins className="h-4 w-4 text-primary" />
              Cost &amp; usage
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex min-h-[14rem] flex-col items-center justify-center gap-3 rounded-md border p-4 text-center">
              <p className="text-sm text-muted-foreground">
                Per-agent spend, tenant breakdowns, and month-end forecasts live
                on the Cost page.
              </p>
              <Link
                to="/cost"
                data-testid="view-cost-details"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                View cost details
              </Link>
            </div>
          </CardContent>
        </Card>

        <Card className="h-full">
          <CardHeader>
            <div className="flex items-center justify-between">
              <CardTitle className="flex items-center gap-2 text-base">
                <Network className="h-4 w-4 text-primary" />
                Live topology
              </CardTitle>
              <Link
                to="/topology"
                data-testid="view-full-topology"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                Open full topology
              </Link>
            </div>
          </CardHeader>
          <CardContent>
            <div className="min-h-[14rem] overflow-hidden rounded-md border p-4">
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
                <TopologySummary topology={topology.data} />
              )}
            </div>
          </CardContent>
        </Card>
      </div>

      {/* 3. Recent runs — each row navigates to /traces/:id (native trace
          explorer, m16.7); "View all runs" links to /runs (m16.8).
          The embedded Langfuse iframe (formerly section 4) was demoted in
          m16.11 — use the forensics link-out on /traces/:id instead. */}
      <div className="space-y-2">
        <div>
          <h3 className="text-base font-semibold tracking-tight">Recent runs</h3>
          <p className="text-xs text-muted-foreground">
            Latest traced invocations — click a row to open its trace.
          </p>
        </div>
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
