import { useEffect, useState } from "react";
import { Activity } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { api, type HealthResponse } from "@/lib/api";

// DashboardPage — the foundation landing surface. The live topology / cost /
// recent-runs views are m12.5; here it renders the BFF health/version (the
// second proof endpoint) on-theme so the foundation is visibly wired.
export function DashboardPage() {
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    api
      .health(controller.signal)
      .then(setHealth)
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setError(err instanceof Error ? err.message : "request failed");
      });
    return () => controller.abort();
  }, []);

  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Dashboard</h2>
        <p className="text-sm text-muted-foreground">
          Live topology, cost, and recent runs land in m12.5. This is the
          foundation.
        </p>
      </div>

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="flex items-center gap-2 text-base">
              <Activity className="h-4 w-4 text-primary" />
              BFF connection
            </CardTitle>
            <CardDescription>
              Served static SPA calling the Go BFF (creds server-side).
            </CardDescription>
          </div>
          {health && <Badge variant="success">{health.status}</Badge>}
          {error && <Badge variant="destructive">unreachable</Badge>}
        </CardHeader>
        <CardContent>
          {health ? (
            <p className="font-mono text-xs text-muted-foreground">
              version {health.version}
            </p>
          ) : error ? (
            <p className="text-sm text-destructive">{error}</p>
          ) : (
            <p className="text-sm text-muted-foreground">Checking…</p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
