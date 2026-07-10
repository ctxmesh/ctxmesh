import type { Milestone } from "@/design/scaffold";
import {
  DashboardWireframe,
  LoginWireframe,
  ShellWireframe,
} from "@/design/wireframes/auth-shell";
import {
  AddMcpWireframe,
  ConnectProviderWireframe,
  CreateAgentConfigureWireframe,
  CreateAgentDescribeWireframe,
} from "@/design/wireframes/wizards";
import {
  AgentDetailViewerWireframe,
  AgentDetailWireframe,
  AgentsListViewerWireframe,
  AgentsListWireframe,
} from "@/design/wireframes/agents";
import { TopologyScaleWireframe } from "@/design/wireframes/topology";
import {
  CostWireframe,
  FeedbackBrowserWireframe,
  RunsBrowserWireframe,
  TraceExplorerWireframe,
} from "@/design/wireframes/observability";
import {
  EvalWireframe,
  PromptDiffWireframe,
  ToolCatalogWireframe,
} from "@/design/wireframes/authoring";
import {
  CreateEntranceWireframe,
  DevModeWireframe,
  RegistryEditorWireframe,
  RoutesSecretsWireframe,
  SettingsProvidersWireframe,
} from "@/design/wireframes/platform";

// The wireframe registry — the source of truth for the gallery index and the
// per-screen routes. Each entry has a stable slug, a title, the milestone that
// OWNS the surface, a one-line purpose, and its component. Grouped by section
// for the gallery's navigation. Adding a screen = adding one entry here.

export interface Wireframe {
  slug: string;
  title: string;
  milestone: Milestone;
  section: string;
  purpose: string;
  component: React.ComponentType;
}

export const WIREFRAMES: Wireframe[] = [
  // ── Auth & shell ──────────────────────────────────────────────────────
  { slug: "login", title: "Login (+ wrong-token error)", milestone: "M13", section: "Auth & shell", purpose: "Token login; honest 401 state", component: LoginWireframe },
  { slug: "shell", title: "App shell + cmd-K", milestone: "M13", section: "Auth & shell", purpose: "Sidebar IA, who-am-I header, ⌘K palette", component: ShellWireframe },
  { slug: "dashboard", title: "Dashboard v2 (+ teaching empty)", milestone: "M13", section: "Auth & shell", purpose: "Operator landing; connect-provider empty state", component: DashboardWireframe },

  // ── First agent (the aha) ─────────────────────────────────────────────
  { slug: "connect-provider", title: "Connect-provider wizard", milestone: "M14", section: "First agent", purpose: "provider → key → live models → done", component: ConnectProviderWireframe },
  { slug: "add-mcp", title: "Add-MCP wizard", milestone: "M14", section: "First agent", purpose: "URL → probe → auth → discovered tools", component: AddMcpWireframe },
  { slug: "create-entrance", title: "Create-agent entrance (the fork)", milestone: "M14", section: "First agent", purpose: "Describe it vs Configure it", component: CreateEntranceWireframe },
  { slug: "create-describe", title: "Create-agent · Describe it", milestone: "M14", section: "First agent", purpose: "Prompt-first hero → review → tools", component: CreateAgentDescribeWireframe },
  { slug: "create-configure", title: "Create-agent · Configure it", milestone: "M14", section: "First agent", purpose: "Multi-step form → shared review", component: CreateAgentConfigureWireframe },

  // ── Fleet management ──────────────────────────────────────────────────
  { slug: "agents-list", title: "Agents list (DataTable)", milestone: "M13", section: "Fleet management", purpose: "Paginated, filterable list", component: AgentsListWireframe },
  { slug: "agent-detail", title: "Agent detail (tabs + Run)", milestone: "M14", section: "Fleet management", purpose: "Overview/logs/runs/bindings + Run panel", component: AgentDetailWireframe },
  { slug: "topology-scale", title: "Topology v2 at scale (200)", milestone: "M15", section: "Fleet management", purpose: "Grouped/collapsed, search, zoom, node drawer", component: TopologyScaleWireframe },
  { slug: "registry-editor", title: "Registry editor", milestone: "M15", section: "Fleet management", purpose: "Members, roles, tool allowlist", component: RegistryEditorWireframe },
  { slug: "routes-secrets", title: "Routes & secrets lists", milestone: "M15", section: "Fleet management", purpose: "Model routes + secret bindings", component: RoutesSecretsWireframe },

  // ── Observability ─────────────────────────────────────────────────────
  { slug: "trace-explorer", title: "Native trace explorer", milestone: "M16", section: "Observability", purpose: "Span tree + waterfall + span detail; redacted state", component: TraceExplorerWireframe },
  { slug: "runs-browser", title: "Runs browser", milestone: "M16", section: "Observability", purpose: "Filterable runs on the list contract", component: RunsBrowserWireframe },
  { slug: "feedback-browser", title: "Feedback browser", milestone: "M16", section: "Observability", purpose: "Scores/comments ↔ traces", component: FeedbackBrowserWireframe },
  { slug: "cost", title: "Cost v2 (per-agent drill-down)", milestone: "M16", section: "Observability", purpose: "Spend rollups + per-agent breakdown", component: CostWireframe },

  // ── Authoring depth ───────────────────────────────────────────────────
  { slug: "tool-catalog", title: "Tool catalog", milestone: "M14", section: "Authoring depth", purpose: "Curated + user-added + pending-approval", component: ToolCatalogWireframe },
  { slug: "eval", title: "Eval builder + results", milestone: "M17", section: "Authoring depth", purpose: "Build a suite; browse results", component: EvalWireframe },
  { slug: "prompt-diff", title: "Prompt version diff", milestone: "M17", section: "Authoring depth", purpose: "Two-pane version diff", component: PromptDiffWireframe },
  { slug: "settings-providers", title: "Settings / providers", milestone: "M13", section: "Authoring depth", purpose: "Connected providers; danger zone", component: SettingsProvidersWireframe },

  // ── RBAC & dev-mode variants ──────────────────────────────────────────
  { slug: "agents-list-viewer", title: "Agents list — viewer variant", milestone: "M13", section: "RBAC & dev variants", purpose: "Read-only chrome (no write affordances)", component: AgentsListViewerWireframe },
  { slug: "agent-detail-viewer", title: "Agent detail — viewer variant", milestone: "M13", section: "RBAC & dev variants", purpose: "Edit/Delete hidden; Run explained-away", component: AgentDetailViewerWireframe },
  { slug: "dev-mode", title: "dev --ui chrome variant", milestone: "M18", section: "RBAC & dev variants", purpose: "Local badge, reduced nav, no cluster", component: DevModeWireframe },
];

export function wireframeBySlug(slug: string): Wireframe | undefined {
  return WIREFRAMES.find((w) => w.slug === slug);
}

// Section order for the gallery index (stable, first-seen).
export const SECTION_ORDER: string[] = Array.from(
  new Set(WIREFRAMES.map((w) => w.section)),
);
