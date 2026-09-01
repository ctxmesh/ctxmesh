import { lazy, Suspense } from "react";
import { Navigate, Route, Routes, useParams } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentChatboxPage } from "@/pages/agent-chatbox-page";
import { AgentDetailPage } from "@/pages/agent-detail-page";
import { AgentRegistriesPage } from "@/pages/agent-registries-page";
import { EvalsPage } from "@/pages/evals-page";
import { PromptsPage } from "@/pages/prompts-page";
import {
  AgentRegistryDetailPage,
  NewAgentRegistryPage,
} from "@/pages/agent-registry-detail-page";
import { AgentsPage } from "@/pages/agents-page";
import { AddMcpPage } from "@/pages/add-mcp-page";
import { McpServersPage } from "@/pages/mcp-servers-page";
import { McpApprovalsPage } from "@/pages/mcp-approvals-page";
import { TemplateGalleryPage } from "@/pages/template-gallery-page";
import { ToolCatalogPage } from "@/pages/tool-catalog-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { CostPage } from "@/pages/cost-page";
import { ConnectProviderPage } from "@/pages/connect-provider-page";
import { ProvidersPage } from "@/pages/providers-page";
import { TenantsPage } from "@/pages/tenants-page";
import { CreateAgentPage } from "@/pages/create-agent-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { LoginPage } from "@/pages/login-page";
import { AuthCallbackPage } from "@/pages/auth-callback-page";
import {
  ModelRouteDetailPage,
  NewModelRoutePage,
} from "@/pages/model-route-detail-page";
import { ModelRoutesPage } from "@/pages/model-routes-page";
import { PlaceholderPage } from "@/pages/placeholder-page";
import { PlaygroundPage } from "@/pages/playground-page";
import { RunsPage } from "@/pages/runs-page";
import { RunDetailPage } from "@/pages/run-detail-page";
import { AuditPage } from "@/pages/audit-page";
import { AlertsPage } from "@/pages/alerts-page";
import { TeamsPage } from "@/pages/teams-page";
import { TeamDetailPage } from "@/pages/team-detail-page";
import { CreateTeamPage } from "@/pages/create-team-page";
import { GuardrailPoliciesPage } from "@/pages/guardrail-policies-page";
import { WorkflowsPage } from "@/pages/workflows-page";
import { WorkflowDetailPage } from "@/pages/workflow-detail-page";
import { KnowledgeBasesPage, KBDetailPage } from "@/pages/knowledge-bases-page";
import { DatasetsPage, DatasetDetailPage } from "@/pages/datasets-page";
import {
  SecretBindingDetailPage,
  NewSecretBindingPage,
} from "@/pages/secret-binding-detail-page";
import { SecretBindingsPage } from "@/pages/secret-bindings-page";
import { TopologyPage } from "@/pages/topology-page";
import { TracePage } from "@/pages/trace-page";
import { MySharesPage } from "@/pages/my-shares-page";
import { ApprovalsPage } from "@/pages/approvals-page";
import { StopsPage } from "@/pages/stops-page";
import { SharedRunPage } from "@/pages/shared-run-page";
import { RequireAuth, SessionProvider } from "@/lib/session-provider";
import { ToastProvider } from "@/components/kit";
import { NAV_ITEMS } from "@/lib/nav";
import { DESIGN_GALLERY_ENABLED } from "@/design/flag";

// SoonPage resolves the /soon/:id placeholder for a nav destination the approved
// IA lists but a later milestone ships (m13.5 keeps the full IA walkable without
// pulling those surfaces forward). It reads the owning milestone + label from the
// shared nav source so the "arrives in M<n>" copy is always correct.
function SoonPage() {
  const { id } = useParams();
  const item = NAV_ITEMS.find((n) => n.id === id);
  return (
    <PlaceholderPage
      title={item?.label ?? "Coming soon"}
      milestone={item?.milestone ?? "a later milestone"}
    />
  );
}

