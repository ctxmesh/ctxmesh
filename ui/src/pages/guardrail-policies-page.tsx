import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Shield } from "lucide-react";

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
  StatusBadge,
  UnknownValue,
  humanizeStatusReason,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { api, ApiError, type GuardrailPolicySummary } from "@/lib/api";

// GuardrailPoliciesPage — archetype A1 (index/table), resource-list column
// budget (M151 spec §6.1/§4.4). The GuardrailPolicy content-governance policies
// (m66.10, ADR 0059).
//
// Read-only (caller-scoped, ADR 0011): each row is a policy — its detectors,
// fail mode, effective streaming mode, blast-radius agents and validated state.
// A policy is authored via YAML/kubectl; the console surfaces it for visibility
// and operator awareness. A 403 surfaces as an honest forbidden state (never a
// fake empty list).
//
// ── WHY THERE IS A DRAWER ───────────────────────────────────────────────────
// A GuardrailPolicy has no detail route, and the §4.4 budget drops four of its
// columns by 1024px. "Dropped ≠ lost" is only true if the row opens SOMEWHERE,
// so the row opens a drawer where every field renders — including the full
// controller reason, which is the one thing a broken policy exists to tell you
// and is far too long for a table cell.
//
// ── ORDER ───────────────────────────────────────────────────────────────────
// Blocking-first (§6.1 A1), never alphabetical: a policy that will not apply is
// the reason to open this page, so it is the first row.
//
// data-testid contract:
//   guardrail-policies-page  — root container
//   streaming-<name>         — the effective-streaming Tag (absent until reconciled)
//   next-step-<name>         — the row's Next step cell
//   guardrail-drawer         — the row's drawer body

/** Attention order inside a next-step group (§6.1 A1). */
const TONE_RANK: Record<StatusTone, number> = {
  failed: 0,
  waiting: 1,
  progressing: 2,
  ready: 3,
  draft: 4,
};

interface NextStep {
  label?: string;
  tone: NextStepTone;
}

/**
 * The reason CODE, humanised, with the full controller sentence kept for the
 * `title` and the drawer. The BFF sends `InvalidPattern: pattern #3 failed to
 * compile (…)`; the leading token is the part that fits a table cell, and
 * nothing is lost because the whole string renders in the drawer.
 */
function reasonCode(reason?: string): string {
  const head = (reason ?? "").split(":")[0]?.trim();
  return head ? humanizeStatusReason(head) : "";
}

/**
 * The closing line's copy (§5.18) — a SIGHTED FLOURISH restating the table's
 * ratio, built only from counts of the rows the response actually contained.
 */
function closingNote(
  total: number,
  inForce: number,
  unattached: number,
  broken: number,
): string {
  if (total === 1)
    return broken > 0
      ? "The one policy here isn't being applied."
      : unattached > 0
        ? "The one policy here is valid, and guarding nothing yet."
        : "The one policy here is in force.";
  const needs = broken + unattached;
  if (needs === 0)
    return `All ${total} policies are in force. Nothing here needs a person.`;
  const parts: string[] = [];
  if (broken > 0)
    parts.push(`${broken} won't apply until someone fixes ${broken === 1 ? "it" : "them"}`);
  if (unattached > 0)
    parts.push(`${unattached} ${unattached === 1 ? "guards" : "guard"} nothing yet`);
  const tail =
    inForce === 0
      ? ""
      : inForce === 1
        ? " The other one is in force."
        : ` The other ${inForce} are in force.`;
  const verb = needs === 1 ? "needs" : "need";
  return `${needs} of the ${total} policies ${verb} a person: ${parts.join(", ")}.${tail}`;
}

/** The detector summary the row states in words. Empty means the policy inspects nothing. */
function detectorParts(p: GuardrailPolicySummary): string[] {
  const parts: string[] = [];
  if (p.piiEnabled) parts.push("PII");
  if (p.denylistCount > 0) parts.push(`${p.denylistCount} deny rules`);
  if (p.judgeEnabled) parts.push("judge");
  if (p.userRateLimited) parts.push("rate-limited");
  return parts;
}

/**
 * The user's next action on one policy (§7.2). The console cannot edit a
 * GuardrailPolicy — it is authored as YAML — so a broken one opens the drawer,
 * which names exactly what to fix. A valid policy that guards nothing points at
 * the agents that could use it. Everything else needs nothing.
 */
function nextStep(p: GuardrailPolicySummary, tone: StatusTone): NextStep {
  if (tone !== "ready") return { label: "Fix the policy", tone: "crit" };
  if (p.referencingAgents.length === 0)
    return { label: "Attach it to an agent", tone: "default" };
  return { tone: "none" };
}

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; policies: GuardrailPolicySummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

type View = "all" | "attention" | "failopen";

