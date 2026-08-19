import {
  Bell,
  BookOpen,
  Boxes,
  Building2,
  Coins,
  Database,
  FlaskConical,
  GitBranch,
  GitFork,
  KeyRound,
  LayoutDashboard,
  Library,
  MessagesSquare,
  Network,
  PlugZap,
  ScrollText,
  Shield,
  SlidersHorizontal,
  TestTube2,
  Users,
  Waypoints,
  Wrench,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

// nav.ts — the ONE source of truth for the console's information architecture
// (ui-foundation §6 re-housing). Both the REAL app shell (components/app-shell)
// and the DESIGN wireframes (design/console-chrome re-exports NAV_SECTIONS from
// here) read this list, so the approved IA and the shipped shell can never drift
// — approve the IA at the design gate and the shell follows by construction.
//
// This module is gallery-free (no design-only imports) so the production shell
// can consume it without pulling the review-only wireframes into the main chunk.
//
// Milestone tags mark which arc milestone SHIPS each surface. Only surfaces that
// exist TODAY (m13.5 re-housing scope) carry a `route`; the rest render a
// PlaceholderPage ("arrives in M<n>") so the full IA is walkable without pulling
// later features forward.

// Milestone is duplicated from design/scaffold to keep this module independent of
// the design gallery (the wireframes' own tag type). It is display-only.
export type Milestone =
  | "M13"
  | "M14"
  | "M15"
  | "M16"
  | "M17"
  | "M18"
  | "M47"
  | "M63"
  | "M64"
  | "M66"
  | "M67"
  | "M68"
  | "M69"
  | "M70"
  | "M73"
  | "M74"
  | "M76";

// The golden CRD resources the console probes capabilities for — the plural
// names the BFF's SelfSubjectAccessReview uses (internal/bff/identity.go). A nav
// item or action gates on `capabilities.allowed[resource][verb]`.
export const RES_AGENTS = "agentdeployments";
export const RES_ROUTES = "modelroutes";
export const RES_SECRETS = "secretbindings";
export const RES_REGISTRIES = "agentregistries";
export const RES_MEMORY = "memorybindings";
export const RES_SCALING = "agentscalingpolicies";
export const RES_EVALSUITES = "evalsuites";
export const RES_PROMPTVERSIONS = "promptversions";
export const RES_TENANTS = "tenants";
// RES_AUDITLOGS gates the operator-only Audit surface (ADR 0056). It is a virtual
// resource: only the operator persona's ClusterRole grants `list auditlogs`, so the
// nav item hides for developer/viewer chrome (display-only; the API still enforces).
export const RES_AUDITLOGS = "auditlogs";
// RES_GUARDRAIL is the plural resource name for GuardrailPolicies (m66.10, ADR 0059).
export const RES_GUARDRAIL = "guardrailpolicies";
// RES_ALERTPOLICIES gates the Alerts feed (M70, ADR 0063 D2). The caller-scoped SSAR
// authorizes against `list alertpolicies` — the same resource the CRD path enforced.
export const RES_ALERTPOLICIES = "alertpolicies";
// RES_KNOWLEDGEBASES gates the Knowledge Bases nav item (M99 C2): a persona that can't
// `list knowledgebases` (e.g. developer) must not see a nav item that then 403s. The BFF
// probes it in the golden set; display-only, the API still enforces.
export const RES_KNOWLEDGEBASES = "knowledgebases";

export interface NavItem {
  id: string;
  label: string;
  icon: LucideIcon;
  milestone: Milestone;
  /**
   * The router path this nav item routes to. Present only for surfaces that
   * exist in the current build (re-housed M12 surfaces); absent items render a
   * PlaceholderPage keyed on the milestone. `/` for the index (dashboard).
   */
  route?: string;
  /**
   * When set, this destination is capability-gated — hidden from any chrome whose
   * caller isn't `allowed[requiresCapability.resource][requiresCapability.verb]`
   * (display-only, ADR 0011). Read-open destinations omit it and are always shown.
   * The COMMON case is a write affordance hidden from a viewer (verb "create"/"update").
   * But a `list` (read) verb is also valid here as a deliberate OPERATOR-ONLY
   * *visibility* gate — e.g. Audit (`list auditlogs`, M63): a read-only page whose
   * data spans users/namespaces, so it's shown only to the operator persona, exactly
   * like MCP approvals. Renamed from `requiresWrite` (m76.5, H4) because the field
   * now also carries read-verb gates — the old name misled.
   */
  requiresCapability?: { resource: string; verb: string };
}

export interface NavSection {
  heading: string;
  items: NavItem[];
}

// THE PROPOSED IA — one flat, grouped sidebar telling a first-run story:
// Overview → Build → Observe → Platform → Settings (ui-foundation §6, the
// approved shell/IA wireframes). The four re-housed M12 surfaces carry a route;
// everything else is a milestone placeholder.
export const NAV_SECTIONS: NavSection[] = [
  {
    heading: "Overview",
    items: [
      {
        id: "dashboard",
        label: "Dashboard",
        icon: LayoutDashboard,
        milestone: "M13",
        route: "/",
      },
      {
        id: "topology",
        label: "Topology",
        icon: Network,
        milestone: "M15",
        route: "/topology",
      },
    ],
  },
  {
    heading: "Build",
    items: [
      {
        // Connect a provider — the FIRST step of the journey (the dashboard checklist
        // teaches "Connect a provider" first), so it LEADS "Build" rather than sitting
        // low under "Platform" where the eye reaches it last (m49.2 — the onboarding-
        // order fix, M46-close review P0). The connect wizard is the page's CTA; the
        // write actions (Connect / Rotate / Disconnect) are gated in-page on secretbindings.
        id: "providers",
        label: "Providers",
        icon: PlugZap,
        milestone: "M14",
        route: "/providers",
      },
      {
        id: "agents",
        label: "Agents",
        icon: Boxes,
        milestone: "M13",
        route: "/agents",
      },
      {
        // Agent Teams (M64, ADR 0057) — the orchestration rosters: a supervisor + summonable
        // sub-agents + a spawn budget. Read-open (the API is the RBAC gate); a team is authored via
        // YAML/kubectl for now (the conversational "describe → team" builder is M71).
        id: "teams",
        label: "Agent Teams",
        icon: Waypoints,
        milestone: "M64",
        route: "/teams",
      },
      {
        // Template Gallery (m74.6) — the unified gallery for agent templates
        // (recipes ∪ published agents) and the MCP catalog in two tabs. Fork
        // an agent recipe or connect an MCP server from one surface.
        // Read-open: any authenticated caller can browse; Fork/Connect are
        // gated server-side by agent creation rights.
        id: "gallery",
        label: "Gallery",
        icon: Library,
        milestone: "M74",
        route: "/gallery",
      },
      // "New agent" is NOT a nav item (m25 S8) — it lives as the primary action
      // (top-right button) ON the Agents page, next to the list it creates into,
      // rather than duplicating an agent-lifecycle entry in the sidebar. The
      // /agents/new route still exists; the button navigates to it.
      {
        // The re-housed Playground — running an agent is a create/invoke-shaped
        // op; a viewer's chrome hides it (they still get an honest 403 if they
        // reach it directly). Gated on create agentdeployments (the run path).
        id: "playground",
        label: "Playground",
        icon: FlaskConical,
        milestone: "M13",
        route: "/playground",
        requiresCapability: { resource: RES_AGENTS, verb: "create" },
      },
      {
        // Prompt version diff viewer (m17.12). Lists PromptVersions + side-by-side
        // textual diff. Readable by any authenticated caller; create/delete gated.
        id: "prompts",
        label: "Prompts",
        icon: GitBranch,
        milestone: "M17",
        route: "/prompts",
      },
      {
        // EvalSuite builder + results browser (m17.12). Lists EvalSuites + a
        // wizard to create; results view is read-open; create gated.
        id: "evals",
        label: "Evals",
        icon: TestTube2,
        milestone: "M17",
        route: "/evals",
      },
    ],
  },
  {
    // Tools (m23.7) — the three MCP/tool surfaces grouped into ONE area, out of
    // the "Build" agent-lifecycle flow they used to clutter (the audit B5): add a
    // BYO MCP server, approve pending ones, and browse the merged catalog.
    heading: "Tools",
    items: [
      {
        // MCP Servers LIST page (m25 S10) — lists every registered BYO MCP server
        // with an "Add MCP server" button ON the page (the add wizard is reached via
        // that CTA, not a separate add-only nav item). Read-open so a viewer sees the
        // servers; the Add button in-page is gated on create agentregistries.
        id: "mcp-servers",
        label: "MCP Servers",
        icon: Wrench,
        milestone: "M14",
        route: "/tools/mcp-servers",
      },
      {
        // MCP approval queue (m17.9). Operator-only: lists pending MCP servers
        // and lets the operator approve/reject them. Hidden from a viewer's nav
        // (non-operators can't approve). Gates on update agentregistries.
        id: "mcp-approvals",
        label: "MCP approvals",
        icon: Wrench,
        milestone: "M17",
        route: "/tools/approvals",
        requiresCapability: { resource: RES_REGISTRIES, verb: "update" },
      },
      {
        // Tool catalog (m17.10). The merged catalog of curated + user-added +
        // pending-approval tools. Readable by anyone; bind wizard is gated on
        // create mcptoolbindings. Uses BookOpen (distinct from Wrench above).
        id: "tool-catalog",
        label: "Tool catalog",
        icon: BookOpen,
        milestone: "M17",
        route: "/tools/catalog",
      },
    ],
  },
  {
    heading: "Observe",
    items: [
      // Runs IS the trace list — each run row drills into the native trace
      // explorer at /traces/:id (m16.7), which embeds the inline feedback panel
      // (m16.9). There is deliberately no standalone "Traces"/"Feedback" nav
      // destination; both are reached by drilling into a run.
      {
        id: "runs",
        label: "Runs",
        icon: MessagesSquare,
        milestone: "M16",
        route: "/runs",
      },
      {
        id: "cost",
        label: "Cost",
        icon: Coins,
        milestone: "M16",
        route: "/cost",
      },
      {
        // Datasets (M69) — human-labeled eval datasets for the improvement loop
        // (ADR 0062 Fork 5). Each dataset is a collection of redacted trace cases
        // labeled pass/fail/flag. Read-open (the API is the RBAC gate); add-case is
        // done from the trace view or via the from-run on-ramp.
        id: "datasets",
        label: "Datasets",
        icon: Database,
        milestone: "M69",
        route: "/datasets",
      },
      {
        // Audit (M63) — the compliance trail ("who connected/consented/invoked
        // what", ADR 0056). OPERATOR-ONLY: the trail spans users/namespaces, so
        // like MCP approvals it's gated (here on `list auditlogs`) and hidden from
        // developer/viewer chrome. A non-operator who deep-links gets an honest 403.
        id: "audit",
        label: "Audit",
        icon: ScrollText,
        milestone: "M63",
        route: "/audit",
        requiresCapability: { resource: RES_AUDITLOGS, verb: "list" },
      },
      {
        // Alerts (M70, ADR 0063 D2) — the fired-alert console feed: AlertPolicy
        // conditions that crossed their threshold. Read-only; the controller
        // auto-resolves on true→false transitions. Gated on `list alertpolicies`
        // so a caller without that RBAC sees an honest 403 rather than an empty list.
        id: "alerts",
        label: "Alerts",
        icon: Bell,
        milestone: "M70",
        route: "/alerts",
        requiresCapability: { resource: RES_ALERTPOLICIES, verb: "list" },
      },
    ],
  },
  {
    heading: "Platform",
    items: [
      {
        // Tenants (M47) — cluster-scoped namespace groupings with compute + model
        // quotas. Read-only for everyone (viewers/developers); operators manage
        // them (the RBAC split is enforced at the API server, ADR 0011).
        id: "tenants",
        label: "Tenants",
        icon: Building2,
        milestone: "M47",
        route: "/tenants",
      },
      {
        // GuardrailPolicies (m66.10, ADR 0059) — namespace-scoped content-governance
        // policies: PII scanning, pattern deny-lists, optional LLM-judge, per-user
        // rate limits. Read-open (the API is the RBAC gate); authored via YAML/kubectl.
        id: "guardrails",
        label: "Guardrail Policies",
        icon: Shield,
        milestone: "M66",
        route: "/guardrails",
      },
      {
        // Workflows (m67.9, ADR 0060) — declarative graphs of agent invocations:
        // conditional branching, map/loop control flow, deterministic execution.
        // Read-open (RBAC gate at the API server, ADR 0011); authored via YAML/kubectl.
        // Invoke affordance starts a workflow instance run from the console.
        id: "workflows",
        label: "Workflows",
        icon: GitFork,
        milestone: "M67",
        route: "/workflows",
      },
      {
        // KnowledgeBases (m68.13, ADR 0061) — managed RAG corpora: upload docs → ingest →
        // watch phase → test-query with citations. GATED on `list knowledgebases` (M99 C2) so a
        // persona that can't list them never sees a nav item that 403s; the API still enforces.
        id: "knowledgebases",
        label: "Knowledge Bases",
        icon: BookOpen,
        milestone: "M68",
        route: "/knowledgebases",
        requiresCapability: { resource: RES_KNOWLEDGEBASES, verb: "list" },
      },
    ],
  },
  {
    // Advanced (m20.8) — the raw Kubernetes objects that back the intent surfaces.
    // A user connects a provider and picks a model per agent; they never NEED these.
    // They live here (bottom, under "Advanced") for operators who want to inspect or
    // hand-author the AgentRegistry / ModelRoute / SecretBinding objects directly,
    // rather than sitting in the primary nav next to Providers (the object-shaped IA
    // the console used to expose). The pages are unchanged — only their placement.
    heading: "Advanced",
    items: [
      {
        // The config-builder — the raw agent.yaml → CRD-apply surface. Moved out
        // of "Build" (m23.7 / audit B3): "New agent" is the ONE primary create
        // entry; this hand-authoring path lives under Advanced for power users who
        // want to edit the YAML directly. Unchanged page — placement only. A WRITE
        // surface (applies CRDs), hidden from a viewer's nav.
        id: "config",
        label: "Config builder",
        icon: SlidersHorizontal,
        milestone: "M13",
        route: "/config",
        requiresCapability: { resource: RES_AGENTS, verb: "create" },
      },
      {
        id: "registries",
        label: "Registries",
        icon: Users,
        milestone: "M15",
        route: "/registries",
      },
      {
        id: "routes",
        label: "Model routes",
        icon: GitBranch,
        milestone: "M15",
        route: "/routes",
      },
      {
        id: "secrets",
        label: "Secret bindings",
        icon: KeyRound,
        milestone: "M15",
        route: "/secrets",
      },
    ],
  },
];

// NAV_ITEMS is the flat list (every section's items) — handy for route/lookup.
export const NAV_ITEMS: NavItem[] = NAV_SECTIONS.flatMap((s) => s.items);

// navRoute resolves a nav item's router path by id — the single lookup so any
// route the console references stays anchored to the IA source of truth above. It
// throws on an unknown id (a nav rename then fails loudly at module-load in tests,
// not silently at runtime).
export function navRoute(id: string): string {
  const item = NAV_ITEMS.find((i) => i.id === id);
  if (!item?.route) {
    throw new Error(`navRoute: no routed nav item with id "${id}"`);
  }
  return item.route;
}

// FirstRunStep is one guided step in the dashboard's first-run checklist. `doneKey`
// maps to the dashboard's live setup signals (provider/agent/run); `to` is derived
// from the nav surface the step drives so the checklist can't drift from the IA.
export interface FirstRunStep {
  label: string;
  to: string;
  doneKey: "provider" | "agent" | "run";
}

// FIRST_RUN_CHECKLIST — the dashboard's guided "get started" steps (m18.10),
// co-located with NAV_SECTIONS (m54.4) so the steps + the IA are the ONE source of
// truth reviewed together. Each `to` derives from the nav route it drives (the
// connect/new suffixes are the action affordances ON those surfaces) — a nav route
// change follows automatically instead of leaving a stale hardcoded path.
export const FIRST_RUN_CHECKLIST: FirstRunStep[] = [
  { label: "Connect a provider", to: `${navRoute("providers")}/connect`, doneKey: "provider" },
  { label: "Create an agent", to: `${navRoute("agents")}/new`, doneKey: "agent" },
  // The Playground — the taught run surface (nav's Build step). Was /agents (a
  // list, not a run affordance); m49.4 UX-review P1.
  { label: "Run your agent", to: navRoute("playground"), doneKey: "run" },
];