// The design gallery is a REVIEW-only surface (m13.1 design gate) holding
// static internal wireframes. It renders OUTSIDE the SessionProvider guard, so
// anything that can reach the route can read it with no login — which is fine
// on a dev server and was a disclosure hole in production (M151 hardening, A2).
//
// The gate is therefore the BUILD, not a runtime check: `lazy()` is called only
// in the enabled branch of a statically-inlined constant, so a production build
// folds this to `null`, Rollup drops the dynamic import, and no gallery chunk
// is emitted at all. `hack/ui-no-internal-routes.sh` asserts that on every
// build. Keep the ternary — hoisting `lazy()` out of it makes the import
// unconditional again and the gallery ships even with the route unmounted.
const DesignGallery = DESIGN_GALLERY_ENABLED
  ? lazy(() => import("@/design/gallery").then((m) => ({ default: m.DesignGallery })))
  : null;

// readAgentPin reads the agent this ORIGIN is pinned to (m37.3): when the SPA is served at an agent's
// OWN hostname, the BFF injects `<meta name="agent-pin" content="namespace/name">` into index.html (a
// meta tag, NOT a script — the CSP forbids inline scripts). Absent on the console origin. This is the
// signal that flips the whole app into the chrome-less single-agent chatbox below.
function readAgentPin(): { ns: string; name: string } | null {
  const content = document
    .querySelector('meta[name="agent-pin"]')
    ?.getAttribute("content")
    ?.trim();
  if (!content) return null;
  const slash = content.indexOf("/");
  if (slash <= 0 || slash === content.length - 1) return null;
  return { ns: content.slice(0, slash), name: content.slice(slash + 1) };
}

// ChatboxApp is the entire app when served at an agent's OWN hostname (m37.3): ONLY that agent's
// chrome-less chatbox, its login, and the OIDC/MCP callbacks. The operator-console router is never
// mounted, so the console is unreachable at agent origins — the tight surface falls out for free.
// Same login (the user's own token) + own/org/MCP-connect creds as the console; the agent is pinned
// by the host, so there's no picker. Trace link-outs resolve here too (same BFF, same origin).
function ChatboxApp({ pin }: { pin: { ns: string; name: string } }) {
  return (
    <SessionProvider>
      <ToastProvider>
        <Routes>
          <Route path="login" element={<LoginPage />} />
          <Route path="auth/callback" element={<AuthCallbackPage />} />
          <Route
            path="traces/:id"
            element={
              <RequireAuth>
                <TracePage />
              </RequireAuth>
            }
          />
          <Route
            path="*"
            element={
              <RequireAuth>
                <AgentChatboxPage ns={pin.ns} name={pin.name} />
              </RequireAuth>
            }
          />
        </Routes>
      </ToastProvider>
    </SessionProvider>
  );
}

