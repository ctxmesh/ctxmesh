import * as React from "react";
import { CheckCircle2, RefreshCw } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  ClosingNote,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  EmptyState,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  UnknownValue,
  UNKNOWN,
  isKnown,
  useToast,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type KeyValueItem,
  type Quantity,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { api, ApiError, type McpApproval } from "@/lib/api";
import { formatDateTime, formatRelativeTime } from "@/lib/format";

// McpApprovalsPage — the operator's queue of MCP servers awaiting review, on the
// M151 editorial system: archetype A1, the §4.4 QUEUE budget (Kind · Ask ·
// Requester · Age · Decide). On a hardened install, user-submitted MCP servers
// land here; approving one binds it — and every tool it declares — into the
// catalog, so this is a grant, not a formality.
//
// ── THE HUE DEFECT THIS PAGE CARRIED (§2.4 sweep) ───────────────────────────
// The row used to render `<Badge variant="warning">pending</Badge>` beside an
// amber clock icon. Amber now means exactly one thing — "a bound is near or
// crossed, or a thing is degraded but still serving" — and a server sitting in
// a queue is neither. It is the archetypal HOLD: work paused because a person
// must decide. The tag moves to `hold` (violet) and the word moves with it:
// "pending" reads as "the machine is working on it", which is the meaning that
// now belongs to the pine-tint `progressing` chip. It says "Awaiting review".
// The amber clock icon went with the old row entirely.
//
// Every row carries that same tag, deliberately — this queue holds one kind of
// thing, and stops-page sets the precedent (every row a `crit` tag, because
// every row is a stop). Here the state IS the taxonomy: there is nothing else a
// row in this queue can be.
//
// ── WHAT MOVED OUT OF THE ROW, AND WHY ──────────────────────────────────────
// The endpoint URL used to sit in the row. §4.5 is explicit that URLs belong in
// a code well and never in a bare table cell — and this page was the proof: the
// 63-character server name plus its badges was the one thing in the whole sweep
// that overflowed its own cell at 768. The URL now renders in the row's drawer,
// in a mono well with `break-all`, whole and copyable, alongside every column
// the budget drops at 1280/1024/768 ("dropped ≠ lost", §4.4).
//
// RBAC: approve/reject are OPERATOR-ONLY. The UI gates the controls on
// `can(RES_REGISTRIES, "update")` — operators have update, a viewer does not and
// gets the read-only "Review the ask" next step instead of a dead button. The
// REAL gate is the API: a forced attempt gets a 403 surfaced honestly.
//
// data-testid contract:
//   mcp-approvals              — root container
//   mcp-approvals-refresh      — the manual refresh control
//   mcp-approval-row-<ns>-<n>  — one queue row
//   mcp-approve-<ns>-<n> / mcp-reject-<ns>-<n> — the decision controls
//   mcp-review-<ns>-<n>        — the row's "Review the ask" next step
//   action-error               — an approve/reject the backend rejected

/** The one tag this queue can carry: a person must decide (§2.2 hold). */
const HOLD_WORD = "Awaiting review";

const AGE_UNKNOWN_TITLE =
  "The queue did not record when this server was submitted. It is unknown — not zero.";

const TOOLS_UNKNOWN_TITLE =
  "This submission did not report how many tools it declares. It is unknown — not zero.";

const SUBMITTER_UNKNOWN =
  "The queue did not record who submitted this server.";

const REJECT_CONTRACT =
  "Rejecting removes the submission from the queue permanently. Nothing is added to the catalog and no tool is granted; whoever submitted it would have to submit it again.";

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; approvals: McpApproval[] }
  // 501 — the approval queue is not built into this install (§7.1).
  | { kind: "unavailable" }
  | { kind: "error"; message: string; forbidden: boolean };

interface ActionState {
  kind: "idle" | "approving" | "rejecting" | "confirm-reject";
  ns: string;
  name: string;
  error?: string;
}

