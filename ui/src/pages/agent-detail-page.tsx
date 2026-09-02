import * as React from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Boxes,
  ChevronRight,
  ExternalLink,
  GitFork,
  Pencil,
  Play,
  Plus,
  RotateCcw,
  Server,
  Share2,
  SlidersHorizontal,
  Trash2,
  X,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  ClosingNote,
  ConfirmDialog,
  DataTable,
  DetailDrawer,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  LifecycleStrip,
  Meter,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  Skeleton,
  StatusBadge,
  Timeline,
  UNKNOWN,
  Wizard,
  humanizeStatusReason,
  lifecycleFactNumber,
  resolveStatus,
  truncateId,
  useFocusTrap,
  useToast,
  type Column,
  type KeyValueItem,
  type LifecycleStage,
  type LifecycleStageCell,
  type PageHeaderAction,
  type TimelineStep,
  type TimelineTone,
  type WizardStep,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { ChatPanel } from "@/components/agent-chat";
import { RunInspector } from "@/components/dashboard/run-inspector";
import { UseAgentPanel } from "@/components/dashboard/use-agent-panel";
import {
  api,
  ApiError,
  openLogStream,
  type AgentBinding,
  type AgentCondition,
  type AgentDetailResponse,
  type AgentReference,
  type AgentRuntimeDetail,
  type AgentRunSummary,
  type AgentMemoryEntry,
  type LongTermMemoryConfig,
  type SessionMemoryConfig,
  type AgentScalingPolicySummary,
  type AgentSimplifiedSpec,
  type LogEventType,
  type MemoryBindingSummary,
  type OnlineScoreResponse,
  type OnlineScoreWindow,
  type PublishTemplateResponse,
} from "@/lib/api";

// Fork provenance labels (ADR 0068 §6) — forwarded from the AgentDeployment CR.
// Used for lineage display (U12, m76.3) and banner repair links (U5).
const LABEL_FORK_ORIGIN_NS = "agents.ctxmesh.ai/fork-origin-namespace";
const LABEL_FORK_ORIGIN_NAME = "agents.ctxmesh.ai/fork-origin-name";
const LABEL_FORK_ORIGIN_VERSION = "agents.ctxmesh.ai/fork-origin-version";
import { useCapabilities } from "@/lib/capabilities";
import { formatDateTime, formatLatency, formatUSD } from "@/lib/format";
import { navRoute, RES_AGENTS, RES_LOGS, RES_MEMORY, RES_SCALING } from "@/lib/nav";

// AgentDetailPage — the landing page for ONE agent, and the second-most-read
// surface in the console (M151, spec §6.1 archetype A2 + the §6.2 row for this
// file). Route: /agents/:ns/:name
//
// ── THE PAGE'S ONE IDEA: WHAT CAN IT REACH, AND IS THAT WORKING? ────────────
// An agent is not interesting on its own; it is interesting because of what it
// is wired to — a model route, a prompt, a guardrail policy, a set of tools, a
// memory store. So the page leads with "What it can reach": every binding, one
// per row, each wearing exactly what the platform can say about it.
//
// That panel is where the honesty rules bite hardest, and they cut BOTH ways:
//
//   • A tool that has never been called is NOT broken. It wears the dashed
//     `open` Tag ("never called", §2.5 — declared but never exercised), never a
//     failure hue. Dressing an unexercised binding as a fault teaches operators
//     to ignore the real ones.
//   • A binding that does not RESOLVE *is* broken, and the panel says so in
//     crit and in words. It will not proceed without a change (§2.2), and the
//     panel offers the next step rather than leaving the reader to hunt.
//   • Whether a resolved tool has actually been CALLED is a span-level fact
//     that lives in the trace backend, not on this endpoint. So `working` is
//     defined as "the binding resolves and the agent is up" — and the panel's
//     QuietNote says exactly that, rather than letting a green tag imply a
//     measurement nobody made.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ─────────────────────────────────────
// `GET /api/agents/{ns}/{name}` returns identity, spec, status conditions,
// bindings and the version list. It returns NO live replica count, NO per-tool
// call counts, NO spend. Each of those renders the honest dash plus one note
// saying which absence it is — never a zero, never an estimate.
//
// ── THE LIFECYCLE STRIP IS A POSITION CLAIM, SO IT IS DERIVED, NOT GUESSED ──
// Build → Govern → Ship → Improve is read off `isDraft`, the eval-gate
// projection, readiness and the RegressionDetected condition (see
// `lifecycleStage`). When those cannot place the agent, NO stage is lit and a
// QuietNote says so: a guessed position is the one thing §5.20 forbids.
//
// ── TABS (§6.2): Overview / Equipment / Runs / Quality / Versions ───────────
// Five tabs, and the rail persists across all of them (§6.1 A2). Equipment is
// everything you attach to an agent — tools, runtime policy, memory, scaling,
// redaction. Runs is what it has done, including the live output tail (gated on
// `get pods/log`). Quality is the improvement loop. Versions is the history and
// the snapshot diff.
//
// m15.11 behaviour that survives verbatim: drift / managedOutsideUI provenance,
// the Edit Wizard (full round-trip vs safe-field patch, drift-overwrite
// warning), typed-name Delete with its references impact, the per-agent Runs
// list with its 501-degrade, and RBAC-aware write affordances (display-only,
// ADR 0011).

const TABS = ["Overview", "Equipment", "Runs", "Quality", "Versions"] as const;
type Tab = (typeof TABS)[number];

/**
 * Deep links minted before the M151 consolidation still exist in the wild — a
 * trace's `?tab=Memory` back-link (m49.3), the m14 playbook's `?tab=Logs`. They
 * resolve to the tab that now OWNS that surface instead of silently dumping the
 * reader on Overview, so an old bookmark still lands on the thing it named.
 */
const TAB_ALIAS: Record<string, Tab> = {
  bindings: "Equipment",
  memory: "Equipment",
  scaling: "Equipment",
  redaction: "Equipment",
  runtime: "Equipment",
  logs: "Runs",
  improve: "Quality",
};

function resolveTab(raw: string | null): Tab {
  const t = (raw ?? "").trim().toLowerCase();
  if (!t) return "Overview";
  return TABS.find((x) => x.toLowerCase() === t) ?? TAB_ALIAS[t] ?? "Overview";
}

/** How often an UNSETTLED agent's page re-checks. Fast enough that a pod coming up
 *  feels live, slow enough that a page left open costs nothing worth counting. */
const AGENT_SETTLE_POLL_MS = 4_000;

type Load =
  | { kind: "loading" }
  | { kind: "ready"; detail: AgentDetailResponse }
  | { kind: "error"; message: string; status?: number; forbidden: boolean };