export function GuardrailPoliciesPage() {
  const [query, setQuery] = useState("");
  const [view, setView] = useState<View>("all");
  const [open, setOpen] = useState<GuardrailPolicySummary | null>(null);
  const [loadState, setLoadState] = useState<LoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listGuardrailPolicies(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", policies: res.items });
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

  const all = useMemo(
    () => (loadState.kind === "ready" ? loadState.policies : []),
    [loadState],
  );

  // Decorate once, then order by what is blocking (§6.1 A1). The q filter is
  // applied after the sort, so the ordering a user sees never changes shape
  // while they type.
  const decorated = useMemo(
    () =>
      all
        .map((p) => {
          const status = resolveStatus(p.validated, undefined, p.reason);
          return { row: p, tone: status.tone, step: nextStep(p, status.tone) };
        })
        .sort(
          (a, b) =>
            nextStepRank(a.step.tone) - nextStepRank(b.step.tone) ||
            TONE_RANK[a.tone] - TONE_RANK[b.tone] ||
            a.row.name.localeCompare(b.row.name),
        ),
    [all],
  );

  const q = query.trim().toLowerCase();
  const inView = decorated.filter((d) => {
    if (view === "attention") return d.step.tone !== "none";
    if (view === "failopen") return d.row.failMode === "open";
    return true;
  });
  const visible = q ? inView.filter((d) => d.row.name.toLowerCase().includes(q)) : inView;
  const rows = visible.map((d) => d.row);
  const stepFor = new Map(
    decorated.map((d) => [`${d.row.namespace}/${d.row.name}`, d.step] as const),
  );

  // Honest ratio inputs — every one a count of what the backend actually sent.
  const broken = decorated.filter((d) => d.tone !== "ready").length;
  const unattached = decorated.filter(
    (d) => d.tone === "ready" && d.row.referencingAgents.length === 0,
  ).length;
  const inForce = decorated.length - broken - unattached;

  // The backend-cannot-answer case (§7 A1): `status.streaming` arrived in M139.
  // Against an older controller NO policy reports it, and a whole column of
  // dashes with no explanation reads as a bug. One QuietNote above the table
  // says what is absent — never an error, never a zero.
  const streamingUnreported =
    decorated.length > 0 && decorated.every((d) => !d.row.streamingMode);

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "guardrail policies",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  // §4.4 resource-list budget: Policy / State / Next step are priority 1 and
  // survive 768; detectors + streaming take the p4 slot, fail mode + agents p3.
  const columns: Column<GuardrailPolicySummary>[] = [
    {
      id: "name",
      header: "Policy",
      className: "max-w-[18rem]",
      cell: (p) => <CellEntity name={p.name} namespace={p.namespace} />,
    },
    {
      id: "detectors",
      header: "Detectors",
      priority: 4,
      className: "max-w-[16rem]",
      cell: (p) => {
        const parts = detectorParts(p);
        return parts.length > 0 ? (
          <span
            className="block truncate text-sm text-muted-foreground"
            title={parts.join(", ")}
          >
            {parts.join(", ")}
          </span>
        ) : (
          // Declared but inspecting nothing — the `open` Tag, not a dash: this
          // is a readable state, not a missing measurement (§2.5).
          <Badge variant="open">no detectors</Badge>
        );
      },
    },
    {
      id: "failMode",
      header: "Fail mode",
      priority: 3,
      cell: (p) => (
        // Fail-closed is the safe posture; fail-open is degraded-but-serving,
        // which is exactly what warn means (§2.2). Neither is health, so
        // neither may ever take pine.
        <Badge variant={p.failMode === "closed" ? "ok" : "warn"}>{p.failMode}</Badge>
      ),
    },
    {
      id: "streaming",
      header: "Streaming",
      priority: 4,
      cell: (p) =>
        p.streamingMode ? (
          // A mode, not a health state: both modes are muted Tags. The reason —
          // especially a downgrade — rides in the title.
          <Badge
            variant="muted"
            title={p.streamingReason}
            data-testid={`streaming-${p.name}`}
          >
            {p.streamingMode === "Streaming"
              ? `Streaming${p.streamingWindow ? ` · W=${p.streamingWindow}` : ""}`
              : "Buffered"}
          </Badge>
        ) : (
          <UnknownValue title="Not reconciled yet — the effective streaming mode is unknown, not off." />
        ),
    },
    {
      id: "agents",
      header: "Agents",
      priority: 3,
      className: "max-w-[12rem]",
      cell: (p) =>
        p.referencingAgents.length === 0 ? (
          // Declared but never exercised (§2.5) — a policy nothing references.
          // NOT a dash: an empty array is a real answer, and a dash would make
          // it indistinguishable from a field the backend never sent.
          <Badge variant="open">not applied</Badge>
        ) : p.referencingAgents.length === 1 ? (
          <span
            className="block truncate font-mono text-xs"
            title={p.referencingAgents[0]}
          >
            {p.referencingAgents[0]}
          </span>
        ) : (
          <span
            className="font-mono text-xs tabular-nums"
            title={p.referencingAgents.join(", ")}
          >
            {`${p.referencingAgents.length} agents`}
          </span>
        ),
    },
    {
      id: "status",
      header: "State",
      className: "w-[9rem]",
      cell: (p) => {
        const code = p.validated ? "" : reasonCode(p.reason);
        return (
          <div className="min-w-0">
            <StatusBadge ready={p.validated} reason={p.validated ? undefined : p.reason} />
            {code && (
              // The cause, subordinate to the state (M144.1): readable faint,
              // never ghost — this is information you have to read.
              <div className="mt-1 truncate text-xs text-faint" title={p.reason}>
                {code}
              </div>
            )}
          </div>
        );
      },
    },
    {
      id: "nextStep",
      header: "Next step",
      className: "w-[11rem]",
      cell: (p) => {
        const step = stepFor.get(`${p.namespace}/${p.name}`);
        return (
          <NextStepLink
            label={step?.label}
            tone={step?.tone ?? "none"}
            testId={`next-step-${p.name}`}
            onClick={step?.tone === "crit" ? () => setOpen(p) : undefined}
            to={step?.tone === "default" ? "/agents" : undefined}
          />
        );
      },
    },
  ];

  const emptyState =
    view !== "all"
      ? {
          icon: Shield,
          intent: "filtered" as const,
          title: view === "attention" ? "Nothing here needs you" : "Nothing fails open",
          description:
            view === "attention"
              ? "Every policy is valid and attached to at least one agent."
              : "Every policy here fails closed — a guardrail error blocks the request rather than letting it through.",
          totalCount: decorated.length > 0 ? decorated.length : undefined,
          countNoun: "policies",
          action: {
            label: "Show everything",
            variant: "outline" as const,
            onClick: () => setView("all"),
          },
        }
      : {
          icon: Shield,
          title: "No guardrail policies",
          description:
            "No guardrail policies yet. A guardrail policy applies content governance — PII scanning, deny-lists, an optional LLM-judge, and per-user rate limits — to your agents.",
        };

  return (
    <div className="min-w-0 space-y-6" data-testid="guardrail-policies-page">
      <PageHeader
        title="Guardrail policies"
        lede="Namespace-scoped content governance: PII scanning, pattern deny-lists, an optional LLM-judge, and per-user rate limits. Applied at inference time; authored as YAML, so this page reads rather than edits."
      />

      {/* Views, not filters (§5.28) — one question, one answer. No counts: the
          chip contract takes backend counts only, and this list is what one
          response happened to carry. The ClosingNote states the ratio in words. */}
      {decorated.length > 0 && (
        <FilterChipRow
          label="Filter policies"
          value={view}
          onChange={(id) => setView(id as View)}
          chips={[
            { id: "all", label: "Everything" },
            { id: "attention", label: "Needs attention" },
            { id: "failopen", label: "Fails open" },
          ]}
        />
      )}

      {streamingUnreported && (
        <QuietNote title="Effective streaming mode isn't reported here.">
          This cluster's guardrail controller predates the streaming decision
          (ADR 0086), so no policy carries one. Every other column is live.
          Nothing here is estimated — the mode is simply absent.
        </QuietNote>
      )}

      <DataTable<GuardrailPolicySummary>
        columns={columns}
        rows={rows}
        rowKey={(p) => `${p.namespace}/${p.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter policies by name…"
        ariaLabel="Guardrail policies"
        onRowClick={(p) => setOpen(p)}
        empty={emptyState}
      />

      {loadState.kind === "ready" && decorated.length > 0 && (
        <ClosingNote>
          {closingNote(decorated.length, inForce, unattached, broken)}
        </ClosingNote>
      )}

      {/* Dropped ≠ lost (§4.4): every field the budget hides renders here, plus
          the full controller reason the State cell can only abbreviate. */}
      <DetailDrawer
        open={open !== null}
        onClose={() => setOpen(null)}
        title={open?.name ?? ""}
        subtitle={open?.namespace}
        status={
          open ? (
            <StatusBadge
              ready={open.validated}
              reason={open.validated ? undefined : open.reason}
            />
          ) : undefined
        }
      >
        {open && (
          <div className="space-y-5" data-testid="guardrail-drawer">
            {!open.validated && open.reason && (
              <QuietNote title="This policy is not being applied.">
                <p className="font-mono text-xs">{open.reason}</p>
                <p className="mt-2">
                  A GuardrailPolicy is authored as YAML — fix it where it is
                  defined, and the controller re-validates it.
                </p>
              </QuietNote>
            )}
            <KeyValueList
              items={[
                {
                  key: "Detectors",
                  value: detectorParts(open).join(", "),
                  absent: "none enabled",
                  mono: false,
                },
                { key: "Fail mode", value: open.failMode },
                { key: "Streaming", value: open.streamingMode, absent: "not yet known" },
                {
                  key: "Hold window",
                  value: open.streamingWindow,
                  absent: "not yet known",
                },
                {
                  key: "Streaming reason",
                  value: open.streamingReason,
                  absent: "not stated",
                  mono: false,
                },
                { key: "Policy hash", value: open.policyHash, absent: "not yet known" },
                {
                  key: "Agents",
                  value: open.referencingAgents.join(", "),
                  absent: "not applied to any agent",
                },
              ]}
            />
          </div>
        )}
      </DetailDrawer>
    </div>
  );
}