const ACTION_IDLE: ActionState = { kind: "idle", ns: "", name: "" };

/** Stable row key AND the testid suffix the black-box suite asserts on. */
function rowKey(a: McpApproval): string {
  return `${a.namespace}-${a.name}`;
}

/** Epoch ms of a submission, or undefined when the queue did not record it. */
function submittedAtMs(a: McpApproval): number | undefined {
  if (!a.submittedAt) return undefined;
  const t = Date.parse(a.submittedAt);
  return Number.isNaN(t) ? undefined : t;
}

/** Oldest first: the submission that has waited longest is owed an answer. */
function byAge(a: McpApproval, b: McpApproval): number {
  const x = submittedAtMs(a);
  const y = submittedAtMs(b);
  if (x !== undefined && y !== undefined) {
    return x - y || rowKey(a).localeCompare(rowKey(b));
  }
  if (x !== undefined) return -1;
  if (y !== undefined) return 1;
  return rowKey(a).localeCompare(rowKey(b));
}

/**
 * One count, in the right register (the stops-page pattern).
 *
 * `QuantityValue` draws its unknown branch in `text-ghost`, which the system
 * reserves for decoration — but "how many tools does approving this grant?" is
 * meta that MUST be read, so the unknown branch goes through `UnknownValue`
 * (`text-faint`) instead. A known count keeps QuantityValue's mono tabular
 * register, and a known ZERO renders a real `0` rather than the same dash.
 */
function Count({ value, title }: { value: Quantity; title: string }) {
  if (!isKnown(value)) return <UnknownValue title={title} />;
  return <QuantityValue value={value} />;
}

/**
 * The §5.18 closing line, counted from the rows in hand and grammatical at one.
 * It restates what approving actually does, because that is the fact the table
 * cannot show: this queue grants tools.
 */
export function closingLine(rows: McpApproval[]): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const counted = rows.filter((r) => typeof r.toolCount === "number");
  const tools = counted.reduce((sum, r) => sum + (r.toolCount ?? 0), 0);
  const head =
    total === 1
      ? "One MCP server is waiting on an operator."
      : `${total} MCP servers are waiting on an operator.`;
  // Only the submissions that REPORTED a tool count are summed, and the
  // sentence says how many those were — an unreported count is not a zero.
  if (counted.length === 0) {
    return `${head} Approving one binds it, and every tool it declares, into the catalog.`;
  }
  const scope =
    counted.length === total
      ? "Approving them all"
      : `Approving the ${counted.length} that reported their tools`;
  return `${head} ${scope} would add ${tools} tool${tools === 1 ? "" : "s"} to the catalog.`;
}

