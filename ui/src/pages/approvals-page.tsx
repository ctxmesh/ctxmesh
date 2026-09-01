import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { CheckCircle2, RefreshCw } from "lucide-react";

import {
  CellEntity,
  ClosingNote,
  ConfirmDialog,
  DataTable,
  EmptyState,
  NextStepLink,
  PageHeader,
  QuietNote,
  UnknownValue,
  truncateId,
  type Column,
  type DataTableError,
  type EmptyStateProps,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useNamespace } from "@/lib/namespace";
import { api, ApiError, type ApprovalQueueItem } from "@/lib/api";
import { formatRelativeTime, formatDateTime } from "@/lib/format";

// ApprovalsPage — the unified approval inbox (M113), on the M151 editorial
// system: archetype A1, the §4.4 QUEUE budget (Kind · Ask · Requester · Age ·
// Decide), and the one surface whose entire subject is "a person must decide".
//
// ── WHY THE KIND TAG IS NOT A STATUS TAG (the §2.4 sweep item) ──────────────
// This column used to render `<Badge variant="default">Plan gate</Badge>`, and
// `default` is now an alias of `progressing` — the pine-tint chip that means
// "the machine is converging on its own". On a queue of runs that are stopped
// dead waiting for a human, that label said the opposite of the truth.
//
// It does not become `hold` either. "Plan gate" vs. "Step approval" answers
// WHICH GATE is asking, not WHAT CONDITION the row is in — and the condition is
// identical on every row of this page. A hold chip repeated on all 40 rows
// carries no information while spending the one hue that means "act now", which
// is how a hue stops being read. So the kind tag is `muted` (the neutral,
// non-status register), and the hold violet is spent exactly ONCE, in the page
// header, on the sentence that is actually a fact about the page: "6 awaiting a
// person" (§2.2's own example).
//
// This deliberately diverges from stops-page, where every row carries a `crit`
// tag: there the tag IS the row's state (a stop) and the word narrows its blast
// radius. Here the state belongs to the page, and the word names a taxonomy.
//
// ── INLINE DECIDE, BUT ONLY WHERE THE ROW STATES THE WHOLE ASK ──────────────
// §4.4 puts Approve/Deny in the row. That is right only when the row shows
// everything the decision rests on. A STEP approval's message is the entire ask
// ("Approve create_refund for charge ch_2 (€4,412.00)?") — decidable in place.
// A PLAN gate's message is a SUMMARY of a multi-step plan that lives on the run
// page; approving it from a one-line précis would be authority the row has not
// earned. So plan gates — and any row whose ask was not recorded — get the
// "Next step" treatment instead of buttons, and the Decide column is the one
// column that renders either. Below 768 both collapse to a single "Review →".
//
// ── THE EMPTY STATE IS GOOD NEWS, AND IT IS NOT THE OTHER THREE ─────────────
// Four different truths, four different renderings (§7):
//   • nothing waiting  → the teaching EmptyState, "Nothing is waiting on you."
//   • no backend (501) → the `unavailable` EmptyState, a calm solid frame that
//                        says the queue is not part of this install.
//   • forbidden (403)  → the DataTable forbidden variant, never a fake empty.
//   • error            → ErrorState + Retry, with the reason.
// The dangerous confusion is the first against the third: "nothing is waiting
// on you" and "you cannot see what is waiting" must never render alike, so the
// all-workspaces path below COUNTS the namespaces that refused instead of
// swallowing them into an empty array (it used to `.catch(() => [])` them).
//
// data-testid contract:
//   approvals-page          — root container
//   approvals-refresh       — the manual refresh control
//   approvals-action-error  — an approve/deny the backend rejected
//   approvals-approve-<id> / approvals-deny-<id> — the inline decision controls
//   approvals-review-<id>   — the row's "Review"/"Read the plan" next step

/** The kind label. A taxonomy word, never a state (see the header note). */
const KIND_WORD: Record<ApprovalQueueItem["kind"], string> = {
  plan_approval: "Plan gate",
  approval: "Step approval",
};

const AGE_UNKNOWN_TITLE =
  "The queue did not record when this run started waiting. It is unknown — not zero.";

