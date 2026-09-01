import * as React from "react";
import { useParams } from "react-router-dom";
import { Link2Off } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import {
  ClosingNote,
  EmptyState,
  KeyValueList,
  PageHeader,
  QuietNote,
  Skeleton,
  Timeline,
  TimelineSkeleton,
  truncateId,
  type KeyValueItem,
  type TimelineStep,
} from "@/components/kit";
import { api, ApiError, type SharedRunView } from "@/lib/api";
import { formatDateTime, formatLatency } from "@/lib/format";

// SharedRunPage — the PUBLIC run reader (m75.4; M151 §6.1 archetype A5, public
// variant). Route: /shared/runs/:token, mounted OUTSIDE RequireAuth + AppShell.
//
// ── CHROME-LESS BY CONSTRUCTION, NOT BY OMISSION ────────────────────────────
// A5's public variant is "the same composition, with no shell": a slim paper
// masthead carrying the wordmark and a mono `shared run` eyebrow, then the run
// story, then the record. Zero actions. Every affordance the console has —
// nav, namespace picker, stop control, share, dataset, approve/deny — is absent
// because the reader has no session and no standing to use any of it. Nothing
// on this page links into the console: an anonymous visitor clicking such a
// link lands on a sign-in wall, which reads as a paywall around someone else's
// work rather than as the read-only courtesy this page is.
//
// ── IT SHOWS ONLY WHAT THE OWNER CHOSE TO SHARE ─────────────────────────────
// `SharedRunView` is the whole surface: identity, status, timestamps, message
// counts, and — only when the share was minted with `includeContent` — the
// input and the messages. A share without content is NOT a broken share and is
// not an empty run: it is a deliberate redaction, and the page says so in
// words. Inventing a transcript from `messageRoles` would be the same class of
// lie as printing $0.0000 for an unattributed cost, so it is not done: the
// roles are reported as the counted fact they are, and nothing more.
//
// ── ONE 404 MEANS FOUR THINGS, ON PURPOSE ───────────────────────────────────
// The backend answers 404 uniformly for a bad, expired, or revoked token. The
// UI must honour that: distinguishing them here would leak whether a token ever
// existed. So every failure — including a network error — renders the one
// "unavailable" state.
//
// data-testid contract:
//   shared-run-page          — root container
//   shared-run-unavailable   — the 404/expired/revoked friendly state
//   shared-run-content       — the run view when data is present
//   shared-run-metadata      — the record block (always present in content)
//   shared-run-transcript    — the story block (only when includeContent=true)

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
          <Masthead />
          <main className="mx-auto max-w-3xl px-4 py-10 md:px-6">
            <EmptyState
              intent="unavailable"
              title="This shared run couldn't be displayed"
              description="Something in this page failed to render. The link may reference content this view cannot show. Nothing about the run itself has changed."
            />
          </main>
        </div>
      );
    }
    return this.props.children;
  }
}

// ── The public masthead (§6.1 A5, public variant) ───────────────────────────

/**
 * The wordmark, and one mono word saying what this page is.
 *
 * Deliberately NOT a link. The console's own wordmark navigates home; here
 * "home" is a sign-in wall, and sending an anonymous reader there turns a
 * courtesy into a locked door.
 */
function Masthead() {
  return (
    <header className="border-b border-border bg-card">
      <div className="mx-auto flex max-w-5xl flex-wrap items-baseline gap-x-3 gap-y-1 px-4 py-3 md:px-6">
        <span className="font-serif text-xl tracking-snug text-foreground">
          ctx<span className="italic text-primary">mesh</span>
        </span>
        <span className="font-mono text-2xs uppercase tracking-wide text-faint">
          shared run
        </span>
      </div>
    </header>
  );
}

// ── The run-status vocabulary, for the statuses a share can carry ───────────

/**
 * One run status → its semantic hue (§2.2).
 *
 * Duplicated from `run-detail-page.tsx` rather than shared, and that is a
 * recorded compromise, not an oversight. It belongs in
 * `kit/status-badge.tsx` beside `resolveStatus` — which today routes K8s
 * readiness phrases, not run statuses, so `resolveStatus("running")` resolves
 * to `failed`. Until the kit gains a run-status resolver, the public page keeps
 * its own copy rather than importing the authenticated console page for a map.
 *
 * The two hues that moved in M151 are the two that matter here: `running` was
 * drawn in the info blue that is now the HOLD violet — "a person must decide"
 * painted on a run that needs nobody — and it is `progressing`; and a held run
 * is `hold`, never the bound-crossed amber and never crit.
 */