export function McpApprovalsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [action, setAction] = React.useState<ActionState>(ACTION_IDLE);
  const [refreshing, setRefreshing] = React.useState(false);
  const [reviewing, setReviewing] = React.useState<McpApproval | null>(null);
  const abortRef = React.useRef<AbortController | null>(null);

  const { can, reprobe } = useCapabilities();
  // Operator gate: only update permission holders see approve/reject actions.
  // A viewer sees the queue (list) but not the action buttons — the real gate
  // is the API 403 if they somehow invoke the action.
  const canApprove = can(RES_REGISTRIES, "update");

  const { toast } = useToast();

  // `silent` refreshes the rows IN PLACE: no skeleton over already-visible rows,
  // and a failed silent refresh keeps what the operator is looking at. A queue
  // that flashes a spinner every 30 seconds is unusable during an incident, and
  // one that blanks itself after every decision is worse.
  const load = React.useCallback((silent = false) => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    if (!silent) setPage({ kind: "loading" });
    api
      .mcpApprovals(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // The BFF returns the pending servers under `items` (list-contract key);
        // fall back to `servers`, and default to [] so an unexpected shape can
        // never crash on `.length` (the integration-shape bug this fixes).
        setPage({ kind: "ready", approvals: res.items ?? res.servers ?? [] });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        if (silent) return;
        if (err instanceof ApiError) {
          if (err.status === 501) {
            setPage({ kind: "unavailable" });
            return;
          }
          setPage({ kind: "error", message: err.message, forbidden: err.isForbidden });
          return;
        }
        setPage({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: false,
        });
      })
      .finally(() => {
        if (silent) setRefreshing(false);
      });
  }, []);

  React.useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // A ~30s silent background poll, so a submission a colleague already actioned
  // is not decided blind. Torn down on unmount.
  React.useEffect(() => {
    const id = window.setInterval(() => load(true), 30_000);
    return () => window.clearInterval(id);
  }, [load]);

  function manualRefresh() {
    setRefreshing(true);
    load(true);
  }

  async function onApprove(ns: string, name: string) {
    setAction({ kind: "approving", ns, name });
    try {
      await api.approveMcp(ns, name);
      toast({ variant: "success", title: `Approved ${name}`, description: "The MCP server is now in the catalog." });
      setReviewing(null);
      setAction(ACTION_IDLE);
      load(true);
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setAction({ kind: "idle", ns: "", name: "", error: `Not allowed to approve: ${err.message}` });
          return;
        }
        setAction({ kind: "idle", ns: "", name: "", error: err.message });
        return;
      }
      setAction({ kind: "idle", ns: "", name: "", error: err instanceof Error ? err.message : "approve failed" });
    }
  }

  async function onReject(ns: string, name: string) {
    setAction({ kind: "rejecting", ns, name });
    try {
      await api.rejectMcp(ns, name);
      toast({ variant: "success", title: `Rejected ${name}`, description: "The pending MCP server has been removed." });
      setReviewing(null);
      setAction(ACTION_IDLE);
      load(true);
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setAction({ kind: "idle", ns: "", name: "", error: `Not allowed to reject: ${err.message}` });
          return;
        }
        setAction({ kind: "idle", ns: "", name: "", error: err.message });
        return;
      }
      setAction({ kind: "idle", ns: "", name: "", error: err instanceof Error ? err.message : "reject failed" });
    }
  }

  const busy = action.kind === "approving" || action.kind === "rejecting";
  const rows = React.useMemo(
    () => (page.kind === "ready" ? [...page.approvals].sort(byAge) : []),
    [page],
  );

  const error: DataTableError | null =
    page.kind === "error"
      ? {
          message: page.message,
          forbidden: page.forbidden,
          resource: "the MCP approval queue",
          // `load` takes a boolean, so handing it straight to onRetry would pass
          // the click EVENT as `silent` and retry down the path that suppresses
          // errors — the retry would look like it did nothing.
          onRetry: page.forbidden ? undefined : () => load(),
        }
      : null;

  function rowBusy(a: McpApproval): boolean {
    return busy && action.ns === a.namespace && action.name === a.name;
  }

  // The §4.4 QUEUE budget, in visual order. Kind, Ask and Decide survive every
  // width; Requester leaves at 1024 and Age at 768, where Decide collapses to a
  // single "Review →". Everything dropped renders in the row's drawer.
  const columns: Column<McpApproval>[] = [
    {
      id: "state",
      // "Kind" would be a lie on this page: every row is the same kind of
      // thing, and what the tag reports is its STATE. It is constant by
      // construction — and it stays, because the header chip scrolls away and a
      // reader landing mid-table still has to know these rows are held.
      header: "State",
      priority: 1,
      className: "w-[8rem]",
      // The §2.4 fix: hold, not warn. Amber means a bound is near or crossed;
      // this is a person-must-decide state, which is what violet is for.
      cell: () => <Badge variant="hold">{HOLD_WORD}</Badge>,
    },
    {
      id: "ask",
      header: "Ask",
      priority: 1,
      className: "min-w-[10rem]",
      cell: (a) => (
        // The row's identity cell carries the black-box row testid — it is
        // priority 1, so it is the one cell present at every width.
        //
        // The RESPONSIVE cap is load-bearing, not decoration. A `truncate`d
        // line is `white-space: nowrap`, so its min-content contribution is the
        // WHOLE 63-character name — which in an auto-layout table sets the
        // column's width and pushes Decide off the right edge. `max-width`
        // clamps that contribution, so the cap is what lets the budget's other
        // four columns fit; it widens with the viewport so the name is only as
        // clipped as the room genuinely requires.
        <div
          className="min-w-0 max-w-[15rem] lg:max-w-[18rem] xl:max-w-[20rem] 2xl:max-w-[26rem]"
          data-testid={`mcp-approval-row-${rowKey(a)}`}
        >
          {/* A server name is a machine string: mono, one line, end-ellipsis,
              full value in `title` — never `break-all` (§4.5). The 63-char
              fixture name is exactly why this cell is capped. */}
          <div className="truncate font-mono text-sm font-semibold" title={a.name}>
            {a.name}
          </div>
          <div className="flex min-w-0 items-baseline gap-1.5 font-mono text-xs text-faint">
            <span className="truncate" title={a.namespace}>
              {a.namespace}
            </span>
            <span aria-hidden="true" className="text-ghost">
              ·
            </span>
            {/* The tool count is the SIZE OF THE GRANT, so it is the one thing
                on this line that never truncates away. */}
            <span className="shrink-0 whitespace-nowrap">
              <Count value={a.toolCount ?? UNKNOWN} title={TOOLS_UNKNOWN_TITLE} />{" "}
              {a.toolCount === 1 ? "tool" : "tools"}
            </span>
          </div>
        </div>
      ),
    },
    {
      id: "requester",
      header: "Requester",
      priority: 3,
      className: "max-w-[12rem]",
      cell: (a) =>
        a.submittedBy ? (
          <span className="block truncate font-mono text-xs" title={a.submittedBy}>
            {a.submittedBy}
          </span>
        ) : (
          <span className="font-mono text-xs text-faint" title={SUBMITTER_UNKNOWN}>
            not recorded
          </span>
        ),
    },
    {
      id: "age",
      header: "Waiting",
      priority: 2,
      className: "w-[6.5rem]",
      cell: (a) =>
        a.submittedAt ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums"
            title={formatDateTime(a.submittedAt)}
          >
            {formatRelativeTime(a.submittedAt)}
          </span>
        ) : (
          <UnknownValue title={AGE_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "decide",
      header: "Decide",
      // Never dropped: it is the page's point (§4.4).
      priority: 1,
      className: "w-[11.5rem]",
      cell: (a) => (
        <DecideCell
          approval={a}
          canApprove={canApprove}
          busy={rowBusy(a)}
          onApprove={() => void onApprove(a.namespace, a.name)}
          onRejectRequest={() =>
            setAction({ kind: "confirm-reject", ns: a.namespace, name: a.name })
          }
          onReview={() => setReviewing(a)}
        />
      ),
    },
  ];

  // Good news, and it must read like good news — a different shape from "you
  // cannot see the queue" (the forbidden variant) and from "this install has no
  // approval queue" (the `unavailable` frame below).
  const empty: EmptyStateProps = {
    icon: CheckCircle2,
    title: "Nothing is waiting on you.",
    description:
      "No MCP server is queued for operator review. When someone submits one, it appears here with what it would grant and a way to approve or reject it.",
  };

  const waiting = page.kind === "ready" ? rows.length : undefined;
  const closing = closingLine(rows);

  return (
    <div className="min-w-0 space-y-6" data-testid="mcp-approvals">
      <PageHeader
        title="MCP approvals"
        // The one hold chip on the page, on the one thing that is a state: how
        // many submissions are blocked on a person (§2.2).
        status={
          waiting !== undefined && waiting > 0 ? (
            <Badge variant="hold" data-testid="mcp-approvals-waiting-count">
              {waiting} awaiting a person
            </Badge>
          ) : undefined
        }
        lede="MCP servers submitted by users, waiting on an operator. Oldest first. Approving one binds it — and every tool it declares — into the catalog, where any agent granted that registry can call it."
        actionsSlot={
          <Button
            variant="outline"
            size="sm"
            className="text-sm"
            onClick={manualRefresh}
            disabled={page.kind === "loading" || refreshing}
            data-testid="mcp-approvals-refresh"
          >
            <RefreshCw className={`h-4 w-4${refreshing ? " animate-spin" : ""}`} />
            {refreshing ? "Refreshing…" : "Refresh"}
          </Button>
        }
      />

      {action.error && (
        <p
          className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
          role="alert"
          data-testid="action-error"
        >
          {action.error}
        </p>
      )}

      {page.kind === "unavailable" ? (
        // The backend-cannot-answer state (§7.1): calm, solid, no hue, no CTA.
        <EmptyState
          intent="unavailable"
          title="The MCP approval queue isn’t enabled on this install."
          description="This control plane was built without the caller-scoped MCP approval queue, so submissions are not held for review and there is nothing here to decide. Enabling it is an operator change to the control plane. Nothing here is estimated — the queue is simply absent."
        />
      ) : (
        <DataTable<McpApproval>
          columns={columns}
          rows={rows}
          rowKey={(a) => `${a.namespace}/${a.name}`}
          loading={page.kind === "loading"}
          error={error}
          ariaLabel="MCP approvals"
          tableClassName="min-w-[38rem]"
          onRowClick={(a) => setReviewing(a)}
          empty={empty}
        />
      )}

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {/* The row's full record — §4.4's "dropped ≠ lost". The endpoint lives
          here rather than in a cell because §4.5 puts URLs in a code well, and
          because the 63-character name plus a URL is what used to push this
          page's row past its own width at 768. */}
      <DetailDrawer
        open={reviewing !== null}
        onClose={() => setReviewing(null)}
        title={reviewing?.name ?? ""}
        subtitle={reviewing?.namespace}
        status={reviewing ? <Badge variant="hold">{HOLD_WORD}</Badge> : null}
        footer={
          reviewing ? (
            <div className="flex items-center gap-3">
              {canApprove ? (
                <>
                  <Button
                    variant="outline"
                    disabled={rowBusy(reviewing)}
                    onClick={() => void onApprove(reviewing.namespace, reviewing.name)}
                  >
                    Approve
                  </Button>
                  <Button
                    variant="outline"
                    disabled={rowBusy(reviewing)}
                    onClick={() =>
                      setAction({
                        kind: "confirm-reject",
                        ns: reviewing.namespace,
                        name: reviewing.name,
                      })
                    }
                  >
                    Reject
                  </Button>
                </>
              ) : (
                // Not a greyed-out button: a control a reader cannot use is
                // noise, and the sentence is the useful part.
                <p className="text-sm text-secondary-foreground">
                  Deciding on a submission needs operator permission on registries. Ask whoever
                  administers this cluster.
                </p>
              )}
              <Button variant="ghost" onClick={() => setReviewing(null)}>
                Close
              </Button>
            </div>
          ) : null
        }
      >
        {reviewing && (
          <div className="space-y-5">
            <div>
              <p className="font-mono text-2xs uppercase tracking-wide text-faint">Endpoint</p>
              {reviewing.url ? (
                // A single opaque string: a code well with `break-all`, whole
                // and copyable, never end-ellipsised into something that no
                // longer identifies the server (§4.5).
                <p className="mt-1 break-all rounded-md bg-surface-3 px-3 py-2 font-mono text-xs">
                  {reviewing.url}
                </p>
              ) : (
                <p className="mt-1 text-sm text-faint">
                  The submission did not record an endpoint. It is unknown — not empty.
                </p>
              )}
            </div>
            <KeyValueList items={recordItems(reviewing)} />
            <p className="text-sm text-secondary-foreground">
              Approving binds this server into the catalog. Every tool it declares becomes callable
              by any agent granted its registry, so the tool count is the size of the grant.
            </p>
          </div>
        )}
      </DetailDrawer>

      {/* Reject confirmation — rejection is permanent, so it is gated. */}
      <ConfirmDialog
        // It stays open WHILE the reject is in flight: a dialog that vanishes
        // the instant you press the button leaves the operator with no idea
        // whether the request went out.
        open={action.kind === "confirm-reject" || action.kind === "rejecting"}
        onCancel={() => setAction(ACTION_IDLE)}
        onConfirm={() => {
          const { ns, name } = action;
          void onReject(ns, name);
        }}
        busy={action.kind === "rejecting"}
        title={action.name ? `Reject ${action.name}?` : "Reject this MCP server?"}
        description={REJECT_CONTRACT}
        confirmLabel="Reject"
      />
    </div>
  );
}

