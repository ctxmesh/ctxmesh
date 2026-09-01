import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Bell, Filter, Plus } from "lucide-react";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  DetailDrawer,
  FilterChipRow,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  UnknownValue,
  nextStepRank,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type KeyValueItem,
  type NextStepTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NewAlertPolicyDialog } from "@/components/dashboard/new-alert-policy-dialog";
import { useNamespace } from "@/lib/namespace";
import { formatDateTime, formatRelativeTime } from "@/lib/format";
import { api, ApiError, type AlertSummary } from "@/lib/api";

// AlertsPage — the fired-alert feed, on the editorial ACTIVITY-FEED archetype
// (M70, ADR 0063 D2; M151 §6.1 A1 composition, §4.4 activity-feed budget:
// Time(1) · Agent(1) · What(1) · Duration(3) · Value(3) · State(2) · Next step(1)).
//
// ── A FEED IS CHRONOLOGICAL, NOT TRIAGED ────────────────────────────────────
// The backend returns newest-first and this page keeps it: an alert feed is a
// record of what fired, when. `nextStepRank` breaks a timestamp tie so "Nothing
// needed" still sinks, and the attention view — everything still firing — is
// one chip away.
//
// ── THE COST SLOT CARRIES THE ALERT'S VALUE (documented deviation, §4.4) ────
// The archetype's second numeric column is "Cost". An alert has no cost at all:
// it is a condition, not a run. What it DOES carry is the measured value that
// tripped it (`82%`, `$1,204.10`, `1,842`) — a machine-owned quantity that
// belongs in exactly that mono tabular register. So the slot renders "Value".
// A column of dashes labelled Cost would be worse than honest: the dash means
// "unknown", and an alert's cost is not unknown, it does not exist.
//
// The Duration slot IS real here: how long the condition has been true —
// firedAt→resolvedAt for a cleared alert, firedAt→now for one still firing.
//
// ── COLOUR (§2.2), AND WHY "FIRING" IS NOT ALWAYS CRIT ──────────────────────
// crit means "it will not proceed without a change"; warn means "a bound is
// near or crossed, or degraded but serving". A hard budget that is rejecting
// runs is the first; a soft budget at 82%, a latency objective missed, a score
// that slipped, are the second. Both are firing, and the WORD is the same — the
// hue says which kind of firing it is, read from the words the backend sent.
//
// Backend: GET /api/alerts?namespace=&limit=
//   • namespace comes from the GLOBAL namespace selector.
//   • The feed is newest-first, limit-bounded (server-side). No keyset cursor for
//     now (the alertstore.List contract is a simple bounded slice), so the page
//     can never prove it holds the whole feed — the chips therefore carry NO
//     counts, and the closing line says "the newest N" out loud when the window
//     is full.
//
// 501-calm / 403-forbidden / 5xx-error discipline (mirrors audit-page):
//   • 501 (alert store not configured): listAlerts() returns null → a calm
//     QuietNote saying the capability is absent. NEVER an error, never a toast.
//   • 403 (caller lacks `list alertpolicies`): ApiError.isForbidden →
//     the DataTable's forbidden variant, never a fake empty list.
//   • 500 + other non-2xx: a visible, retryable error state.
//
// The page is READ-only for alerts themselves. The controller auto-resolves an
// alert on a condition's true→false transition; a manual ack write path is a
// follow-up task. The one write affordance is creating a POLICY.
//
// data-testid contract:
//   alerts-page             — root container
//   alerts-unavailable      — the 501 "not configured" QuietNote
//   alerts-detail-drawer    — the row's full record (row-click)
//   new-alert-policy-open   — the header's create action
//   next-step-{id}          — the row's Next step cell

const PAGE_LIMIT = 50;

/**
 * The words a firing alert uses when work is being REFUSED rather than merely
 * bounded (§2.2). Read from what the backend actually sent — the alert `type`
 * is an open vocabulary written by AlertPolicy authors, so a hard-coded list
 * would silently mis-colour every condition someone adds later.
 */
const REFUSING = /(hard|reject|refus|block|denied|error|fail)/i;

