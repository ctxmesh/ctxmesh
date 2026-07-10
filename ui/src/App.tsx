import { Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentsPage } from "@/pages/agents-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { PlaceholderPage } from "@/pages/placeholder-page";

// App — the SPA route table. The AppShell wraps every surface; the three future
// surfaces (config-builder m12.6, Playground m12.7) render placeholders until
// they ship. Dashboard + Agents are the foundation surfaces (BFF proof).
export function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<DashboardPage />} />
        <Route path="agents" element={<AgentsPage />} />
        <Route
          path="config"
          element={<PlaceholderPage title="Config builder" milestone="m12.6" />}
        />
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