/** The full record, for the drawer. Every field the columns can drop. */
function recordItems(a: McpApproval): KeyValueItem[] {
  return [
    { key: "Workspace", value: a.namespace },
    {
      key: "Submitted by",
      value: a.submittedBy,
      absent: "not recorded",
      title: a.submittedBy ?? SUBMITTER_UNKNOWN,
    },
    {
      key: "Submitted at",
      value: a.submittedAt ? formatDateTime(a.submittedAt) : undefined,
      absent: "not recorded",
      title: AGE_UNKNOWN_TITLE,
    },
    {
      key: "Tools declared",
      value: a.toolCount,
      absent: "not reported",
      title: TOOLS_UNKNOWN_TITLE,
    },
  ];
}

/**
 * The Decide cell — §4.4's "inline Approve/Deny, collapsing to a single
 * Review → at 768", plus the viewer's read-only path.
 *
 * The collapse is pure CSS (`hidden md:flex` / `md:hidden`), not a matchMedia
 * hook: no resize listener, no hydration flash, and `display:none` keeps the
 * control that is not showing out of the accessibility tree too.
 */
function DecideCell({
  approval,
  canApprove,
  busy,
  onApprove,
  onRejectRequest,
  onReview,
}: {
  approval: McpApproval;
  canApprove: boolean;
  busy: boolean;
  onApprove: () => void;
  onRejectRequest: () => void;
  onReview: () => void;
}) {
  const base = rowKey(approval);
  const review = (
    <NextStepLink
      label="Review the ask"
      onClick={onReview}
      ariaLabel={`Review the ask — ${approval.name}`}
      testId={`mcp-review-${base}`}
    />
  );

  // A viewer can read the queue but not decide it. The cell says what they CAN
  // do rather than showing two controls that would 403.
  if (!canApprove) {
    return <div onClick={(e) => e.stopPropagation()}>{review}</div>;
  }

  return (
    <div onClick={(e) => e.stopPropagation()}>
      <div className="hidden items-center gap-2 md:flex">
        {/* Both `outline`, deliberately: a pine-filled Approve beside an
            outlined Reject is the console recommending one answer to a question
            that is the operator's to settle. Reject's permanence is carried by
            its confirmation dialog, not by shouting in every row (§2.3). */}
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={onApprove}
          data-testid={`mcp-approve-${base}`}
          aria-label={`Approve ${approval.name}`}
        >
          {busy ? "Working…" : "Approve"}
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={onRejectRequest}
          data-testid={`mcp-reject-${base}`}
          aria-label={`Reject ${approval.name}`}
        >
          Reject
        </Button>
      </div>
      {/* Below 768 two consequential buttons cannot sit beside the ask, so the
          row opens its record instead of shrinking them into a mis-tap. */}
      <div className="md:hidden">{review}</div>
    </div>
  );
}