const STATUS_VARIANT: Record<string, "ok" | "progressing" | "hold" | "crit" | "muted"> = {
  succeeded: "ok",
  completed: "ok",
  done: "ok",
  failed: "crit",
  error: "crit",
  cancelled: "crit",
  requires_action: "hold",
  paused: "hold",
  running: "progressing",
  queued: "progressing",
  pending: "progressing",
  starting: "progressing",
};

const STATUS_WORD: Record<string, string> = {
  succeeded: "Succeeded",
  completed: "Succeeded",
  done: "Done",
  failed: "Failed",
  error: "Failed",
  running: "Running",
  queued: "Queued",
  pending: "Pending",
  starting: "Starting",
  requires_action: "Waiting on approval",
  paused: "Held",
  cancelled: "Cancelled",
};

function statusWord(status: string): string {
  if (!status) return "Unknown";
  return (
    STATUS_WORD[status] ??
    status.replace(/[_-]+/g, " ").replace(/^./, (c) => c.toUpperCase())
  );
}

const TERMINAL = new Set(["succeeded", "completed", "failed", "error", "cancelled"]);

/** The story, from the messages the owner chose to include. */
function buildStory(view: SharedRunView): TimelineStep[] {
  const msgs = view.messages ?? [];
  const steps: TimelineStep[] = [];
  const lastIdx = msgs.length - 1;
  let seenUser = false;

  msgs.forEach((m, i) => {
    const id = `msg-${i}`;
    if (m.role === "user") {
      const first = !seenUser;
      seenUser = true;
      steps.push({
        id,
        title: first ? "The request arrived" : "The person added to the request",
        detail: <span className="whitespace-pre-wrap">{m.content}</span>,
      });
      return;
    }
    if (m.role === "assistant") {
      const closes = i === lastIdx && view.status === "succeeded";
      steps.push({
        id,
        title: closes ? "The run finished and answered" : "The model replied",
        detail: <span className="whitespace-pre-wrap">{m.content}</span>,
        tone: closes ? "done" : "step",
      });
      return;
    }
    if (m.role === "tool") {
      steps.push({
        id,
        title: "A tool returned its result",
        // Machine words are evidence — inline mono, never the vocabulary.
        detail: <span className="break-words font-mono text-xs">{m.content}</span>,
      });
      return;
    }
    steps.push({
      id,
      title: `A ${m.role} message was recorded`,
      detail: <span className="whitespace-pre-wrap">{m.content}</span>,
    });
  });

  if (view.error) {
    steps.push({
      id: "end",
      title: "The run stopped and did not finish",
      detail: <span className="whitespace-pre-wrap">{view.error}</span>,
      tone: "failed",
    });
  }

  return steps;
}

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; view: SharedRunView }
  | { kind: "unavailable" }; // 404 uniform — bad/expired/revoked

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
      <Masthead />
      <main className="mx-auto min-w-0 max-w-5xl px-4 py-8 md:px-6">
        {state.kind === "loading" && (
          <div className="space-y-6">
            <div role="status" aria-busy="true" aria-label="Loading the shared run">
              <Skeleton decorative className="h-8 w-60" />
              <Skeleton decorative className="mt-3 h-4 w-96 max-w-full" />
            </div>
            <div className="rounded-lg border border-border bg-card p-5">
              <TimelineSkeleton />
            </div>
          </div>
        )}

        {state.kind === "unavailable" && (
          <div data-testid="shared-run-unavailable">
            <EmptyState
              intent="unavailable"
              icon={Link2Off}
              title="This shared run is unavailable"
              description="The link may have expired, or its owner may have revoked it. Whoever shared it can mint a new one; nothing about the run itself has changed."
            />
          </div>
        )}

        {state.kind === "ready" && (
          <SharedRunContent view={state.view} />
        )}
      </main>

      {/* Minimal by contract: no console nav, no secret links, nothing an
          anonymous reader has standing to press. */}
      <footer className="mx-auto max-w-5xl px-4 pb-10 pt-6 text-sm text-faint md:px-6">
        A read-only view of one run, shared from{" "}
        <span className="font-serif text-md text-secondary-foreground">
          ctx<span className="italic text-primary">mesh</span>
        </span>
        . It shows only what its owner chose to include.
      </footer>
    </div>
  );
}