/**
 * Approving authorises the call the run stopped to ask about — it does not
 * merely unblock the run. The dialog quotes the ask back, because the number in
 * it is the thing being agreed to.
 */
const APPROVE_CONTRACT =
  "The run continues and makes this call. Whatever it does — a refund, a write, a charge — happens for real, and it is recorded against you.";

/** Denying resolves the run permanently; the dialog says so before it happens. */
const DENY_CONTRACT =
  "Denying cancels the run. It stops where it is, keeps everything it has already done, and cannot be resumed — whoever started it would have to start it again.";

/**
 * One queue row as this page reads it.
 *
 * `decidable` is computed once, here, so the Decide cell and the closing line
 * can never disagree about which rows this page is prepared to act on.
 */
interface Row {
  item: ApprovalQueueItem;
  /** The row states the whole ask, so Approve/Deny may live in the row. */
  decidable: boolean;
  /** Epoch ms of `waitingSince`, or undefined when the queue did not record it. */
  waitedAt?: number;
}

function toRow(item: ApprovalQueueItem): Row {
  const at = item.waitingSince ? Date.parse(item.waitingSince) : NaN;
  return {
    item,
    // A plan gate's summary is not the plan, and an unrecorded ask is not an
    // ask — neither may be approved from this page.
    decidable: item.kind === "approval" && !!item.message?.trim(),
    waitedAt: Number.isNaN(at) ? undefined : at,
  };
}

/** One run, once — see the all-workspaces scan below. First sighting wins. */
function dedupe(list: ApprovalQueueItem[]): ApprovalQueueItem[] {
  const seen = new Set<string>();
  const out: ApprovalQueueItem[] = [];
  for (const item of list) {
    if (seen.has(item.runId)) continue;
    seen.add(item.runId);
    out.push(item);
  }
  return out;
}

/** Oldest first: the longest-blocked ask is the one a person owes an answer to. */
function byAge(a: Row, b: Row): number {
  if (a.waitedAt !== undefined && b.waitedAt !== undefined) {
    return a.waitedAt - b.waitedAt || a.item.runId.localeCompare(b.item.runId);
  }
  // A row with no recorded age sorts last — it cannot claim to be the oldest.
  if (a.waitedAt !== undefined) return -1;
  if (b.waitedAt !== undefined) return 1;
  return a.item.runId.localeCompare(b.item.runId);
}

/**
 * The §5.18 closing line: the ratio the table already showed, in a sentence,
 * counted from the rows in hand. Grammatical at one, at many, and when one kind
 * is absent — a closing note that reads "1 decisions" undoes the register it is
 * written in.
 */
export function closingLine(rows: Row[]): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const plans = rows.filter((r) => r.item.kind === "plan_approval").length;
  const steps = total - plans;

  let head: string;
  if (total === 1) {
    head = `One decision is waiting on you: ${plans === 1 ? "a plan gate" : "a step approval"}.`;
  } else {
    const mix =
      plans === 0
        ? "all of them step approvals"
        : steps === 0
          ? "all of them plan gates"
          : `${plans} plan gate${plans === 1 ? "" : "s"} and ${steps} step approval${steps === 1 ? "" : "s"}`;
    head = `${total} decisions are waiting on you — ${mix}.`;
  }

  // The oldest row is FIRST (the sort guarantees it), so this is a fact about
  // the list rather than a second, differently-ordered pass over it.
  const oldest = rows[0]?.waitedAt;
  if (oldest === undefined) return head;
  const rel = formatRelativeTime(new Date(oldest).toISOString());
  return rel ? `${head} The oldest arrived ${rel}.` : head;
}

/** One namespace's outcome in the all-workspaces scan. */
type Scan =
  | { kind: "ok"; items: ApprovalQueueItem[] }
  | { kind: "forbidden" }
  | { kind: "unavailable" }
  | { kind: "error"; message: string };

type LoadState =
  | { kind: "loading" }
  | {
      kind: "ready";
      items: ApprovalQueueItem[];
      /** Workspaces that refused to answer — never silently folded into "none". */
      unreadable: number;
      /** Workspaces that did answer. */
      readable: number;
    }
  // 501 — this install has no approval queue at all (§7.1 backend-cannot-answer).
  | { kind: "unavailable" }
  | { kind: "error"; message: string; forbidden: boolean };

