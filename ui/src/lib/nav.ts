import {
  Boxes,
  Coins,
  FlaskConical,
  GitBranch,
  KeyRound,
  LayoutDashboard,
  MessagesSquare,
  Network,
  PlugZap,
  SlidersHorizontal,
  Sparkles,
  TestTube2,
  Users,
  Wrench,
  BookOpen,
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
export type Milestone = "M13" | "M14" | "M15" | "M16" | "M17" | "M18";

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
   * When set, this destination is a WRITE affordance — hidden from a viewer's
   * chrome (display-only, ADR 0011). Gated on `allowed[requiresWrite.resource]
   * [requiresWrite.verb]`. Read-only destinations omit it and are always shown.
   */
  requiresWrite?: { resource: string; verb: string };
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
        id: "agents",
        label: "Agents",
        icon: Boxes,
        milestone: "M13",
        route: "/agents",
      },
      {
        // The create-agent wizard (m14.10) — the aha's heart: Describe it /
        // Configure it → one review + tool picker → Create. A WRITE surface
        // (it creates AgentDeployments), hidden from a viewer's nav; gates on
        // create agentdeployments. This is the primary "new agent" entry point.
        id: "new-agent",
        label: "New agent",
        icon: Sparkles,
        milestone: "M14",
        route: "/agents/new",
        requiresWrite: { resource: RES_AGENTS, verb: "create" },
      },
      {
        // The re-housed config-builder — a WRITE surface (it applies CRDs), so
        // it is hidden from a viewer's nav. It gates on create agentdeployments.
        id: "config",
        label: "Config builder",
        icon: SlidersHorizontal,
        milestone: "M13",
        route: "/config",
        requiresWrite: { resource: RES_AGENTS, verb: "create" },
      },
      {
        // The re-housed Playground — running an agent is a create/invoke-shaped
        // op; a viewer's chrome hides it (they still get an honest 403 if they
        // reach it directly). Gated on create agentdeployments (the run path).
        id: "playground",
        label: "Playground",
        icon: FlaskConical,
        milestone: "M13",
        route: "/playground",
        requiresWrite: { resource: RES_AGENTS, verb: "create" },
      },
      {
        // Add-an-MCP wizard (m14.9). Registering a BYO MCP server creates a
        // ToolRegistry entry (+ a Secret for its key) — a WRITE surface, hidden
        // from a viewer's nav. Gates on create agentregistries (the catalog seam
        // the discovered tools land in). The full tool CATALOG page is a later
        // task; this entry opens the add-server wizard directly.
        id: "tools",
        label: "Add MCP server",
        icon: Wrench,
        milestone: "M14",
        route: "/tools/add-mcp",
        requiresWrite: { resource: RES_REGISTRIES, verb: "create" },
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
        requiresWrite: { resource: RES_REGISTRIES, verb: "update" },
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
    ],
  },
  {
    heading: "Platform",
    items: [
      {
        // Connected-providers LIST page (m18.5). Read-only for everyone (a viewer
        // sees connected providers); the write actions (Connect / Rotate key /
        // Disconnect) are gated IN the page on secretbindings create/update/delete.
        // The connect wizard is reached via the page's "Connect provider" CTA.
        id: "providers",
        label: "Providers",
        icon: PlugZap,
        milestone: "M14",
        route: "/providers",
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
