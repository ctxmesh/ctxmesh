import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Filter, Share2 } from "lucide-react";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuietNote,
  UnknownValue,
  nextStepRank,
  truncateId,
  useToast,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { formatDateTime, formatRelativeTime } from "@/lib/format";
import { api, ApiError, type MySharesItem } from "@/lib/api";

// MySharesPage — the caller's share links, on the editorial ACTIVITY-FEED
// archetype (V13; M151 §6.1 A1 composition, §4.4 activity-feed budget:
// Time(1) · Agent(1) · What(1) · Expires(3) · Opened(3) · State(2) · Next step(1)).
//
// ── A FEED IS CHRONOLOGICAL, NOT TRIAGED ────────────────────────────────────
// Newest-minted first: "what have I handed out lately" is the question this
// page exists to answer. `nextStepRank` breaks a timestamp tie so "Nothing
// needed" still sinks, and the live links that expose a whole transcript — the
// only rows that could want a decision — are one chip away.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// The one fact a person actually wants before revoking a link is how many times
// it has been opened. `GET /api/my/shares` does not carry it (the views ARE
// recorded — as `share.view` rows in the audit trail — but they are not
// projected here), so the Opened column renders the readable dash with ONE
// QuietNote above the table saying why. Never a 0: "nobody opened it" and "we
// were not told" are different facts, and a fabricated zero is exactly the one
// that would talk someone out of revoking.
//
// The archetype's other numeric slot is "Cost"; a share link has none, and a
// column of dashes labelled Cost would be dishonest in the other direction —
// the dash means unknown, and a share's cost does not exist. So the slot
// carries the share's real quantity instead: when it expires.
//
// Backend:
//   GET  /api/my/shares                          — list caller's shares
//   DELETE /api/runs/{runId}/shares/{shareId}    — revoke a live share
//
// The page is caller-scoped: the BFF returns ONLY the shares created by the
// authenticated caller, and the whole set in one response — so the chip counts
// here are FACTS, not a windowed guess. There is no token in the response (the
// backend never returns it after creation); the id is an opaque DB id, not a
// secret, and is never labelled as one.
//
// data-testid contract:
//   my-shares-page   — root container
//   run-link-{id}    — the row's link to the shared run
//   next-step-{id}   — the row's Next step cell
//   revoke-{id}      — the Revoke row action (live shares only)

const OPENED_NOTE_TITLE = "How often a link has been opened isn’t in this list.";

const OPENED_UNKNOWN_TITLE =
  "This list does not report how many times the link was opened — unknown, not zero.";

const AGENT_UNKNOWN_TITLE =
  "This link was minted before the agent was recorded on a share — unknown, not absent.";

const EXPIRY_UNKNOWN_TITLE =
  "This link carries no expiry date — unknown, not never.";

/**
 * A timestamp, compact (§4.5): same day → `16:36`; same year → `Sep 7`; older →
 * `2025-08-29`. The full form always rides along in `title`.
 */
export function compactDate(iso: string, now: Date = new Date()): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const d = new Date(t);
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  if (d.getFullYear() === now.getFullYear()) {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  return d.toISOString().slice(0, 10);
}

/** What the link exposes — the compliance-relevant half of a share. */
function exposure(s: MySharesItem): string {
  return s.includeContent ? "Full transcript" : "Metadata only";
}

// ── Triage: what the page renders and sorts by, decided once ────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
}

interface Row {
  share: MySharesItem;
  /** Live AND showing the whole transcript — the only standing disclosure here. */
  open: boolean;
  next: NextStep;
}

/**
 * One share → its next step. A revoked or expired link asks nothing of anyone:
 * it is already dead, and inventing an errand for it would make the column
 * noise. A live link that shows only metadata is working as intended. The one
 * row that can want a decision is a live link showing the WHOLE transcript to
 * anyone holding it — so that is the only row that carries a step.
 */