/** Conditions whose subject is money — their next step is the spend view. */
const SPEND = /(budget|spend|cost|forecast)/i;

const VALUE_UNKNOWN_TITLE =
  "No measured value was recorded with this alert — unknown, not zero.";

const SPAN_UNKNOWN_TITLE =
  "This alert carries no usable fired-at time, so how long it has been true is unknown — not zero.";

const AGENT_SCOPE_TITLE =
  "This alert is scoped to the workspace, not to one agent — every agent in it is covered.";

/**
 * A span, compact (§4.5): `45s`, `12m`, `3h 20m`, `2d`. Never wraps (the
 * `numeric` column register supplies `whitespace-nowrap`).
 */
export function formatSpan(ms: number): string {
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return mins % 60 === 0 ? `${hours}h` : `${hours}h ${mins % 60}m`;
  const days = Math.floor(hours / 24);
  return hours % 24 === 0 ? `${days}d` : `${days}d ${hours % 24}h`;
}

/** How long the condition has been true, in ms. `null` when it cannot be read. */
function firingSpanMs(a: AlertSummary, now: number): number | null {
  const started = Date.parse(a.firedAt);
  if (Number.isNaN(started)) return null;
  const ended = a.resolvedAt ? Date.parse(a.resolvedAt) : now;
  if (Number.isNaN(ended)) return null;
  const ms = ended - started;
  return ms < 0 ? null : ms;
}

/** `ns/name` split — the wire packs the agent as one string, the cell needs two. */
function agentParts(a: AlertSummary): { ns: string; name: string } | null {
  if (!a.agent) return null;
  const cut = a.agent.indexOf("/");
  return cut > 0
    ? { ns: a.agent.slice(0, cut), name: a.agent.slice(cut + 1) }
    : { ns: a.namespace, name: a.agent };
}

// ── Triage: what the page renders and sorts by, decided once ────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
  to?: string;
  onClick?: () => void;
}

interface Row {
  alert: AlertSummary;
  /** True when the words say work is being refused, not merely bounded (§2.2). */
  refusing: boolean;
  next: NextStep;
}

/**
 * One alert → (severity reading, next step). A RESOLVED alert asks nothing of
 * anyone: the condition went false again and the controller cleared it, so
 * inventing an errand for it would make the column noise. A FIRING one always
 * asks something, and where it sends you depends on what tripped.
 */
