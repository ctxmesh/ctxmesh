// routes.ts — the visual sweep's route inventory (M151).
//
// Every reachable product surface, with its params filled in so the route
// resolves to a real render rather than a 404. This list is derived from the
// route table in src/App.tsx and is asserted against it by a test, so a new
// route cannot be added to the app without also being swept.
//
// `/design/*` is deliberately absent: it is a flag-gated review artifact, not a
// product surface, and a normal build ships no route for it.

export type Chrome = "shell" | "bare";

export interface RouteCase {
  /** URL path to visit. */
  path: string;
  /** Stable id used for the screenshot filename and the report key. */
  id: string;
  /** Human label for the report. */
  label: string;
  /** Whether the page renders inside AppShell or stands alone (login, chat). */
  chrome: Chrome;
  /** Archetype the design spec assigns this page (M151 m151.2). */
  archetype: string;
}

export const ROUTES: RouteCase[] = [
  // ── Home ────────────────────────────────────────────────────────────────
  { id: "home", path: "/", label: "Home", chrome: "shell", archetype: "home" },

  // ── Agents ──────────────────────────────────────────────────────────────
  { id: "agents", path: "/agents", label: "Fleet", chrome: "shell", archetype: "index" },
  { id: "agents-new", path: "/agents/new", label: "Create agent", chrome: "shell", archetype: "wizard" },
  { id: "agent-detail", path: "/agents/default/demo-assistant", label: "Agent detail", chrome: "shell", archetype: "detail" },
  { id: "teams", path: "/teams", label: "Teams", chrome: "shell", archetype: "index" },
  { id: "teams-new", path: "/teams/new", label: "Create team", chrome: "shell", archetype: "wizard" },
  { id: "playground", path: "/playground", label: "Playground", chrome: "shell", archetype: "playground" },

  // ── Library ─────────────────────────────────────────────────────────────
  { id: "tools-catalog", path: "/tools/catalog", label: "Tool catalog", chrome: "shell", archetype: "index" },
  { id: "mcp-servers", path: "/tools/mcp-servers", label: "MCP servers", chrome: "shell", archetype: "index" },
  { id: "mcp-add", path: "/tools/add-mcp", label: "Add MCP server", chrome: "shell", archetype: "wizard" },
  { id: "knowledgebases", path: "/knowledgebases", label: "Knowledge bases", chrome: "shell", archetype: "index" },
  { id: "kb-detail", path: "/knowledgebases/default/demo-kb", label: "Knowledge base detail", chrome: "shell", archetype: "detail" },
  { id: "prompts", path: "/prompts", label: "Prompts", chrome: "shell", archetype: "index" },
  { id: "gallery", path: "/gallery", label: "Template gallery", chrome: "shell", archetype: "gallery" },
  { id: "workflows", path: "/workflows", label: "Workflows", chrome: "shell", archetype: "index" },
  { id: "workflow-detail", path: "/workflows/default/demo-flow", label: "Workflow detail", chrome: "shell", archetype: "detail" },
  { id: "config", path: "/config", label: "Config builder", chrome: "shell", archetype: "authoring" },

  // ── Govern ──────────────────────────────────────────────────────────────
  { id: "approvals", path: "/approvals", label: "Approvals", chrome: "shell", archetype: "index" },
  { id: "mcp-approvals", path: "/tools/approvals", label: "MCP approvals", chrome: "shell", archetype: "index" },
  { id: "guardrails", path: "/guardrails", label: "Guardrails", chrome: "shell", archetype: "index" },
  { id: "evals", path: "/evals", label: "Evals", chrome: "shell", archetype: "index" },
  { id: "datasets", path: "/datasets", label: "Datasets", chrome: "shell", archetype: "index" },
  { id: "dataset-detail", path: "/datasets/demo-dataset", label: "Dataset detail", chrome: "shell", archetype: "detail" },
  { id: "registries", path: "/registries", label: "Registries", chrome: "shell", archetype: "index" },
  { id: "registries-new", path: "/registries/new", label: "New registry", chrome: "shell", archetype: "form" },
  { id: "registry-detail", path: "/registries/default/demo-registry", label: "Registry detail", chrome: "shell", archetype: "detail" },
  { id: "audit", path: "/audit", label: "Audit", chrome: "shell", archetype: "index" },

  // ── Activity ────────────────────────────────────────────────────────────
  { id: "runs", path: "/runs", label: "Runs", chrome: "shell", archetype: "index" },
  { id: "run-detail", path: "/runs/run-9f2a41c8-0d17-4b6e-9a55-3c1e77b42d90", label: "Run detail", chrome: "shell", archetype: "timeline" },
  { id: "trace", path: "/traces/trace-1", label: "Trace", chrome: "shell", archetype: "timeline" },
  { id: "cost", path: "/cost", label: "Cost", chrome: "shell", archetype: "index" },
  { id: "alerts", path: "/alerts", label: "Alerts", chrome: "shell", archetype: "index" },
  { id: "my-shares", path: "/my-shares", label: "My shares", chrome: "shell", archetype: "index" },
  { id: "topology", path: "/topology", label: "Topology", chrome: "shell", archetype: "canvas" },

  // ── Admin ───────────────────────────────────────────────────────────────
  { id: "providers", path: "/providers", label: "Providers", chrome: "shell", archetype: "index" },
  { id: "providers-connect", path: "/providers/connect", label: "Connect provider", chrome: "shell", archetype: "wizard" },
  { id: "routes-list", path: "/routes", label: "Model routes", chrome: "shell", archetype: "index" },
  { id: "routes-new", path: "/routes/new", label: "New model route", chrome: "shell", archetype: "form" },
  { id: "route-detail", path: "/routes/default/demo-route", label: "Model route detail", chrome: "shell", archetype: "detail" },
  { id: "secrets", path: "/secrets", label: "Secret bindings", chrome: "shell", archetype: "index" },
  { id: "secrets-new", path: "/secrets/new", label: "New secret binding", chrome: "shell", archetype: "form" },
  { id: "secret-detail", path: "/secrets/default/demo-secret", label: "Secret binding detail", chrome: "shell", archetype: "detail" },
  { id: "tenants", path: "/tenants", label: "Tenants", chrome: "shell", archetype: "index" },

  // ── Shells and edges ────────────────────────────────────────────────────
  { id: "soon", path: "/soon/settings", label: "Placeholder (not yet built)", chrome: "shell", archetype: "placeholder" },
  { id: "notfound", path: "/no-such-page", label: "Not found", chrome: "shell", archetype: "placeholder" },
  { id: "login", path: "/login", label: "Login", chrome: "bare", archetype: "auth" },
  { id: "auth-callback", path: "/auth/callback", label: "OIDC callback", chrome: "bare", archetype: "auth" },
  { id: "shared-run", path: "/shared/runs/share-tok-1", label: "Shared run (public)", chrome: "bare", archetype: "timeline" },
  { id: "chat", path: "/chat/default/demo-assistant", label: "Agent chatbox", chrome: "bare", archetype: "chat" },
];

/** Viewport widths the design must hold at. Height is generous so a full page
 *  fits in one shot; overflow is judged on the horizontal axis. */
export const WIDTHS = [1440, 1280, 1024, 768] as const;

export const THEMES = ["light", "dark"] as const;

export type Theme = (typeof THEMES)[number];