function triage(s: MySharesItem): Row {
  const open = s.status === "live" && s.includeContent;
  return {
    share: s,
    open,
    next: open
      ? // Pine, not crit: nothing has failed. The destructive act (Revoke) is a
        // separate, deliberately-shaped control in the row's action slot (§2.3).
        { label: "Review the transcript", tone: "default", to: `/runs/${encodeURIComponent(s.runId)}` }
      : { tone: "none" },
  };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "live" | "ended" | "all";

const VIEWS: { id: ViewId; label: string; match: (r: Row) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (r) => r.next.tone !== "none" },
  { id: "live", label: "Live", match: (r) => r.share.status === "live" },
  { id: "ended", label: "Ended", match: (r) => r.share.status !== "live" },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "No live link is showing a whole transcript. Show everything to see the links you have made.",
  },
  live: {
    title: "No link is still live",
    description:
      "Every link you have made has been revoked or has expired. Show everything to see the history.",
  },
  ended: {
    title: "Nothing has ended yet",
    description:
      "Every link you have made is still live. Show everything to see them.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. The whole set is in hand here — the BFF returns every share
 * the caller made in one response — so every number in it is a fact.
 */
export function closingLine(rows: Row[]): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const live = rows.filter((r) => r.share.status === "live").length;
  const open = rows.filter((r) => r.open).length;

  if (total === 1) {
    if (live === 0) return "The one link here is no longer usable.";
    return open === 1
      ? "The one link here is live, and it shows the whole transcript to anyone holding it."
      : "The one link here is live, and it shows metadata only.";
  }

  const liveClause =
    live === 0
      ? `None of the ${total} links is still usable.`
      : live === total
        ? `All ${total} links are still live.`
        : `${live} of the ${total} links ${live === 1 ? "is" : "are"} still live.`;

  // Counted in WORDS at one, so the sentence never reads "1 of them show".
  const openClause =
    open === 0
      ? live === 0
        ? ""
        : " None of them shows the transcript itself."
      : open === 1
        ? " One of them shows the whole transcript to anyone holding it."
        : ` ${open} of them show the whole transcript to anyone holding it.`;

  return `${liveClause}${openClause}`;
}

/**
 * The share's state as a tag: uppercase mono on its own tint, never interactive
 * (§2.1's form rule) — the action lives next door. `expired` is warn (a bound
 * was crossed), `revoked` is the sunk muted chip (deliberately off, and not a
 * problem), `live` is ok.
 */
function StateTag({ status }: { status: MySharesItem["status"] }) {
  if (status === "live") return <Badge variant="ok">Live</Badge>;
  if (status === "expired") return <Badge variant="warn">Expired</Badge>;
  return <Badge variant="muted">Revoked</Badge>;
}

/**
 * The row's one destructive act. It lives in the DataTable's trailing action
 * slot rather than in a column of its own — the §4.4 budget has no slot for a
 * button. Crit is the sanctioned action hue for a destructive control (§2.3);
 * it stays distinguishable from a crit STATUS tag by form (a bordered control
 * in sentence case vs. an uppercase mono chip on a tint). Clicks do not
 * propagate, so revoking never also opens the run.
 */