// App — the SPA route table (ADR 0012 auth routing):
//   • agent-pinned origin    — the single-agent chatbox ONLY (ChatboxApp, m37.3), no console.
//   • /design (flag-gated)   — static wireframes, NO auth (outside RequireAuth).
//   • /login                 — the token login; public. RedirectIfAuthed handled
//                              inside LoginPage (an authed visit bounces to /).
//   • everything else        — the console, behind RequireAuth (→ /login with the
//                              return path preserved when signed out).
//
// SessionProvider wraps everything: it registers the api.ts token/401 seams and
// restores a persisted token before the guards run. The design gallery still
// mounts under it (so restore() runs) but is NOT wrapped in RequireAuth.
export function App() {
  const agentPin = readAgentPin();
  if (agentPin) {
    return <ChatboxApp pin={agentPin} />;
  }

  return (
    <SessionProvider>
      <ToastProvider>
        <Routes>
          {DESIGN_GALLERY_ENABLED && DesignGallery && (
            <Route
              path="design/*"
              element={
                <Suspense fallback={null}>
                  <DesignGallery />
                </Suspense>
              }
            />
          )}
          <Route path="login" element={<LoginPage />} />
          {/* OIDC redirect target (ADR 0020) — public, completes Auth-Code+PKCE. */}
          <Route path="auth/callback" element={<AuthCallbackPage />} />
          {/* Public shared-run page (m75.4) — no auth, no app-shell chrome */}
          <Route path="shared/runs/:token" element={<SharedRunPage />} />
          {/* Standalone per-agent chatbox (m37) — authenticated (same console login) but
              CHROME-LESS: it sits OUTSIDE the AppShell so there's no nav/sidebar, just the
              chat. Pinned to one agent by the URL. */}
          <Route
            path="chat/:ns/:name"
            element={
              <RequireAuth>
                <AgentChatboxPage />
              </RequireAuth>
            }
          />
          <Route
            element={
              <RequireAuth>
                <AppShell />
              </RequireAuth>
            }
          >
            <Route index element={<DashboardPage />} />
            <Route path="agents" element={<AgentsPage />} />
            {/* m64.11: AgentTeams — the orchestration rosters (read-only). */}
            <Route path="teams" element={<TeamsPage />} />
            {/* m71.7: CreateTeamPage — describe → generate → review → create. */}
            <Route path="teams/new" element={<CreateTeamPage />} />
            {/* m151: the team OUTLINE (spec §6.1 archetype A3) — the roster as an
                indented tree, its spawn bounds, and the delegation tree of a run. */}
            <Route path="teams/:ns/:name" element={<TeamDetailPage />} />
            {/* m66.10: GuardrailPolicies — the content-governance policies (read-only). */}
            <Route path="guardrails" element={<GuardrailPoliciesPage />} />
            {/* m67.9: Workflows — the declarative agent graph CRs (read-only list + invoke). */}
            <Route path="workflows" element={<WorkflowsPage />} />
            <Route path="workflows/:ns/:name" element={<WorkflowDetailPage />} />
            {/* m68.13: KnowledgeBases — managed RAG corpora (list + detail, upload, ingest, test-query). */}
            <Route path="knowledgebases" element={<KnowledgeBasesPage />} />
            <Route path="knowledgebases/:ns/:name" element={<KBDetailPage />} />
            {/* m69.3: Datasets — human-labeled eval cases (list + detail + label form). */}
            <Route path="datasets" element={<DatasetsPage />} />
            <Route path="datasets/:name" element={<DatasetDetailPage />} />
            {/* The agent LANDING page (m14.11) — closes the aha loop: status
                timeline + live log tail + Run panel + the native run inspector.
                Reached from the agents list row-click and the create→landing
                swap. Placed before /agents/new so the wizard route (a literal
                segment) still wins its exact match. */}
            <Route path="agents/:ns/:name" element={<AgentDetailPage />} />
            {/* The create-agent wizard (m14.10) — the heart of the aha: two
                entrances (Describe it / Configure it) → one review + tool
                picker → Create. Placed under /agents/new (the agents surface's
                primary create action). */}
            <Route path="agents/new" element={<CreateAgentPage />} />
            <Route path="config" element={<ConfigBuilderPage />} />
            <Route path="playground" element={<PlaygroundPage />} />
            {/* The M14 first-agent wizards (m14.9) — these nav destinations were
                /soon placeholders; they're now the real connect-provider +
                add-MCP flows, the first UI of the aha. */}
            <Route path="providers" element={<ProvidersPage />} />
            <Route path="providers/connect" element={<ConnectProviderPage />} />
            {/* Tenants (M47) — the multi-tenant quota surface (read-only). */}
            <Route path="tenants" element={<TenantsPage />} />
            {/* m25 S10: MCP Servers list — the primary MCP surface (Add is in-page) */}
            <Route path="tools/mcp-servers" element={<McpServersPage />} />
            <Route path="tools/add-mcp" element={<AddMcpPage />} />
            {/* m17.9: MCP approval queue — operator-only, lists pending MCPs */}
            <Route path="tools/approvals" element={<McpApprovalsPage />} />
            {/* m17.10: Tool catalog — merged curated + user-added + pending tools */}
            <Route path="tools/catalog" element={<ToolCatalogPage />} />
            {/* m76.1: /tools/mcp-catalog retired — redirect to Gallery (the single
                discovery surface, which renders its own inline McpCatalogTab). The old
                standalone mcp-catalog-page was deleted as dead code. */}
            <Route path="tools/mcp-catalog" element={<Navigate to="/gallery?tab=mcp" replace />} />
            {/* m74.6: Template Gallery — agent templates (recipes ∪ published) + MCP catalog in tabs */}
            <Route path="gallery" element={<TemplateGalleryPage />} />
            {/* m15.12: ModelRoute CRUD surfaces */}
            <Route path="routes" element={<ModelRoutesPage />} />
            <Route path="routes/new" element={<NewModelRoutePage />} />
            <Route path="routes/:ns/:name" element={<ModelRouteDetailPage />} />
            {/* m15.12: SecretBinding CRUD surfaces */}
            <Route path="secrets" element={<SecretBindingsPage />} />
            <Route path="secrets/new" element={<NewSecretBindingPage />} />
            <Route
              path="secrets/:ns/:name"
              element={<SecretBindingDetailPage />}
            />
            {/* m15.12: AgentRegistry CRUD surfaces */}
            <Route path="registries" element={<AgentRegistriesPage />} />
            <Route path="registries/new" element={<NewAgentRegistryPage />} />
            <Route
              path="registries/:ns/:name"
              element={<AgentRegistryDetailPage />}
            />
            {/* m15.13: Topology v2 — grouped/searchable/list↔graph */}
            <Route path="topology" element={<TopologyPage />} />
            {/* m16.8: runs browser — paginated + filterable global run history. */}
            <Route path="runs" element={<RunsPage />} />
            {/* V5, M112: per-run detail page — approval deep-link target + My Shares link target. */}
            <Route path="runs/:id" element={<RunDetailPage />} />
            {/* V13: My Shares — the caller's share links across all runs. */}
            <Route path="my-shares" element={<MySharesPage />} />
            {/* V5/V15, M112-M113: Approvals — namespace-scoped unified queue of runs paused on plan_approval OR mid-run approval. */}
            <Route path="approvals" element={<ApprovalsPage />} />
            {/* m151.7: Stops — the scoped kill switch's landing surface (ADR 0126, spec §6.2
                gap 2). What is halted right now, what it holds, who stopped it, and the lift. */}
            <Route path="stops" element={<StopsPage />} />
            {/* m16.7: native trace page — full one-trace view with TraceExplorer
                + Langfuse link-out demotion + FeedbackPanel (m16.9). */}
            <Route path="traces/:id" element={<TracePage />} />
            {/* m16.10: cost drill-down — per-agent breakdown (recent window). */}
            <Route path="cost" element={<CostPage />} />
            {/* m63.5: the compliance audit trail viewer (operator-only; ADR 0056). */}
            <Route path="audit" element={<AuditPage />} />
            {/* m70.6: the fired-alert console feed (M70, ADR 0063 D2). */}
            <Route path="alerts" element={<AlertsPage />} />

            {/* m17.12: EvalSuite builder + results browser */}
            <Route path="evals" element={<EvalsPage />} />
            {/* m17.12: PromptVersion list + textual diff viewer */}
            <Route path="prompts" element={<PromptsPage />} />
            {/* Not-yet-built IA destinations (Tools, Traces, … ,
                Settings) render their milestone placeholder — the full approved
                nav is walkable without pulling later features forward. */}
            <Route path="soon/:id" element={<SoonPage />} />
            <Route
              path="*"
              element={
                <PlaceholderPage title="Not found" milestone="this build" />
              }
            />
          </Route>
        </Routes>
      </ToastProvider>
    </SessionProvider>
  );
}
