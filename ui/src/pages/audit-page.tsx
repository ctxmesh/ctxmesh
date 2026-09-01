import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Filter, ScrollText } from "lucide-react";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  DetailDrawer,
  FilterChipRow,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuietNote,
  SectionHeader,
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
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useNamespace } from "@/lib/namespace";
import { formatDateTime, formatRelativeTime } from "@/lib/format";
import { api, ApiError, type AuditEvent, type AuditListParams } from "@/lib/api";

// AuditPage — the compliance trail, on the editorial ACTIVITY-FEED archetype
// (m63.5, ADR 0056 §4; M151 §6.1 A1 composition, §4.4 activity-feed budget:
// Time(1) · Actor(1) · What(1) · Duration(3) · Cost(3) · State(2) · Next step(1)).
//
// ── A TRAIL IS CHRONOLOGICAL ────────────────────────────────────────────────
// The backend already returns newest-first keyset order and this page keeps it:
// an audit trail read out of time order is not an audit trail. `nextStepRank`
// breaks a timestamp tie so "Nothing needed" still sinks, and the attention
// view — the acts that were refused or failed — is one chip away.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// An audit row records an ACT, not a run. The budget's two numeric columns
// therefore have nothing to read: an act that belongs to a run (it carries a
// traceId) has a duration and a cost, but `GET /api/audit` does not project
// them, and an act that belongs to no run has neither. Both render the readable
// dash — with DIFFERENT reasons in `title`, because "not projected here" and
// "there was no run" are different truths — plus ONE QuietNote above the table.
// Never a zero, never `$0.0000`.
//
// Backend: GET /api/audit?namespace=&actor=&action=&kind=&from=&to=&limit=&cursor=
//   • All filters are SERVER-SIDE (the store filters by exact match); there is NO
//     page-windowed client substring — the audit_log is Postgres-backed, so unlike
//     Runs every filter narrows the whole table, not just the loaded page. The bar
//     sits ABOVE the view chips: the bar changes what the backend sends, the chips
//     change what you are looking at within it.
//   • namespace comes from the GLOBAL namespace selector ("" = cluster-wide, which
//     only the operator persona can read). cursor is the opaque keyset token.
//   • action and kind are SELECT dropdowns (closed vocabularies) — a free-text
//     typo would silently produce zero rows; a dropdown prevents that (m76.5 H1).
//   • from/to are datetime-local inputs (m76.5 H2) matching the Runs filter bar.
//
// 501-calm / 403-forbidden / 502-error discipline (mirrors runs-page):
//   • 501 (audit store not configured — control-plane DSN absent): listAudit()
//     returns null → a calm QuietNote saying the capability is absent. NEVER an
//     error state, never a toast.
//   • 403 (caller lacks the operator `auditlogs` persona): ApiError.isForbidden →
//     the DataTable's forbidden variant, never a fake empty list.
//   • 500 (DB read failed) + other non-2xx: a visible, retryable error state.
//
// RBAC: OPERATOR-ONLY. The nav item is hidden from developer/viewer chrome (gated
// on `list auditlogs`, nav.ts); a non-operator who deep-links gets an honest 403.
// The page is READ-only — the audit trail is append-only, never edited here.
//
// data-testid contract:
//   audit-page          — root container
//   audit-filter-bar    — the server-side filter inputs
//   audit-unavailable   — the 501 "not configured" QuietNote
//   audit-detail-drawer — the row's full record (row-click)
//   audit-trace-link-{id} — the drawer's "View run" link
//   next-step-{id}      — the row's Next step cell

const PAGE_LIMIT = 50;

