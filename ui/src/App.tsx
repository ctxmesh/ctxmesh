import { Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentsPage } from "@/pages/agents-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { PlaceholderPage } from "@/pages/placeholder-page";

// App — the SPA route table. The AppShell wraps every surface. Dashboard +
// Agents + Config builder are live; the Playground (m12.7) renders a placeholder
// until it ships.
export function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<DashboardPage />} />
        <Route path="agents" element={<AgentsPage />} />
        <Route path="config" element={<ConfigBuilderPage />} />
        <Route
          path="playground"
          element={<PlaceholderPage title="Playground" milestone="m12.7" />}
        />
        <Route
          path="*"
          element={<PlaceholderPage title="Not found" milestone="this build" />}
        />
      </Route>
    </Routes>
  );
}