type Decision = "approve" | "deny";

export function ApprovalsPage() {
  const { namespace } = useNamespace();
  // Always start loading — load() fetches whether a namespace is selected (that ns)
  // or not (aggregates across all visible namespaces, M144.6). No dead-end state.
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  // refreshing drives the manual Refresh button's spinner. The manual refresh is SILENT (it does not blank
  // the table with a skeleton — matching the background poll); the spinner is the only feedback (V16 close-
  // gate UX finding: a non-silent manual refresh flashed a skeleton over already-visible rows).
  const [refreshing, setRefreshing] = useState(false);
  /** Run ids this session has already decided — dropped from the view at once. */
  const [decided, setDecided] = useState<string[]>([]);
  const [busyRun, setBusyRun] = useState<string | null>(null);
  const [denying, setDenying] = useState<Row | null>(null);
  // Approving is the CONSEQUENTIAL click, and it was the unguarded one: Deny
  // merely cancels a run, while Approve authorises the very call a person was
  // asked about — a refund, a write, a spend — and it fired on a single click
  // eight pixels from an identically-shaped Deny. The guard was on the wrong
  // button (M151 UX review, finding 2).
  const [approving, setApproving] = useState<Row | null>(null);
  const [denyReason, setDenyReason] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  // load fetches the queue. `silent` (used by the poll) refreshes the rows IN PLACE — it does not flip to
  // the loading skeleton, and a failed silent refresh keeps the current rows rather than blowing the table
  // away with an error (a background poll must never disrupt what the reviewer is looking at). A manual load
  // (the Refresh button / initial mount) is non-silent: it shows loading + surfaces errors normally.
  const load = useCallback(
    (silent = false) => {
      abortRef.current?.abort();
      // The per-namespace backend REQUIRES a namespace (it 400s without one). When
      // none is selected, DON'T dead-end on "select a namespace" (M144.6) — aggregate
      // the queue across every namespace the caller can see, so the inbox shows all
      // pending decisions at once (each row still carries its namespace).
      if (!namespace) {
        const allController = new AbortController();
        abortRef.current = allController;
        if (!silent) setLoadState({ kind: "loading" });
        api
          .namespaces(allController.signal)
          .then(async (resp) => {
            // Each workspace answers for itself, and a workspace that REFUSES is
            // counted rather than swallowed: a 403 turned into `[]` renders as
            // "nothing is waiting on you", which is the one lie this page cannot
            // afford to tell.
            const scans: Scan[] = await Promise.all(
              resp.namespaces.map((n) =>
                api
                  .listApprovals(n.name, allController.signal)
                  .then((list): Scan => ({ kind: "ok", items: list }))
                  .catch((err: unknown): Scan => {
                    if (err instanceof ApiError) {
                      if (err.status === 501) return { kind: "unavailable" };
                      if (err.isForbidden) return { kind: "forbidden" };
                      return { kind: "error", message: err.message };
                    }
                    return {
                      kind: "error",
                      message: err instanceof Error ? err.message : "request failed",
                    };
                  }),
              ),
            );
            if (allController.signal.aborted) return;
            const ok = scans.filter(
              (s): s is Extract<Scan, { kind: "ok" }> => s.kind === "ok",
            );

            // Nothing answered. WHY nothing answered is the whole point: an
            // install without the queue, a caller without the grant, and a
            // backend that broke are three different sentences.
            if (scans.length > 0 && ok.length === 0) {
              if (scans.every((s) => s.kind === "unavailable")) {
                setLoadState({ kind: "unavailable" });
                return;
              }
              if (scans.some((s) => s.kind === "forbidden")) {
                setLoadState({ kind: "error", message: "", forbidden: true });
                return;
              }
              const first = scans.find((s) => s.kind === "error");
              setLoadState({
                kind: "error",
                message: first?.kind === "error" ? first.message : "request failed",
                forbidden: false,
              });
              return;
            }
            // No workspace is visible at all, so no approval can be. That is a
            // permission boundary, not an empty inbox.
            if (scans.length === 0) {
              setLoadState({ kind: "error", message: "", forbidden: true });
              return;
            }
            setLoadState({
              kind: "ready",
              // A run id is globally unique, so the same run reported by two
              // workspace queues is one decision, not two. Concatenating the
              // scans without this also hands the table duplicate row keys.
              items: dedupe(ok.flatMap((s) => s.items)),
              unreadable: scans.length - ok.length,
              readable: ok.length,
            });
          })
          .catch((err: unknown) => {
            if (allController.signal.aborted || silent) return;
            if (err instanceof ApiError && err.status === 501) {
              setLoadState({ kind: "unavailable" });
              return;
            }
            setLoadState({
              kind: "error",
              message: err instanceof Error ? err.message : "request failed",
              forbidden: err instanceof ApiError && err.isForbidden,
            });
          })
          .finally(() => {
            if (silent) setRefreshing(false);
          });
        return;
      }
      const controller = new AbortController();
      abortRef.current = controller;
      if (!silent) setLoadState({ kind: "loading" });

      api
        .listApprovals(namespace, controller.signal)
        .then((list) => {
          if (controller.signal.aborted) return;
          setLoadState({ kind: "ready", items: list, unreadable: 0, readable: 1 });
        })
        .catch((err: unknown) => {
          if (controller.signal.aborted) return;
          // A background poll must not disrupt the view — keep the current rows on a silent failure.
          if (silent) return;
          if (err instanceof ApiError && err.status === 501) {
            setLoadState({ kind: "unavailable" });
            return;
          }
          setLoadState({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
            forbidden: err instanceof ApiError && err.isForbidden,
          });
        })
        .finally(() => {
          if (silent) setRefreshing(false);
        });
    },
    [namespace],
  );

  // Manual refresh: refresh IN PLACE (silent — no skeleton flash) with a button spinner for feedback.
  const manualRefresh = useCallback(() => {
    setRefreshing(true);
    load(true);
  }, [load]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // A ~30s background poll so a row a colleague already resolved isn't clicked blind (V16). Silent (no
  // skeleton flash), and only while a namespace is selected — the interval resets when the namespace changes
  // (load's identity changes) and is torn down on unmount.
  useEffect(() => {
    if (!namespace) return;
    const id = window.setInterval(() => load(true), 30_000);
    return () => window.clearInterval(id);
  }, [load, namespace]);

  const unreadable = loadState.kind === "ready" ? loadState.unreadable : 0;
  const readable = loadState.kind === "ready" ? loadState.readable : 0;

  // Derived from `loadState` itself, not from a `kind === "ready" ? … : []`
  // temporary: that temporary is a fresh array on every render, so the memo
  // would recompute every time and never memoise anything.
  const rows = useMemo(() => {
    if (loadState.kind !== "ready") return [];
    return loadState.items
      .filter((i) => !decided.includes(i.runId))
      .map(toRow)
      .sort(byAge);
  }, [loadState, decided]);

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "approvals",
          // `load` takes a boolean; handing it straight to onRetry would pass
          // the click EVENT as `silent` and retry down the silent path, which
          // never clears the error state it was pressed to clear.
          onRetry: loadState.forbidden ? undefined : () => load(),
        }
      : null;

  /**
   * Send one decision. The row leaves the view on success because the run is
   * genuinely no longer awaiting a person; the silent reload then reconciles
   * with the backend without blanking the table.
   */
  async function decide(row: Row, decision: Decision, reason?: string) {
    const id = row.item.runId;
    setActionError(null);
    setBusyRun(id);
    try {
      await api.resumeRun(id, decision, decision === "deny" ? reason : undefined);
      setDecided((d) => [...d, id]);
      setDenying(null);
      setDenyReason("");
      load(true);
    } catch (err) {
      const detail = err instanceof Error ? err.message : "the request failed";
      // Never swallowed and never dressed as success: the run is STILL waiting,
      // and the reader has to know that before they walk away from the screen.
      setActionError(
        err instanceof ApiError && err.isForbidden
          ? `You are not allowed to ${decision} this run, and it is still waiting: ${detail}`
          : `The run was not ${decision === "approve" ? "approved" : "denied"}, and it is still waiting: ${detail}`,
      );
    } finally {
      setBusyRun(null);
    }
  }

  // The §4.4 QUEUE budget, in visual order. Kind, Ask and Decide are priority 1
  // and survive every width; Requester leaves at 1024 and Age at 768, where the
  // Decide cell collapses to a single "Review →". Dropped ≠ lost — the row's
  // run page renders every field.
  const columns: Column<Row>[] = [
    {
      id: "kind",
      header: "Kind",
      priority: 1,
      className: "w-[7.5rem]",
      // `muted`, not `hold`: this names WHICH gate is asking, not what condition
      // the row is in — and that condition is the same on every row (header).
      cell: ({ item }) => <Badge variant="muted">{KIND_WORD[item.kind]}</Badge>,
    },
    {
      id: "ask",
      header: "Ask",
      priority: 1,
      className: "min-w-[15rem] max-w-[32rem]",
      cell: ({ item }) => (
        <div className="min-w-0">
          {item.message ? (
            // Prose wraps to two lines in a cell, with the whole of it in
            // `title` (§4.5). The plan itself lives on the run page.
            <p className="line-clamp-2 text-sm" title={item.message}>
              {item.message}
            </p>
          ) : (
            <p className="text-sm text-faint">
              No summary was recorded. Open the run to read what it is asking.
            </p>
          )}
          {/* Narrow widths drop the Requester and Waiting columns, and on a
              queue those are not decoration — WHO is asking and HOW LONG it has
              waited are two of the three facts the decision turns on. Losing
              them while keeping the run id would leave a person authorising a
              refund with no idea who asked (M151 UX review, finding 3). They
              fold into this cell instead, and only one copy is ever displayed. */}
          <p className="mt-0.5 truncate font-mono text-xs text-faint lg:hidden">
            {item.agent} · {item.namespace}
            {item.waitingSince ? ` · waiting ${formatRelativeTime(item.waitingSince)}` : " · age not recorded"}
          </p>
          <p className="mt-0.5 truncate font-mono text-xs text-faint">
            {/* A run id is machine-owned: mono, middle-truncated head-8…tail-4
                so the tail that disambiguates two ids survives (§4.5). */}
            <Link
              to={`/runs/${encodeURIComponent(item.runId)}`}
              onClick={(e) => e.stopPropagation()}
              title={item.runId}
              className="border-b border-accent text-primary hover:border-primary"
            >
              {truncateId(item.runId)}
            </Link>
            {item.rootRunId && (
              <span className="ml-1.5">
                part of{" "}
                <Link
                  to={`/runs/${encodeURIComponent(item.rootRunId)}`}
                  onClick={(e) => e.stopPropagation()}
                  title={item.rootRunId}
                  className="border-b border-accent text-primary hover:border-primary"
                >
                  {truncateId(item.rootRunId)}
                </Link>
              </span>
            )}
          </p>
        </div>
      ),
    },
    {
      id: "requester",
      header: "Requester",
      priority: 3,
      className: "max-w-[13rem]",
      // The agent is what is asking; its workspace is the second line and never
      // shares the name's line (§4.5).
      cell: ({ item }) => <CellEntity name={item.agent} namespace={item.namespace} />,
    },
    {
      id: "age",
      header: "Waiting",
      priority: 2,
      className: "w-[7rem]",
      cell: ({ item }) =>
        item.waitingSince ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums"
            title={formatDateTime(item.waitingSince)}
          >
            {formatRelativeTime(item.waitingSince)}
          </span>
        ) : (
          // An unrecorded age is meta that must still be READ, so it takes
          // `text-faint` via UnknownValue — never a hand-rolled dash, and never
          // a zero wearing the same glyph.
          <UnknownValue title={AGE_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "decide",
      header: "Decide",
      // Never dropped: it is the page's point (§4.4).
      priority: 1,
      className: "w-[12rem]",
      cell: (row) => (
        <DecideCell
          row={row}
          busy={busyRun === row.item.runId}
          onApprove={() => {
            setActionError(null);
            setApproving(row);
          }}
          onDeny={() => {
            setDenyReason("");
            setActionError(null);
            setDenying(row);
          }}
        />
      ),
    },
  ];

  // Good news, and it must READ like good news — never the same shape as "you
  // cannot see approvals" (the DataTable's forbidden variant) or "this install
  // has no approval queue" (the `unavailable` state rendered below).
  const empty: EmptyStateProps = {
    icon: CheckCircle2,
    title: "Nothing is waiting on you.",
    description: namespace
      ? `No run in ${namespace} is stopped for a person. When one stops on a plan gate or a mid-run step, it appears here with what it is asking and a way to decide.`
      : "No run in any workspace you can see is stopped for a person. When one stops on a plan gate or a mid-run step, it appears here with what it is asking and a way to decide.",
  };

  const waiting = loadState.kind === "ready" ? rows.length : undefined;
  const closing = closingLine(rows);
  const scope = namespace || "all workspaces";

  return (
    <div className="min-w-0 space-y-6" data-testid="approvals-page">
      <PageHeader
        title="Approvals"
        // The ONE hold chip on the page, on the one thing here that genuinely is
        // a state: how many decisions are blocked on a person (§2.2's example).
        status={
          waiting !== undefined && waiting > 0 ? (
            <Badge variant="hold" data-testid="approvals-waiting-count">
              {waiting} awaiting a person
            </Badge>
          ) : undefined
        }
        meta={scope}
        lede="Every run here is stopped because a person must decide. Oldest first — whatever has been blocked longest sits at the top. A step approval can be decided in its row; a plan gate opens its run, because the plan is the thing being approved."
        // actionsSlot rather than a structured action: PageHeaderAction carries
        // no testId, and the refresh control is asserted by id.
        actionsSlot={
          <Button
            variant="outline"
            size="sm"
            className="text-sm"
            onClick={manualRefresh}
            disabled={loadState.kind === "loading" || refreshing}
            data-testid="approvals-refresh"
          >
            <RefreshCw className={`h-4 w-4${refreshing ? " animate-spin" : ""}`} />
            {refreshing ? "Refreshing…" : "Refresh"}
          </Button>
        }
      />

      {actionError && (
        <p
          className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
          role="alert"
          data-testid="approvals-action-error"
        >
          {actionError}
        </p>
      )}

      {/* Some workspaces refused. Said once, out loud, so a short list is never
          mistaken for a quiet one. */}
      {unreadable > 0 && (
        <QuietNote title="Some workspaces didn’t answer.">
          {unreadable} of the {unreadable + readable} workspaces you can see did not return their
          queue — either you cannot read approvals there, or the request failed. Anything waiting in{" "}
          {unreadable === 1 ? "it" : "them"} is not counted below. Nothing here is estimated; those
          queues are simply not readable from this page.
        </QuietNote>
      )}

      {loadState.kind === "unavailable" ? (
        // The backend-cannot-answer state (§7.1): calm, solid, no hue, no CTA —
        // and visibly NOT the dashed "nothing is waiting on you" fence.
        <EmptyState
          intent="unavailable"
          title="The approval queue isn’t part of this install."
          description="This control plane was built without the caller-scoped approval queue, so there is no inbox to read. Runs that stop for a person are still visible on their own run pages. Nothing here is estimated — the queue is simply absent."
        />
      ) : (
        <DataTable<Row>
          columns={columns}
          rows={rows}
          rowKey={(r) => r.item.runId}
          loading={loadState.kind === "loading"}
          error={error}
          ariaLabel="Approvals"
          tableClassName="min-w-[42rem]"
          empty={empty}
        />
      )}

      {closing && <ClosingNote>{closing}</ClosingNote>}

      <ConfirmDialog
        open={approving !== null}
        onCancel={() => setApproving(null)}
        onConfirm={() => {
          if (approving) void decide(approving, "approve");
        }}
        busy={approving !== null && busyRun === approving.item.runId}
        title="Approve this call?"
        description={APPROVE_CONTRACT}
        confirmLabel="Approve"
        impact={
          approving ? (
            <div className="space-y-3">
              {approving.item.message && (
                <p className="font-serif text-md italic" title={approving.item.message}>
                  “{approving.item.message}”
                </p>
              )}
              <p className="font-mono text-xs text-faint">
                {approving.item.agent} · {approving.item.namespace} ·{" "}
                {truncateId(approving.item.runId)}
              </p>
            </div>
          ) : null
        }
      />

      <ConfirmDialog
        open={denying !== null}
        onCancel={() => {
          setDenying(null);
          setDenyReason("");
        }}
        onConfirm={() => {
          if (denying) void decide(denying, "deny", denyReason);
        }}
        busy={denying !== null && busyRun === denying.item.runId}
        title="Deny this run?"
        description={DENY_CONTRACT}
        confirmLabel="Deny"
        impact={
          denying ? (
            <div className="space-y-3">
              {denying.item.message && (
                <p className="font-serif text-md italic" title={denying.item.message}>
                  “{denying.item.message}”
                </p>
              )}
              <p className="font-mono text-xs text-faint">
                {denying.item.agent} · {denying.item.namespace} ·{" "}
                {truncateId(denying.item.runId)}
              </p>
              <div>
                <Label htmlFor="approvals-deny-reason" className="text-xs">
                  Reason (optional — recorded on the run)
                </Label>
                <Textarea
                  id="approvals-deny-reason"
                  data-testid="approvals-deny-reason"
                  className="mt-1 min-h-16"
                  value={denyReason}
                  onChange={(e) => setDenyReason(e.target.value)}
                  placeholder="Why are you denying this? Whoever started the run will read it."
                />
              </div>
            </div>
          ) : null
        }
      />
    </div>
  );
}

/**
 * The Decide cell — §4.4's "inline Approve/Deny, collapsing to a single
 * Review → at 768", plus the honest third case.
 *
 * The collapse is pure CSS (`hidden md:flex` / `md:hidden`), not a matchMedia
 * hook: no resize listener, no hydration flash, and `display:none` keeps the
 * copy that is not showing out of the accessibility tree too — so a screen
 * reader hears one control per row, never two.
 */
function DecideCell({
  row,
  busy,
  onApprove,
  onDeny,
}: {
  row: Row;
  busy: boolean;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const to = `/runs/${encodeURIComponent(row.item.runId)}`;

  // Not decidable from the row: a plan gate (the plan is on the run page) or an
  // ask the queue never recorded. Verb-first, ≤22 chars (§7.2).
  if (!row.decidable) {
    const label = row.item.kind === "plan_approval" ? "Read the plan" : "Open the ask";
    return (
      <NextStepLink
        label={label}
        to={to}
        ariaLabel={`${label} — ${row.item.agent}`}
        testId={`approvals-review-${row.item.runId}`}
      />
    );
  }

  return (
    <div onClick={(e) => e.stopPropagation()}>
      <div className="hidden items-center gap-2 md:flex">
        {/* Both controls are `outline`, deliberately. A pine-filled Approve
            beside an outlined Deny is the console recommending one answer to a
            question that is the reader's to settle — cosmetic authority over a
            human decision (§2.1: pine means "you can act here", not "do this
            one"). Deny's destructiveness is carried by its confirmation, which
            is crit-filled, rather than by shouting in every row (§2.3). */}
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={onApprove}
          data-testid={`approvals-approve-${row.item.runId}`}
          aria-label={`Approve — ${row.item.agent}`}
        >
          {busy ? "Approving…" : "Approve"}
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={onDeny}
          data-testid={`approvals-deny-${row.item.runId}`}
          aria-label={`Deny — ${row.item.agent}`}
        >
          Deny
        </Button>
      </div>
      {/* Below 768 the two controls cannot sit beside the ask, so the row hands
          the decision to the run page rather than shrinking a consequential
          button into a tap target nobody meant to hit. */}
      <div className="md:hidden">
        <NextStepLink
          label="Review"
          to={to}
          ariaLabel={`Review — ${row.item.agent}`}
          testId={`approvals-review-${row.item.runId}`}
        />
      </div>
    </div>
  );
}
