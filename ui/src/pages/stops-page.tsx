import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";

import {
  ClosingNote,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  UnknownValue,
  UNKNOWN,
  isKnown,
  LIFT_CONTRACT,
  STOP_HELD_EXPLAINER,
  STOP_LIMIT,
  STOP_NOTICE_CONTRACT,
  type Column,
  type KeyValueItem,
  type Quantity,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { FieldError } from "@/components/ui/label";
import {
  api,
  ApiError,
  type ActiveStop,
  type AuditEvent,
  type StopLevel,
  type StopScopeRequest,
} from "@/lib/api";
import { formatDateTime, formatRelativeTime } from "@/lib/format";

// StopsPage — the Stops surface (M151 §6.2 gap 2, ADR 0126 "The scoped kill switch").
//
// The frame carries a StopControl on every page; this is where the stops it creates LAND. An
// operator opens it during an incident, and it has to answer five questions before they read a
// sentence: what is stopped, how much is held because of it, why, who did it and when, and how do
// I lift it. Archetype A1 (PageHeader → sections → DataTable → ClosingNote), §4.4 column budget.
//
// ── What the backend actually answers, and what it does not ──────────────────
//
// `GET /api/kills` (internal/bff/kill_handler.go, `activeKill`) returns exactly seven fields:
// scope, level, namespace, agent, tenant, reason, principal. It reports NO impact counts and NO
// timestamp. That absence is the page's central design problem, because the tempting fix — render
// the counts as `0` and the time as "just now" — is a claim the platform never made. §7.1: zero and
// unknown must never share a glyph. So every impact count on this page is UNKNOWN, drawn as the
// readable `—` (`text-faint`, with a title), and one QuietNote above the table says why once.
//
// The time a stop began IS recoverable, from a different backend: the kill handler writes an
// `audit_log` row (`killswitch.kill`, ResourceName = the scope key) on every outcome. So the page
// reads the audit trail too, and joins it by scope key. When the audit store is absent (501) or the
// caller lacks the operator persona (403), "when" stays honestly unknown and the trail section says
// which of the two happened — it never degrades into an empty list that implies nothing happened.
//
// ── The stop list is CLUSTER-WIDE, deliberately ──────────────────────────────
//
// The frame's nav count is filtered to the selected workspace (`stopsReaching` in app-shell). This
// page is not: during an incident the fleet stop that is holding another team's work is exactly the
// thing you must see, and a workspace filter would hide it. The lede says so out loud, because a
// page that silently disagrees with the number in the sidebar is worse than either alone.
//
// data-testid contract:
//   stops-page        — root
//   stops-active      — the in-force DataTable (aria-label "Active stops")
//   stops-trail       — the lifted-stops DataTable (aria-label "Recently lifted stops")
//   stops-lift-<key>  — the row's "Lift the stop →" next step

// ── The blast-radius vocabulary ──────────────────────────────────────────────
//
// The wire levels are the backend's (killscope.Level). The words beside them are this page's, and
// they exist because the kit's `StopScopeKind` — agent | team | workspace | fleet — cannot express
// `tenant` at all, so its `SCOPE_REACH` copy is not reusable here without lying about one of the
// four levels a stop can actually have.

/** Widest blast radius first. This is the page's default sort: what holds the most, on top. */
const LEVEL_RANK: Record<StopLevel, number> = {
  fleet: 0,
  tenant: 1,
  namespace: 2,
  agent: 3,
};

/** The tag word. Uppercased by the Badge recipe, so these are written sentence-case. */
const LEVEL_WORD: Record<StopLevel, string> = {
  fleet: "Fleet",
  tenant: "Tenant",
  namespace: "Workspace",
  agent: "Agent",
};

/** What the scope covers, in an operator's words — stated wherever a consequence is being weighed. */
const LEVEL_REACH: Record<StopLevel, string> = {
  fleet: "every agent in every workspace, cluster-wide",
  tenant: "every agent in every workspace this tenant owns",
  namespace: "every agent in this workspace",
  agent: "every run of this one agent",
};

/**
 * The impact counts, said once, above the table.
 *
 * This is the §7 A1 "backend-cannot-answer" pattern verbatim: the columns render `—` and ONE note
 * above the table explains it. Repeating the explanation per row would turn an honest absence into
 * noise, and dropping it entirely would leave three columns of dashes looking like a bug.
 */
const IMPACT_NOTE_TITLE = "How much each stop is holding isn't reported.";

/** The `title` on every unreported count. It must say unknown, and it must say not-zero. */
const IMPACT_UNKNOWN_TITLE =
  "GET /api/kills does not report this count. It is unknown — not zero.";

const WHEN_UNKNOWN_TITLE =
  "The audit trail is where a stop's start time lives. It isn't readable here, so this is unknown — not never.";

/**
 * The backend's marker for a stop it could not attribute to a principal (`killPrincipal` falls back
 * to this rather than to an empty string). It is a real recorded value meaning "nobody was
 * resolved", so it is shown — but in the register of an absence, not of a username.
 */
const UNATTRIBUTED = "unattributed";

// ── Small honest cells ───────────────────────────────────────────────────────

/**
 * One count, in the right register.
 *
 * `QuantityValue` draws its unknown branch in `text-ghost`, which the system reserves for
 * decoration — but an unreported blast radius is meta that MUST be read, so the unknown branch goes
 * through `UnknownValue` (`text-faint`) instead. Known numbers keep QuantityValue's mono tabular
 * register. This is a two-line choice between two kit primitives, not a third way of saying it.
 */
function Count({ value }: { value: Quantity }) {
  if (!isKnown(value)) return <UnknownValue title={IMPACT_UNKNOWN_TITLE} />;
  return <QuantityValue value={value} />;
}

/** The row's impact line: `12 held · 6 refusing · 1 in flight`, or a single stated absence. */
function ImpactLine({ stop }: { stop: StopRow }) {
  const terms: Array<{ n: number; words: string }> = [];
  if (isKnown(stop.runsHeld)) terms.push({ n: stop.runsHeld, words: "held" });
  if (isKnown(stop.agentsRefusing))
    terms.push({ n: stop.agentsRefusing, words: "refusing" });
  if (isKnown(stop.runsInFlight))
    terms.push({ n: stop.runsInFlight, words: "in flight" });

  if (terms.length === 0) {
    return (
      <span className="whitespace-nowrap text-sm">
        <UnknownValue title={IMPACT_UNKNOWN_TITLE} />{" "}
        <span className="text-faint">not reported</span>
      </span>
    );
  }
  return (
    <span className="whitespace-nowrap text-sm">
      {terms.map((t, i) => (
        <span key={t.words}>
          {i > 0 ? <span className="px-1.5 text-ghost">·</span> : null}
          <Count value={t.n} />{" "}
          <span className="text-secondary-foreground">{t.words}</span>
        </span>
      ))}
    </span>
  );
}

// ── The row model ────────────────────────────────────────────────────────────

/**
 * One active stop as this page reads it.
 *
 * The impact fields are `Quantity`, never `number`, so the compiler refuses to let an unreported
 * count reach a number formatter. Today `toRow` sets all three to UNKNOWN because the BFF does not
 * send them; when it starts to, this adapter is the single place that changes and every consumer
 * below already renders the known branch correctly.
 */
interface StopRow {
  /** The control-plane key (`fleet`, `namespace:team-b`, `agent:team-b:ingest`) — the row's id. */
  key: string;
  level: StopLevel;
  /** The stopped thing, qualified: `team-b`, `team-b/ingest-coordinator`, `everything`. */
  name: string;
  reason: string;
  principal: string;
  /** RFC3339, joined from the audit trail. Absent ⇒ unknown, never "just now". */
  startedAt?: string;
  agentsRefusing: Quantity;
  runsHeld: Quantity;
  runsInFlight: Quantity;
  /** The body that lifts exactly this scope. Built from the wire fields, never re-parsed. */
  request: StopScopeRequest;
}

function stopName(s: ActiveStop): string {
  switch (s.level) {
    case "agent":
      return `${s.namespace ?? "?"}/${s.agent ?? "?"}`;
    case "namespace":
      return s.namespace || s.scope;
    case "tenant":
      return s.tenant || s.scope;
    default:
      return "everything";
  }
}

function toRow(s: ActiveStop, startedAt?: string): StopRow {
  return {
    key: s.scope,
    level: s.level,
    name: stopName(s),
    reason: s.reason,
    principal: s.principal,
    startedAt,
    // Not sent by GET /api/kills. UNKNOWN, not 0 — see the header note.
    agentsRefusing: UNKNOWN,
    runsHeld: UNKNOWN,
    runsInFlight: UNKNOWN,
    request: {
      level: s.level,
      ...(s.namespace ? { namespace: s.namespace } : {}),
      ...(s.agent ? { agent: s.agent } : {}),
      ...(s.tenant ? { tenant: s.tenant } : {}),
    },
  };
}

/** Widest first; ties broken by the scope key so the order is stable across polls. */
function byBlastRadius(a: StopRow, b: StopRow): number {
  const d = LEVEL_RANK[a.level] - LEVEL_RANK[b.level];
  return d !== 0 ? d : a.key.localeCompare(b.key);
}

// ── State ───────────────────────────────────────────────────────────────────

type StopsState =
  | { kind: "loading" }
  | { kind: "ready"; stops: ActiveStop[] }
  | { kind: "error"; message: string; forbidden: boolean };

/** The audit-backed history. Its four failure modes are four different truths (§7). */
type TrailState =
  | { kind: "loading" }
  | { kind: "ready"; events: AuditEvent[] }
  | { kind: "unavailable" } // 501 — no audit store on this install
  | { kind: "forbidden" } // 403 — the caller lacks the auditlogs persona
  | { kind: "error"; message: string };

/** A successful kill/un-kill row for a scope, newest first. */
function killEvents(events: AuditEvent[], action: string): AuditEvent[] {
  return events
    .filter((e) => e.action === action && e.outcome === "success" && !!e.resourceName)
    .sort((a, b) => b.occurredAt.localeCompare(a.occurredAt));
}

export function StopsPage() {
  const [stopsState, setStopsState] = useState<StopsState>({ kind: "loading" });
  const [trailState, setTrailState] = useState<TrailState>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  /** Set only when the backend says so (501 on a lift) — never guessed from an empty list. */
  const [noKillStore, setNoKillStore] = useState(false);
  const [reviewing, setReviewing] = useState<StopRow | null>(null);
  const [lifting, setLifting] = useState<StopRow | null>(null);
  const [liftBusy, setLiftBusy] = useState(false);
  const [liftError, setLiftError] = useState<string | null>(null);
  const [liftNoop, setLiftNoop] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback((silent = false) => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    if (!silent) {
      setStopsState({ kind: "loading" });
      setTrailState({ kind: "loading" });
    }

    api
      .listStops(controller.signal)
      .then((stops) => {
        if (controller.signal.aborted) return;
        setStopsState({ kind: "ready", stops });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setStopsState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      })
      .finally(() => {
        if (silent) setRefreshing(false);
      });

    // The trail is a SEPARATE backend with its own permissions, so it is fetched and degraded
    // independently: a caller who cannot read the audit log still gets the live stop list.
    api
      .listAudit({ kind: "KillScope", limit: 100 }, controller.signal)
      .then((resp) => {
        if (controller.signal.aborted) return;
        if (resp === null) {
          setTrailState({ kind: "unavailable" });
          return;
        }
        setTrailState({ kind: "ready", events: resp.items ?? [] });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setTrailState({ kind: "forbidden" });
          return;
        }
        setTrailState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  function refresh() {
    setRefreshing(true);
    load(true);
  }

  const events = trailState.kind === "ready" ? trailState.events : [];

  /** scope key → when it was stopped, from the newest successful `killswitch.kill` row. */
  const startedAt = useMemo(() => {
    const map = new Map<string, string>();
    for (const e of killEvents(events, "killswitch.kill")) {
      if (e.resourceName && !map.has(e.resourceName)) map.set(e.resourceName, e.occurredAt);
    }
    return map;
  }, [events]);

  const rows = useMemo(() => {
    if (stopsState.kind !== "ready") return [];
    return stopsState.stops
      .map((s) => toRow(s, startedAt.get(s.scope)))
      .sort(byBlastRadius);
  }, [stopsState, startedAt]);

  /** The lifted trail: successful un-kills, newest first. */
  const lifted = useMemo(() => killEvents(events, "killswitch.unkill"), [events]);

  const activeCount = stopsState.kind === "ready" ? rows.length : undefined;

  function openLift(row: StopRow) {
    setLiftError(null);
    setLiftNoop(null);
    setLifting(row);
  }

  function closeLift() {
    setLifting(null);
    setLiftError(null);
    setLiftNoop(null);
  }

  async function confirmLift() {
    if (!lifting) return;
    setLiftError(null);
    setLiftNoop(null);
    setLiftBusy(true);
    try {
      const res = await api.liftStop(lifting.request);
      if (!res.applied) {
        // Not a failure and not a success: the scope was not stopped, so nothing was released.
        // Saying "lifted" here would be the same lie as printing 0 for an unknown count.
        setLiftNoop(
          "That scope was not stopped, so nothing was lifted. The list below is now up to date.",
        );
        load(true);
        return;
      }
      setLifting(null);
      setReviewing(null);
      load(true);
    } catch (err) {
      if (err instanceof ApiError && err.status === 501) setNoKillStore(true);
      // Never swallowed, and the dialog stays open: the stop is STILL IN FORCE and the operator
      // has to know that before they walk away from the screen.
      setLiftError(
        err instanceof Error
          ? `The stop was not lifted, and it is still in force: ${err.message}`
          : "The stop was not lifted. It is still in force.",
      );
    } finally {
      setLiftBusy(false);
    }
  }

  const columns: Column<StopRow>[] = [
    {
      id: "scope",
      header: "Scope",
      priority: 1,
      className: "min-w-[11rem] max-w-[20rem]",
      // Not CellEntity: its second line is mono, and the second line here is prose (the reach
      // sentence). A machine key set in a prose register — or prose set in mono — is the same
      // register error in either direction.
      cell: (r) => (
        <div className="min-w-0">
          <div className="truncate font-mono text-sm font-semibold" title={r.key}>
            {r.name}
          </div>
          <div className="truncate text-xs text-faint" title={LEVEL_REACH[r.level]}>
            {LEVEL_REACH[r.level]}
          </div>
        </div>
      ),
    },
    {
      id: "radius",
      header: "Blast radius",
      priority: 1,
      className: "w-[6.5rem]",
      // Crit on every row, because every row is a stop — "it will not proceed without a change"
      // (§2.2). The WORD carries the blast radius; the hue is not a severity ladder.
      cell: (r) => <Badge variant="crit">{LEVEL_WORD[r.level]}</Badge>,
    },
    {
      id: "impact",
      header: "Holding",
      priority: 2,
      className: "min-w-[11rem]",
      cell: (r) => <ImpactLine stop={r} />,
    },
    {
      id: "reason",
      header: "Reason",
      priority: 2,
      className: "min-w-[10rem] max-w-[24rem]",
      cell: (r) => (
        <span className="block truncate font-serif text-md italic" title={r.reason}>
          “{r.reason}”
        </span>
      ),
    },
    {
      id: "who",
      header: "Stopped by",
      priority: 3,
      className: "max-w-[9rem]",
      cell: (r) =>
        r.principal === UNATTRIBUTED ? (
          <span
            className="truncate font-mono text-xs text-faint"
            title="The backend could not resolve who recorded this stop. It was recorded as unattributed."
          >
            {UNATTRIBUTED}
          </span>
        ) : (
          <span className="block truncate font-mono text-xs" title={r.principal}>
            {r.principal}
          </span>
        ),
    },
    {
      id: "when",
      header: "Stopped at",
      priority: 4,
      className: "w-[7rem]",
      cell: (r) =>
        r.startedAt ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums"
            title={formatDateTime(r.startedAt)}
          >
            {formatRelativeTime(r.startedAt)}
          </span>
        ) : (
          <UnknownValue title={WHEN_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      className: "w-[9rem]",
      cell: (r) => (
        <NextStepLink
          label="Lift the stop"
          tone="crit"
          onClick={() => openLift(r)}
          ariaLabel={`Lift the stop on ${r.name}`}
          testId={`stops-lift-${r.key}`}
        />
      ),
    },
  ];

  const trailColumns: Column<AuditEvent>[] = [
    {
      id: "scope",
      header: "Scope",
      priority: 1,
      className: "min-w-[11rem] max-w-[22rem]",
      cell: (e) => (
        <span className="block truncate font-mono text-sm" title={e.resourceName}>
          {e.resourceName}
        </span>
      ),
    },
    {
      id: "actor",
      header: "Lifted by",
      priority: 2,
      className: "max-w-[12rem]",
      cell: (e) => (
        <span className="block truncate font-mono text-xs" title={e.actor}>
          {e.actor}
        </span>
      ),
    },
    {
      id: "when",
      header: "Lifted at",
      priority: 1,
      className: "w-[8rem]",
      cell: (e) => (
        <span
          className="whitespace-nowrap font-mono text-xs tabular-nums"
          title={formatDateTime(e.occurredAt)}
        >
          {formatRelativeTime(e.occurredAt)}
        </span>
      ),
    },
  ];

  const listError =
    stopsState.kind === "error"
      ? {
          message: stopsState.message,
          forbidden: stopsState.forbidden,
          resource: "stops",
          onRetry: () => load(),
        }
      : null;

  return (
    <div data-testid="stops-page" className="space-y-8">
      <PageHeader
        title="Stops"
        lede="A stop refuses new runs and holds queued ones at the scope it names. Nothing is discarded. Every stop in force is listed here, cluster-wide — not only the workspace you have selected."
        meta={activeCount === undefined ? undefined : `${activeCount} in force`}
        actions={[
          {
            id: "refresh",
            label: refreshing ? "Refreshing…" : "Refresh",
            icon: RefreshCw,
            variant: "outline",
            onClick: refresh,
            disabled: refreshing,
          },
        ]}
        loading={stopsState.kind === "loading"}
      />

      {noKillStore && (
        <QuietNote title="The kill switch isn't configured on this install.">
          No kill store is wired up, so nothing can be stopped or lifted from here and{" "}
          <span className="font-mono text-xs">GET /api/kills</span> answers with an empty list
          either way. Configuring it needs a control-plane store — the same one the rest of the
          platform uses. Nothing here is estimated; there is simply nothing to record against.
        </QuietNote>
      )}

      <section aria-labelledby="stops-active-head">
        <SectionHeader
          id="stops-active-head"
          title="In force now"
          lede="Widest blast radius first: everything, then a tenant, then a workspace, then a single agent."
        />

        {stopsState.kind === "ready" && rows.length > 0 && (
          <QuietNote className="mb-3" title={IMPACT_NOTE_TITLE}>
            {STOP_NOTICE_CONTRACT} How many agents are refusing and how many runs are held is not
            in what the platform reports about a stop, so the Holding column reads{" "}
            <span className="font-mono">—</span>. Nothing here is estimated — the counts are
            unknown, and unknown is not zero.
          </QuietNote>
        )}

        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(r) => r.key}
          loading={stopsState.kind === "loading"}
          error={listError}
          onRowClick={(r) => setReviewing(r)}
          ariaLabel="Active stops"
          tableClassName="min-w-[48rem]"
          className="[&_table]:w-full"
          empty={{
            title: "Nothing is stopped.",
            description:
              "Every agent is accepting runs and no queued work is being held back. When someone stops a scope, it appears here with its reason, who recorded it, and a way to lift it.",
          }}
        />

        {rows.length > 0 && (
          <ClosingNote>
            {rows.length === 1
              ? "One scope is stopped right now. Lifting it releases everything it holds, at once."
              : `${rows.length} scopes are stopped right now. Lifting one releases everything it holds, at once.`}
          </ClosingNote>
        )}
      </section>

      <section aria-labelledby="stops-trail-head">
        <SectionHeader
          id="stops-trail-head"
          title="Recently lifted"
          lede="Every un-kill is its own authorized, audited act — never a silent expiry. This is that record."
          actions={
            <NextStepLink label="Open the audit trail" to="/audit" />
          }
        />

        {trailState.kind === "unavailable" && (
          <QuietNote title="The audit trail isn't configured on this install.">
            Stops that were already lifted are recorded in the control-plane audit log, and this
            install has none — so there is no history to list, and the time each active stop began
            is not recoverable here either. Nothing is estimated; the record is simply absent.
          </QuietNote>
        )}

        {trailState.kind === "forbidden" && (
          <ForbiddenInline
            title="You don't have permission to read the audit trail."
            resource="the audit trail"
          />
        )}

        {trailState.kind === "error" && (
          <ErrorState
            title="The audit trail didn't load."
            description="The stops above are still current — only their history is missing."
            detail={trailState.message}
            onRetry={() => load()}
          />
        )}

        {(trailState.kind === "loading" || trailState.kind === "ready") && (
          <DataTable
            columns={trailColumns}
            rows={lifted}
            rowKey={(e) => String(e.id)}
            loading={trailState.kind === "loading"}
            ariaLabel="Recently lifted stops"
            tableClassName="min-w-[32rem]"
            empty={{
              title: "No stop has been lifted yet.",
              description:
                "The audit trail is readable and holds no un-kill for any scope. When a stop is lifted, the act is recorded here with who lifted it and when.",
            }}
          />
        )}
      </section>

      {/* The row's full record — §4.4's "dropped ≠ lost": the columns that leave at 1280/1024/768
          all render here, so a narrow viewport never costs the reader a fact. */}
      <DetailDrawer
        open={reviewing !== null}
        onClose={() => setReviewing(null)}
        title={reviewing?.name ?? ""}
        subtitle={reviewing ? LEVEL_REACH[reviewing.level] : undefined}
        status={reviewing ? <Badge variant="crit">{LEVEL_WORD[reviewing.level]}</Badge> : null}
        footer={
          reviewing ? (
            <div className="flex items-center gap-3">
              <Button variant="destructive" onClick={() => openLift(reviewing)}>
                Lift the stop
              </Button>
              <Button variant="ghost" onClick={() => setReviewing(null)}>
                Close
              </Button>
            </div>
          ) : null
        }
      >
        {reviewing && (
          <div className="space-y-5">
            <p className="font-serif text-md italic">
              “{reviewing.reason}”
              <span className="ml-2 font-sans text-xs not-italic text-faint">
                — {reviewing.principal}
              </span>
            </p>
            <KeyValueList items={recordItems(reviewing)} />
            <div>
              <p className="text-sm text-secondary-foreground">{STOP_HELD_EXPLAINER}</p>
              <p className="mt-1 text-xs text-faint">{STOP_LIMIT}</p>
            </div>
          </div>
        )}
      </DetailDrawer>

      <ConfirmDialog
        open={lifting !== null}
        onCancel={closeLift}
        onConfirm={confirmLift}
        busy={liftBusy}
        title={lifting ? `Lift the stop on ${lifting.name}?` : "Lift the stop?"}
        description={LIFT_CONTRACT}
        confirmLabel="Lift the stop"
        // A fleet-wide un-kill restarts every held run in the cluster. It gets the same typed gate
        // the fleet-wide stop has; the frame has no cluster name to offer, so the word is the gate
        // (identical fallback to StopControl's).
        confirmText={lifting?.level === "fleet" ? "everything" : undefined}
        impact={
          lifting ? (
            <div className="space-y-3">
              <div>
                <p className="font-mono text-2xs uppercase tracking-wide text-faint">
                  What starts again
                </p>
                <KeyValueList
                  className="mt-1"
                  items={[
                    { key: "Scope", value: lifting.key },
                    { key: "Reach", value: LEVEL_REACH[lifting.level], mono: false },
                    {
                      key: "Held runs that will start",
                      absent: "not reported",
                      title: IMPACT_UNKNOWN_TITLE,
                      value: isKnown(lifting.runsHeld) ? lifting.runsHeld : undefined,
                    },
                    {
                      key: "Agents accepting runs again",
                      absent: "not reported",
                      title: IMPACT_UNKNOWN_TITLE,
                      value: isKnown(lifting.agentsRefusing)
                        ? lifting.agentsRefusing
                        : undefined,
                    },
                  ]}
                />
              </div>
              {/* The sentence this page exists to say out loud. It is duplicated from
                  kit/stop.tsx's lift dialog, which does not export it; the two must stay
                  identical, and exporting it from the kit is the fix. */}
              <p className="text-sm text-secondary-foreground">
                Held runs start as soon as the stop is lifted — there is no staggered release, so
                expect the backlog to run at once.
              </p>
              <p className="text-xs text-faint">
                Nothing is replayed and nothing was lost: held runs kept their place in the queue
                the whole time.
              </p>
              {liftNoop ? (
                <p className="text-sm text-secondary-foreground">{liftNoop}</p>
              ) : null}
              <FieldError>{liftError}</FieldError>
            </div>
          ) : null
        }
      />
    </div>
  );
}

/** The full record, for the drawer. Every field the columns can drop, plus the machine key. */
function recordItems(r: StopRow): KeyValueItem[] {
  return [
    { key: "Control-plane key", value: r.key },
    { key: "Blast radius", value: LEVEL_WORD[r.level] },
    { key: "Reason", value: r.reason, mono: false },
    {
      key: "Stopped by",
      value: r.principal === UNATTRIBUTED ? undefined : r.principal,
      absent: UNATTRIBUTED,
      title:
        r.principal === UNATTRIBUTED
          ? "The backend could not resolve who recorded this stop."
          : r.principal,
    },
    {
      key: "Stopped at",
      value: r.startedAt ? formatDateTime(r.startedAt) : undefined,
      absent: "not recorded here",
      title: WHEN_UNKNOWN_TITLE,
    },
    {
      key: "Agents refusing new runs",
      value: isKnown(r.agentsRefusing) ? r.agentsRefusing : undefined,
      absent: "not reported",
      title: IMPACT_UNKNOWN_TITLE,
    },
    {
      key: "Queued runs held",
      value: isKnown(r.runsHeld) ? r.runsHeld : undefined,
      absent: "not reported",
      title: IMPACT_UNKNOWN_TITLE,
    },
    {
      key: "Runs in flight",
      value: isKnown(r.runsInFlight) ? r.runsInFlight : undefined,
      absent: "not reported",
      title: IMPACT_UNKNOWN_TITLE,
    },
  ];
}