// AUDIT_ACTIONS — the closed vocabulary for the `action` field. Enumerated directly
// from the Go source that writes each audit row (do NOT add values not present there;
// a non-existent value silently produces zero rows — the exact bug H1 was built to kill):
//   "connect"         — internal/bff/audit_events.go:auditActionConnect
//   "grant.create"    — internal/bff/audit_events.go:auditActionGrantCreate
//   "grant.revoke"    — internal/bff/audit_events.go:auditActionGrantRevoke
//   "share.create"    — internal/bff/shares.go:auditActionShareCreate
//   "share.revoke"    — internal/bff/shares.go:auditActionShareRevoke
//   "guardrail.block" — internal/bff/guardrail_event_handler.go:auditActionGuardrailBlock
//   "create"          — internal/audit/audit.go:VerbCreate (controller CRD mutations)
//   "update"          — internal/audit/audit.go:VerbUpdate (controller CRD mutations)
//   "delete"          — internal/audit/audit.go:VerbDelete (controller CRD mutations)
// NOTE: "connect.denied" was REMOVED — denial is outcome="denied" on action="connect",
// not a separate action string. No audit row ever carries action="connect.denied".
const AUDIT_ACTIONS = [
  "connect",
  "grant.create",
  "grant.revoke",
  "share.create",
  "share.revoke",
  "guardrail.block",
  "create",
  "update",
  "delete",
] as const;

// AUDIT_KINDS — the closed vocabulary for the `resourceKind` field. Enumerated from
// the Go source that writes ResourceKind in each audit row:
//   BFF rows (internal/bff/):
//     "Provider"       — providers.go:resourceKindProvider
//     "MCPGrant"       — mcp_grant_handlers.go:resourceKindMCPGrant
//     "GuardrailPolicy" — guardrail_event_handler.go (literal "GuardrailPolicy")
//     "SharedRun"      — shares.go:auditKindSharedRun
//   Controller rows (internal/audit/auditor.go:auditedTypes — the scheme-resolved Kind):
//     "AgentDeployment", "AgentVersion", "ModelRoute", "SecretBinding",
//     "MCPToolBinding", "MemoryBinding", "AgentRegistry", "AgentScalingPolicy",
//     "EvalSuite"
// NOTE: "PromptVersion"/"ToolRegistry" were retired to Postgres (ADR 0044) and are
// no longer CRDs — they are NOT in auditedTypes() and produce no controller rows.
// "AgentTeam"/"Workflow"/"KnowledgeBase" were REMOVED — not in auditedTypes() and
// have no BFF audit rows.
const AUDIT_KINDS = [
  "Provider",
  "MCPGrant",
  "GuardrailPolicy",
  "SharedRun",
  "AgentDeployment",
  "AgentVersion",
  "ModelRoute",
  "SecretBinding",
  "MCPToolBinding",
  "MemoryBinding",
  "AgentRegistry",
  "AgentScalingPolicy",
  "EvalSuite",
] as const;

// ── The honest-unknown vocabulary (§7.1) ────────────────────────────────────

const MEASURE_NOTE_TITLE = "An audit row records an act, not a run.";

/** The act belongs to a run — the figures exist, this endpoint just doesn't send them. */
const IN_RUN_UNKNOWN_TITLE =
  "This act belongs to a run, but the audit trail does not project the run's figures — unknown, not zero. Open the run to read them.";

/** The act belongs to no run at all — there is no figure to be unaware of. */
const NO_RUN_UNKNOWN_TITLE =
  "This act was not part of a run, so there is nothing to measure — absent, not zero.";

const ACTOR_UNKNOWN_TITLE =
  "This act was recorded without an actor — unattributed, not anonymous.";

// toRFC3339 converts a <input type="datetime-local"> value ("YYYY-MM-DDTHH:MM",
// LOCAL time, no seconds/timezone) into a UTC RFC3339 string the BFF's audit
// filter accepts. Returns "" for empty/unparseable input so the caller omits the
// param rather than sending a malformed one. Mirrors runs-page.tsx (m76.5 H2).
function toRFC3339(dtLocal: string): string {
  if (!dtLocal) return "";
  const d = new Date(dtLocal);
  return Number.isNaN(d.getTime()) ? "" : d.toISOString();
}

