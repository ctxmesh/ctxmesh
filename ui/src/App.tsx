import { lazy, Suspense } from "react";
import { Route, Routes, useParams } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentDetailPage } from "@/pages/agent-detail-page";
import { AgentRegistriesPage } from "@/pages/agent-registries-page";
import { AgentRegistryDetailPage, NewAgentRegistryPage } from "@/pages/agent-registry-detail-page";
import { AgentsPage } from "@/pages/agents-page";
import { AddMcpPage } from "@/pages/add-mcp-page";
import { McpApprovalsPage } from "@/pages/mcp-approvals-page";
import { ToolCatalogPage } from "@/pages/tool-catalog-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { CostPage } from "@/pages/cost-page";
import { ConnectProviderPage } from "@/pages/connect-provider-page";
import { CreateAgentPage } from "@/pages/create-agent-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { LoginPage } from "@/pages/login-page";
import { ModelRouteDetailPage, NewModelRoutePage } from "@/pages/model-route-detail-page";
import { ModelRoutesPage } from "@/pages/model-routes-page";
import { PlaceholderPage } from "@/pages/placeholder-page";
import { PlaygroundPage } from "@/pages/playground-page";
import { RunsPage } from "@/pages/runs-page";
import { SecretBindingDetailPage, NewSecretBindingPage } from "@/pages/secret-binding-detail-page";
import { SecretBindingsPage } from "@/pages/secret-bindings-page";
import { TopologyPage } from "@/pages/topology-page";
import { TracePage } from "@/pages/trace-page";
import { RequireAuth, SessionProvider } from "@/lib/session-provider";
import { ToastProvider } from "@/components/kit";
import { NAV_ITEMS } from "@/lib/nav";
import { designGalleryEnabled } from "@/design/flag";

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

// The design gallery is a REVIEW-only surface (m13.1 design gate). It's loaded
// lazily and ONLY when the flag is on, so a normal production build splits it
// into a separate chunk that is never requested (the route isn't mounted) —
// invisible by construction. VITE_DESIGN_GALLERY=1 (build) or ?design (runtime)
// enables it. It is STATIC wireframes and is reachable WITHOUT auth (it renders
// OUTSIDE the SessionProvider guard) — the design gate must not require a login.
const DesignGallery = lazy(() =>
  import("@/design/gallery").then((m) => ({ default: m.DesignGallery })),
);

// App — the SPA route table (ADR 0012 auth routing):
//   • /design (flag-gated)  — static wireframes, NO auth (outside RequireAuth).
//   • /login                — the token login; public. RedirectIfAuthed handled
//                             inside LoginPage (an authed visit bounces to /).
//   • everything else       — the console, behind RequireAuth (→ /login with the
//                             return path preserved when signed out).
//
// SessionProvider wraps everything: it registers the api.ts token/401 seams and
// restores a persisted token before the guards run. The design gallery still
// mounts under it (so restore() runs) but is NOT wrapped in RequireAuth.
export function App() {
  const designOn = designGalleryEnabled();

  return (
    <SessionProvider>
      <ToastProvider>
        <Routes>
          {designOn && (
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
          <Route
            element={
              <RequireAuth>
                <AppShell />
              </RequireAuth>
            }
          >
            <Route index element={<DashboardPage />} />
            <Route path="agents" element={<AgentsPage />} />
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
            <Route path="providers/connect" element={<ConnectProviderPage />} />
            <Route path="tools/add-mcp" element={<AddMcpPage />} />
            {/* m17.9: MCP approval queue — operator-only, lists pending MCPs */}
            <Route path="tools/approvals" element={<McpApprovalsPage />} />
            {/* m17.10: Tool catalog — merged curated + user-added + pending tools */}
            <Route path="tools/catalog" element={<ToolCatalogPage />} />
            {/* m15.12: ModelRoute CRUD surfaces */}
            <Route path="routes" element={<ModelRoutesPage />} />
            <Route path="routes/new" element={<NewModelRoutePage />} />
            <Route path="routes/:ns/:name" element={<ModelRouteDetailPage />} />
            {/* m15.12: SecretBinding CRUD surfaces */}
            <Route path="secrets" element={<SecretBindingsPage />} />
            <Route path="secrets/new" element={<NewSecretBindingPage />} />
            <Route path="secrets/:ns/:name" element={<SecretBindingDetailPage />} />
            {/* m15.12: AgentRegistry CRUD surfaces */}
            <Route path="registries" element={<AgentRegistriesPage />} />
            <Route path="registries/new" element={<NewAgentRegistryPage />} />
            <Route path="registries/:ns/:name" element={<AgentRegistryDetailPage />} />
            {/* m15.13: Topology v2 — grouped/searchable/list↔graph */}
            <Route path="topology" element={<TopologyPage />} />
            {/* m16.8: runs browser — paginated + filterable global run history. */}
            <Route path="runs" element={<RunsPage />} />
            {/* m16.7: native trace page — full one-trace view with TraceExplorer
                + Langfuse link-out demotion + FeedbackPanel (m16.9). */}
            <Route path="traces/:id" element={<TracePage />} />
            {/* m16.10: cost drill-down — per-agent breakdown (recent window). */}
            <Route path="cost" element={<CostPage />} />
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
