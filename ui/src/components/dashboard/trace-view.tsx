import { useEffect, useState } from "react";
import { ExternalLink } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";
import { buttonVariants } from "@/components/ui/button";
import { api } from "@/lib/api";

// TraceView is the Langfuse deep-view for a selected run: it resolves the trace
// URL via the BFF (GET /api/traces/{id} — the SPA never hardcodes a Langfuse
// URL, so swapping the backend is a server-side config change, ADR 0005), then
// renders BOTH the embedded iframe deep-view AND a link-out to full Langfuse.
//
// The embedded iframe is the ONE accepted off-theme panel (spec §4): it shows
// Langfuse's own UI/theme. Everything around it (the frame, the link-out) uses
// the design tokens.

type State =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ready"; url: string };

export function TraceView({ traceId }: { traceId: string }) {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .traceLink(traceId, controller.signal)
      .then((res) => setState({ kind: "ready", url: res.url }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
    return () => controller.abort();
  }, [traceId]);

  return (
    <Card className="overflow-hidden">
      <div className="flex items-center justify-between border-b px-4 py-3">
        <div className="min-w-0">
          <p className="text-sm font-medium text-card-foreground">
            Trace deep-view
          </p>
          <p className="truncate font-mono text-xs text-muted-foreground">
            {traceId}
          </p>
        </div>
        {state.kind === "ready" && (
          <a
            href={state.url}
            target="_blank"
            rel="noreferrer"
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            <ExternalLink className="h-4 w-4" />
            Open in Langfuse
          </a>
        )}
      </div>

      <CardContent className="p-0">
        {state.kind === "loading" && (
          <div className="flex h-96 items-center justify-center text-sm text-muted-foreground">
            Resolving trace…
          </div>
        )}
        {state.kind === "error" && (
          <div className="flex h-96 items-center justify-center text-sm text-destructive">
            Failed to load trace: {state.message}
          </div>
        )}
        {state.kind === "ready" && (
          // The iframe is the accepted off-theme surface — it renders Langfuse's
          // own UI. The src comes from the BFF-resolved TraceURL, never a
          // hardcoded Langfuse origin.
          <iframe
            title="Langfuse trace deep-view"
            src={state.url}
            className="h-[36rem] w-full border-0 bg-background"
          />
        )}
      </CardContent>
    </Card>
  );
}