// AuditFilterBar holds the server-side filters: actor (free-text, open cardinality),
// action + kind (Select dropdowns — closed vocabularies), and from/to date range.
function AuditFilterBar({
  actor,
  action,
  kind,
  from,
  to,
  onActor,
  onAction,
  onKind,
  onFrom,
  onTo,
}: {
  actor: string;
  action: string;
  kind: string;
  from: string;
  to: string;
  onActor: (v: string) => void;
  onAction: (v: string) => void;
  onKind: (v: string) => void;
  onFrom: (v: string) => void;
  onTo: (v: string) => void;
}) {
  const labelClass =
    "font-mono text-2xs font-medium uppercase tracking-wide text-faint";
  return (
    <div className="flex min-w-0 flex-wrap items-end gap-3" data-testid="audit-filter-bar">
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-actor" className={labelClass}>
          Actor
        </label>
        <Input
          id="audit-filter-actor"
          value={actor}
          onChange={(e) => onActor(e.target.value)}
          placeholder="username"
          className="h-8 w-44 text-sm"
          aria-label="Filter by actor"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-action" className={labelClass}>
          Action
        </label>
        <Select
          id="audit-filter-action"
          value={action}
          onChange={(e) => onAction(e.target.value)}
          className="h-8 w-44 text-sm"
          aria-label="Filter by action"
        >
          <option value="">Any</option>
          {AUDIT_ACTIONS.map((a) => (
            <option key={a} value={a}>{a}</option>
          ))}
        </Select>
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-kind" className={labelClass}>
          Resource kind
        </label>
        <Select
          id="audit-filter-kind"
          value={kind}
          onChange={(e) => onKind(e.target.value)}
          className="h-8 w-44 text-sm"
          aria-label="Filter by resource kind"
        >
          <option value="">Any</option>
          {AUDIT_KINDS.map((k) => (
            <option key={k} value={k}>{k}</option>
          ))}
        </Select>
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-from" className={labelClass}>
          From
        </label>
        <Input
          id="audit-filter-from"
          type="datetime-local"
          value={from}
          onChange={(e) => onFrom(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter from date"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="audit-filter-to" className={labelClass}>
          To
        </label>
        <Input
          id="audit-filter-to"
          type="datetime-local"
          value={to}
          onChange={(e) => onTo(e.target.value)}
          className="h-8 w-48 text-sm"
          aria-label="Filter to date"
        />
      </div>
    </div>
  );
}

/**
 * The act's outcome as a tag: uppercase mono on its own tint, never interactive
 * (§2.1's form rule) — the action lives next door in the Next step link.
 *
 * `denied` renders CRIT, not warn (M151 §2.2): crit means "it will not proceed
 * without a change — failed, halted, refused", and a denial is precisely a
 * refusal. Warn is reserved for a bound near or crossed while still serving,
 * which a refused act is not.
 */
function OutcomeTag({ outcome }: { outcome: string }) {
  if (outcome === "success") return <Badge variant="ok">{outcome}</Badge>;
  if (outcome === "denied" || outcome === "error")
    return <Badge variant="crit">{outcome}</Badge>;
  return <Badge variant="muted">{outcome}</Badge>;
}

/** `Kind/name · namespace` — the thing the act was performed on, in one mono line. */
function resourceLine(e: AuditEvent): string {
  const thing = [e.resourceKind, e.resourceName].filter(Boolean).join("/");
  return [thing, e.namespace].filter(Boolean).join(" · ");
}

/** The drawer's fact register — every field, including the ones the table drops. */
function recordItems(e: AuditEvent): KeyValueItem[] {
  return [
    { key: "Action", value: e.action },
    { key: "Resource", value: e.resourceName, absent: "not named" },
    { key: "Kind", value: e.resourceKind, absent: "not recorded" },
    { key: "Namespace", value: e.namespace, absent: "not namespaced" },
    { key: "Actor", value: e.actor, absent: "unattributed", title: ACTOR_UNKNOWN_TITLE },
    { key: "Actor kind", value: e.actorKind },
    { key: "Source", value: e.source },
    { key: "Outcome", value: e.outcome },
    { key: "When", value: formatDateTime(e.occurredAt), title: e.occurredAt },
    { key: "Event id", value: String(e.id) },
    {
      key: "Trace id",
      value: e.traceId,
      absent: "not part of a run",
      title: NO_RUN_UNKNOWN_TITLE,
    },
  ];
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
  event: AuditEvent;
  next: NextStep;
}

/**
 * One act → its next step. Only a REFUSED or FAILED act asks anything of a
 * person: an act that went through is the trail doing its job, and inventing an
 * errand for it would make the column noise instead of a signal.
 */
function triage(e: AuditEvent, review: (e: AuditEvent) => void): Row {
  if (e.outcome === "denied") {
    return {
      event: e,
      // Crit is the one action hue allowed here (§2.3): the target genuinely is
      // a refusal.
      next: { label: "Review the denial", tone: "crit", onClick: () => review(e) },
    };
  }
  if (e.outcome === "error") {
    return {
      event: e,
      next: e.traceId
        ? { label: "Open the failure", tone: "crit", to: `/traces/${encodeURIComponent(e.traceId)}` }
        : { label: "Review the failure", tone: "crit", onClick: () => review(e) },
    };
  }
  return { event: e, next: { tone: "none" } };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "needs-you" | "denied" | "all";

const VIEWS: { id: ViewId; label: string; match: (r: Row) => boolean }[] = [
  { id: "needs-you", label: "Needs you", match: (r) => r.next.tone !== "none" },
  { id: "denied", label: "Denied", match: (r) => r.event.outcome === "denied" },
  { id: "all", label: "Everything", match: () => true },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  "needs-you": {
    title: "Nothing needs a person",
    description:
      "Every act in this page window went through. Show everything to read the trail.",
  },
  denied: {
    title: "Nothing was refused",
    description:
      "No act in this page window was denied. Show everything to read the trail.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. Every number is counted from the rows in hand, and the
 * sentence says so whenever the rows in hand are not the whole trail.
 */
export function closingLine(rows: Row[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const refused = rows.filter((r) => r.event.outcome === "denied").length;
  const failed = rows.filter((r) => r.event.outcome === "error").length;
  const clean = total - refused - failed;
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More pages follow.";

  if (total === 1) {
    const one =
      refused === 1
        ? "The one act here was refused."
        : failed === 1
          ? "The one act here failed."
          : "The one act here went through.";
    return `${one}${more}`;
  }

  if (refused === 0 && failed === 0) {
    return `All ${total} acts${where} went through.${more}`;
  }

  // Counted in WORDS at one, so the sentence never reads "1 were refused".
  const parts: string[] = [];
  if (refused > 0) parts.push(refused === 1 ? "one was refused" : `${refused} were refused`);
  if (failed > 0) parts.push(failed === 1 ? "one failed" : `${failed} failed`);
  const breakdown = parts.join(" and ");

  if (clean === 0) {
    return `None of the ${total} acts${where} went through: ${breakdown}.${more}`;
  }
  return `Of the ${total} acts${where}, ${breakdown}; the other ${
    clean === 1 ? "one" : clean
  } went through.${more}`;
}

// AuditDetailDrawer — click-through detail for one audit event (m76.5 H3).
// Opens as a right-side DetailDrawer with the full record as a KeyValueList and
// the "View run" trace link as a first-class action. This is also where §4.4's
// "dropped ≠ lost" is honoured: every column the table sheds at a narrow width
// renders here.
function AuditDetailDrawer({
  event: e,
  onClose,
}: {
  event: AuditEvent | null;
  onClose: () => void;
}) {
  if (!e) return null;
  const detail = e.detail ? Object.entries(e.detail) : [];
  return (
    <DetailDrawer
      open={!!e}
      onClose={onClose}
      title={e.action}
      subtitle={`${e.actor || "unattributed"} · ${formatDateTime(e.occurredAt)}`}
      status={<OutcomeTag outcome={e.outcome} />}
      size="md"
      footer={
        e.traceId ? (
          <Link
            to={`/traces/${encodeURIComponent(e.traceId)}`}
            data-testid={`audit-trace-link-${e.id}`}
            className="text-sm font-semibold text-primary border-b border-accent hover:border-primary"
          >
            View run
          </Link>
        ) : undefined
      }
    >
      <div data-testid="audit-detail-drawer" className="space-y-6">
        <KeyValueList items={recordItems(e)} />

        {detail.length > 0 && (
          <section>
            <SectionHeader
              as="h3"
              title="Detail"
              lede="The non-secret context recorded with the act. Tokens and credentials never reach this store."
            />
            <KeyValueList
              items={detail.map(([k, v]) => ({
                key: k,
                value: typeof v === "string" ? v : JSON.stringify(v),
              }))}
            />
          </section>
        )}
      </div>
    </DetailDrawer>
  );
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: AuditEvent[]; nextCursor: string }
  | { kind: "unavailable" } // 501 — audit store not configured
  | { kind: "error"; message: string; forbidden: boolean };

const LEDE =
  "Who connected, consented, and invoked what — newest first. Every filter narrows server-side; switch the namespace scope with the global namespace selector.";

export function AuditPage() {
  const { namespace } = useNamespace();

  // Server-side filters.
  const [actorFilter, setActorFilter] = useState("");
  const [actionFilter, setActionFilter] = useState("");
  const [kindFilter, setKindFilter] = useState("");
  const [fromFilter, setFromFilter] = useState("");
  const [toFilter, setToFilter] = useState("");

  // The chip row is a set of VIEWS over the loaded window (§5.28).
  const [view, setView] = useState<ViewId>("all");

  // Cursor pagination: a stack of keyset cursors, one per page. [""] = page 0.
  const [pageStack, setPageStack] = useState<string[]>([""]);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });

  // Detail drawer — null when closed.
  const [drawerEvent, setDrawerEvent] = useState<AuditEvent | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const cursor = pageStack[pageStack.length - 1] ?? "";

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });

    // The <input type="datetime-local"> value is "YYYY-MM-DDTHH:MM" in LOCAL time
    // (no seconds/zone). Convert to UTC RFC3339 before sending to the BFF.
    const fromRfc = toRFC3339(fromFilter);
    const toRfc = toRFC3339(toFilter);

    const params: AuditListParams = {
      limit: PAGE_LIMIT,
      ...(namespace ? { namespace } : {}),
      ...(cursor ? { cursor } : {}),
      ...(actorFilter ? { actor: actorFilter } : {}),
      ...(actionFilter ? { action: actionFilter } : {}),
      ...(kindFilter ? { kind: kindFilter } : {}),
      ...(fromRfc ? { from: fromRfc } : {}),
      ...(toRfc ? { to: toRfc } : {}),
    };

    api
      .listAudit(params, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (audit store not configured) — calm degrade, NOT an error.
        if (res === null) {
          setLoadState({ kind: "unavailable" });
          return;
        }
        setLoadState({
          kind: "ready",
          items: res.items,
          nextCursor: res.nextCursor ?? "",
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 403 (no operator persona) + 500 (DB) surface as a real error; 403 renders
        // the forbidden variant (never a fake empty list).
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [namespace, cursor, actorFilter, actionFilter, kindFilter, fromFilter, toFilter]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  // Reset to page 0 whenever a filter (or the global namespace) changes.
  const resetPaging = useCallback(() => setPageStack([""]), []);
  function onActorChange(v: string) {
    setActorFilter(v);
    resetPaging();
  }
  function onActionChange(v: string) {
    setActionFilter(v);
    resetPaging();
  }
  function onKindChange(v: string) {
    setKindFilter(v);
    resetPaging();
  }
  function onFromChange(v: string) {
    setFromFilter(v);
    resetPaging();
  }
  function onToChange(v: string) {
    setToFilter(v);
    resetPaging();
  }
  function clearFilters() {
    setActorFilter("");
    setActionFilter("");
    setKindFilter("");
    setFromFilter("");
    setToFilter("");
    resetPaging();
  }

  const items = useMemo(
    () => (loadState.kind === "ready" ? loadState.items : []),
    [loadState],
  );
  const nextCursor = loadState.kind === "ready" ? loadState.nextCursor : "";

  // hasNext keys off the CURSOR, never off items.length (keyset contract).
  const hasNext = nextCursor !== "";
  const hasPrev = pageStack.length > 1;
  const pageNumber = pageStack.length;
  // The loaded window IS the whole trail only when no cursor precedes or follows
  // it — the one condition under which counting the rows in hand is a FACT
  // rather than a windowed guess (kit FilterChipRow contract).
  const trailComplete = loadState.kind === "ready" && !hasNext && !hasPrev;

  // Newest first — a trail read out of time order is not a trail. `nextStepRank`
  // breaks a timestamp tie so "Nothing needed" still sinks; the row id breaks
  // that, so the order is stable across refetches.
  const sorted = useMemo(() => {
    const rows = items.map((e) => triage(e, setDrawerEvent));
    rows.sort(
      (a, b) =>
        b.event.occurredAt.localeCompare(a.event.occurredAt) ||
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        b.event.id - a.event.id,
    );
    return rows;
  }, [items]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[VIEWS.length - 1];
  const visible = useMemo(() => sorted.filter(activeView.match), [sorted, activeView]);

  // Chips are built FROM the view union, so a chip whose id is not a view stops
  // compiling. Counts appear only when the loaded window provably IS the whole
  // trail (kit FilterChipRow contract).
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: trailComplete ? sorted.filter(v.match).length : undefined,
  }));

  function onNext() {
    if (!hasNext) return;
    setPageStack((s) => [...s, nextCursor]);
  }
  function onPrev() {
    if (!hasPrev) return;
    setPageStack((s) => s.slice(0, -1));
  }

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "the audit log",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 ACTIVITY-FEED budget, in visual order. Priorities are the whole
  // responsive story: 4 leaves below 1280, 3 below 1024, 2 below 768, 1 never.
  // When, Actor, What and Next step survive every width.
  const columns: Column<Row>[] = [
    {
      id: "when",
      header: "When",
      priority: 1,
      className: "w-[6.5rem]",
      cell: ({ event: e }) => (
        // Relative time is sanctioned in feeds (§4.5) — always with the
        // absolute in `title`, never instead of it.
        <span
          className="whitespace-nowrap font-mono text-xs tabular-nums"
          title={formatDateTime(e.occurredAt)}
        >
          {formatRelativeTime(e.occurredAt)}
        </span>
      ),
    },
    {
      id: "actor",
      header: "Actor",
      priority: 1,
      // The cap is what makes `truncate` bite, and it is the column's FLOOR as
      // well as its ceiling: a `white-space: nowrap` cell contributes its whole
      // text as min-content, clamped by this max-width — so the cap is exactly
      // how wide this column will be. It steps with the viewport so the four
      // columns that may never be dropped (§4.4) all stay on screen at 768,
      // 1024, 1280 and 1440 without the frame having to scroll to reach them.
      className:
        "max-w-[9rem] lg:max-w-[9.5rem] xl:max-w-[11rem] min-[1440px]:max-w-[14rem]",
      cell: ({ event: e }) =>
        e.actor ? (
          <CellEntity
            title={e.actor}
            name={e.actor}
            namespace={[e.actorKind, e.source].filter(Boolean).join(" · ")}
          />
        ) : (
          <UnknownValue title={ACTOR_UNKNOWN_TITLE} />
        ),
    },
    {
      id: "what",
      header: "What",
      priority: 1,
      // Steps with the viewport for the same reason the Actor cap does.
      className:
        "max-w-[13.5rem] lg:max-w-[15.5rem] xl:max-w-[18rem] min-[1440px]:max-w-[25rem]",
      cell: ({ event: e }) => {
        const resource = resourceLine(e);
        return (
          <div className="min-w-0">
            {/* The verb is a machine string, so it stays in the mono face. */}
            <div className="truncate font-mono text-sm font-semibold" title={e.action}>
              {e.action}
            </div>
            {resource && (
              <div className="truncate font-mono text-xs text-faint" title={resource}>
                {resource}
              </div>
            )}
            {/* §4.4: below 768 the State column folds into the What line as a
                tag. Exactly one of the two copies is ever displayed, so the
                accessibility tree never carries the outcome twice. */}
            <div className="mt-1 md:hidden">
              <OutcomeTag outcome={e.outcome} />
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
      // Never a zero: an act that belongs to a run HAS a duration this endpoint
      // does not send; an act that belongs to no run has none at all. One glyph,
      // two honest reasons — see the QuietNote above the table.
      cell: ({ event: e }) => (
        <UnknownValue title={e.traceId ? IN_RUN_UNKNOWN_TITLE : NO_RUN_UNKNOWN_TITLE} />
      ),
    },
    {
      id: "cost",
      header: "Cost",
      priority: 3,
      numeric: true,
      cell: ({ event: e }) => (
        <UnknownValue title={e.traceId ? IN_RUN_UNKNOWN_TITLE : NO_RUN_UNKNOWN_TITLE} />
      ),
    },
    {
      id: "outcome",
      header: "State",
      priority: 2,
      className: "w-[6.5rem]",
      cell: ({ event: e }) => <OutcomeTag outcome={e.outcome} />,
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
          ariaLabel={row.next.label ? `${row.next.label} — ${row.event.action}` : undefined}
          testId={`next-step-${row.event.id}`}
        />
      ),
    },
  ];

  const serverFiltered = !!(
    actorFilter ||
    actionFilter ||
    kindFilter ||
    fromFilter ||
    toFilter
  );
  // The chip views filter the LOADED window, so an emptied view is the
  // "empty-filtered" truth (§7), not the first-run one: it offers a way back
  // out instead of teaching an operator what an audit trail is.
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
        totalCount: trailComplete ? sorted.length : undefined,
        countNoun: "acts",
      }
    : serverFiltered
      ? {
          intent: "filtered",
          icon: Filter,
          title: "No acts match these filters",
          description:
            "Nothing in the trail matches this actor, action, resource kind, and date range together. Clear them to read the whole trail — every filter here narrows the whole table, not just this page.",
          action: {
            label: "Clear filters",
            variant: "outline",
            onClick: clearFilters,
          },
        }
      : {
          icon: ScrollText,
          title: "No audit events",
          description: namespace
            ? `Nothing has been recorded in ${namespace} yet. The trail is append-only: it fills as people connect providers, consent to tools, and change resources. Widen the namespace scope in the top bar to see more.`
            : "Nothing has been recorded yet. The trail is append-only: it fills as people connect providers, consent to tools, and change resources.",
        };

  // 501 — the audit store is not configured (no control-plane DSN). This is the
  // calm backend-cannot-answer state (§7.1), NOT an error: nothing broke, the
  // platform is simply not wired to answer.
  if (loadState.kind === "unavailable") {
    return (
      <div className="min-w-0 space-y-6" data-testid="audit-page">
        <PageHeader title="Audit" lede={LEDE} />
        <div data-testid="audit-unavailable">
          <QuietNote title="The audit trail isn’t configured on this install.">
            Audit rows are written to the control-plane database, and this install
            has none wired up — so there is nothing to read, and no compliance
            history to export either. Configuring it needs a control-plane store,
            the same one the rest of the platform uses. Nothing here is estimated;
            the record is simply absent.
          </QuietNote>
        </div>
      </div>
    );
  }

  const closing = closingLine(sorted, trailComplete);
  const showChips = loadState.kind !== "error";
  const showMeasureNote = loadState.kind === "ready" && items.length > 0;
  const metaLine =
    loadState.kind === "ready"
      ? trailComplete
        ? `${items.length} act${items.length === 1 ? "" : "s"}`
        : `${items.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="audit-page">
      <PageHeader title="Audit" meta={metaLine} lede={LEDE} />

      {/* The server-side filters sit ABOVE the views: they change what the
          backend sends, the chips change what you are looking at within it. */}
      <AuditFilterBar
        actor={actorFilter}
        action={actionFilter}
        kind={kindFilter}
        from={fromFilter}
        to={toFilter}
        onActor={onActorChange}
        onAction={onActionChange}
        onKind={onKindChange}
        onFrom={onFromChange}
        onTo={onToChange}
      />

      {showChips && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter audit events"
          className="min-w-0"
        />
      )}

      {showMeasureNote && (
        <QuietNote title={MEASURE_NOTE_TITLE}>
          The trail records that something happened, who did it, and how it came
          out — never how long it took or what it cost. An act that belongs to a
          run has both, but they live with the run, not here; an act that belongs
          to no run has neither. So the Duration and Cost columns read{" "}
          <span className="font-mono">—</span>. Nothing is estimated — those
          figures are simply absent.
        </QuietNote>
      )}

      <DataTable<Row>
        columns={columns}
        rows={visible}
        rowKey={(row) => String(row.event.id)}
        loading={loadState.kind === "loading"}
        error={error}
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={onPrev}
        onNext={onNext}
        rangeLabel={`Page ${pageNumber}`}
        ariaLabel="Audit events"
        // Row-click → the full record, where every column this table drops at a
        // narrow width still renders (§4.4 "dropped ≠ lost").
        onRowClick={(row) => setDrawerEvent(row.event)}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      <AuditDetailDrawer event={drawerEvent} onClose={() => setDrawerEvent(null)} />
    </div>
  );
}