export function AgentDetailPage() {
  const { ns = "", name = "" } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [state, setState] = React.useState<Load>({ kind: "loading" });
  const [tab, setTab] = React.useState<Tab>(() => resolveTab(searchParams.get("tab")));
  // Gate the live log tail (M100 UI99-logs): tailing needs `get pods/log`, so a
  // persona who can't read pod logs must not be shown a panel that then 403s.
  // Display-only + fail-OPEN (an unknown probe reads as allowed; the API is the
  // real gate). The one case that needs memory is a DEEP LINK to the old
  // `?tab=Logs`: a denied persona must land on Overview rather than on a Runs
  // tab whose log panel silently isn't there. The flag clears the moment the
  // reader picks a tab themselves, so it can never strand them.
  const { can } = useCapabilities();
  const { toast } = useToast();
  const canLogs = can(RES_LOGS, "get");
  const [viaLogsLink, setViaLogsLink] = React.useState(
    () => (searchParams.get("tab") ?? "").trim().toLowerCase() === "logs",
  );
  const activeTab: Tab = viaLogsLink && !canLogs ? "Overview" : tab;

  function selectTab(next: Tab) {
    setViaLogsLink(false);
    setTab(next);
  }

  // The trace to inspect — set when a run returns a traceId; opens the inspector
  // drawer over the page (list context preserved).
  const [inspectTrace, setInspectTrace] = React.useState<string | null>(null);

  // Edit + delete dialogs are opened by ?edit=1 / ?delete=1 search params so
  // they survive a hard reload and can be triggered from the list's row actions.
  const editOpen = searchParams.get("edit") === "1";
  const deleteOpen = searchParams.get("delete") === "1";
  // Publish-as-template dialog — local state (not deep-linked, no reload needed).
  const [publishOpen, setPublishOpen] = React.useState(false);
  // U7: track in-session published state. When the user publishes, we store the
  // response (version + visibility) so the header badge can show it without a
  // reload. U13: it is ALSO seeded from the durable `detail.published` on load
  // (below), so the badge + Unpublish survive a reload — previously in-session only.
  const [publishedState, setPublishedState] = React.useState<{
    version: string;
    visibility: string;
  } | null>(null);

  function openEdit() {
    setSearchParams((p) => { p.set("edit", "1"); return p; });
  }
  function closeEdit() {
    setSearchParams((p) => { p.delete("edit"); return p; });
  }
  function openDelete() {
    setSearchParams((p) => { p.set("delete", "1"); return p; });
  }
  function closeDelete() {
    setSearchParams((p) => { p.delete("delete"); return p; });
  }

  // `silent` re-fetches WITHOUT dropping back to the skeleton — the difference between
  // a refresh and a reload. The settle poll below uses it, because flashing the whole
  // page every few seconds while an agent starts is worse than the staleness it fixes.
  const load = React.useCallback((silent = false) => {
    const controller = new AbortController();
    if (!silent) setState({ kind: "loading" });
    api
      .agentDetail(ns, name, controller.signal)
      .then((detail) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", detail });
        // U13: seed the publish badge from the DURABLE published state so it survives a reload.
        // (An in-session publish/unpublish still overrides this immediately.)
        setPublishedState(
          detail.published
            ? { version: String(detail.published.version), visibility: detail.published.visibility }
            : null,
        );
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const apiErr = err instanceof ApiError ? err : null;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the agent",
          status: apiErr?.status,
          forbidden: apiErr?.isForbidden ?? false,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  React.useEffect(() => load(), [load]);

  // ── Watch it settle ────────────────────────────────────────────────────────
  // The page used to fetch exactly once. Create an agent, land here while it is still
  // provisioning, and the status chip said "still working" forever — the agent reached
  // Ready forty seconds later and the screen never noticed. The user's only recourse
  // was to reload a page that gave them no reason to think reloading would help.
  //
  // That is the seam this page sits on: creating works, the controller works, and the
  // one surface joining them was a snapshot. So poll while the agent is UNSETTLED and
  // stop the moment it settles — no timer on a steady-state page, no websocket to keep
  // alive, and nothing to clean up but an interval.
  const settled = state.kind === "ready" && state.detail.ready;
  React.useEffect(() => {
    if (state.kind !== "ready" || settled) return;
    const id = window.setInterval(() => {
      // Skip while the tab is hidden: a backgrounded page polling a cluster is pure
      // cost, and it re-syncs on the next visible tick anyway.
      if (document.visibilityState === "visible") load(true);
    }, AGENT_SETTLE_POLL_MS);
    return () => window.clearInterval(id);
  }, [state.kind, settled, load]);

  // ── Loading (§7 A2: header band + a panel skeleton + rail kv bars) ─────────
  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6" data-testid="agent-detail-loading">
        <PageHeader title={name || "Agent"} titleMono loading />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <Card className="min-w-0">
            <PanelHeader title="What it can reach" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label={`Loading ${name}`}>
                {[0, 1, 2, 3, 4].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
          <Card className="min-w-0">
            <PanelHeader title="Who governs it" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label="Loading the agent's record">
                {[0, 1, 2, 3, 4, 5].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  if (state.kind === "error") {
    if (state.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader
            breadcrumb={[{ label: "Agents", to: "/agents" }, { label: name }]}
            title={name || "Agent"}
            titleMono
          />
          <ForbiddenInline
            title={`Not allowed to view ${name}`}
            description="Your account can't read this agent in this namespace."
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="min-w-0 space-y-6" data-testid="agent-not-found">
          <PageHeader
            breadcrumb={[{ label: "Agents", to: "/agents" }, { label: name }]}
            title={name || "Agent"}
            titleMono
          />
          <EmptyState
            icon={Boxes}
            title="Agent not found"
            description={`No agent "${name}" in ${ns || "this namespace"}. It may have been deleted, or the name is wrong.`}
            action={{ label: "Back to agents", onClick: () => navigate("/agents") }}
          />
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="agent-detail-error">
        <PageHeader
          breadcrumb={[{ label: "Agents", to: "/agents" }, { label: name }]}
          title={name || "Agent"}
          titleMono
        />
        <ErrorState
          title="The agent didn't load."
          description="Nothing has changed about the agent itself — only this page failed to read it."
          detail={state.message}
          onRetry={() => load()}
        />
      </div>
    );
  }

  const detail = state.detail;

  // m74.6 / U14: fork-needs-rebinding notice — driven by the explicit `needsRebinding` flag the BFF
  // now sends (it previously keyed on a `labels` map the BFF never populated → the banner was dead in
  // production). `forkUnresolvedRefs` carries the SPECIFIC dangling refs to itemize.
  const needsRebinding = detail.needsRebinding === true;
  const unresolvedRefs = detail.forkUnresolvedRefs ?? [];

  return (
    <div className="min-w-0 space-y-6" data-testid="agent-detail-page">
      <AgentBand
        detail={detail}
        activeTab={activeTab}
        onTab={selectTab}
        onEdit={openEdit}
        onDelete={openDelete}
        onPublish={() => setPublishOpen(true)}
        publishedState={publishedState}
        onUnpublish={() => {
          void (async () => {
            try {
              await api.unpublishTemplate("agent", detail.namespace, detail.name);
              setPublishedState(null);
              toast({ title: "Template unpublished", variant: "success" });
            } catch (err) {
              // U15: a swallowed unpublish looked like a dead button — surface the failure so the
              // user knows the template is still published (the badge intentionally stays).
              toast({
                title: "Couldn't unpublish",
                description:
                  err instanceof Error ? err.message : "The template is still published — try again.",
                variant: "error",
              });
            }
          })();
        }}
      />

      <ForkLineage labels={detail.labels} />

      {needsRebinding && (
        <RebindNotice
          detail={detail}
          refs={unresolvedRefs}
          onGoEquipment={() => selectTab("Equipment")}
        />
      )}

      <AgentLifecycle detail={detail} />

      {/* §4.7 hub grid: the tab's panels on the left, the register that governs
          the agent on the right. The rail persists across tabs (§6.1 A2) and
          stacks under the main column below `lg`. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          {activeTab === "Overview" && (
            <OverviewTab
              detail={detail}
              onTraced={(id) => setInspectTrace(id)}
              onGoEquipment={() => selectTab("Equipment")}
            />
          )}
          {activeTab === "Equipment" && <EquipmentTab detail={detail} />}
          {activeTab === "Runs" && (
            <RunsAndOutputTab
              detail={detail}
              canLogs={canLogs}
              onInspect={(id) => setInspectTrace(id)}
            />
          )}
          {activeTab === "Quality" && (
            <ImprovementLoopSection
              ns={detail.namespace}
              name={detail.name}
              conditions={detail.conditions}
              gatePhase={detail.gate?.phase}
              versions={detail.versions}
            />
          )}
          {activeTab === "Versions" && <VersionsTab detail={detail} />}
        </div>

        <AgentRail detail={detail} />
      </div>

      {/* The run inspector opens over the page (drawer) so list/tab context is
          kept. It closes back to exactly where you were. */}
      <DetailDrawer
        open={inspectTrace !== null}
        onClose={() => setInspectTrace(null)}
        title="Run inspector"
        subtitle={inspectTrace ?? undefined}
        size="lg"
      >
        {inspectTrace && <RunInspector traceId={inspectTrace} />}
      </DetailDrawer>

      {/* Edit Wizard — opened by ?edit=1 search param. */}
      <DetailDrawer
        open={editOpen}
        onClose={closeEdit}
        title={`Edit ${detail.name}`}
        subtitle={detail.managedOutsideUI ? "Managed outside the UI — safe fields only" : undefined}
        size="lg"
      >
        {editOpen && (
          <EditWizard
            detail={detail}
            onClose={closeEdit}
            onSaved={() => {
              closeEdit();
              load();
            }}
          />
        )}
      </DetailDrawer>

      {/* Delete dialog — opened by ?delete=1 search param. */}
      {deleteOpen && (
        <DeleteDialog
          detail={detail}
          onClose={closeDelete}
          onDeleted={() => navigate("/agents")}
        />
      )}

      {/* Publish-as-template dialog (m74.6) — opened by the Share action in the
          header band (RBAC-gated: update agentdeployments). */}
      {publishOpen && (
        <PublishTemplateDialog
          agentNamespace={detail.namespace}
          agentName={detail.name}
          alreadyPublished={publishedState !== null}
          onClose={() => setPublishOpen(false)}
          onDone={(res, visibility) => {
            setPublishOpen(false);
            // U7: store session-level published state so header shows the badge.
            setPublishedState({ version: res.version ?? "1", visibility });
          }}
        />
      )}
    </div>
  );
}

// ── The page band (§4.3 / §5.17) ────────────────────────────────────────────
// One PageHeader, not a hand-rolled header: the h1 is the resource NAME, so it
// is mono and it TRUNCATES on one line with the full value in `title` (§4.5) —
// a 63-character Kubernetes name clips, it never wraps and it is never
// `break-all`ed. Provenance and publication ride in the status group next to
// the name because they describe WHAT this agent is; readiness and drift are
// the only health signals there.
//
// The write affordances go through the STRUCTURED `actions` list rather than
// `actionsSlot`, because four buttons do not fit a 360px band: the structured
// list is what lets §4.3's collapse keep the primary (Edit) and the destructive
// (Delete) and fold Share/Unpublish into the "⋯" menu below `lg`.
function AgentBand({
  detail,
  activeTab,
  onTab,
  onEdit,
  onDelete,
  onPublish,
  publishedState,
  onUnpublish,
}: {
  detail: AgentDetailResponse;
  activeTab: Tab;
  onTab: (t: Tab) => void;
  onEdit: () => void;
  onDelete: () => void;
  onPublish: () => void;
  // U7: in-session published state — shown as a badge; if null, the agent is not (yet) published.
  publishedState: { version: string; visibility: string } | null;
  onUnpublish: () => void;
}) {
  const { can } = useCapabilities();
  const canEdit = can(RES_AGENTS, "update");
  const canDelete = can(RES_AGENTS, "delete");
  // Publish-as-template is gated on agent update rights (the publisher must own
  // the agent). Display-only — the API is the real RBAC gate (ADR 0011).
  const canPublish = can(RES_AGENTS, "update");

  const actions: PageHeaderAction[] = [];
  if (canPublish) {
    actions.push({
      id: "publish",
      label: publishedState ? "Share new version" : "Share as template",
      icon: Share2,
      variant: "outline",
      onClick: onPublish,
    });
    if (publishedState) {
      actions.push({
        id: "unpublish",
        label: "Unpublish",
        icon: X,
        variant: "outline",
        onClick: onUnpublish,
      });
    }
  }
  if (canEdit) {
    // THE primary — the action that survives §4.3's collapse alongside Delete.
    actions.push({ id: "edit", label: "Edit", icon: Pencil, variant: "outline", primary: true, onClick: onEdit });
  }
  if (canDelete) {
    actions.push({ id: "delete", label: "Delete", icon: Trash2, variant: "destructive", onClick: onDelete });
  }

  const meta = [detail.namespace, detail.latestVersion, detail.executionModel]
    .filter(Boolean)
    .join(" · ");

  return (
    <PageHeader
      breadcrumb={[{ label: "Agents", to: "/agents" }, { label: detail.name }]}
      title={detail.name}
      titleMono
      status={
        // The header's own status cluster. Named because "the agent's status" has to be
        // addressable on a page that renders several StatusBadges (versions, gates,
        // reachability) — an unscoped lookup finds whichever happens to come first in
        // the DOM and reports its verdict as the agent's (M153).
        <span className="flex flex-wrap items-center gap-1.5" data-testid="agent-status">
          <StatusBadge
            ready={detail.ready}
            phase={detail.phase}
            reason={readyReason(detail.conditions)}
          />
          {detail.drift && (
            <Badge
              variant="warn"
              data-testid="drift-badge"
              title="The live spec has diverged from the console config (ADR 0017)."
            >
              drift
            </Badge>
          )}
          {detail.managedOutsideUI && (
            <Badge
              variant="muted"
              data-testid="managed-outside-badge"
              title="Created outside the console (e.g. kubectl) — edits are limited to safe fields."
            >
              external
            </Badge>
          )}
          {publishedState && (
            <Badge
              variant="muted"
              data-testid="published-badge"
              title="This agent is published to the template gallery."
            >
              {`Published · ${publishedState.visibility} · v${publishedState.version}`}
            </Badge>
          )}
        </span>
      }
      meta={meta || undefined}
      actions={actions}
      tabs={TABS.map((t) => ({ id: t, label: t, current: activeTab === t }))}
      onTabChange={(id) => onTab(id as Tab)}
    />
  );
}

/** The Ready condition's reason, so the status chip speaks the controller's word. */
function readyReason(conditions: AgentCondition[]): string | undefined {
  return conditions.find((c) => c.type === "Ready")?.reason || undefined;
}

// U12: fork lineage — "forked from ns/name @ version", when the provenance
// labels are set. Machine-owned coordinates, so mono (§4.8).
function ForkLineage({ labels }: { labels?: Record<string, string> }) {
  const originNs = labels?.[LABEL_FORK_ORIGIN_NS];
  const originName = labels?.[LABEL_FORK_ORIGIN_NAME];
  const originVersion = labels?.[LABEL_FORK_ORIGIN_VERSION];
  if (!originNs || !originName) return null;
  return (
    <p
      className="flex flex-wrap items-center gap-1.5 text-sm text-faint"
      data-testid="fork-lineage"
    >
      <GitFork aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-ghost" />
      Forked from{" "}
      <span className="font-mono text-xs text-secondary-foreground">
        {originNs}/{originName}
      </span>
      {originVersion && (
        <span className="font-mono text-xs text-secondary-foreground">@ {originVersion}</span>
      )}
    </p>
  );
}

// ── The lifecycle spine (§5.20) ─────────────────────────────────────────────
//
// The strip is a POSITION CLAIM, so every cell is read off something the
// backend actually sent. Where it cannot be read, the cell renders §7.1's "not
// yet known" (the component's own copy) rather than a plausible-sounding
// placeholder — and when the agent's stage itself cannot be placed, NO stage is
// lit and a QuietNote says why. A guessed position is the one thing §5.20 says
// this component must never draw.

/** Gate phases that mean a PERSON is between the agent and production. */
const GATE_HUMAN = /(canary|await|promot|approv|hold|block)/i;
/** Gate phases that mean the gate stopped it. */
const GATE_STOPPED = /(fail|error|reject|denied)/i;

/**
 * Where this agent sits in its life, or `null` when the payload cannot place
 * it. Read only from real fields: `isDraft`, the eval-gate projection, the
 * readiness flags, and the RegressionDetected condition.
 */
export function lifecycleStage(detail: AgentDetailResponse): LifecycleStage | null {
  if (detail.isDraft) return "Build";

  const gatePhase = detail.gate?.phase ?? "";
  // A human gate outranks everything else: a canary awaiting a promotion
  // decision is being GOVERNED, however healthy the revision underneath is.
  if (GATE_HUMAN.test(gatePhase) || GATE_STOPPED.test(gatePhase)) return "Govern";
  if (resolveStatus(detail.ready, detail.phase, readyReason(detail.conditions)).tone === "waiting") {
    return "Govern";
  }

  if (detail.ready) {
    // Improve is claimed only when the improvement loop has actually reported —
    // a RegressionDetected condition is the durable "the loop is watching this"
    // fact in this payload. Serving with no such signal is Ship, not Improve.
    return detail.conditions.some((c) => c.type === "RegressionDetected") ? "Improve" : "Ship";
  }

  // Not ready and not a draft. If it has a revision it is being rolled out;
  // if it has nothing at all, the payload cannot place it and we say so.
  if (detail.versions.length > 0 || detail.latestVersion) return "Ship";
  if (!detail.phase && detail.conditions.length === 0) return null;
  return "Build";
}

function AgentLifecycle({ detail }: { detail: AgentDetailResponse }) {
  const stage = lifecycleStage(detail);
  const gatePhase = detail.gate?.phase ?? "";
  const versionCount = detail.versions.length;
  const regression = detail.conditions.find((c) => c.type === "RegressionDetected");

  const cells: LifecycleStageCell[] = [
    {
      name: "Build",
      active: stage === "Build",
      fact:
        versionCount > 0 ? (
          <>
            <span className={lifecycleFactNumber}>{versionCount}</span>
            {versionCount === 1 ? " version built" : " versions built"}
          </>
        ) : detail.isDraft ? (
          "draft — not built yet"
        ) : undefined,
    },
    {
      name: "Govern",
      active: stage === "Govern",
      fact: detail.guardrailPolicyRef ? (
        <>guardrails: {detail.guardrailPolicyRef}</>
      ) : gatePhase ? (
        <>gate: {gatePhase}</>
      ) : detail.gate ? (
        "gated, phase not reported"
      ) : (
        "no policy, no eval gate"
      ),
    },
    {
      name: "Ship",
      active: stage === "Ship",
      fact: detail.ready
        ? detail.latestVersion
          ? <>serving {detail.latestVersion}</>
          : "serving"
        : detail.phase
          ? <>not serving — {detail.phase.toLowerCase()}</>
          : undefined,
    },
    {
      name: "Improve",
      active: stage === "Improve",
      // Only the condition can answer this one. No condition, no claim.
      fact: regression
        ? regression.status === "True"
          ? "a regression was detected"
          : "no regression detected"
        : undefined,
    },
  ];

  return (
    <div className="space-y-3">
      <LifecycleStrip
        stages={cells}
        label={
          stage
            ? `Lifecycle: ${stage}`
            : "Lifecycle — this agent's stage is not yet known"
        }
      />
      {stage === null && (
        <QuietNote title="No stage is lit, on purpose.">
          This agent has reported no phase and no status conditions yet, so the
          console cannot say where it sits in its life. The strip shows the four
          stages with no position marked rather than guessing one — the facts
          appear as the controller reports them.
        </QuietNote>
      )}
    </div>
  );
}

// ── The rail: the register that governs this agent (§4.7 / §5.25) ───────────
// A kv register and one meter — never a table (§4.7). Every row that the
// backend did not answer states its absence in words; none is ever blank.
function AgentRail({ detail }: { detail: AgentDetailResponse }) {
  // A guardrail policy that stopped the agent coming up is surfaced NEXT TO the
  // ref, so the reader does not have to correlate it with the conditions panel.
  const guardrailReason = detail.conditions.find(
    (c) =>
      c.type === "Ready" &&
      c.status !== "True" &&
      ["GuardrailPolicyNotFound", "GuardrailPolicyInvalid"].includes(c.reason),
  )?.reason;

  const linkClass = "border-b border-accent text-primary hover:border-primary";

  const governs: KeyValueItem[] = [
    {
      key: "Workspace",
      // The namespace links to the tenant that governs it (m49.4) — the console
      // rule is that a resource name is a link, never dead-end text.
      value: (
        <Link
          to={`/tenants?q=${encodeURIComponent(detail.namespace)}`}
          data-testid="agent-namespace-link"
          title={`View the tenant governing namespace "${detail.namespace}"`}
          className={linkClass}
        >
          {detail.namespace}
        </Link>
      ),
      absent: "not recorded",
    },
    {
      key: "Model route",
      value: detail.modelRoute ? (
        <Link
          to={`/routes/${encodeURIComponent(detail.namespace)}/${encodeURIComponent(detail.modelRoute)}`}
          className={linkClass}
          data-testid="agent-modelroute-link"
        >
          {detail.modelRoute}
        </Link>
      ) : undefined,
      absent: "no model route",
      title: detail.modelRoute
        ? undefined
        : "No route is attached, so this agent has no model to run on.",
    },
    {
      key: "Prompt",
      value: detail.promptRef ? (
        <Link to={navRoute("prompts")} className={linkClass} data-testid="agent-promptref-link">
          {detail.promptRef}
        </Link>
      ) : undefined,
      absent: "not attached",
    },
    {
      key: "Guardrails",
      value: detail.guardrailPolicyRef ? (
        <span className="inline-flex flex-wrap items-center justify-end gap-1.5">
          <Link
            to={navRoute("guardrails")}
            className={linkClass}
            data-testid="agent-guardrail-policy-link"
          >
            {detail.guardrailPolicyRef}
          </Link>
          {guardrailReason && (
            <Badge
              variant="crit"
              data-testid="agent-guardrail-notready-reason"
              title={guardrailReason}
            >
              {humanizeStatusReason(guardrailReason)}
            </Badge>
          )}
        </span>
      ) : undefined,
      absent: "not attached",
    },
    {
      key: "Endpoint",
      value: detail.url ? (
        <a
          href={detail.url}
          target="_blank"
          rel="noreferrer"
          title={detail.url}
          data-testid="agent-url"
          className={`inline-flex max-w-full items-center gap-1 ${linkClass}`}
        >
          {/* One line, end-ellipsis, full value in `title` (§4.5) — a URL is
              never allowed to set the rail's width. */}
          <span className="truncate">{hostOf(detail.url)}</span>
          <ExternalLink aria-hidden="true" className="h-3 w-3 shrink-0" />
        </a>
      ) : undefined,
      absent: "not serving yet",
    },
    {
      key: "Image",
      value: detail.image ? <span className="block truncate">{detail.image}</span> : undefined,
      title: detail.image || undefined,
      absent: "not recorded",
    },
    { key: "Revision", value: detail.latestVersion, absent: "not yet built" },
    { key: "Execution", value: detail.executionModel, mono: false, absent: "not recorded" },
    { key: "Role", value: detail.role, mono: false, absent: "not set" },
  ];

  return (
    <div className="min-w-0 space-y-5">
      <Card className="min-w-0">
        <PanelHeader title="Who governs it" />
        <CardContent>
          <KeyValueList items={governs} />
        </CardContent>
      </Card>

      <Card className="min-w-0">
        <PanelHeader title="What bounds it" />
        <CardContent className="space-y-3">
          {/* No live replica count is on this endpoint, so the meter draws the
              real bound with no fill: an empty track claims nothing, whereas a
              zero-width fill labelled 0 would claim the agent is scaled down
              (§5.24). The Meter emits the "unknown, not zero" note itself. */}
          <Meter
            label="replicas"
            used={UNKNOWN}
            cap={detail.scaling.max}
            thing="agent"
          />
          <p className="text-sm text-faint">
            {detail.scaling.min > 0 ? (
              <>
                <span className="font-mono tabular-nums text-secondary-foreground">
                  {detail.scaling.min}
                </span>{" "}
                {detail.scaling.min === 1 ? "replica is" : "replicas are"} always kept
                warm, so there is no cold start.
              </>
            ) : (
              "The floor is zero: with no traffic it scales all the way down and cold-starts on the next request."
            )}
          </p>
        </CardContent>
      </Card>
    </div>
  );
}

/** The host of a URL, for a rail cell too narrow to carry the whole thing. */
function hostOf(url: string): string {
  const stripped = url.replace(/^[a-z][a-z0-9+.-]*:\/\//i, "");
  const slash = stripped.indexOf("/");
  return slash > 0 ? stripped.slice(0, slash) : stripped;
}

// ── "What it can reach" (§6.2) — the page's flagship panel ──────────────────
//
// Three states, and the whole point is that they are three DIFFERENT things:
//
//   unresolved   crit    the controller could not resolve the reference. This
//                        one is broken and the panel is not quiet about it.
//   never called dashed  declared and resolved, but this agent has never come
//                `open`  up, so nothing has exercised it. NOT a failure and it
//                        must never wear a failure hue (§2.2 draft/idle).
//   working      ok      the binding resolves and the agent is up, so the path
//                        to it is live.
//
// What the payload does NOT carry is per-tool call counts — those are span-level
// facts in the trace backend. So `working` is deliberately defined as "resolves
// and is reachable", and the QuietNote says so out loud rather than letting a
// green tag imply a measurement nobody made.
type ReachState = "working" | "never-called" | "unresolved";

const REACH_TAG: Record<
  ReachState,
  { variant: "ok" | "open" | "crit"; label: string; title: string }
> = {
  working: {
    variant: "ok",
    label: "working",
    title:
      "The controller resolved this binding and the agent is up, so the path to it is live. This is not a count of calls.",
  },
  "never-called": {
    variant: "open",
    label: "never called",
    title:
      "Declared and resolved, but this agent has never come up — so nothing has exercised it yet. This is not a failure.",
  },
  unresolved: {
    variant: "crit",
    label: "unresolved",
    title:
      "The controller could not resolve this reference. The agent cannot call it until that is fixed.",
  },
};

function reachState(b: AgentBinding, agentIsUp: boolean): ReachState {
  if (!b.ready) return "unresolved";
  return agentIsUp ? "working" : "never-called";
}

/** How many bindings the summary shows before handing off to Equipment. */
const REACH_PREVIEW = 8;

function reachClosingLine(total: number, broken: number): string {
  if (total === 0) return "";
  if (broken === 0) {
    return total === 1
      ? "One binding, and it resolves."
      : `All ${total} of them resolve. Nothing here is blocking the agent.`;
  }
  if (broken === total) {
    return total === 1
      ? "The one binding it has does not resolve."
      : `None of the ${total} resolve — the agent can reach nothing it was wired to.`;
  }
  return `${broken} of the ${total} do not resolve. The other ${total - broken} are fine.`;
}

function ReachPanel({
  detail,
  onSeeAll,
}: {
  detail: AgentDetailResponse;
  onSeeAll: () => void;
}) {
  const bindings = detail.bindings;
  // "Up" is the strongest statement this payload supports about exercise: an
  // agent that has never reached Ready cannot have called anything.
  const agentIsUp = detail.ready && !detail.isDraft;
  const broken = bindings.filter((b) => !b.ready).length;
  const shown = bindings.slice(0, REACH_PREVIEW);
  const hidden = bindings.length - shown.length;
  // The controller's own words about the bindings, when it has any — better
  // evidence than anything this page could infer per-row.
  const bindingsCondition = detail.conditions.find(
    (c) => c.type === "BindingsReady" && c.status !== "True",
  );

  const seen = new Set<string>();
  const items: KeyValueItem[] = shown.map((b) => {
    const tool = b.detail || b.name;
    const via = b.server || b.kind;
    let label = via ? `${via} / ${tool}` : tool;
    if (seen.has(label)) label = `${label} · ${b.name}`;
    seen.add(label);
    const tag = REACH_TAG[reachState(b, agentIsUp)];
    return {
      key: label,
      title: label,
      value: (
        <Badge variant={tag.variant} data-testid={`reach-${b.name}`} title={tag.title}>
          {tag.label}
        </Badge>
      ),
    };
  });

  return (
    <Card className="min-w-0" data-testid="reach-panel">
      <PanelHeader
        title="What it can reach"
        meta={
          bindings.length === 0
            ? undefined
            : `${bindings.length} binding${bindings.length === 1 ? "" : "s"}`
        }
      />
      <CardContent>
        <p className="mb-4 max-w-[66ch] text-sm text-secondary-foreground">
          Everything this agent is wired to. It holds no credentials of its own —
          every tool is called through its egress sidecar, which attaches the
          credential at call time, so nothing on this page is a secret and none is
          stored on the agent.
        </p>

        {bindings.length === 0 ? (
          <QuietNote title="Nothing is bound to this agent yet.">
            It has no tools and no memory, so it can only answer from the model
            and its prompt. Bind a tool from an MCP server to give it something
            to reach.
            <span className="mt-3 block">
              <NextStepLink label="Bind a tool" onClick={onSeeAll} />
            </span>
          </QuietNote>
        ) : (
          <>
            {broken > 0 && (
              // A crit RULE, not a crit fill: §2.2 allows exactly two full-bleed
              // semantic surfaces console-wide and this is not one of them.
              <div
                className="mb-4 border border-border border-l-2 border-l-destructive bg-surface-2 px-4 py-3"
                data-testid="reach-unresolved-note"
              >
                <p className="text-sm text-secondary-foreground">
                  <span className="font-mono tabular-nums font-semibold text-destructive">
                    {broken}
                  </span>{" "}
                  of these {bindings.length === 1 ? "does" : "do"} not resolve. Until
                  that is fixed the agent cannot call {broken === 1 ? "it" : "them"} —
                  a call that reaches an unresolved binding fails at the sidecar.
                </p>
                {bindingsCondition?.message && (
                  <p className="mt-2 font-mono text-xs text-faint">
                    {bindingsCondition.message}
                  </p>
                )}
                <span className="mt-3 block">
                  <NextStepLink label="Fix the bindings" tone="crit" onClick={onSeeAll} />
                </span>
              </div>
            )}

            <KeyValueList items={items} />

            {hidden > 0 && (
              <p className="mt-3">
                <NextStepLink label={`See all ${bindings.length}`} onClick={onSeeAll} />
              </p>
            )}

            <ClosingNote>{reachClosingLine(bindings.length, broken)}</ClosingNote>

            <QuietNote className="mt-4" title="Call counts aren’t on this endpoint.">
              This panel reads what the agent DECLARES and whether the controller
              resolved each reference. Whether a tool has actually been called is a
              span-level fact that lives in the trace backend — so “working” means
              the binding resolves and the agent is up, not that we counted a call.
              Nothing here is estimated.
            </QuietNote>
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ── The needs-rebinding notice (m74.6 / U5 / U14) ───────────────────────────
// A forked agent whose references did not survive the fork. This is a real
// problem with a real repair, so it is a warn RULE with itemized actions — not
// a QuietNote (nothing is merely unconfigured) and not a filled amber banner
// (§2.2: hues annotate, they do not flood).
function RebindNotice({
  detail,
  refs,
  onGoEquipment,
}: {
  detail: AgentDetailResponse;
  refs: string[];
  onGoEquipment: () => void;
}) {
  const linkClass = "border-b border-warning-surface text-warning hover:border-warning";
  return (
    <div
      className="border border-border border-l-2 border-l-warning bg-surface-2 px-5 py-4"
      role="alert"
      data-testid="needs-rebinding-banner"
    >
      <p className="font-serif text-md font-medium">
        This fork can’t run until its references are connected.
      </p>
      <p className="mt-1 max-w-[66ch] text-sm text-secondary-foreground">
        It was forked from a template, and the resources the template pointed at
        do not exist here. Connect the ones below and the controller clears this
        notice on its next pass.
      </p>
      {/* U5 + U14: itemize the ACTUAL dangling refs (when the BFF recorded them) with the
          right repair action per category — no more generic "review and bind tools" line
          that showed even when nothing tool-shaped dangled. */}
      <ul className="mt-3 space-y-1.5 text-sm" data-testid="rebind-ref-list">
        {refs.length > 0 ? (
          refs.map((ref, i) => {
            const isRoute = ref.startsWith("model route:");
            const isPrompt = ref.startsWith("prompt:");
            const isEval = ref.startsWith("evalSuite:");
            const isTool = !isRoute && !isPrompt && !isEval;
            return (
              <li
                key={`${ref}-${i}`}
                className="flex flex-wrap items-center gap-2"
                data-testid={`rebind-ref-${i}`}
              >
                <ChevronRight aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-ghost" />
                <span className="font-mono text-xs text-secondary-foreground">{ref}</span>
                {isRoute && (
                  <Link to="/routes" className={linkClass} data-testid="rebind-model-route-link">
                    connect a model route
                  </Link>
                )}
                {isTool && (
                  <button
                    type="button"
                    onClick={onGoEquipment}
                    className={linkClass}
                    data-testid="rebind-bindings-tab-link"
                  >
                    bind it in the Bindings tab
                  </button>
                )}
                {isPrompt && (
                  <Link to={navRoute("prompts")} className={linkClass}>
                    add the prompt
                  </Link>
                )}
                {isEval && (
                  <Link to="/evals" className={linkClass}>
                    add the eval suite
                  </Link>
                )}
              </li>
            );
          })
        ) : (
          // Fallback for an older fork (flag set, no ref annotation): generic guidance.
          <>
            {!detail.modelRoute && (
              <li className="flex flex-wrap items-center gap-2">
                <ChevronRight aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-ghost" />
                <Link to="/routes" className={linkClass} data-testid="rebind-model-route-link">
                  Connect a model route
                </Link>
                <span className="text-xs text-faint">— required to run</span>
              </li>
            )}
            <li className="flex flex-wrap items-center gap-2">
              <ChevronRight aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-ghost" />
              <button
                type="button"
                onClick={onGoEquipment}
                className={linkClass}
                data-testid="rebind-bindings-tab-link"
              >
                Review and bind tools in the Bindings tab
              </button>
            </li>
          </>
        )}
      </ul>
    </div>
  );
}

// ── Edit Wizard (m15.11) ──────────────────────────────────────────────────────
// Two modes:
//   • Console-managed (managedOutsideUI=false/absent): full round-trip — all
//     simplified spec fields are editable.
//   • Managed outside UI (managedOutsideUI=true): safe fields only (image,
//     scaling, modelRoute, systemPrompt) — the rest are read-only with a note.
//     The BFF applies a degraded safe-field patch (ADR 0017).
// On drift (detail.drift=true): warn the user before submit that the edit will
// overwrite any drift (i.e. the live CRD diverges from the last console spec).

type EditForm = {
  image: string;
  modelRoute: string;
  systemPrompt: string;
  scalingMin: string;
  scalingMax: string;
  executionModel: string;
  role: string;
};

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

function EditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: AgentDetailResponse;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const isManaged = !detail.managedOutsideUI;
  const hasDrift = detail.drift ?? false;

  const [form, setForm] = React.useState<EditForm>({
    image: detail.image ?? "",
    modelRoute: "",
    systemPrompt: "",
    scalingMin: String(detail.scaling.min),
    scalingMax: String(detail.scaling.max),
    executionModel: detail.executionModel ?? "",
    role: detail.role ?? "",
  });
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  // Drift-overwrite confirmation: set when the user tries to submit with drift.
  const [confirmDriftOverwrite, setConfirmDriftOverwrite] = React.useState(false);

  function set<K extends keyof EditForm>(k: K, v: EditForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      const spec: AgentSimplifiedSpec = {
        image: form.image.trim() || undefined,
        modelRoute: form.modelRoute.trim() || undefined,
        systemPrompt: form.systemPrompt.trim() || undefined,
        scaling: {
          min: parseInt(form.scalingMin, 10) || 0,
          max: parseInt(form.scalingMax, 10) || 1,
        },
        // Full round-trip fields only for console-managed agents.
        ...(!detail.managedOutsideUI
          ? {
              executionModel: form.executionModel.trim() || undefined,
              role: form.role.trim() || undefined,
            }
          : {}),
      };
      await api.updateAgent(detail.namespace, detail.name, spec);
      toast({
        variant: "success",
        title: "Agent updated",
        description: `${detail.name} saved successfully.`,
      });
      onSaved();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSaveState({
        kind: "error",
        message: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  function onFinish() {
    // If there's drift, confirm before overwriting.
    if (hasDrift && !confirmDriftOverwrite) {
      setConfirmDriftOverwrite(true);
      return;
    }
    void doSave();
  }

  // Step 1: Safe fields (image, scaling, modelRoute, systemPrompt) — always
  // shown regardless of managedOutsideUI.
  const safeFieldsStep: WizardStep = {
    id: "safe-fields",
    title: "Image & scaling",
    description: "Editable on all agents",
    content: (
      <div className="space-y-4">
        {detail.managedOutsideUI && (
          <div
            className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
            data-testid="managed-outside-note"
          >
            <p className="font-medium">Managed outside the UI</p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              This agent was created or last modified outside the console. Only safe
              fields (image, scaling, model route, system prompt) are editable here.
              Other fields are read-only to avoid overwriting your configuration.
            </p>
          </div>
        )}
        <FormField id="edit-image" label="Image">
          <Input
            id="edit-image"
            value={form.image}
            onChange={(e) => set("image", e.target.value)}
            placeholder="ghcr.io/acme/agent:v1"
            data-testid="edit-image"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="edit-min" label="Min replicas">
            <Input
              id="edit-min"
              inputMode="numeric"
              value={form.scalingMin}
              onChange={(e) => set("scalingMin", e.target.value)}
              data-testid="edit-scaling-min"
            />
          </FormField>
          <FormField id="edit-max" label="Max replicas">
            <Input
              id="edit-max"
              inputMode="numeric"
              value={form.scalingMax}
              onChange={(e) => set("scalingMax", e.target.value)}
              data-testid="edit-scaling-max"
            />
          </FormField>
        </div>
        <FormField id="edit-model-route" label="Model route">
          <Input
            id="edit-model-route"
            value={form.modelRoute}
            onChange={(e) => set("modelRoute", e.target.value)}
            placeholder="default-model"
            data-testid="edit-model-route"
          />
        </FormField>
        <FormField id="edit-system-prompt" label="System prompt">
          <Textarea
            id="edit-system-prompt"
            rows={4}
            value={form.systemPrompt}
            onChange={(e) => set("systemPrompt", e.target.value)}
            placeholder="You are a support agent…"
            data-testid="edit-system-prompt"
          />
        </FormField>
      </div>
    ),
  };

  // Step 2: Full round-trip fields — only for console-managed agents. For
  // managedOutsideUI agents, shown as read-only with a note.
  const fullFieldsStep: WizardStep = {
    id: "full-fields",
    title: "Execution & role",
    description: isManaged ? "Full round-trip" : "Read-only (managed outside UI)",
    content: (
      <div className="space-y-4">
        {!isManaged && (
          <div
            className="rounded-md border border-border bg-surface-2 px-3 py-2 text-xs text-muted-foreground"
            data-testid="readonly-fields-note"
          >
            These fields are read-only because this agent is managed outside the
            UI. Edit them via the tool that manages this agent.
          </div>
        )}
        <FormField id="edit-execution-model" label="Execution model">
          <Select
            id="edit-execution-model"
            value={form.executionModel}
            onChange={(e) => set("executionModel", e.target.value)}
            disabled={!isManaged}
            data-testid="edit-execution-model"
          >
            <option value="">— unchanged —</option>
            <option value="serving">serving (request-driven)</option>
            <option value="eventing">eventing (broker-triggered)</option>
            <option value="job">job (one-shot)</option>
          </Select>
        </FormField>
        <FormField id="edit-role" label="Role">
          <Input
            id="edit-role"
            value={form.role}
            onChange={(e) => set("role", e.target.value)}
            disabled={!isManaged}
            data-testid="edit-role"
          />
        </FormField>
      </div>
    ),
  };

  // Review step.
  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {hasDrift && (
          <div
            className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
            data-testid="drift-overwrite-warning"
          >
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
            <p>
              This agent has <strong>drift</strong> — the live CRD diverges from the
              last console-applied spec. Saving here will overwrite that drift with the
              values you've entered.
            </p>
          </div>
        )}
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this agent"
            description="Your account can't update agents in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="edit-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <ReviewRow k="Image" v={form.image || "—"} />
          <ReviewRow k="Scaling" v={`${form.scalingMin} – ${form.scalingMax}`} />
          {form.modelRoute && <ReviewRow k="Model route" v={form.modelRoute} />}
          {form.systemPrompt && (
            <ReviewRow k="System prompt" v={truncate(form.systemPrompt, 80)} />
          )}
          {isManaged && form.executionModel && (
            <ReviewRow k="Execution" v={form.executionModel} />
          )}
          {isManaged && form.role && <ReviewRow k="Role" v={form.role} />}
        </dl>
      </div>
    ),
  };

  const steps = isManaged
    ? [safeFieldsStep, fullFieldsStep, reviewStep]
    : [safeFieldsStep, reviewStep];

  return (
    <>
      <Wizard
        steps={steps}
        current={current}
        onStepChange={setCurrent}
        busy={saveState.kind === "saving"}
        onFinish={onFinish}
        finishLabel="Save changes"
        onCancel={onClose}
        dirty={form.image !== (detail.image ?? "") ||
          form.scalingMin !== String(detail.scaling.min) ||
          form.scalingMax !== String(detail.scaling.max)}
      />
      {/* Drift-overwrite confirmation dialog */}
      <ConfirmDialog
        open={confirmDriftOverwrite}
        onCancel={() => setConfirmDriftOverwrite(false)}
        onConfirm={() => {
          setConfirmDriftOverwrite(false);
          void doSave();
        }}
        title="Overwrite drift?"
        description="The live CRD has drifted from the last console-applied spec. Saving will overwrite it with your edits. Changes made outside the console will be lost."
        confirmLabel="Save and overwrite drift"
        destructive={false}
      />
    </>
  );
}

function ReviewRow({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex items-start justify-between gap-4 px-3 py-2">
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="text-right">{v}</dd>
    </div>
  );
}

// ── Delete Dialog (m15.11) ───────────────────────────────────────────────────
// Loads agentReferences (delete-impact preview) before showing the typed-name
// ConfirmDialog. The impact section shows what GC'd vs what's orphaned. On
// confirm → deleteAgent → navigate back to the agents list.

type RefsLoad =
  | { kind: "loading" }
  | { kind: "ready"; references: AgentReference[] }
  | { kind: "error"; message: string };

function DeleteDialog({
  detail,
  onClose,
  onDeleted,
}: {
  detail: AgentDetailResponse;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilities();
  const [refs, setRefs] = React.useState<RefsLoad>({ kind: "loading" });
  const [deleting, setDeleting] = React.useState(false);
  // U4: when this agent is published, offer to ALSO unpublish its template — pre-checked (the common
  // intent), but visible + uncheckable to preserve ADR 0068's publish-then-delete-the-dev-agent flow.
  const isPublished = detail.published != null;
  const [alsoUnpublish, setAlsoUnpublish] = React.useState(true);

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .agentReferences(detail.namespace, detail.name, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRefs({ kind: "ready", references: res.references });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setRefs({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load delete impact",
        });
      });
    return () => controller.abort();
  }, [detail.namespace, detail.name]);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.deleteAgent(detail.namespace, detail.name, isPublished && alsoUnpublish);
      toast({
        variant: "success",
        title: "Agent deleted",
        description:
          isPublished && alsoUnpublish
            ? `${detail.name} has been removed and its template unpublished.`
            : `${detail.name} has been removed.`,
      });
      onDeleted();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      toast({
        variant: "error",
        title: "Delete failed",
        description: err instanceof Error ? err.message : "delete failed",
      });
      setDeleting(false);
      onClose();
    }
  }

  // Delete-impact preview rendered inside the ConfirmDialog's `impact` slot.
  const impact =
    refs.kind === "loading" ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-loading">
        Loading delete impact…
      </p>
    ) : refs.kind === "error" ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-error">
        Couldn't load delete impact ({refs.message}) — proceeding will still
        delete the agent.
      </p>
    ) : refs.references.length === 0 ? (
      <p className="text-sm text-muted-foreground" data-testid="refs-empty">
        No referencing objects found.
      </p>
    ) : (
      <div data-testid="refs-list">
        <p className="mb-2 text-xs font-medium text-muted-foreground">
          Objects affected by this delete:
        </p>
        <ul className="space-y-1.5">
          {refs.references.map((r) => (
            <li
              key={`${r.kind}/${r.namespace}/${r.name}`}
              className="flex items-center justify-between gap-2 text-sm"
              data-testid={`ref-${r.name}`}
            >
              <span>
                <span className="font-mono text-xs">{r.kind}/{r.name}</span>
                {r.namespace && r.namespace !== detail.namespace && (
                  <span className="ml-1 text-xs text-muted-foreground">({r.namespace})</span>
                )}
              </span>
              <Badge
                variant={r.disposition === "gc" ? "warn" : "muted"}
              >
                {r.disposition === "gc" ? "will be deleted" : "will be orphaned"}
              </Badge>
            </li>
          ))}
        </ul>
      </div>
    );

  // U4: when the agent is published, append an "also unpublish the template" checkbox to the impact
  // slot — pre-checked (default intent) but the user can uncheck it to keep the template in the
  // gallery (ADR 0068 registry semantics). Shown only when there is a published template to withdraw.
  const impactWithUnpublish = (
    <>
      {impact}
      {isPublished && (
        <label
          className="mt-3 flex items-start gap-2 text-sm"
          data-testid="delete-unpublish-row"
        >
          <input
            type="checkbox"
            checked={alsoUnpublish}
            onChange={(e) => setAlsoUnpublish(e.target.checked)}
            className="mt-0.5"
            data-testid="delete-unpublish-checkbox"
          />
          <span>
            Also unpublish this agent's template
            {detail.published?.version ? ` (v${detail.published.version})` : ""} from the gallery.
            <span className="block text-xs text-muted-foreground">
              Uncheck to keep the published template available for others to fork.
            </span>
          </span>
        </label>
      )}
    </>
  );

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${detail.name}?`}
      description={`This will permanently delete the agent "${detail.name}" and may affect related objects (see below).`}
      confirmText={detail.name}
      confirmLabel="Delete agent"
      busy={deleting}
      impact={impactWithUnpublish}
    />
  );
}

// ── Overview: what it reaches, how it came up, and a way to try it ──────────
// The reach panel leads because it is the question the page exists to answer.
// The condition story follows (why it is or is not serving), then the two ways
// to actually exercise it — from here, or from code.
function OverviewTab({
  detail,
  onTraced,
  onGoEquipment,
}: {
  detail: AgentDetailResponse;
  onTraced: (traceId: string) => void;
  onGoEquipment: () => void;
}) {
  return (
    <>
      <ReachPanel detail={detail} onSeeAll={onGoEquipment} />

      <ConditionsPanel
        conditions={detail.conditions}
        ready={detail.ready}
        phase={detail.phase}
      />

      <ChatPanel
        ns={detail.namespace}
        name={detail.name}
        ready={detail.ready}
        memoryBound={detail.bindings.some((b) => b.kind === "memory")}
        onTraced={onTraced}
      />

      <UseAgentPanel
        name={detail.name}
        executionModel={detail.executionModel}
        url={detail.url}
        ns={detail.namespace}
      />
    </>
  );
}

// ── Equipment: everything you attach to an agent ────────────────────────────
// Tools, the runtime policy that governs how they are called, memory, scaling,
// and the redaction rules applied to what it records. These were five separate
// tabs; they are one surface because they answer one question — what is this
// agent fitted with?
function EquipmentTab({ detail }: { detail: AgentDetailResponse }) {
  return (
    <>
      <Card className="min-w-0" data-testid="bindings-tab">
        <PanelHeader
          title="Tools and bindings"
          meta={
            detail.bindings.length === 0
              ? undefined
              : `${detail.bindings.length} binding${detail.bindings.length === 1 ? "" : "s"}`
          }
        />
        <CardContent>
          <p className="mb-4 max-w-[66ch] text-sm text-secondary-foreground">
            Every binding, grouped by the MCP server that serves it. Groups stay
            closed until you open one: an agent can bind dozens of tools, and a
            flat list of them is a wall rather than an answer.
          </p>
          <BindingsList
            bindings={detail.bindings}
            agentIsUp={detail.ready && !detail.isDraft}
          />
        </CardContent>
      </Card>

      {detail.runtime && <RuntimeSection runtime={detail.runtime} />}

      <Card className="min-w-0">
        <CardContent className="p-5">
          <MemoryPanel ns={detail.namespace} agentName={detail.name} />
        </CardContent>
      </Card>

      <Card className="min-w-0">
        <CardContent className="p-5">
          <ScalingPanel ns={detail.namespace} agentName={detail.name} />
        </CardContent>
      </Card>

      <RedactionPanel ns={detail.namespace} agentName={detail.name} />
    </>
  );
}

// ── Runs: what it has done, and what it printed while doing it ─────────────
// The live tail is a SECTION here rather than a tab of its own (§6.2 budgets
// this page to five). It is gated on `get pods/log` exactly as the tab was:
// display-only and fail-open, with the API as the real gate (ADR 0011).
function RunsAndOutputTab({
  detail,
  canLogs,
  onInspect,
}: {
  detail: AgentDetailResponse;
  canLogs: boolean;
  onInspect: (traceId: string) => void;
}) {
  return (
    <>
      <div className="min-w-0">
        <SectionHeader
          title="Runs"
          lede="Every traced run this agent has served. Open one to read its spans."
        />
        <AgentRunsTab ns={detail.namespace} name={detail.name} onInspect={onInspect} />
      </div>

      {canLogs && (
        <div className="min-w-0">
          <SectionHeader
            title="Live output"
            lede="What the agent's own process is printing, tailed from its pod. This is not the run record — it is stdout."
          />
          <LogsTab ns={detail.namespace} name={detail.name} ready={detail.ready} />
        </div>
      )}
    </>
  );
}

// ── Versions: the history, and what changed between two of them ────────────
function VersionsTab({ detail }: { detail: AgentDetailResponse }) {
  const versions = detail.versions;
  const items: KeyValueItem[] = versions.map((v) => ({
    key: v,
    value:
      v === detail.latestVersion ? (
        <Badge variant="ok">latest</Badge>
      ) : (
        <span className="text-sm text-faint">superseded</span>
      ),
  }));

  return (
    <Card className="min-w-0" data-testid="versions-list">
      <PanelHeader
        title="Versions"
        meta={
          versions.length === 0
            ? undefined
            : `${versions.length} snapshot${versions.length === 1 ? "" : "s"}`
        }
      />
      <CardContent>
        {versions.length === 0 ? (
          <QuietNote title="No version snapshot has been recorded yet.">
            A snapshot is written each time the deployed spec changes. This agent
            has not produced one — it may never have been applied, or the
            controller has not reconciled it yet. Nothing is missing from this
            page; there is simply no history to show.
          </QuietNote>
        ) : (
          <>
            <KeyValueList items={items} />
            {/* V3: a read-only diff of two snapshots — only useful with ≥2. */}
            {versions.length >= 2 && (
              <VersionDiffPanel
                ns={detail.namespace}
                name={detail.name}
                versions={versions}
                latestVersion={detail.latestVersion}
              />
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ── Runtime section (m65.9, ADR 0058) ────────────────────────────────────────
// Read-only card rendered in the Overview tab when spec.runtime is present AND
// at least one sub-section has content (outputSchemaSet, toolPolicy, resilience).
// When runtime is absent or every sub-section is empty, nothing is rendered —
// no clutter for agents that don't use it.
function RuntimeSection({ runtime }: { runtime: AgentRuntimeDetail }) {
  const [schemaOpen, setSchemaOpen] = React.useState(false);

  const hasContent =
    runtime.outputSchemaSet || runtime.toolPolicy != null || runtime.resilience != null;
  if (!hasContent) return null;

  return (
    <Card className="min-w-0" data-testid="runtime-section">
      <PanelHeader title="Runtime" />
      <CardContent className="space-y-4">
        {/* --- Structured output --- */}
        {runtime.outputSchemaSet && (
          <div data-testid="runtime-output-schema">
            <div className="flex items-center gap-2">
              <span className="text-sm text-muted-foreground">Structured output</span>
              <Badge variant="ok" data-testid="runtime-output-schema-badge">
                ✓ set
              </Badge>
              {/* J6(a) m76.6: outputSchemaSet=true but no body returned — make it
                  clear the schema is configured but not echoed (avoids the false
                  impression that "✓ set" with no expand = no schema). */}
              {!runtime.outputSchema && (
                <span
                  className="text-2xs text-faint"
                  data-testid="runtime-output-schema-not-returned"
                >
                  (content not returned)
                </span>
              )}
            </div>
            {runtime.outputSchema && (
              <details
                open={schemaOpen}
                onToggle={(e) => setSchemaOpen((e.currentTarget as HTMLDetailsElement).open)}
                className="mt-1"
                data-testid="runtime-schema-details"
              >
                <summary className="cursor-pointer text-xs text-primary hover:underline">
                  {schemaOpen ? "Hide schema" : "Show schema"}
                </summary>
                <pre className="mt-1 max-h-40 overflow-y-auto rounded bg-surface-3 p-2 text-xs leading-relaxed">
                  {(() => {
                    try {
                      return JSON.stringify(JSON.parse(runtime.outputSchema), null, 2);
                    } catch {
                      return runtime.outputSchema;
                    }
                  })()}
                </pre>
              </details>
            )}
          </div>
        )}

        {/* --- Tool policy --- */}
        {runtime.toolPolicy && (
          <div data-testid="runtime-tool-policy">
            <div className="mb-1.5 flex items-baseline gap-2">
              <p className="text-sm text-muted-foreground">Tool policy</p>
              {/* J6(c) m76.6: honesty qualifier — tool-policy is an SDK-layer authoring
                  convention (the SDK enforces it inside the agent loop), not a hard
                  platform enforcement boundary at the network/proxy layer. */}
              <span
                className="text-2xs text-faint"
                title="Tool policy is enforced by the agent SDK at runtime, not by a platform proxy. It is an authoring convention, not a hard network-level boundary."
                data-testid="runtime-tool-policy-note"
              >
                SDK-layer convention
              </span>
            </div>
            <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
              <dt className="text-muted-foreground">Default rule</dt>
              <dd>
                <Badge variant="muted">
                  {runtime.toolPolicy.default || "allow"}
                </Badge>
              </dd>
              {runtime.toolPolicy.parallelLimit !== undefined && runtime.toolPolicy.parallelLimit > 0 && (
                <>
                  <dt className="text-muted-foreground">Parallel limit</dt>
                  <dd className="text-sm">{runtime.toolPolicy.parallelLimit} concurrent calls</dd>
                </>
              )}
              {runtime.toolPolicy.forcedChoice && (
                <>
                  <dt className="text-muted-foreground">Forced choice</dt>
                  <dd className="font-mono text-xs">
                    {runtime.toolPolicy.forcedChoice}
                    <span className="ml-1 font-sans text-2xs text-faint">
                      {runtime.toolPolicy.forcedChoice === "auto"
                        ? "(model chooses)"
                        : runtime.toolPolicy.forcedChoice === "required"
                          ? "(must call a tool)"
                          : `(must call ${runtime.toolPolicy.forcedChoice})`}
                    </span>
                  </dd>
                </>
              )}
            </dl>
            {runtime.toolPolicy.overrides.length > 0 && (
              <div className="mt-2" data-testid="runtime-tool-overrides">
                <p className="mb-1 text-xs text-muted-foreground">
                  Per-tool overrides ({runtime.toolPolicy.overrides.length})
                </p>
                <ul className="space-y-1">
                  {runtime.toolPolicy.overrides.map((o) => (
                    <li
                      key={o.name}
                      className="flex items-center gap-2 text-sm"
                      data-testid={`tool-override-${o.name}`}
                    >
                      <span className="font-mono text-xs">{o.name}</span>
                      <Badge variant="muted">
                        {o.rule}
                      </Badge>
                      {o.retryable && (
                        <Badge variant="open">
                          retryable
                        </Badge>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        )}

        {/* --- Resilience --- */}
        {runtime.resilience && (
          <div data-testid="runtime-resilience">
            <p className="mb-1.5 text-sm text-muted-foreground">Resilience</p>
            <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
              {runtime.resilience.modelCall && (
                <>
                  <dt className="text-muted-foreground">Model call</dt>
                  <dd className="text-xs">
                    {[
                      runtime.resilience.modelCall.timeoutSeconds
                        ? `${runtime.resilience.modelCall.timeoutSeconds}s timeout`
                        : null,
                      runtime.resilience.modelCall.maxRetries
                        ? `${runtime.resilience.modelCall.maxRetries} ${runtime.resilience.modelCall.maxRetries === 1 ? "retry" : "retries"}`
                        : null,
                    ]
                      .filter(Boolean)
                      .join(", ") || "—"}
                  </dd>
                </>
              )}
              {runtime.resilience.toolCall && (
                <>
                  <dt className="text-muted-foreground">Tool call</dt>
                  <dd className="text-xs">
                    {[
                      runtime.resilience.toolCall.timeoutSeconds
                        ? `${runtime.resilience.toolCall.timeoutSeconds}s timeout`
                        : null,
                      runtime.resilience.toolCall.maxRetries
                        ? `${runtime.resilience.toolCall.maxRetries} ${runtime.resilience.toolCall.maxRetries === 1 ? "retry" : "retries"}`
                        : null,
                    ]
                      .filter(Boolean)
                      .join(", ") || "—"}
                  </dd>
                  {runtime.resilience.toolCall.circuitBreaker && (
                    <>
                      <dt className="text-muted-foreground">Circuit breaker</dt>
                      <dd className="text-xs" data-testid="runtime-circuit-breaker">
                        opens at {runtime.resilience.toolCall.circuitBreaker.failureThreshold} failures
                        {runtime.resilience.toolCall.circuitBreaker.cooldownSeconds
                          ? `, ${runtime.resilience.toolCall.circuitBreaker.cooldownSeconds}s cooldown`
                          : ""}
                      </dd>
                    </>
                  )}
                </>
              )}
            </dl>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// ── "How it came up" — the status conditions, told as one story (§5.26) ─────
//
// The controller's conditions ARE the agent's story: the route was admitted,
// the bindings resolved, the guardrail policy validated, it went ready. They
// were a hand-rolled dot ladder; they are the kit Timeline now, so a condition
// reads the same way a run's steps do.
//
// Two rules from §5.26 shape the copy. A step's title is a SENTENCE, never an
// event enum — `BindingsReady` is vocabulary the reader should not have to
// learn, so the machine words move to the detail line where they are evidence
// rather than jargon. And the tone comes from the shared status vocabulary, not
// from a local True/False → green/red map: a `Ready=False` whose reason is
// `ToolApprovalRequired` is a HOLD — a person must decide — and §2.4 exists
// because that is the state most often confused with a failure.

/** Condition type → [satisfied sentence, unsatisfied sentence]. */
const CONDITION_SENTENCE: Record<string, [string, string]> = {
  Ready: ["The agent is ready and serving", "The agent is not ready yet"],
  RouteReady: ["Its route was admitted", "Its route has not been admitted"],
  BindingsReady: ["Every binding resolved", "Not every binding resolved"],
  GuardrailsReady: [
    "Its guardrail policy resolved",
    "Its guardrail policy did not resolve",
  ],
  ScalingReady: ["Its scaling policy is in force", "Its scaling policy is not in force"],
};

/** `RevisionMissing` → `revision missing`, for the generic sentence. */
function spacedType(type: string): string {
  const spaced = type.replace(/([a-z0-9])([A-Z])/g, "$1 $2").trim();
  return spaced.charAt(0).toLowerCase() + spaced.slice(1);
}

function conditionSentence(c: AgentCondition): string {
  // RegressionDetected inverts — True is the bad news, and False is the ABSENCE
  // of a regression rather than a check that failed. The old ladder got this
  // right and the distinction survives verbatim.
  if (c.type === "RegressionDetected") {
    return c.status === "True"
      ? "A regression was detected against the baseline"
      : "No regression has been detected";
  }
  const pair = CONDITION_SENTENCE[c.type];
  if (pair) return c.status === "True" ? pair[0] : pair[1];
  return c.status === "True"
    ? `Its ${spacedType(c.type)} check passed`
    : `Its ${spacedType(c.type)} check has not passed`;
}

function conditionTone(c: AgentCondition): TimelineTone {
  if (c.type === "RegressionDetected") return c.status === "True" ? "failed" : "step";
  if (c.status === "True") return "done";
  // "Unknown" is the controller saying it has not decided. That is not a
  // failure and it is not a hold — it is a plain moment in the story.
  if (c.status !== "False") return "step";
  const { tone } = resolveStatus(false, c.type, c.reason);
  if (tone === "waiting") return "hold";
  if (tone === "failed") return "failed";
  return "step";
}

/** Same-day → a clock; older → a date (§4.5). A moment we did not record has none. */
function conditionClock(iso?: string): string | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return undefined;
  const d = new Date(t);
  return d.toDateString() === new Date().toDateString()
    ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function ConditionsPanel({
  conditions,
  ready,
  phase,
}: {
  conditions: AgentCondition[];
  ready: boolean;
  phase: string;
}) {
  const steps: TimelineStep[] = conditions.map((c, i) => ({
    id: `${c.type}-${i}`,
    time: conditionClock(c.lastTransitionTime),
    title: <span data-testid={`condition-${c.type}`}>{conditionSentence(c)}</span>,
    detail:
      c.reason || c.message ? (
        <>
          {c.reason && (
            // Machine words are EVIDENCE here, so they render inline-mono in the
            // detail line rather than becoming the headline (§5.26).
            <span className="font-mono text-xs text-faint">{c.reason}</span>
          )}
          {c.reason && c.message ? " — " : null}
          {c.message}
        </>
      ) : undefined,
    tone: conditionTone(c),
  }));

  const held = steps.filter((s) => s.tone === "hold").length;
  const failed = steps.filter((s) => s.tone === "failed").length;

  return (
    <Card className="min-w-0" data-testid="status-timeline">
      <PanelHeader
        title="How it came up"
        meta={
          steps.length === 0
            ? undefined
            : `${steps.length} condition${steps.length === 1 ? "" : "s"}${
                held > 0 ? ` · ${held} held` : ""
              }${failed > 0 ? ` · ${failed} failing` : ""}`
        }
      >
        <StatusBadge ready={ready} phase={phase} reason={readyReason(conditions)} />
      </PanelHeader>
      <CardContent>
        {steps.length === 0 ? (
          <QuietNote title="The controller hasn’t reported on this agent yet.">
            No status conditions have been written against it — not a readiness
            verdict, not a route, not a binding check. They appear here in order
            as the controller makes them. Nothing is missing; there is simply
            nothing recorded yet.
          </QuietNote>
        ) : (
          <Timeline steps={steps} label="How this agent came up" />
        )}
      </CardContent>
    </Card>
  );
}

// ── Version diff (V3) ────────────────────────────────────────────────────────
// A read-only diff of two of the agent's version snapshots (the deployed spec, as YAML). Two selects
// (never free-text) populated from the agent's versions; defaults to previous → latest. Renders the
// unified line diff with +/- coloring, a calm "no changes" for identical, and an honest error — it
// never fabricates a diff (mirrors the prompt-diff contract).
type VersionDiffState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; diff: string; identical: boolean }
  | { kind: "error"; message: string };

function VersionDiffPanel({
  ns,
  name,
  versions,
  latestVersion,
}: {
  ns: string;
  name: string;
  versions: string[];
  latestVersion: string;
}) {
  const defaultTo = latestVersion || versions[0] || "";
  const defaultFrom =
    versions.find((v) => v !== defaultTo) ?? versions[0] ?? "";
  const [from, setFrom] = React.useState(defaultFrom);
  const [to, setTo] = React.useState(defaultTo);
  const [state, setState] = React.useState<VersionDiffState>({ kind: "idle" });

  function compare() {
    if (!from || !to) return;
    setState({ kind: "loading" });
    api
      .agentVersionDiff(ns, name, from, to)
      .then((res) =>
        setState({ kind: "ready", diff: res.diff, identical: res.identical }),
      )
      .catch((err: unknown) =>
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't diff the versions",
        }),
      );
  }

  const selectClass =
    "h-8 rounded-md border bg-background px-2 font-mono text-xs";

  return (
    <div className="mt-4 border-t pt-4" data-testid="version-diff-panel">
      <p className="text-xs font-medium">Compare versions</p>
      <p className="mb-2 text-xs text-faint">
        Diff of the deployed spec snapshot.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <select
          value={from}
          onChange={(e) => setFrom(e.target.value)}
          className={selectClass}
          aria-label="From version"
          data-testid="version-diff-from"
        >
          {versions.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
        <span className="text-xs text-muted-foreground">→</span>
        <select
          value={to}
          onChange={(e) => setTo(e.target.value)}
          className={selectClass}
          aria-label="To version"
          data-testid="version-diff-to"
        >
          {versions.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
        <Button
          size="sm"
          variant="outline"
          onClick={compare}
          data-testid="version-diff-compare"
        >
          Compare
        </Button>
      </div>

      {state.kind === "loading" && (
        <p className="mt-2 text-xs text-muted-foreground">Loading diff…</p>
      )}
      {state.kind === "error" && (
        <p className="mt-2 text-xs text-destructive" role="alert">
          {state.message}
        </p>
      )}
      {state.kind === "ready" && state.identical && (
        <p
          className="mt-2 text-xs text-muted-foreground"
          data-testid="version-diff-identical"
        >
          No changes between these versions.
        </p>
      )}
      {state.kind === "ready" && !state.identical && (
        <pre
          className="mt-2 max-h-96 overflow-auto rounded-md bg-surface-3 p-3 text-xs leading-relaxed"
          data-testid="version-diff-output"
        >
          {state.diff.split("\n").map((line, i) => {
            const cls = line.startsWith("+")
              ? "text-success"
              : line.startsWith("-")
                ? "text-destructive"
                : "text-muted-foreground";
            return (
              <div key={i} className={cls}>
                {line || " "}
              </div>
            );
          })}
        </pre>
      )}
    </div>
  );
}

// ── Logs tab (live SSE tail, bearer-attached fetch-stream) ───────────────────
type LogLine = { seq: number; text: string };
type LogPhase = "connecting" | "waiting" | "streaming" | "ended" | "error" | "forbidden";

function LogsTab({ ns, name, ready }: { ns: string; name: string; ready: boolean }) {
  const [lines, setLines] = React.useState<LogLine[]>([]);
  const [phase, setPhase] = React.useState<LogPhase>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
  const seqRef = React.useRef(0);

  React.useEffect(() => {
    setLines([]);
    setPhase("connecting");
    setErrorMsg("");
    seqRef.current = 0;

    // The SSE tail over fetch-stream: the Bearer rides the request (EventSource
    // can't set headers). We follow the stream and render every frame honestly.
    const cancel = openLogStream(
      ns,
      name,
      {
        onEvent: (type: LogEventType, data: string) => {
          if (type === "log") {
            setPhase("streaming");
            setLines((prev) => [...prev, { seq: seqRef.current++, text: data }]);
          } else if (type === "waiting") {
            setPhase((p) => (p === "streaming" ? p : "waiting"));
          } else if (type === "error") {
            // An IN-STREAM error frame (mid-stream break / pods-log denied after
            // the stream opened) — surfaced honestly, distinct from a pre-stream
            // 403 (handled by onForbidden below).
            setPhase("error");
            setErrorMsg(data);
          } else if (type === "end") {
            setPhase((p) => (p === "error" || p === "forbidden" ? p : "ended"));
          }
        },
        // A PRE-STREAM 403 (RBAC denied pods list) — an HTTP status before any
        // frame. Rendered as a forbidden state, NOT an in-stream error.
        onForbidden: (message: string) => {
          setPhase("forbidden");
          setErrorMsg(message);
        },
        onError: (message: string) => {
          setPhase("error");
          setErrorMsg(message);
        },
      },
      { follow: true, tailLines: 200 },
    );

    // Cancel the stream on unmount / tab-switch — no leak.
    return cancel;
  }, [ns, name]);

  if (phase === "forbidden") {
    return (
      <ForbiddenInline
        title="Not allowed to read logs"
        description="Your account can't read pod logs in this namespace."
        detail={errorMsg}
      />
    );
  }

  return (
    // A code well (§4.5): log lines keep their own spacing and scroll inside
    // their own frame — the page never widens to fit one.
    <div className="min-w-0 overflow-hidden rounded-lg border bg-card" data-testid="logs-tab">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-border px-5 py-3">
        <span className="font-mono text-2xs uppercase tracking-wide text-faint">
          stdout · last 200 lines
        </span>
        <span
          className="flex items-center gap-2 font-mono text-xs text-faint"
          data-testid="logs-status"
        >
          {phase === "streaming" && (
            <span
              aria-hidden="true"
              className="h-1.5 w-1.5 animate-pulse rounded-full bg-success"
            />
          )}
          {phase === "connecting" && "connecting…"}
          {phase === "waiting" && "waiting for the agent to start"}
          {phase === "streaming" && `${lines.length} lines`}
          {phase === "ended" && "stream ended"}
          {phase === "error" && "stream error"}
        </span>
      </div>

      {phase === "waiting" && lines.length === 0 ? (
        <p
          className="px-5 py-8 text-sm text-secondary-foreground"
          data-testid="logs-waiting"
        >
          {ready
            ? "Waiting for the agent to start — no running pod yet."
            : "The agent is still coming up — waiting for its first pod."}
        </p>
      ) : (
        <pre className="max-h-80 overflow-auto bg-surface-3 p-4 font-mono text-xs leading-relaxed">
          {lines.map((l) => (
            <div key={l.seq} data-testid="log-line">
              {l.text}
            </div>
          ))}
          {phase === "error" && (
            <div className="mt-2 text-destructive" role="alert" data-testid="logs-error">
              — log stream error: {errorMsg}
            </div>
          )}
          {phase === "ended" && lines.length === 0 && (
            <div className="text-faint">No log output.</div>
          )}
        </pre>
      )}
    </div>
  );
}

// ── Per-agent Runs (m15.11) ──────────────────────────────────────────────────
// Uses the bounded GET /api/agents/{ns}/{name}/runs endpoint. On 501 (no trace
// backend) it renders a calm QuietNote, never an error — nothing is broken, the
// platform is simply not wired to answer.
function AgentRunsTab({
  ns,
  name,
  onInspect,
}: {
  ns: string;
  name: string;
  onInspect: (traceId: string) => void;
}) {
  const [state, setState] = React.useState<
    | { kind: "loading" }
    | { kind: "ready"; runs: AgentRunSummary[] }
    | { kind: "unavailable" } // 501 — no trace backend configured
    | { kind: "error"; message: string; forbidden: boolean }
  >({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    api
      .agentRuns(ns, name, 50, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // null = 501 (no trace backend wired) — degrade calmly.
        if (res === null) {
          setState({ kind: "unavailable" });
          return;
        }
        setState({ kind: "ready", runs: res.runs });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load runs",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  // §4.4 activity-feed budget: identity and the next step never drop; the
  // numerics go at 1024 and the timestamp at 768.
  const cols: Column<AgentRunSummary>[] = [
    {
      id: "traceId",
      header: "Run",
      priority: 1,
      cell: (r) => (
        <span className="font-mono text-xs" title={r.traceId}>
          {truncateId(r.traceId)}
        </span>
      ),
    },
    {
      id: "timestamp",
      header: "When",
      priority: 2,
      cell: (r) => (
        <span className="whitespace-nowrap font-mono text-xs tabular-nums text-faint" title={r.timestamp}>
          {formatDateTime(r.timestamp) || r.timestamp}
        </span>
      ),
    },
    {
      id: "tokens",
      header: "Tokens",
      priority: 3,
      numeric: true,
      // The traces-LIST API does not carry per-trace token usage, so a 0 here
      // means "not captured", not "used no tokens". Unknown and zero never
      // share a glyph (§7.1), so a 0 renders the dash with its reason.
      cell: (r) => (
        <QuantityValue
          value={r.tokens > 0 ? r.tokens : UNKNOWN}
          title="Per-trace token usage isn’t carried by the runs list — unknown, not zero."
        />
      ),
    },
    {
      id: "cost",
      header: "Cost",
      priority: 3,
      numeric: true,
      cell: (r) => <QuantityValue value={r.costUSD} format={formatUSD} />,
    },
    {
      id: "latency",
      header: "Latency",
      priority: 3,
      numeric: true,
      cell: (r) => (
        <QuantityValue
          value={r.latencyMs > 0 ? r.latencyMs : UNKNOWN}
          format={formatLatency}
        />
      ),
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      className: "w-[9rem]",
      cell: (r) => (
        <NextStepLink
          label="Read the run"
          onClick={() => onInspect(r.traceId)}
          ariaLabel={`Read run ${r.traceId}`}
        />
      ),
    },
  ];

  // 501-degrade: a calm note, NOT an error toast and NOT an error state.
  if (state.kind === "unavailable") {
    return (
      <div data-testid="runs-unavailable">
        <QuietNote title="This install records no runs.">
          Runs are unavailable — tracing not configured. This agent may well have
          served many; without a trace backend the console has nothing to read, so
          the list is absent rather than empty. Nothing here is estimated.
        </QuietNote>
      </div>
    );
  }

  if (state.kind === "error" && state.forbidden) {
    return (
      <ForbiddenInline
        title="Not allowed to read runs"
        description="Your account can't read run history in this cluster."
        detail={state.message}
      />
    );
  }

  return (
    <div className="min-w-0" data-testid="runs-tab">
      <DataTable<AgentRunSummary>
        columns={cols}
        rows={state.kind === "ready" ? state.runs : []}
        rowKey={(r) => r.traceId}
        loading={state.kind === "loading"}
        error={
          state.kind === "error"
            ? { message: state.message, forbidden: false, onRetry: undefined }
            : null
        }
        onRowClick={(r) => onInspect(r.traceId)}
        ariaLabel="Agent runs"
        empty={{
          icon: Play,
          title: "No runs yet",
          description:
            "Nothing has been asked of this agent. Send it something from the Overview tab and its runs appear here.",
        }}
      />
    </div>
  );
}

// ── Bindings list ────────────────────────────────────────────────────────────
// The full, grouped view behind the Overview summary. It speaks the SAME three
// words as "What it can reach" (working / never called / unresolved) — two
// surfaces describing the same binding must not use two vocabularies, or the
// reader has to work out which one to believe.

const OTHER_TOOLS_GROUP = "Other tools";

function BindingsList({
  bindings,
  agentIsUp,
}: {
  bindings: AgentBinding[];
  agentIsUp: boolean;
}) {
  if (bindings.length === 0) {
    return (
      <p className="text-sm text-secondary-foreground" data-testid="bindings-empty">
        No bindings reference this agent yet.
      </p>
    );
  }

  // Tool bindings are grouped by MCP server and COLLAPSED by default (an agent can bind
  // dozens of tools — a flat list is an unscrollable wall). Non-tool bindings (memory, …)
  // are few, so they render flat below.
  const tools = bindings.filter((b) => b.kind === "tool");
  const others = bindings.filter((b) => b.kind !== "tool");

  const groups = new Map<string, AgentBinding[]>();
  for (const b of tools) {
    const key = b.server?.trim() || OTHER_TOOLS_GROUP;
    const arr = groups.get(key);
    if (arr) arr.push(b);
    else groups.set(key, [b]);
  }
  const serverGroups = [...groups.entries()].sort((a, b) => {
    if (a[0] === OTHER_TOOLS_GROUP) return 1;
    if (b[0] === OTHER_TOOLS_GROUP) return -1;
    return a[0].localeCompare(b[0]);
  });

  const StateTag = ({ b }: { b: AgentBinding }) => {
    const tag = REACH_TAG[reachState(b, agentIsUp)];
    return (
      <Badge variant={tag.variant} className="shrink-0" title={tag.title}>
        {tag.label}
      </Badge>
    );
  };

  return (
    <div className="space-y-2" data-testid="bindings-list">
      {serverGroups.map(([server, group]) => {
        const readyCount = group.filter((b) => b.ready).length;
        const allReady = readyCount === group.length;
        return (
          <details
            key={server}
            className="group min-w-0 rounded-md border border-border bg-surface-2"
            data-testid={`binding-group-${server}`}
          >
            <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-sm [&::-webkit-details-marker]:hidden">
              <span className="flex min-w-0 items-center gap-2">
                <ChevronRight
                  aria-hidden="true"
                  className="h-4 w-4 shrink-0 text-ghost transition-transform group-open:rotate-90"
                />
                <Server aria-hidden="true" className="h-4 w-4 shrink-0 text-faint" />
                <span className="truncate font-mono text-sm font-semibold">{server}</span>
                <span className="shrink-0 font-mono text-2xs uppercase tracking-wide text-faint">
                  {group.length} tool{group.length === 1 ? "" : "s"}
                </span>
              </span>
              {/* The rollup is a RESOLUTION count, so it wears the resolution
                  hues: all resolved is ok, a shortfall is crit — never amber,
                  which means "a bound is near or crossed" (§2.2). */}
              <Badge variant={allReady ? "ok" : "crit"} className="shrink-0">
                {allReady ? "all ready" : `${readyCount}/${group.length} ready`}
              </Badge>
            </summary>
            <div className="space-y-1 border-t border-border-soft px-3 py-2">
              {group.map((b) => (
                <div
                  key={`${b.kind}/${b.name}`}
                  className="flex items-center justify-between gap-3 rounded-sm px-2 py-1.5 text-sm hover:bg-surface-3"
                  data-testid={`binding-${b.name}`}
                >
                  <span className="truncate font-mono text-xs" title={b.detail || b.name}>
                    {b.detail || b.name}
                  </span>
                  <StateTag b={b} />
                </div>
              ))}
            </div>
          </details>
        );
      })}

      {others.map((b) => (
        <div
          key={`${b.kind}/${b.name}`}
          className="flex min-w-0 items-center justify-between gap-3 rounded-md border border-border bg-surface-2 px-4 py-3 text-sm"
          data-testid={`binding-${b.name}`}
        >
          <span className="flex min-w-0 items-center gap-2">
            <span className="shrink-0 font-mono text-2xs uppercase tracking-wide text-faint">
              {b.kind}
            </span>
            <span className="truncate">{b.detail || b.name}</span>
          </span>
          <StateTag b={b} />
        </div>
      ))}
    </div>
  );
}

// ── Memory panel (m17.11) ────────────────────────────────────────────────────
// Shows the MemoryBinding(s) that reference this agent (filtered by agentRef).
// Supports attach (create), edit, and detach (typed-name delete). RBAC-aware:
//   • attach/edit/detach are gated on can("memorybindings", verb)
//   • a forced 403 surfaces honestly in the form; viewers see read-only.

type MemoryForm = {
  scope: string;
  backend: string;
};

type MemoryActionState =
  | { kind: "idle" }
  | { kind: "attach-open"; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "edit-open"; binding: MemoryBindingSummary; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "detach-open"; binding: MemoryBindingSummary; busy: boolean };

type MemoryPanelLoad =
  | { kind: "loading" }
  | { kind: "ready"; bindings: MemoryBindingSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

function MemoryPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can, reprobe } = useCapabilities();
  const { toast } = useToast();
  const canCreate = can(RES_MEMORY, "create");
  const canUpdate = can(RES_MEMORY, "update");
  const canDelete = can(RES_MEMORY, "delete");

  const [load, setLoad] = React.useState<MemoryPanelLoad>({ kind: "loading" });
  const [action, setAction] = React.useState<MemoryActionState>({ kind: "idle" });
  const [form, setForm] = React.useState<MemoryForm>({ scope: "", backend: "" });

  const fetchBindings = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .listMemoryBindings({ namespace: ns }, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.items.filter((b) => b.agentRef === agentName);
        setLoad({ kind: "ready", bindings: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load memory bindings",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => {
    const cancel = fetchBindings();
    return cancel;
  }, [fetchBindings]);

  function openAttach() {
    setForm({ scope: "", backend: "" });
    setAction({ kind: "attach-open", busy: false, error: null, forbidden: false });
  }

  function openEdit(binding: MemoryBindingSummary) {
    setForm({ scope: binding.scope, backend: binding.backend ?? "" });
    setAction({ kind: "edit-open", binding, busy: false, error: null, forbidden: false });
  }

  function openDetach(binding: MemoryBindingSummary) {
    setAction({ kind: "detach-open", binding, busy: false });
  }

  async function doAttach() {
    if (action.kind !== "attach-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.createMemoryBinding({
        namespace: ns,
        agentRef: agentName,
        scope: form.scope.trim(),
        backend: form.backend.trim() || undefined,
      });
      toast({ variant: "success", title: "Memory binding attached", description: `Scope "${form.scope}" attached to ${agentName}.` });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "attach failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doEdit() {
    if (action.kind !== "edit-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.updateMemoryBinding(ns, action.binding.name, {
        scope: form.scope.trim(),
        backend: form.backend.trim() || undefined,
      });
      toast({ variant: "success", title: "Memory binding updated" });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doDetach() {
    if (action.kind !== "detach-open") return;
    setAction({ ...action, busy: true });
    try {
      await api.removeMemoryBinding(ns, action.binding.name);
      toast({ variant: "success", title: "Memory binding detached", description: `Binding "${action.binding.name}" removed.` });
      setAction({ kind: "idle" });
      fetchBindings();
    } catch (err) {
      toast({ variant: "error", title: "Detach failed", description: err instanceof Error ? err.message : "detach failed" });
      setAction({ kind: "idle" });
    }
  }

  const isAttachOpen = action.kind === "attach-open";
  const isEditOpen = action.kind === "edit-open";
  const isDetachOpen = action.kind === "detach-open";
  const formError = (action.kind === "attach-open" || action.kind === "edit-open") ? action.error : null;
  const formForbidden = (action.kind === "attach-open" || action.kind === "edit-open") ? action.forbidden : false;
  const formBusy = (action.kind === "attach-open" || action.kind === "edit-open" || action.kind === "detach-open") ? action.busy : false;

  return (
    <div data-testid="memory-panel">
      <SectionHeader
        title="Memory"
        lede="The session and shared memory backends wired to this agent. This is its configuration, not its contents."
        actions={
          canCreate ? (
            <Button
              variant="outline"
              size="sm"
              onClick={openAttach}
              data-testid="memory-attach"
            >
              <Plus className="h-4 w-4" />
              Attach
            </Button>
          ) : undefined
        }
      />

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="memory-loading">Loading…</p>
      )}
      {load.kind === "error" && load.forbidden && (
        <ForbiddenInline
          title="Not allowed to list memory bindings"
          description="Your account can't read MemoryBindings in this namespace."
          detail={load.message}
        />
      )}
      {load.kind === "error" && !load.forbidden && (
        <p className="text-sm text-destructive" role="alert" data-testid="memory-error">
          {load.message}
        </p>
      )}
      {load.kind === "ready" && load.bindings.length === 0 && (
        <EmptyState
          icon={Boxes}
          title="No memory bindings"
          description="Attach a memory binding to configure this agent's session and shared memory backend. (Long-term, semantically-retrievable memory is shown separately below.)"
        />
      )}
      {load.kind === "ready" && load.bindings.length > 0 && (
        <ul className="space-y-2">
          {load.bindings.map((b) => (
            <li
              key={b.name}
              className="flex items-center justify-between gap-3 rounded-md border bg-surface-2 px-4 py-3 text-sm"
              data-testid={`memory-binding-${b.name}`}
            >
              <div className="flex min-w-0 items-center gap-3">
                <Badge variant="muted">scope</Badge>
                <span className="font-medium">{b.scope}</span>
                {b.backend && (
                  <span className="text-xs text-muted-foreground">via {b.backend}</span>
                )}
              </div>
              <div className="flex items-center gap-2">
                <Badge variant={b.ready ? "ok" : "progressing"}>
                  {b.ready ? "ready" : "pending"}
                </Badge>
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => openEdit(b)} data-testid={`memory-edit-${b.name}`}>
                    <Pencil className="h-3.5 w-3.5" />
                  </Button>
                )}
                {canDelete && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => openDetach(b)}
                    className="text-destructive hover:text-destructive"
                    data-testid={`memory-detach-${b.name}`}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* Attach / edit form inline */}
      {(isAttachOpen || isEditOpen) && (
        <div className="mt-4 rounded-lg border bg-card p-4">
          <p className="mb-3 text-sm font-medium">{isAttachOpen ? "Attach memory binding" : "Edit memory binding"}</p>
          <div className="space-y-3">
            <FormField id="memory-scope" label="Scope">
              <Input
                id="memory-scope"
                value={form.scope}
                onChange={(e) => setForm((f) => ({ ...f, scope: e.target.value }))}
                placeholder="global"
                data-testid="memory-scope-input"
              />
            </FormField>
            <FormField id="memory-backend" label="Backend (optional)">
              <Input
                id="memory-backend"
                value={form.backend}
                onChange={(e) => setForm((f) => ({ ...f, backend: e.target.value }))}
                placeholder="redis"
                data-testid="memory-backend-input"
              />
            </FormField>
            {formForbidden && (
              <ForbiddenInline
                title="Not allowed to manage memory bindings"
                description="Your account can't create or update MemoryBindings."
                detail={formError ?? undefined}
              />
            )}
            {formError && !formForbidden && (
              <p className="text-sm text-destructive" role="alert" data-testid="memory-form-error">
                {formError}
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "idle" })} disabled={formBusy}>
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={isAttachOpen ? doAttach : doEdit}
                disabled={!form.scope.trim() || formBusy}
                data-testid="memory-form-submit"
              >
                {formBusy ? "Saving…" : isAttachOpen ? "Attach" : "Save"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Typed-name detach confirmation */}
      {isDetachOpen && action.kind === "detach-open" && (
        <ConfirmDialog
          open={true}
          onCancel={() => setAction({ kind: "idle" })}
          onConfirm={doDetach}
          title={`Detach memory binding?`}
          description={`This will remove the binding "${action.binding.name}" from ${agentName}. The agent will lose access to the "${action.binding.scope}" memory scope.`}
          confirmText={action.binding.name}
          confirmLabel="Detach"
          busy={action.busy}
        />
      )}

      <SessionMemoryConfigPanel ns={ns} agentName={agentName} />
      <LongTermMemoryConfigPanel ns={ns} agentName={agentName} />
      <LongTermMemoryPanel ns={ns} agentName={agentName} />
    </div>
  );
}

// ── Long-term memory viewer (m46.6, ADR 0045) ────────────────────────────────
// Read-only list of the agent's AGENT-WIDE long-term memories (persistent,
// semantically-retrievable knowledge). Per-user memories are never shown
// (privacy). Degrades to "unavailable" on 501 (no control-plane store), and to a
// friendly empty state when the agent has remembered nothing yet.

type LongTermLoad =
  | { kind: "loading" }
  | { kind: "ready"; items: AgentMemoryEntry[] }
  | { kind: "unavailable" }
  | { kind: "error"; message: string; forbidden: boolean };

// SessionMemoryConfigPanel (M137/EU1d, ADR 0080) — the console toggle for per-user SESSION memory. M98
// shipped the CRD + runtime but no console affordance; an operator could only set spec.sessionMemory.perUser
// via YAML. It patches the folded field via the BFF (the longtermmemory pattern). Read-only for viewers
// (gated on agent-update); hides when the agent has no session memory or is unreadable. The inline help
// states the three caveats: it isolates memory per end-user, is PRODUCT-grade (not security-grade), and it
// BREAKS conversation handoff + share-links for the agent.
function SessionMemoryConfigPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can } = useCapabilities();
  const canUpdate = can(RES_AGENTS, "update");
  const [config, setConfig] = React.useState<SessionMemoryConfig | null>(null);
  const [perUser, setPerUser] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState<string | null>(null);

  const apply = React.useCallback((c: SessionMemoryConfig) => {
    setConfig(c);
    setPerUser(c.perUser);
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .sessionMemoryConfig(ns, agentName, controller.signal)
      .then((c) => !controller.signal.aborted && apply(c))
      .catch(() => !controller.signal.aborted && setConfig(null));
    return () => controller.abort();
  }, [ns, agentName, apply]);

  if (config === null) return null; // unreadable (403/404) — hide, no noise
  if (!config.enabled) return null; // no session memory ⇒ nothing to isolate; hide the toggle
  const shared = config.scope === "shared";

  function save(next: boolean) {
    if (!config) return;
    setBusy(true);
    setErr(null);
    api
      .setSessionMemory(ns, agentName, { enabled: true, scope: config.scope, perUser: next })
      .then(apply)
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : "update failed"))
      .finally(() => setBusy(false));
  }

  return (
    <div className="mt-8 border-t pt-6" data-testid="sessionmem-config">
      <h3 className="mb-1 text-sm font-medium">Session memory</h3>
      <p className="mb-3 text-xs text-muted-foreground">
        Per-user isolation for this agent&rsquo;s conversation memory (ADR 0080).
      </p>
      <div className="flex items-center gap-2 text-sm">
        <Badge variant={config.perUser ? "ok" : "muted"} data-testid="sessionmem-state">
          {config.perUser ? "per-user" : "agent-wide"}
        </Badge>
        <span className="text-xs text-muted-foreground">scope {config.scope}</span>
      </div>
      {shared ? (
        <p className="mt-2 text-xs text-muted-foreground" data-testid="sessionmem-shared-note">
          Per-user isolation applies only to private (&ldquo;session&rdquo;) scope. This agent uses the
          shared team scratchpad, which is per-conversation by design.
        </p>
      ) : (
        canUpdate && (
          <div className="mt-3 space-y-2" data-testid="sessionmem-config-form">
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={perUser}
                disabled={busy}
                onChange={(e) => {
                  setPerUser(e.target.checked);
                  save(e.target.checked);
                }}
                data-testid="sessionmem-peruser"
              />
              Per-user session memory (each end-user&rsquo;s own conversation history, isolated)
            </label>
            <p className="text-xs text-muted-foreground" data-testid="sessionmem-caveat">
              Product-grade isolation, not a security boundary (launcher-stamped inside the pod). Turning
              this on breaks conversation handoff and share-links for this agent.
            </p>
            {err && (
              <p className="text-sm text-destructive" data-testid="sessionmem-config-error">
                {err}
              </p>
            )}
          </div>
        )
      )}
    </div>
  );
}

// LongTermMemoryConfigPanel (m49.3) — the ENABLE surface for M46's folded long-term-memory capability. The
// console could VIEW an agent's long-term memories (LongTermMemoryPanel) but had no way to TURN THE
// CAPABILITY ON (the m49.1 capability pocket). Patches spec.longTermMemory via the BFF (the tracepolicy
// pattern). Read-only for viewers (the form is gated on the agent-update capability); hides if unreadable.
function LongTermMemoryConfigPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can } = useCapabilities();
  const canUpdate = can(RES_AGENTS, "update");
  const [config, setConfig] = React.useState<LongTermMemoryConfig | null>(null);
  const [route, setRoute] = React.useState("");
  const [perUser, setPerUser] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState<string | null>(null);

  const apply = React.useCallback((c: LongTermMemoryConfig) => {
    setConfig(c);
    setRoute(c.embeddingRoute ?? "");
    setPerUser(c.perUser);
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .longTermMemoryConfig(ns, agentName, controller.signal)
      .then((c) => !controller.signal.aborted && apply(c))
      .catch(() => !controller.signal.aborted && setConfig(null));
    return () => controller.abort();
  }, [ns, agentName, apply]);

  function save(enabled: boolean) {
    setBusy(true);
    setErr(null);
    api
      .setLongTermMemory(ns, agentName, {
        enabled,
        perUser,
        embeddingRoute: route.trim() || undefined,
      })
      .then(apply)
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : "update failed"))
      .finally(() => setBusy(false));
  }

  if (config === null) return null; // unreadable (403/404) — hide, no noise

  return (
    <div className="mt-8 border-t pt-6" data-testid="longterm-config">
      <h3 className="mb-1 text-sm font-medium">Long-term memory</h3>
      <p className="mb-3 text-xs text-muted-foreground">
        Let this agent remember facts across conversations and recall them by meaning (ADR 0045).
      </p>
      <div className="flex items-center gap-2 text-sm">
        <Badge variant={config.enabled ? "ok" : "muted"} data-testid="longterm-state">
          {config.enabled ? "Enabled" : "Disabled"}
        </Badge>
        {config.enabled && (
          <span className="text-xs text-muted-foreground">
            {config.perUser ? "per-user" : "agent-wide"}
            {config.embeddingRoute ? ` · route ${config.embeddingRoute}` : ""}
          </span>
        )}
      </div>
      {canUpdate && (
        <div className="mt-3 space-y-2" data-testid="longterm-config-form">
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={perUser}
              onChange={(e) => setPerUser(e.target.checked)}
              data-testid="longterm-peruser"
            />
            Per-user memory (each end-user's own facts, isolated)
          </label>
          <Input
            placeholder="Embedding route (optional — a ModelRoute name)"
            value={route}
            onChange={(e) => setRoute(e.target.value)}
            data-testid="longterm-route"
          />
          <div className="flex gap-2">
            {config.enabled ? (
              <>
                <Button size="sm" variant="outline" disabled={busy} onClick={() => save(true)} data-testid="longterm-save">
                  Save
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive hover:text-destructive"
                  disabled={busy}
                  onClick={() => save(false)}
                  data-testid="longterm-disable"
                >
                  Disable
                </Button>
              </>
            ) : (
              <Button size="sm" disabled={busy} onClick={() => save(true)} data-testid="longterm-enable">
                Enable
              </Button>
            )}
          </div>
          {err && (
            <p className="text-sm text-destructive" data-testid="longterm-config-error">
              {err}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function LongTermMemoryPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const [load, setLoad] = React.useState<LongTermLoad>({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .agentMemory(ns, agentName, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoad(res === null ? { kind: "unavailable" } : { kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load long-term memory",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  // Unavailable (no store wired) — hide the section entirely, like other degraded surfaces.
  if (load.kind === "unavailable") return null;

  return (
    <div className="mt-8 border-t pt-6" data-testid="longterm-memory-panel">
      <h3 className="mb-1 text-sm font-medium">Long-term memory</h3>
      <p className="mb-3 text-xs text-muted-foreground">
        Facts this agent has remembered and can recall by meaning across conversations. Only
        agent-wide memories appear here; per-user memories are scoped to each end-user's own
        conversations and are never exposed in the console, for privacy.
      </p>

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="longterm-loading">Loading…</p>
      )}
      {load.kind === "error" && (
        <p className="text-sm text-destructive" data-testid="longterm-error">
          {load.forbidden ? "Not allowed to read this agent's memory." : load.message}
        </p>
      )}
      {load.kind === "ready" && load.items.length === 0 && (
        <div data-testid="longterm-empty">
          <EmptyState
            icon={Boxes}
            title="Nothing remembered yet"
            description="When this agent stores a long-term memory (via memory.remember), its agent-wide facts will appear here."
          />
        </div>
      )}
      {load.kind === "ready" && load.items.length > 0 && (
        <ul className="space-y-2" aria-label="Long-term memories" data-testid="longterm-list">
          {load.items.map((m, i) => (
            <li
              key={`${m.createdAt}-${i}`}
              className="rounded-md border bg-card p-3 text-sm"
              data-testid="longterm-item"
            >
              <p className="whitespace-pre-wrap">{m.content}</p>
              {m.tags && Object.keys(m.tags).length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1" data-testid="longterm-tags">
                  {Object.entries(m.tags).map(([k, v]) => (
                    <Badge key={k} variant="muted">
                      {k}: {v}
                    </Badge>
                  ))}
                </div>
              )}
              <div className="mt-1 flex flex-wrap items-center gap-x-3 text-xs text-muted-foreground">
                <span>{formatTimestamp(m.createdAt)}</span>
                {/* memory→trace back-link (m54.3, M49 UX review A2): jump from a
                    remembered fact to the run/trace that produced it. Absent when
                    the memory was written outside a traced run. */}
                {m.traceId && (
                  <Link
                    to={`/traces/${encodeURIComponent(m.traceId)}`}
                    data-testid={`longterm-trace-link-${m.traceId}`}
                    aria-label={`View the trace that produced this memory (${m.traceId})`}
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <Activity className="h-3 w-3" />
                    trace
                  </Link>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// formatTimestamp renders an ISO timestamp in the viewer's locale, falling back to the raw
// string if it is missing or unparseable (never the literal "Invalid Date").
function formatTimestamp(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

// ── Scaling panel (m17.11) ────────────────────────────────────────────────────
// Shows the AgentScalingPolicy for this agent (filtered by agentRef). Supports
// attach (create), edit, and detach (typed-name delete). RBAC-aware. A 422
// from the CRD XValidations (max < min, or schedule-without-scheduled-mode)
// surfaces in the form with the server's message — never faked as success.

type ScalingForm = {
  minReplicas: string;
  maxReplicas: string;
  mode: string;
  schedule: string;
};

type ScalingActionState =
  | { kind: "idle" }
  | { kind: "attach-open"; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "edit-open"; policy: AgentScalingPolicySummary; busy: boolean; error: string | null; forbidden: boolean }
  | { kind: "detach-open"; policy: AgentScalingPolicySummary; busy: boolean };

type ScalingPanelLoad =
  | { kind: "loading" }
  | { kind: "ready"; policies: AgentScalingPolicySummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

function ScalingPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can, reprobe } = useCapabilities();
  const { toast } = useToast();
  const canCreate = can(RES_SCALING, "create");
  const canUpdate = can(RES_SCALING, "update");
  const canDelete = can(RES_SCALING, "delete");

  const [load, setLoad] = React.useState<ScalingPanelLoad>({ kind: "loading" });
  const [action, setAction] = React.useState<ScalingActionState>({ kind: "idle" });
  const [form, setForm] = React.useState<ScalingForm>({ minReplicas: "0", maxReplicas: "3", mode: "static", schedule: "" });
  // Keep-warm (m32.5): a one-switch view over the agent's min-replicas floor — "warm" iff any
  // attached policy holds ≥1 replica, so a latency-sensitive agent avoids Knative cold-starts.
  const [keepWarmBusy, setKeepWarmBusy] = React.useState(false);

  const fetchPolicies = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .listAgentScalingPolicies({ namespace: ns }, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        const mine = res.items.filter((p) => p.agentRef === agentName);
        setLoad({ kind: "ready", policies: mine });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load scaling policies",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => {
    const cancel = fetchPolicies();
    return cancel;
  }, [fetchPolicies]);

  function openAttach() {
    setForm({ minReplicas: "0", maxReplicas: "3", mode: "static", schedule: "" });
    setAction({ kind: "attach-open", busy: false, error: null, forbidden: false });
  }

  function openEdit(policy: AgentScalingPolicySummary) {
    setForm({
      minReplicas: String(policy.minReplicas),
      maxReplicas: String(policy.maxReplicas),
      mode: policy.mode ?? "static",
      schedule: policy.schedule ?? "",
    });
    setAction({ kind: "edit-open", policy, busy: false, error: null, forbidden: false });
  }

  function openDetach(policy: AgentScalingPolicySummary) {
    setAction({ kind: "detach-open", policy, busy: false });
  }

  async function doAttach() {
    if (action.kind !== "attach-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.createAgentScalingPolicy({
        namespace: ns,
        agentRef: agentName,
        minReplicas: parseInt(form.minReplicas, 10) || 0,
        maxReplicas: parseInt(form.maxReplicas, 10) || 1,
        mode: form.mode || undefined,
        schedule: form.mode === "scheduled" && form.schedule.trim() ? form.schedule.trim() : undefined,
      });
      toast({ variant: "success", title: "Scaling policy attached" });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      // 422 = XValidation rejection (max<min, schedule-without-scheduled-mode) —
      // surfaced in the form with the server's message. NOT faked as a success.
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "attach failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  async function doEdit() {
    if (action.kind !== "edit-open") return;
    setAction({ ...action, busy: true, error: null });
    try {
      await api.updateAgentScalingPolicy(ns, action.policy.name, {
        minReplicas: parseInt(form.minReplicas, 10) || 0,
        maxReplicas: parseInt(form.maxReplicas, 10) || 1,
        mode: form.mode || undefined,
        schedule: form.mode === "scheduled" && form.schedule.trim() ? form.schedule.trim() : undefined,
      });
      toast({ variant: "success", title: "Scaling policy updated" });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setAction({
        ...action,
        busy: false,
        error: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  // doKeepWarm flips the agent's min-replicas floor (m32.5): ON holds ≥1 replica warm (creating a
  // default policy if none is attached), OFF returns every attached policy to scale-to-zero (min 0).
  async function doKeepWarm(enable: boolean) {
    if (load.kind !== "ready") return;
    setKeepWarmBusy(true);
    try {
      if (load.policies.length === 0) {
        if (enable) {
          await api.createAgentScalingPolicy({
            namespace: ns,
            agentRef: agentName,
            minReplicas: 1,
            maxReplicas: 3,
          });
        }
      } else {
        for (const p of load.policies) {
          const target = enable ? Math.max(1, p.minReplicas) : 0;
          if (target !== p.minReplicas) {
            await api.updateAgentScalingPolicy(ns, p.name, { minReplicas: target });
          }
        }
      }
      toast({
        variant: "success",
        title: enable ? "Keeping the agent warm" : "Scale-to-zero enabled",
      });
      fetchPolicies();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      toast({
        variant: "error",
        title: "Couldn't change keep-warm",
        description: err instanceof Error ? err.message : "failed",
      });
    } finally {
      setKeepWarmBusy(false);
    }
  }

  async function doDetach() {
    if (action.kind !== "detach-open") return;
    setAction({ ...action, busy: true });
    try {
      await api.removeAgentScalingPolicy(ns, action.policy.name);
      toast({ variant: "success", title: "Scaling policy detached", description: `Policy "${action.policy.name}" removed.` });
      setAction({ kind: "idle" });
      fetchPolicies();
    } catch (err) {
      toast({ variant: "error", title: "Detach failed", description: err instanceof Error ? err.message : "detach failed" });
      setAction({ kind: "idle" });
    }
  }

  const isAttachOpen = action.kind === "attach-open";
  const isEditOpen = action.kind === "edit-open";
  const isDetachOpen = action.kind === "detach-open";
  const formError = (action.kind === "attach-open" || action.kind === "edit-open") ? action.error : null;
  const formForbidden = (action.kind === "attach-open" || action.kind === "edit-open") ? action.forbidden : false;
  const formBusy = (action.kind === "attach-open" || action.kind === "edit-open" || action.kind === "detach-open") ? action.busy : false;

  // warm iff any attached policy holds ≥1 replica (the min-replicas floor is up, no cold-starts).
  const warm =
    load.kind === "ready" && load.policies.some((p) => p.minReplicas >= 1);

  return (
    <div data-testid="scaling-panel">
      <SectionHeader
        title="Scaling"
        lede="How many copies of this agent run, and when. A policy with a floor above zero keeps it warm."
        actions={
          canCreate ? (
            <Button
              variant="outline"
              size="sm"
              onClick={openAttach}
              data-testid="scaling-attach"
            >
              <Plus className="h-4 w-4" />
              Attach
            </Button>
          ) : undefined
        }
      />

      {load.kind === "ready" && (canCreate || canUpdate) && (
        <div
          className="mb-4 flex items-center justify-between rounded-md border p-3"
          data-testid="keep-warm"
        >
          <div className="space-y-0.5">
            <p className="text-sm font-medium">Keep warm</p>
            <p className="text-xs text-muted-foreground">
              Hold at least one replica ready so latency-sensitive invokes skip the
              Knative cold-start.
            </p>
          </div>
          <Button
            variant={warm ? "default" : "outline"}
            size="sm"
            disabled={keepWarmBusy}
            aria-pressed={warm}
            onClick={() => void doKeepWarm(!warm)}
            data-testid="keep-warm-toggle"
          >
            {keepWarmBusy ? "Saving…" : warm ? "On" : "Off"}
          </Button>
        </div>
      )}

      {load.kind === "loading" && (
        <p className="text-sm text-muted-foreground" data-testid="scaling-loading">Loading…</p>
      )}
      {load.kind === "error" && load.forbidden && (
        <ForbiddenInline
          title="Not allowed to list scaling policies"
          description="Your account can't read AgentScalingPolicies in this namespace."
          detail={load.message}
        />
      )}
      {load.kind === "error" && !load.forbidden && (
        <p className="text-sm text-destructive" role="alert" data-testid="scaling-error">
          {load.message}
        </p>
      )}
      {load.kind === "ready" && load.policies.length === 0 && (
        <EmptyState
          icon={SlidersHorizontal}
          title="No scaling policies"
          description="Attach a scaling policy to control how this agent scales."
        />
      )}
      {load.kind === "ready" && load.policies.length > 0 && (
        <ul className="space-y-2">
          {load.policies.map((p) => (
            <li
              key={p.name}
              className="flex items-center justify-between gap-3 rounded-md border bg-surface-2 px-4 py-3 text-sm"
              data-testid={`scaling-policy-${p.name}`}
            >
              <div className="flex min-w-0 items-center gap-3">
                <Badge variant="muted">{p.mode ?? "static"}</Badge>
                <span className="font-medium">{p.minReplicas}–{p.maxReplicas} replicas</span>
                {p.schedule && (
                  <span className="font-mono text-xs text-muted-foreground">{p.schedule}</span>
                )}
              </div>
              <div className="flex items-center gap-2">
                <Badge variant={p.ready ? "ok" : "progressing"}>
                  {p.ready ? "ready" : "pending"}
                </Badge>
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => openEdit(p)} data-testid={`scaling-edit-${p.name}`}>
                    <Pencil className="h-3.5 w-3.5" />
                  </Button>
                )}
                {canDelete && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => openDetach(p)}
                    className="text-destructive hover:text-destructive"
                    data-testid={`scaling-detach-${p.name}`}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      {/* Attach / edit form inline */}
      {(isAttachOpen || isEditOpen) && (
        <div className="mt-4 rounded-lg border bg-card p-4">
          <p className="mb-3 text-sm font-medium">{isAttachOpen ? "Attach scaling policy" : "Edit scaling policy"}</p>
          <div className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <FormField id="scaling-min" label="Min replicas">
                <Input
                  id="scaling-min"
                  inputMode="numeric"
                  value={form.minReplicas}
                  onChange={(e) => setForm((f) => ({ ...f, minReplicas: e.target.value }))}
                  data-testid="scaling-min-input"
                />
              </FormField>
              <FormField id="scaling-max" label="Max replicas">
                <Input
                  id="scaling-max"
                  inputMode="numeric"
                  value={form.maxReplicas}
                  onChange={(e) => setForm((f) => ({ ...f, maxReplicas: e.target.value }))}
                  data-testid="scaling-max-input"
                />
              </FormField>
            </div>
            <FormField id="scaling-mode" label="Mode">
              <Select
                id="scaling-mode"
                value={form.mode}
                onChange={(e) => setForm((f) => ({ ...f, mode: e.target.value }))}
                data-testid="scaling-mode-input"
              >
                <option value="static">static</option>
                <option value="scheduled">scheduled</option>
              </Select>
            </FormField>
            {form.mode === "scheduled" && (
              <FormField id="scaling-schedule" label="Schedule (cron)">
                <Input
                  id="scaling-schedule"
                  value={form.schedule}
                  onChange={(e) => setForm((f) => ({ ...f, schedule: e.target.value }))}
                  placeholder="0 8 * * 1-5"
                  data-testid="scaling-schedule-input"
                />
              </FormField>
            )}
            {/* 422 from XValidations (max<min, schedule-without-scheduled) surfaces here
                with the server message — never a fabricated success. */}
            {formForbidden && (
              <ForbiddenInline
                title="Not allowed to manage scaling policies"
                description="Your account can't create or update AgentScalingPolicies."
                detail={formError ?? undefined}
              />
            )}
            {formError && !formForbidden && (
              <p className="text-sm text-destructive" role="alert" data-testid="scaling-form-error">
                {formError}
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "idle" })} disabled={formBusy}>
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={isAttachOpen ? doAttach : doEdit}
                disabled={formBusy}
                data-testid="scaling-form-submit"
              >
                {formBusy ? "Saving…" : isAttachOpen ? "Attach" : "Save"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {/* Typed-name detach confirmation */}
      {isDetachOpen && action.kind === "detach-open" && (
        <ConfirmDialog
          open={true}
          onCancel={() => setAction({ kind: "idle" })}
          onConfirm={doDetach}
          title="Detach scaling policy?"
          description={`This will remove the scaling policy "${action.policy.name}" from ${agentName}.`}
          confirmText={action.policy.name}
          confirmLabel="Detach"
          busy={action.busy}
        />
      )}
    </div>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────
function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

// ── Redaction policy editor (m18.14, ADR 0019) ────────────────────────────────
// The per-agent custom redaction detectors (name + RE2 pattern), on top of the
// always-on built-in redaction. Editing gates on agentdeployments update; a bad
// name/regex (rejected by the CRD validation) surfaces inline as a 422. No secret
// material is ever rendered.
type RedactionLoad =
  | { kind: "loading" }
  | { kind: "ready" }
  | { kind: "error"; message: string; forbidden: boolean };

function RedactionPanel({ ns, agentName }: { ns: string; agentName: string }) {
  const { can } = useCapabilities();
  const canEdit = can(RES_AGENTS, "update");
  const { toast } = useToast();
  const [load, setLoad] = React.useState<RedactionLoad>({ kind: "loading" });
  const [rows, setRows] = React.useState<{ name: string; pattern: string }[]>([]);
  const [saving, setSaving] = React.useState(false);
  const [saveError, setSaveError] = React.useState<string | null>(null);

  const reload = React.useCallback(() => {
    const controller = new AbortController();
    setLoad({ kind: "loading" });
    api
      .getTracePolicy(ns, agentName, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRows(res.customDetectors ?? []);
        setLoad({ kind: "ready" });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const forbidden = err instanceof ApiError && err.isForbidden;
        setLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load the redaction policy",
          forbidden,
        });
      });
    return () => controller.abort();
  }, [ns, agentName]);

  React.useEffect(() => reload(), [reload]);

  async function save() {
    setSaving(true);
    setSaveError(null);
    try {
      const cleaned = rows.filter((r) => r.name.trim() || r.pattern.trim());
      const res = await api.updateTracePolicy(ns, agentName, { customDetectors: cleaned });
      setRows(res.customDetectors ?? []);
      toast({
        variant: "success",
        title: "Redaction policy saved",
        description: `${cleaned.length} custom detector${cleaned.length === 1 ? "" : "s"}.`,
      });
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "save failed");
    } finally {
      setSaving(false);
    }
  }

  if (load.kind === "loading") {
    return (
      <div className="rounded-lg border border-border bg-card p-5 text-sm text-faint">
        Loading the redaction policy…
      </div>
    );
  }
  if (load.kind === "error") {
    return (
      <div className="rounded-lg border border-border bg-card p-5 text-sm text-destructive" role="alert">
        {load.forbidden
          ? "Not allowed to read this agent's redaction policy."
          : load.message}
      </div>
    );
  }

  return (
    <Card className="min-w-0" data-testid="redaction-panel">
      <PanelHeader title="Redaction" />
      <CardContent className="space-y-4">
        <p className="max-w-[66ch] text-sm text-secondary-foreground">
          Extra named regex rules applied to trace payloads before they are stored —
          on top of the always-on built-in detectors (emails, keys, SSNs). Each match
          becomes <span className="font-mono text-xs">[REDACTED:name]</span>.
        </p>

      {rows.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No custom detectors — only the built-in redaction is active.
        </p>
      )}

      <div className="space-y-2">
        {rows.map((r, i) => (
          <div key={i} className="flex items-center gap-2" data-testid={`detector-${i}`}>
            <Input
              aria-label={`Detector ${i} name`}
              placeholder="name (e.g. badge)"
              value={r.name}
              disabled={!canEdit}
              className="w-40"
              onChange={(e) =>
                setRows((rs) => rs.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))
              }
            />
            <Input
              aria-label={`Detector ${i} pattern`}
              placeholder="RE2 pattern (e.g. BADGE-[0-9]+)"
              value={r.pattern}
              disabled={!canEdit}
              className="flex-1 font-mono"
              onChange={(e) =>
                setRows((rs) => rs.map((x, j) => (j === i ? { ...x, pattern: e.target.value } : x)))
              }
            />
            {canEdit && (
              <Button
                variant="ghost"
                size="sm"
                data-testid={`remove-detector-${i}`}
                onClick={() => setRows((rs) => rs.filter((_, j) => j !== i))}
              >
                Remove
              </Button>
            )}
          </div>
        ))}
      </div>

      {saveError && (
        <p className="text-sm text-destructive" role="alert" data-testid="redaction-error">
          {saveError}
        </p>
      )}

      {canEdit && (
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            data-testid="add-detector"
            onClick={() => setRows((rs) => [...rs, { name: "", pattern: "" }])}
          >
            Add detector
          </Button>
          <Button
            size="sm"
            disabled={saving}
            data-testid="save-redaction"
            onClick={() => void save()}
          >
            {saving ? "Saving…" : "Save policy"}
          </Button>
        </div>
      )}
      </CardContent>
    </Card>
  );
}

// ── Improvement-Loop Section (m69.11, ADR 0062) ───────────────────────────────
// Surfaces on the Overview tab. Shows:
//   1. The serving version's 3-component online score (operational/feedback/judge)
//      + a RegressionDetected badge (from status.conditions).
//   2. When gate.phase == "canary": two arms side-by-side (old vs candidate) each
//      with their per-version online-score components.
//   3. A rollback button (confirm-guarded): picks a version from history, POSTs
//      the rollback annotation via the caller's token. Degrades calmly when the
//      online-score store is unconfigured (501 = calm "not available").

type OnlineScoreLoad =
  | { kind: "loading" }
  | { kind: "ready"; data: OnlineScoreResponse }
  | { kind: "unavailable" }  // 501 — store not configured
  | { kind: "error"; message: string };

function ImprovementLoopSection({
  ns,
  name,
  conditions,
  gatePhase,
  versions,
}: {
  ns: string;
  name: string;
  conditions: AgentCondition[];
  gatePhase?: string;
  versions: string[];
}) {
  const { toast } = useToast();
  const [scoreLoad, setScoreLoad] = React.useState<OnlineScoreLoad>({ kind: "loading" });
  const [rollbackTarget, setRollbackTarget] = React.useState<string>("");
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const [rolling, setRolling] = React.useState(false);

  React.useEffect(() => {
    const controller = new AbortController();
    setScoreLoad({ kind: "loading" });
    api
      .agentOnlineScore(ns, name, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        if (res === null) {
          setScoreLoad({ kind: "unavailable" });
          return;
        }
        setScoreLoad({ kind: "ready", data: res });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setScoreLoad({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load online score",
        });
      });
    return () => controller.abort();
  }, [ns, name]);

  // RegressionDetected condition from status.conditions.
  const regressionCond = conditions.find((c) => c.type === "RegressionDetected");
  const regressionDetected = regressionCond?.status === "True";

  const isCanary = gatePhase === "canary";

  // Per-version windows for canary arms (group the most recent window per version).
  const latestByVersion = React.useMemo((): Map<string, OnlineScoreWindow> => {
    if (scoreLoad.kind !== "ready") return new Map();
    const map = new Map<string, OnlineScoreWindow>();
    for (const w of scoreLoad.data.windows) {
      if (!map.has(w.agentVersion)) map.set(w.agentVersion, w);
    }
    return map;
  }, [scoreLoad]);

  async function doRollback() {
    if (!rollbackTarget) return;
    setRolling(true);
    try {
      await api.agentRollback(ns, name, rollbackTarget);
      toast({
        variant: "success",
        title: "Rollback requested",
        description: `Annotation set: rollback to "${rollbackTarget}" requested. The controller will actuate it.`,
      });
      setConfirmOpen(false);
      setRollbackTarget("");
    } catch (err) {
      toast({
        variant: "error",
        title: "Rollback failed",
        description: err instanceof Error ? err.message : "rollback failed",
      });
    } finally {
      setRolling(false);
    }
  }

  // Nothing to score, nothing gated, nothing to roll back to. This is now a
  // whole TAB, so it may not render nothing — an empty tab reads as a bug. It
  // says what the loop is and what wiring it would take instead (§7.1).
  if (
    scoreLoad.kind === "unavailable" &&
    !regressionDetected &&
    !isCanary &&
    versions.length === 0
  ) {
    return (
      <Card className="min-w-0" data-testid="improvement-loop-section">
        <PanelHeader title="Online score" />
        <CardContent>
          <QuietNote title="The improvement loop isn’t configured.">
            Online scoring reads production runs back into a control-plane store
            and compares each version against its baseline. No store is wired up
            here, and this agent has no version history to compare, so there is
            nothing to show. Nothing on this tab is estimated — the scores are
            simply absent.
          </QuietNote>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card className="min-w-0" data-testid="improvement-loop-section">
      <PanelHeader title="Online score">
        {regressionDetected && (
          <Badge variant="crit" data-testid="regression-detected-badge">
            Regression detected
          </Badge>
        )}
        {regressionCond && !regressionDetected && regressionCond.status === "False" && (
          <Badge variant="ok" data-testid="regression-ok-badge">
            Healthy
          </Badge>
        )}
      </PanelHeader>
      <CardContent className="space-y-4">
        <p className="max-w-[66ch] text-sm text-secondary-foreground">
          How the serving version is actually doing in production, across the
          three signals the platform records: what the runtime measured, what
          people said, and what the judge scored.
        </p>

        {/* Online score content */}
        {scoreLoad.kind === "loading" && (
          <div role="status" aria-busy="true" aria-label="Loading the online score" data-testid="online-score-loading">
            <Skeleton decorative className="mb-3 h-3.5 w-full" />
            <Skeleton decorative className="h-3.5 w-2/3" />
          </div>
        )}
        {scoreLoad.kind === "unavailable" && (
          <div data-testid="online-score-unavailable">
            <QuietNote title="Online score not available.">
              The control-plane score store is not configured, so production runs
              are never read back and no version has a score. Wiring one up is
              what fills this panel. Nothing here is estimated — the figures are
              simply absent.
            </QuietNote>
          </div>
        )}
        {scoreLoad.kind === "error" && (
          <p className="text-sm text-destructive" data-testid="online-score-error">
            {scoreLoad.message}
          </p>
        )}

        {scoreLoad.kind === "ready" && (
          <>
            {scoreLoad.data.windows.length === 0 ? (
              <div data-testid="online-score-empty">
                <QuietNote title="No score data yet.">
                  The store is wired up, but no production runs have been
                  recorded against this agent — so there is nothing to score.
                  Figures appear here once it starts serving.
                </QuietNote>
              </div>
            ) : isCanary ? (
              /* Canary arms: two versions side-by-side */
              <CanaryArms latestByVersion={latestByVersion} />
            ) : (
              /* Serving version: most-recent window */
              <OnlineScoreCard window={scoreLoad.data.windows[0]} />
            )}
          </>
        )}

        {/* Rollback — shown when there is a version to go back to (RBAC-permissive:
            the server enforces). It is guarded by a confirm because it changes what
            production serves. */}
        {versions.length > 1 && (
          <div
            className="flex flex-wrap items-center gap-3 border-t border-border-soft pt-4"
            data-testid="rollback-section"
          >
            <RotateCcw aria-hidden="true" className="h-4 w-4 shrink-0 text-faint" />
            <p className="shrink-0 text-sm text-secondary-foreground">Roll back to</p>
            <select
              value={rollbackTarget}
              onChange={(e) => setRollbackTarget(e.target.value)}
              className="h-8 min-w-0 flex-1 rounded-md border border-border-strong bg-card px-3 font-mono text-xs"
              aria-label="Version to roll back to"
              data-testid="rollback-version-select"
            >
              <option value="">— choose a version —</option>
              {versions.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
            <Button
              variant="outline"
              size="sm"
              disabled={!rollbackTarget}
              onClick={() => setConfirmOpen(true)}
              data-testid="rollback-button"
            >
              Rollback
            </Button>
          </div>
        )}

      {/* Confirm dialog — guards the destructive annotation write */}
      <ConfirmDialog
        open={confirmOpen}
        onCancel={() => setConfirmOpen(false)}
        onConfirm={() => void doRollback()}
        title={`Rollback ${name} to ${rollbackTarget}?`}
        description={`This will set the rollback annotation on ${name}. The controller will revert the serving spec to version "${rollbackTarget}", subject to cooldown and flap guards. The annotation is one-shot (cleared after evaluation).`}
        confirmLabel="Rollback"
        busy={rolling}
        destructive
      />
      </CardContent>
    </Card>
  );
}

// OnlineScoreCard renders the most-recent window for the serving version —
// all 3 components with clear labels so the operator sees the full picture.
function OnlineScoreCard({ window: w }: { window: OnlineScoreWindow }) {
  // A rate over zero requests is not zero — it is unmeasurable, and the two
  // must never share a glyph (§7.1). `UNKNOWN` is the honest branch and the
  // compiler will not let it be formatted as a number.
  const pct = (n: number) => `${n.toFixed(1)}%`;
  const avg = (n: number) => n.toFixed(2);
  const errorRate =
    w.operational.total > 0
      ? (w.operational.errorCount / w.operational.total) * 100
      : UNKNOWN;
  const toolFailRate =
    w.operational.total > 0
      ? (w.operational.toolFailCount / w.operational.total) * 100
      : UNKNOWN;
  const feedbackAvg =
    w.feedback.count > 0 ? w.feedback.sumVal / w.feedback.count : UNKNOWN;
  const judgeAvg = w.judge.count > 0 ? w.judge.sumVal / w.judge.count : UNKNOWN;

  const Component = ({
    title,
    testId,
    items,
  }: {
    title: string;
    testId: string;
    items: KeyValueItem[];
  }) => (
    <div className="min-w-0 rounded-md border border-border bg-surface-2 p-4" data-testid={testId}>
      <p className="mb-2 font-mono text-2xs uppercase tracking-wide text-faint">{title}</p>
      <KeyValueList items={items} />
    </div>
  );

  return (
    // Three across only where three fit; stacked below `md`, where a 90px
    // column would make every figure wrap.
    <div className="grid grid-cols-1 gap-3 md:grid-cols-3" data-testid="online-score-card">
      <Component
        title="Operational"
        testId="operational-component"
        items={[
          { key: "Requests", value: <QuantityValue value={w.operational.total} /> },
          {
            key: "Error rate",
            value: (
              <QuantityValue
                value={errorRate}
                format={pct}
                title="No requests in this window — the rate is unmeasurable, not zero."
              />
            ),
          },
          {
            key: "Tool fail",
            value: (
              <QuantityValue
                value={toolFailRate}
                format={pct}
                title="No requests in this window — the rate is unmeasurable, not zero."
              />
            ),
          },
          {
            key: "p95 latency",
            value: (
              <QuantityValue
                value={w.operational.latencyP95Ms > 0 ? w.operational.latencyP95Ms : UNKNOWN}
                format={formatLatency}
              />
            ),
          },
        ]}
      />
      <Component
        title="Feedback"
        testId="feedback-component"
        items={[
          { key: "Count", value: <QuantityValue value={w.feedback.count} /> },
          {
            key: "Avg score",
            value: (
              <QuantityValue
                value={feedbackAvg}
                format={avg}
                title="Nobody has rated a run in this window — unknown, not zero."
              />
            ),
          },
        ]}
      />
      <Component
        title="Judge"
        testId="judge-component"
        items={[
          { key: "Count", value: <QuantityValue value={w.judge.count} /> },
          {
            key: "Avg score",
            value: (
              <QuantityValue
                value={judgeAvg}
                format={avg}
                title="The judge has scored nothing in this window — unknown, not zero."
              />
            ),
          },
        ]}
      />
    </div>
  );
}

// CanaryArms renders the two canary arms side-by-side when gate.phase == "canary":
// OLD (baseline serving revision) vs CANDIDATE (new revision being canary-tested).
// Each arm shows its own online-score components so regressions are visible before
// the promotion decision is made.
function CanaryArms({
  latestByVersion,
}: {
  latestByVersion: Map<string, OnlineScoreWindow>;
}) {
  // Derive the two arms: we have ≤N versions; we show all distinct versions.
  // When exactly 2 versions: label the newest as Candidate, the other as Baseline.
  const versions = [...latestByVersion.keys()];
  if (versions.length === 0) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="canary-arms-empty">
        No per-version score data yet — still accumulating.
      </p>
    );
  }

  // Sort by the window start of the version's latest window, newest last = candidate.
  const sorted = versions.sort((a, b) => {
    const wa = latestByVersion.get(a)?.windowStart ?? "";
    const wb = latestByVersion.get(b)?.windowStart ?? "";
    return wa.localeCompare(wb);
  });

  return (
    <div className="space-y-3" data-testid="canary-arms">
      <p className="text-xs text-muted-foreground">
        Canary in progress — comparing serving arms:
      </p>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        {sorted.map((v: string, i: number) => {
          const w = latestByVersion.get(v)!;
          const label = sorted.length === 2
            ? i === 0 ? "Baseline (old)" : "Candidate (new)"
            : v;
          return (
            <div
              key={v}
              className="rounded-md border bg-surface-2 p-3 space-y-2"
              data-testid={`canary-arm-${i === 0 ? "old" : "candidate"}`}
            >
              <div className="flex items-center gap-2">
                <Badge
                  variant={sorted.length === 2 && i === 1 ? "muted" : "open"}
                >
                  {label}
                </Badge>
                <span className="font-mono text-2xs text-faint truncate">{v}</span>
              </div>
              <OnlineScoreCard window={w} />
            </div>
          );
        })}
      </div>
    </div>
  );
}



// ── Publish-as-template dialog (m74.6) ───────────────────────────────────────
// Mirrors the m73.7 PublishDialog in mcp-servers-page.tsx. Pick visibility
// (team/org/public) → POST /api/templates → on success close; on 403 surface
// the tier requirement honestly. Public requires an explicit confirm checkbox
// (blast-radius acknowledgement).
type PublishVisibility = "team" | "org" | "public";

function PublishTemplateDialog({
  agentNamespace,
  agentName,
  alreadyPublished,
  onClose,
  onDone,
}: {
  agentNamespace: string;
  agentName: string;
  // U7: whether the agent is already published this session — warn against silent re-publish.
  alreadyPublished?: boolean;
  onClose: () => void;
  // U8: onDone now passes back the publish response + chosen visibility.
  onDone: (res: PublishTemplateResponse, visibility: string) => void;
}) {
  const { toast } = useToast();
  const [selected, setSelected] = React.useState<PublishVisibility>("team");
  const [publicConfirmed, setPublicConfirmed] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // U8 / U12: inline error state — keep dialog open on failure with an error message.
  const [inlineError, setInlineError] = React.useState<string | null>(null);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  function handleSelect(v: PublishVisibility) {
    setSelected(v);
    if (v !== "public") setPublicConfirmed(false);
    setInlineError(null);
  }

  // U7: block re-publish at the same visibility (prevent silent overwrite without acknowledgement).
  const isPublishDisabled =
    busy || (selected === "public" && !publicConfirmed);

  async function onPublish() {
    if (isPublishDisabled) return;
    setInlineError(null);
    setBusy(true);
    try {
      const res = await api.publishTemplate("agent", agentNamespace, agentName, selected);
      toast({
        variant: "success",
        title: "Shared as template",
        description: `${agentName} v${res.version ?? "1"} is now available as a ${selected}-visible template.`,
      });
      onDone(res, selected);
    } catch (err) {
      const isForbidden = err instanceof ApiError && err.isForbidden;
      const serverMsg = err instanceof ApiError ? err.message : null;
      // U8 / U12: keep dialog open, show error inline instead of closing.
      // U15: prefer the server's REAL message (server-truth roles) — matching the MCP dialog — and
      // fall back to a human role string only when the server gave nothing.
      const fallback = isForbidden
        ? `You need ${
            selected === "public"
              ? "Platform-admin"
              : selected === "org"
              ? "Tenant-admin"
              : "team-admin"
          } rights to share ${selected}-wide.`
        : err instanceof Error
        ? err.message
        : "publish failed";
      setInlineError(serverMsg || fallback);
      setBusy(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${agentName} as template`}
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-md rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">
          Share {agentName} as a template
        </h2>
        {/* U8: immutable snapshot copy note */}
        <p className="mt-1 text-sm text-muted-foreground">
          Sharing a snapshot creates an <strong>immutable snapshot</strong> of the current definition —
          your secrets and credentials are never shared. Share again to publish a new version.
        </p>
        {/* U7: warn if already published */}
        {alreadyPublished && (
          <p
            className="mt-2 text-sm text-warning"
            data-testid="publish-template-already-published-warning"
          >
            This agent is already published. Sharing again creates a new version at the
            selected visibility.
          </p>
        )}
        <div className="mt-4 space-y-2">
          {(["team", "org", "public"] as PublishVisibility[]).map((v) => (
            <label
              key={v}
              className="flex cursor-pointer items-center gap-3 rounded-md border p-3 hover:bg-surface-2"
              data-testid={`publish-template-option-${v}`}
            >
              <input
                type="radio"
                name="template-visibility"
                value={v}
                checked={selected === v}
                onChange={() => handleSelect(v)}
                className="accent-primary"
              />
              <div>
                <p className="font-medium capitalize">{v}</p>
                <p className="text-xs text-muted-foreground">
                  {v === "team"
                    ? "Visible to your team's namespace"
                    : v === "org"
                    ? "Visible org-wide (Tenant-admin required)"
                    : "Visible to everyone (Platform-admin required)"}
                </p>
              </div>
            </label>
          ))}
        </div>
        {selected === "public" && (
          <div className="mt-3 space-y-2">
            <p className="text-sm text-warning" data-testid="publish-template-public-warning">
              Public means every tenant on this cluster can discover and fork this template.
            </p>
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={publicConfirmed}
                onChange={(e) => setPublicConfirmed(e.target.checked)}
                className="accent-primary"
                data-testid="publish-template-public-confirm"
              />
              I understand this template is discoverable by all tenants
            </label>
          </div>
        )}
        {/* U8 / U12: inline error — keep dialog open, no HTTP copy leak */}
        {inlineError && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="publish-template-error"
          >
            {inlineError}
          </p>
        )}
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onPublish()}
            disabled={isPublishDisabled}
            data-testid="publish-template-submit"
          >
            {/* U8: rename from "Publish as X" to "Share as template" */}
            {busy ? "Sharing…" : "Share as template"}
          </Button>
        </div>
      </div>
    </div>
  );
}