function RowActions({
  share,
  busy,
  onRevoke,
}: {
  share: MySharesItem;
  busy: boolean;
  onRevoke: (s: MySharesItem) => void;
}) {
  if (share.status !== "live") return null;
  return (
    <div
      className="flex items-center justify-end"
      onClick={(e) => e.stopPropagation()}
      data-testid={`row-actions-${share.id}`}
    >
      <Button
        variant="outline"
        size="sm"
        className="text-destructive hover:border-destructive"
        disabled={busy}
        title="Revoke this link — it stops working immediately, for everyone holding it."
        onClick={(e) => {
          e.stopPropagation();
          onRevoke(share);
        }}
        data-testid={`revoke-${share.id}`}
      >
        {busy ? "Revoking…" : "Revoke"}
      </Button>
    </div>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: MySharesItem[] }
  | { kind: "error"; message: string; forbidden: boolean };

const LEDE =
  "Every link you have handed out, newest first. A live link works for anyone holding it until you revoke it or it expires.";

export function MySharesPage() {
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const [view, setView] = useState<ViewId>("all");
  // Track which share IDs are in the process of being revoked (optimistic disable).
  const [revoking, setRevoking] = useState<Set<string>>(new Set());
  const abortRef = useRef<AbortController | null>(null);
  const { toast } = useToast();

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
        toast({ variant: "success", title: "Share revoked" });
        load();
      })
      .catch((err: unknown) => {
        // Surface the failure (V16 P3: a silent .catch left the user thinking the link was revoked when it
        // wasn't) and re-enable the button so the user can retry.
        toast({
          variant: "error",
          title: "Revoke failed",
          description: err instanceof Error ? err.message : "could not revoke the share link",
        });
        setRevoking((prev) => {
          const next = new Set(prev);
          next.delete(item.id);
          return next;
        });
      });
  }

  const items = useMemo(
    () => (loadState.kind === "ready" ? loadState.items : []),
    [loadState],
  );

  // Newest-minted first — a feed is chronological. `nextStepRank` breaks a
  // timestamp tie so "Nothing needed" still sinks; the share id breaks that, so
  // the order is stable across refetches.
  const sorted = useMemo(() => {
    const rows = items.map(triage);
    rows.sort(
      (a, b) =>
        b.share.createdAt.localeCompare(a.share.createdAt) ||
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        a.share.id.localeCompare(b.share.id),
    );
    return rows;
  }, [items]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling. The counts ARE facts here: `/api/my/shares` returns the caller's
  // whole set in one response, with no cursor and no window (kit FilterChipRow
  // contract — a count is a fact from the backend, never a windowed guess).
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: loadState.kind === "ready" ? sorted.filter(v.match).length : undefined,
  }));

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "shares",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const now = new Date();

  // The §4.4 ACTIVITY-FEED budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 2 below 768, 1 never.
  // Created, Agent, What and Next step survive every width.
  const columns: Column<Row>[] = [
    {
      id: "created",
      header: "Created",
      priority: 1,
      className: "w-[6.5rem]",
      cell: ({ share: s }) => (
        // Relative time is sanctioned in feeds (§4.5) — always with the
        // absolute in `title`, never instead of it.
        <span
          className="whitespace-nowrap font-mono text-xs tabular-nums"
          title={formatDateTime(s.createdAt)}
        >
          {formatRelativeTime(s.createdAt)}
        </span>
      ),
    },
    {
      id: "agent",
      header: "Agent",
      priority: 1,
      // The cap is what makes `truncate` bite, and it is the column's FLOOR as
      // well as its ceiling: a `white-space: nowrap` cell contributes its whole
      // text as min-content, clamped by this max-width — so the cap is exactly
      // how wide this column will be. It steps with the viewport so the four
      // columns that may never be dropped (§4.4) all stay on screen at 768,
      // 1024, 1280 and 1440 without the frame having to scroll to reach them.
      className:
        "max-w-[7rem] lg:max-w-[8rem] xl:max-w-[11rem] min-[1440px]:max-w-[15rem]",
      cell: ({ share: s }) =>
        s.agent ? (
          // The agent is snapshotted at mint (V16) so the caller recognises which
          // run a link points at without opening it.
          <CellEntity title={s.agent} name={s.agent} namespace={s.namespace || undefined} />
        ) : (
          <UnknownValue title={AGENT_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "what",
      header: "What",
      priority: 1,
      className: "max-w-[22rem]",
      cell: (row) => {
        const s = row.share;
        return (
          <div className="min-w-0">
            {/* Link to the per-run detail page (/runs/:id, m112.4) — keyed by the
                real run.ID, which is exactly what a share's runId is
                (shared_runs.run_id is always run.ID, never the traceId, so this
                must NOT point at /traces/:id). Ids middle-truncate: the tail is
                what disambiguates two ids that share a prefix (§4.5). */}
            <Link
              to={`/runs/${encodeURIComponent(s.runId)}`}
              onClick={(e) => e.stopPropagation()}
              className="whitespace-nowrap border-b border-accent font-mono text-sm text-primary hover:border-primary"
              title={s.runId}
              data-testid={`run-link-${s.id}`}
            >
              {truncateId(s.runId)}
            </Link>
            <div
              className="truncate text-xs text-faint"
              title={
                s.includeContent
                  ? "Anyone holding this link can read the whole conversation."
                  : "Anyone holding this link sees only the run's shape — no message content."
              }
            >
              {exposure(s)}
            </div>
            {/* §4.4: below 768 the State column folds into the What line as a
                tag. Exactly one of the two copies is ever displayed, so the
                accessibility tree never carries the state twice. */}
            <div className="mt-1 md:hidden">
              <StateTag status={s.status} />
            </div>
          </div>
        );
      },
    },
    {
      id: "expires",
      header: "Expires",
      priority: 3,
      numeric: true,
      cell: ({ share: s }) => {
        const shown = compactDate(s.expiresAt, now);
        if (!shown) return <UnknownValue title={EXPIRY_UNKNOWN_TITLE} />;
        return <span title={formatDateTime(s.expiresAt)}>{shown}</span>;
      },
    },
    {
      id: "opened",
      header: "Opened",
      priority: 3,
      numeric: true,
      // Never a zero. The views are recorded (as `share.view` rows in the audit
      // trail) but not projected onto this endpoint — see the QuietNote above
      // the table. A fabricated 0 here is the one number that would talk
      // somebody out of revoking a link that is being used.
      cell: () => <UnknownValue title={OPENED_UNKNOWN_TITLE} />,
    },
    {
      id: "state",
      header: "State",
      priority: 2,
      className: "w-[6.5rem]",
      cell: ({ share: s }) => <StateTag status={s.status} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[12rem]",
      cell: (row) => (
        <NextStepLink
          label={row.next.label}
          to={row.next.to}
          tone={row.next.tone}
          ariaLabel={
            row.next.label
              ? `${row.next.label} — the run behind link ${truncateId(row.share.id)}`
              : undefined
          }
          testId={`next-step-${row.share.id}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED list, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching someone with nine links what a share link is.
  const chipEmptied = sorted.length > 0 && visible.length === 0;
  const empty: EmptyStateProps = chipEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: sorted.length,
        countNoun: "links",
      }
    : {
        icon: Share2,
        title: "You haven't shared any runs",
        description:
          "A share link makes one run readable by anyone holding it, with or without the transcript. Links are created from a run's trace view; every one you make appears here — live, revoked, and expired — and can be revoked from this page.",
      };

  const closing = closingLine(sorted);
  const showChips = loadState.kind === "ready" && sorted.length > 0;
  const showOpenedNote = loadState.kind === "ready" && sorted.length > 0;
  const metaLine =
    loadState.kind === "ready"
      ? `${items.length} link${items.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="my-shares-page">
      <PageHeader title="My shares" meta={metaLine} lede={LEDE} />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter shares"
          className="min-w-0"
        />
      )}

      {showOpenedNote && (
        <QuietNote title={OPENED_NOTE_TITLE}>
          This list reads the share registry, which records what each link{" "}
          <em>is</em> — which run, what it exposes, when it dies — not what has
          been done with it. Every view IS recorded, as an audit-trail entry, but
          it is not projected here, so the Opened column reads{" "}
          <span className="font-mono">—</span>. Nothing is estimated: a link with
          no reported views has not been proven unused.
        </QuietNote>
      )}

      <DataTable<Row>
        columns={columns}
        rows={visible}
        rowKey={(row) => row.share.id}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="My Shares"
        rowActions={(row) => (
          <RowActions
            share={row.share}
            busy={revoking.has(row.share.id)}
            onRevoke={handleRevoke}
          />
        )}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}
    </div>
  );
}
