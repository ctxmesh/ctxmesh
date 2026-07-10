import { Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentsPage } from "@/pages/agents-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { PlaceholderPage } from "@/pages/placeholder-page";
import { PlaygroundPage } from "@/pages/playground-page";

// App — the SPA route table. The AppShell wraps every surface. All three
// surfaces — Dashboard (m12.5), Config builder (m12.6), Playground (m12.7) — are
// live; Agents is the foundation list.
export function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<DashboardPage />} />
        <Route path="agents" element={<AgentsPage />} />
        <Route path="config" element={<ConfigBuilderPage />} />
        <Route path="playground" element={<PlaygroundPage />} />
        <Route
          path="*"
          element={<PlaceholderPage title="Not found" milestone="this build" />}
        />
      </Route>
    </Routes>
  );
}