function SharedRunContent({ view }: { view: SharedRunView }) {
  const story = buildStory(view);
  const hasTranscript =
    view.input !== undefined || (view.messages?.length ?? 0) > 0;

  const requestText =
    view.input === undefined
      ? undefined
      : typeof view.input === "string"
        ? view.input
        : JSON.stringify(view.input, null, 2);

  // Duration is a fact only once the run has stopped moving — `updatedAt` on a
  // live run is its last transition, not now.
  const ranMs = TERMINAL.has(view.status)
    ? Date.parse(view.updatedAt) - Date.parse(view.createdAt)
    : NaN;
  const metaLine = [
    view.agent,
    view.namespace,
    Number.isFinite(ranMs) && ranMs > 0 ? formatLatency(ranMs) : undefined,
  ]
    .filter(Boolean)
    .join(" · ");

  const record: KeyValueItem[] = [
    { key: "Run id", value: view.id, title: view.id },
    { key: "Agent", value: view.agent, absent: "not recorded" },
    { key: "Workspace", value: view.namespace, absent: "not recorded" },
    {
      key: "Started",
      value: view.createdAt ? formatDateTime(view.createdAt) : undefined,
      absent: "not recorded",
    },
    {
      key: "Last change",
      value: view.updatedAt ? formatDateTime(view.updatedAt) : undefined,
      absent: "not recorded",
    },
    // A counted zero is a real zero and renders as one; only an unmeasured
    // value takes the absent branch (§7.1).
    { key: "Messages", value: view.messageCount.toLocaleString() },
    {
      key: "Roles",
      value: view.messageRoles.length > 0 ? view.messageRoles.join(", ") : undefined,
      absent: "not recorded",
      mono: false,
    },
    ...(view.errorCategory
      ? [{ key: "Error category", value: view.errorCategory } as KeyValueItem]
      : []),
  ];

  return (
    <div data-testid="shared-run-content" className="min-w-0 space-y-6">
      <PageHeader
        title="Shared run"
        status={
          <Badge variant={STATUS_VARIANT[view.status] ?? "muted"}>
            {statusWord(view.status)}
          </Badge>
        }
        meta={metaLine || undefined}
        lede="One run, exactly as it was recorded: what was asked, what the agent did about it, and how it ended."
      />

      <div className="grid gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
        <div className="min-w-0 space-y-5">
          {hasTranscript ? (
            <div data-testid="shared-run-transcript" className="min-w-0 space-y-5">
              {requestText !== undefined && (
                <Card className="min-w-0">
                  <PanelHeader title="What was asked" />
                  <CardContent>
                    {/* A code well (§4.5): the raw request keeps its own shape
                        and scrolls inside its frame, never widening the page. */}
                    <div className="max-h-56 overflow-auto rounded-md bg-surface-3 p-4">
                      <pre className="whitespace-pre-wrap break-words font-mono text-xs">
                        {requestText}
                      </pre>
                    </div>
                  </CardContent>
                </Card>
              )}

              {story.length > 0 && (
                <Card className="min-w-0">
                  <PanelHeader
                    title="What happened"
                    meta={`${story.length} step${story.length === 1 ? "" : "s"}`}
                  />
                  <CardContent>
                    <Timeline steps={story} label="Steps in this run" />
                    <ClosingNote>
                      {view.status === "succeeded"
                        ? `${story.length} steps, start to finish.`
                        : `${story.length} steps, as far as this run got.`}
                    </ClosingNote>
                  </CardContent>
                </Card>
              )}
            </div>
          ) : (
            <QuietNote title="The transcript wasn't shared.">
              This link was minted without the run's content, so what was said is
              deliberately absent rather than missing. The record beside it — the
              agent, the outcome, and how many messages were exchanged — is
              everything the owner chose to include. Nothing here is estimated.
            </QuietNote>
          )}
        </div>

        <div className="min-w-0" data-testid="shared-run-metadata">
          <Card className="min-w-0">
            <PanelHeader title="The record" meta={truncateId(view.id)} />
            <CardContent>
              <KeyValueList items={record} />
            </CardContent>
          </Card>
        </div>
      </div>
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
