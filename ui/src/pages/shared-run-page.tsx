import * as React from "react";
import { useParams } from "react-router-dom";
import { api, ApiError, type SharedRunView } from "@/lib/api";

// Page-level ErrorBoundary — the shared-run page is the only unauthenticated
// route, so a render crash here would be seen by anonymous outsiders as a
// blank white screen (there is no global ErrorBoundary in this app). This
// boundary is LOCAL to SharedRunPage; do not widen it to App.tsx.
interface EBState {
  crashed: boolean;
}
class SharedRunErrorBoundary extends React.Component<
  React.PropsWithChildren<object>,
  EBState
> {
  constructor(props: React.PropsWithChildren<object>) {
    super(props);
    this.state = { crashed: false };
  }

  static getDerivedStateFromError(): EBState {
    return { crashed: true };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // Log to console for operator visibility; never rethrow (keeps the boundary intact).
    console.error("[SharedRunPage] render error", error, info.componentStack);
  }

  override render() {
    if (this.state.crashed) {
      return (
        <div className="min-h-screen bg-background">
          <main className="mx-auto max-w-3xl px-4 py-10">
            <div className="rounded-lg border bg-card p-8 text-center shadow-card">
              <p className="text-base font-medium text-foreground">
                This shared run couldn&apos;t be displayed
              </p>
              <p className="mt-2 text-sm text-muted-foreground">
                An unexpected error occurred while rendering this page. The link
                may reference content that cannot be shown.
              </p>
            </div>
          </main>
        </div>
      );
    }
    return this.props.children;
  }
}

// SharedRunPage — the public, unauthenticated shared-run view (m75.4).
// Route: /shared/runs/:token (mounted OUTSIDE RequireAuth + AppShell in App.tsx)
// No console chrome — a standalone read-only page.
// Fetches getSharedRun(token) which does a plain fetch (NO Authorization header).
// 404 (uniform) = bad/expired/revoked → friendly "unavailable" message.
// Do NOT distinguish between expired, revoked, or bad token — the backend
// returns 404 uniformly; the UI must honour that uniform behavior.
//
// data-testid contract:
//   shared-run-page          — root container
//   shared-run-unavailable   — the 404/expired/revoked friendly state
//   shared-run-content       — the run view when data is present
//   shared-run-metadata      — the metadata block (always present in content)
//   shared-run-transcript    — the transcript block (only when includeContent=true)

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; view: SharedRunView }
  | { kind: "unavailable" }; // 404 uniform — bad/expired/revoked

function fmtDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function StatusBadge({ status }: { status: string }) {
  const color =
    status === "succeeded" || status === "completed"
      ? "text-success border-success/30 bg-success/10"
      : status === "failed" || status === "error"
        ? "text-destructive border-destructive/30 bg-destructive/10"
        : status === "running"
          ? "text-info border-info/30 bg-info/10"
          : "text-muted-foreground border-border bg-muted/20";
  return (
    <span
      className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium capitalize ${color}`}
    >
      {status}
    </span>
  );
}

function SharedRunPageInner() {
  const { token = "" } = useParams();
  const [state, setPageState] = React.useState<PageState>({ kind: "loading" });

  React.useEffect(() => {
    if (!token) {
      setPageState({ kind: "unavailable" });
      return;
    }
    const controller = new AbortController();
    setPageState({ kind: "loading" });

    api
      .getSharedRun(token, controller.signal)
      .then((view) => {
        if (controller.signal.aborted) return;
        setPageState({ kind: "ready", view });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // Any error (404, expired, revoked, network) → uniform unavailable.
        // The backend returns 404 uniformly for bad/expired/revoked tokens;
        // we must not distinguish them (per spec).
        void (err instanceof ApiError); // type-check only
        setPageState({ kind: "unavailable" });
      });

    return () => controller.abort();
  }, [token]);

  return (
    <div data-testid="shared-run-page" className="min-h-screen bg-background">
      <main className="mx-auto max-w-3xl px-4 py-10">
        {state.kind === "loading" && (
          <div className="animate-pulse space-y-4">
            <div className="h-8 w-48 rounded bg-muted" />
            <div className="h-4 w-96 rounded bg-muted" />
            <div className="h-32 rounded bg-muted" />
          </div>
        )}

        {state.kind === "unavailable" && (
          <div
            data-testid="shared-run-unavailable"
            className="rounded-lg border bg-card p-8 text-center shadow-card"
          >
            <p className="text-base font-medium text-foreground">
              Shared run unavailable
            </p>
            <p className="mt-2 text-sm text-muted-foreground">
              This shared run is unavailable. It may have expired or been
              revoked by the owner.
            </p>
          </div>
        )}

        {state.kind === "ready" && (
          <div data-testid="shared-run-content" className="space-y-6">
            {/* Header */}
            <div className="space-y-1">
              <h1 className="text-2xl font-semibold tracking-tight">
                Shared run
              </h1>
              <p className="text-sm text-muted-foreground">
                {state.view.agent} · {state.view.namespace}
              </p>
            </div>

            {/* Metadata block — always present */}
            <div
              data-testid="shared-run-metadata"
              className="rounded-lg border bg-card p-5 shadow-card"
            >
              <h2 className="text-sm font-medium text-muted-foreground">
                Run details
              </h2>
              <dl className="mt-3 grid grid-cols-2 gap-x-6 gap-y-3 text-sm sm:grid-cols-3">
                <div>
                  <dt className="text-xs text-muted-foreground">Status</dt>
                  <dd className="mt-0.5">
                    <StatusBadge status={state.view.status} />
                  </dd>
                </div>
                <div>
                  <dt className="text-xs text-muted-foreground">Agent</dt>
                  <dd className="mt-0.5 font-mono text-xs">
                    {state.view.agent}
                  </dd>
                </div>
                <div>
                  <dt className="text-xs text-muted-foreground">Namespace</dt>
                  <dd className="mt-0.5 font-mono text-xs">
                    {state.view.namespace}
                  </dd>
                </div>
                <div>
                  <dt className="text-xs text-muted-foreground">Created</dt>
                  <dd className="mt-0.5">{fmtDate(state.view.createdAt)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-muted-foreground">Updated</dt>
                  <dd className="mt-0.5">{fmtDate(state.view.updatedAt)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-muted-foreground">Messages</dt>
                  <dd className="mt-0.5">
                    {state.view.messageCount}
                    {state.view.messageRoles.length > 0 && (
                      <span className="ml-1 text-muted-foreground">
                        ({state.view.messageRoles.join(", ")})
                      </span>
                    )}
                  </dd>
                </div>
                {state.view.errorCategory && (
                  <div className="col-span-full">
                    <dt className="text-xs text-muted-foreground">
                      Error category
                    </dt>
                    <dd className="mt-0.5 text-destructive">
                      {state.view.errorCategory}
                    </dd>
                  </div>
                )}
              </dl>
            </div>

            {/* Transcript block — present only when includeContent=true
                (fields are present on the SharedRunView) */}
            {(state.view.input !== undefined ||
              (state.view.messages && state.view.messages.length > 0)) && (
              <div
                data-testid="shared-run-transcript"
                className="space-y-3"
              >
                <h2 className="text-sm font-medium">Transcript</h2>

                {state.view.input !== undefined && (
                  <div className="rounded-lg border bg-card p-4 shadow-card">
                    <p className="mb-2 text-xs font-medium text-muted-foreground">
                      Input
                    </p>
                    <pre className="whitespace-pre-wrap text-sm font-mono">
                      {typeof state.view.input === "string"
                        ? state.view.input
                        : JSON.stringify(state.view.input, null, 2)}
                    </pre>
                  </div>
                )}

                {state.view.messages?.map((msg, idx) => (
                  <div
                    key={idx}
                    className={`rounded-lg border p-4 shadow-card ${
                      msg.role === "user" ? "bg-muted/30" : "bg-card"
                    }`}
                  >
                    <p className="mb-1 text-xs font-medium capitalize text-muted-foreground">
                      {msg.role}
                    </p>
                    <p className="whitespace-pre-wrap text-sm">{msg.content}</p>
                  </div>
                ))}

                {state.view.error && (
                  <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-4">
                    <p className="mb-1 text-xs font-medium text-destructive">
                      Error
                    </p>
                    <p className="whitespace-pre-wrap text-sm text-destructive">
                      {state.view.error}
                    </p>
                  </div>
                )}
              </div>
            )}
          </div>
        )}

        {/* Footer — minimal, no console nav or secret links */}
        <footer className="mt-12 text-center text-xs text-muted-foreground">
          Shared via Agent Engine
        </footer>
      </main>
    </div>
  );
}

// SharedRunPage is the public entry point. It wraps the inner page with a
// page-local ErrorBoundary so a render crash (e.g. unexpected content shape)
// is caught and shown as a friendly fallback to anonymous visitors, rather
// than a white-screen.
export function SharedRunPage() {
  return (
    <SharedRunErrorBoundary>
      <SharedRunPageInner />
    </SharedRunErrorBoundary>
  );
}

// Exported ONLY for unit-testing the boundary's fallback render. Do not use
// in production code outside this file.
export { SharedRunErrorBoundary as SharedRunErrorBoundaryForTest };