function triage(a: AlertSummary, review: (a: AlertSummary) => void): Row {
  const words = `${a.type} ${a.condition} ${a.message ?? ""}`;
  const refusing = REFUSING.test(words);
  if (!a.firing) return { alert: a, refusing, next: { tone: "none" } };

  // Crit is the one action hue allowed on a link (§2.3), and only when the
  // target genuinely is a refusal or a stop.
  const tone: NextStepTone = refusing ? "crit" : "default";
  const parts = agentParts(a);
  if (parts) {
    return {
      alert: a,
      refusing,
      next: {
        label: "Open the agent",
        tone,
        to: `/agents/${encodeURIComponent(parts.ns)}/${encodeURIComponent(parts.name)}`,
      },
    };
  }
  if (SPEND.test(words)) {
    return { alert: a, refusing, next: { label: "Open the cost view", tone, to: "/cost" } };
  }
  return { alert: a, refusing, next: { label: "Review the alert", tone, onClick: () => review(a) } };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "resolved" | "all";

const VIEWS: { id: ViewId; label: string; match: (r: Row) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (r) => r.next.tone !== "none" },
  { id: "resolved", label: "Resolved", match: (r) => !r.alert.firing },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing is firing",
    description:
      "Every alert in this window has cleared on its own. Show everything to read the history.",
  },
  resolved: {
    title: "Nothing has cleared yet",
    description:
      "Every alert in this window is still firing. Show everything to see them.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are only the newest slice.
 */
export function closingLine(rows: Row[], windowed: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const firing = rows.filter((r) => r.alert.firing).length;
  const cleared = total - firing;
  const caveat = windowed
    ? ` These are the newest ${PAGE_LIMIT} — older alerts are not in this window.`
    : "";

  if (total === 1) {
    return firing === 1
      ? `The one alert here is still firing.${caveat}`
      : `The one alert here has cleared.${caveat}`;
  }
  if (firing === 0) {
    return `All ${total} alerts have cleared. Nothing is firing right now.${caveat}`;
  }
  if (cleared === 0) {
    return `Every one of the ${total} alerts is still firing.${caveat}`;
  }
  // Counted in WORDS at one, so the sentence never reads "the other 1 have".
  const clearedClause =
    cleared === 1 ? "the other one has cleared" : `the other ${cleared} have cleared`;
  return `${firing} of the ${total} alerts ${
    firing === 1 ? "is" : "are"
  } still firing; ${clearedClause}.${caveat}`;
}

/**
 * The alert's state as a tag: uppercase mono on its own tint, never interactive
 * (§2.1's form rule). "Firing" is warn or crit depending on what tripped — see
 * the colour note in the file header — and a cleared alert is ok.
 */
function StateTag({ row }: { row: Row }) {
  if (!row.alert.firing) return <Badge variant="ok">Resolved</Badge>;
  return <Badge variant={row.refusing ? "crit" : "warn"}>Firing</Badge>;
}

/** The drawer's fact register — every field, including the ones the table drops. */
function recordItems(a: AlertSummary): KeyValueItem[] {
  return [
    { key: "Policy", value: a.policy },
    { key: "Condition", value: a.condition },
    { key: "Type", value: a.type },
    { key: "Value", value: a.value, absent: "not recorded", title: VALUE_UNKNOWN_TITLE },
    { key: "Agent", value: a.agent, absent: "every agent", title: AGENT_SCOPE_TITLE },
    { key: "Namespace", value: a.namespace, absent: "not namespaced" },
    { key: "Fired", value: formatDateTime(a.firedAt), title: a.firedAt },
    {
      key: "Resolved",
      value: a.resolvedAt ? formatDateTime(a.resolvedAt) : undefined,
      absent: "still firing",
      title: "This condition has not gone false again — the controller clears it when it does.",
    },
    { key: "Alert id", value: String(a.id) },
    { key: "Message", value: a.message, absent: "none recorded", mono: false },
  ];
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: AlertSummary[] }
  | { kind: "unavailable" } // 501 — alert store not configured
  | { kind: "error"; message: string; forbidden: boolean };

const LEDE =
  "Conditions your AlertPolicy rules watched and saw cross, newest first. An alert still firing carries what to do about it; a cleared one is history.";

export function AlertsPage() {
  const { namespace } = useNamespace();
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const [view, setView] = useState<ViewId>("all");
  const [newPolicyOpen, setNewPolicyOpen] = useState(false);
  const [drawerAlert, setDrawerAlert] = useState<AlertSummary | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    api
      .listAlerts(
        {
          limit: PAGE_LIMIT,
          ...(namespace ? { namespace } : {}),
        },
        controller.signal,
      )
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (alert store not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        setLoadState({ kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [namespace]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const items = useMemo(
    () => (loadState.kind === "ready" ? loadState.items : []),
    [loadState],
  );

  // A full window means the backend had at least this many — older alerts exist
  // that this page has not been given, and it says so rather than implying the
  // list is the whole history.
  const windowed = items.length >= PAGE_LIMIT;

  // Newest first — a feed is chronological. `nextStepRank` breaks a timestamp
  // tie so "Nothing needed" still sinks; the id breaks that, so the order is
  // stable across refetches.
  const sorted = useMemo(() => {
    const rows = items.map((a) => triage(a, setDrawerAlert));
    rows.sort(
      (x, y) =>
        y.alert.firedAt.localeCompare(x.alert.firedAt) ||
        nextStepRank(x.next.tone) - nextStepRank(y.next.tone) ||
        y.alert.id - x.alert.id,
    );
    return rows;
  }, [items]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling. NO counts: `/api/alerts` is a bounded slice with no cursor, so a
  // client-side count of the rows in hand would look like a total (kit
  // FilterChipRow contract).
  const chips: FilterChip[] = VIEWS.map((v) => ({ id: v.id, label: v.label }));

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "alerts",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // `now` is read once per render so every row in one paint measures its span
  // against the same instant — a per-cell Date.now() would let two rows
  // disagree about what "now" is.
  const now = Date.now();

  // The §4.4 ACTIVITY-FEED budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 2 below 768, 1 never.
  // Fired, Agent, What and Next step survive every width.
  const columns: Column<Row>[] = [
    {
      id: "fired",
      header: "Fired",
      priority: 1,
      className: "w-[6.5rem]",
      cell: ({ alert: a }) => (
        // Relative time is sanctioned in feeds (§4.5) — always with the
        // absolute in `title`, never instead of it.
        <span
          className="whitespace-nowrap font-mono text-xs tabular-nums"
          title={formatDateTime(a.firedAt)}
        >
          {formatRelativeTime(a.firedAt)}
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
        "max-w-[9rem] lg:max-w-[11rem] xl:max-w-[14rem] min-[1440px]:max-w-[15rem]",
      cell: ({ alert: a }) => {
        const parts = agentParts(a);
        if (!parts) {
          return (
            <CellEntity
              name={
                <span className="text-faint" title={AGENT_SCOPE_TITLE}>
                  Every agent
                </span>
              }
              namespace={a.namespace || undefined}
            />
          );
        }
        return (
          <CellEntity
            title={a.agent}
            name={
              <Link
                to={`/agents/${encodeURIComponent(parts.ns)}/${encodeURIComponent(parts.name)}`}
                data-testid={`alert-agent-link-${a.id}`}
                onClick={(e) => e.stopPropagation()}
                className="border-b border-accent text-primary hover:border-primary"
              >
                {parts.name}
              </Link>
            }
            namespace={parts.ns}
          />
        );
      },
    },
    {
      id: "what",
      header: "What",
      priority: 1,
      className: "max-w-[26rem]",
      cell: (row) => {
        const a = row.alert;
        return (
          <div className="min-w-0">
            <div className="truncate text-sm font-semibold" title={a.policy}>
              {a.policy}
            </div>
            {/* Prose wraps to two lines in a cell, with the whole sentence in
                `title` (§4.5). A condition with no message is a machine string
                and stays in the mono face. */}
            <div
              className="line-clamp-2 text-xs text-faint"
              title={a.message || a.condition}
            >
              {a.message ? a.message : <span className="font-mono">{a.condition}</span>}
            </div>
            {/* §4.4: below 768 the State column folds into the What line as a
                tag. Exactly one of the two copies is ever displayed, so the
                accessibility tree never carries the state twice. */}
            <div className="mt-1 md:hidden">
              <StateTag row={row} />
            </div>
          </div>
        );
      },
    },
    {
      id: "duration",
      header: "Duration",
      priority: 3,
      numeric: true,
      cell: ({ alert: a }) => {
        const ms = firingSpanMs(a, now);
        if (ms === null) return <UnknownValue title={SPAN_UNKNOWN_TITLE} />;
        return (
          <span
            title={
              a.resolvedAt
                ? `Fired ${formatDateTime(a.firedAt)}, resolved ${formatDateTime(a.resolvedAt)}`
                : `Firing since ${formatDateTime(a.firedAt)}`
            }
          >
            <QuantityValue value={ms} format={formatSpan} />
          </span>
        );
      },
    },
    {
      id: "value",
      header: "Value",
      priority: 3,
      numeric: true,
      // The measured figure that tripped the condition. It arrives as a string
      // ("82%", "$1,204.10", "1,842") already carrying its unit, so it is shown
      // verbatim in the mono tabular register — never re-parsed into a number
      // the backend did not send.
      cell: ({ alert: a }) =>
        a.value ? (
          <span title={`${a.condition} — ${a.value}`}>{a.value}</span>
        ) : (
          <UnknownValue title={VALUE_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "state",
      header: "State",
      priority: 2,
      className: "w-[6.5rem]",
      cell: (row) => <StateTag row={row} />,
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[11rem]",
      cell: (row) => (
        <NextStepLink
          label={row.next.label}
          to={row.next.to}
          onClick={row.next.onClick}
          tone={row.next.tone}
          ariaLabel={row.next.label ? `${row.next.label} — ${row.alert.policy}` : undefined}
          testId={`next-step-${row.alert.id}`}
        />
      ),
    },
  ];

  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching an operator with eight alerts what an alert is.
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
        totalCount: windowed ? undefined : sorted.length,
        countNoun: "alerts",
      }
    : {
        icon: Bell,
        title: "No alerts",
        description: namespace
          ? `No condition has crossed in ${namespace} yet. An alert is an AlertPolicy rule the controller saw come true — define one, and the moment it fires it appears here.`
          : "No condition has crossed yet. An alert is an AlertPolicy rule the controller saw come true — define one, and the moment it fires it appears here.",
        action: {
          label: "New alert policy",
          icon: Plus,
          onClick: () => setNewPolicyOpen(true),
        },
      };

  // 501 — the alert store is not configured (no control-plane DSN). This is the
  // calm backend-cannot-answer state (§7.1), NOT an error: nothing broke, the
  // platform is simply not wired to answer.
  if (loadState.kind === "unavailable") {
    return (
      <div className="min-w-0 space-y-6" data-testid="alerts-page">
        <PageHeader title="Alerts" lede={LEDE} />
        <div data-testid="alerts-unavailable">
          <QuietNote title="Alerting isn’t configured on this install.">
            Fired alerts are written to the control-plane database, and this
            install has none wired up — so no condition can be recorded here, and
            an empty feed would wrongly read as “nothing has ever fired”.
            Configuring it needs a control-plane store, the same one the rest of
            the platform uses. Nothing here is estimated; the record is simply
            absent.
          </QuietNote>
        </div>
        <NewAlertPolicyDialog
          open={newPolicyOpen}
          onClose={() => setNewPolicyOpen(false)}
          namespace={namespace}
        />
      </div>
    );
  }

  const closing = closingLine(sorted, windowed);
  const showChips = loadState.kind !== "error";
  const metaLine =
    loadState.kind === "ready"
      ? windowed
        ? `newest ${items.length}`
        : `${items.length} alert${items.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="alerts-page">
      <PageHeader
        title="Alerts"
        meta={metaLine}
        lede={LEDE}
        // The create action goes through `actionsSlot` rather than the
        // structured `actions` list: `PageHeaderAction` carries no `testId`, and
        // the suite asserts on `new-alert-policy-open`.
        actionsSlot={
          <Button
            size="sm"
            className="text-sm"
            onClick={() => setNewPolicyOpen(true)}
            data-testid="new-alert-policy-open"
          >
            <Plus className="h-4 w-4" />
            New alert policy
          </Button>
        }
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter alerts"
          className="min-w-0"
        />
      )}

      <DataTable<Row>
        columns={columns}
        rows={visible}
        rowKey={(row) => String(row.alert.id)}
        loading={loadState.kind === "loading"}
        error={error}
        ariaLabel="Fired alerts"
        // Row-click → the full record, where every column this table drops at a
        // narrow width still renders (§4.4 "dropped ≠ lost").
        onRowClick={(row) => setDrawerAlert(row.alert)}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      <DetailDrawer
        open={drawerAlert !== null}
        onClose={() => setDrawerAlert(null)}
        title={drawerAlert?.policy ?? ""}
        subtitle={
          drawerAlert
            ? `${drawerAlert.condition} · fired ${formatDateTime(drawerAlert.firedAt)}`
            : undefined
        }
        status={
          drawerAlert
            ? <StateTag row={triage(drawerAlert, setDrawerAlert)} />
            : null
        }
        size="md"
      >
        {drawerAlert && (
          <div data-testid="alerts-detail-drawer" className="space-y-5">
            {drawerAlert.message && (
              <p className="font-serif text-md italic">“{drawerAlert.message}”</p>
            )}
            <KeyValueList items={recordItems(drawerAlert)} />
          </div>
        )}
      </DetailDrawer>

      <NewAlertPolicyDialog
        open={newPolicyOpen}
        onClose={() => setNewPolicyOpen(false)}
        namespace={namespace}
      />
    </div>
  );
}
