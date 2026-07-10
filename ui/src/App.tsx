import { lazy, Suspense } from "react";
import { Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentsPage } from "@/pages/agents-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { PlaceholderPage } from "@/pages/placeholder-page";
import { PlaygroundPage } from "@/pages/playground-page";
import { designGalleryEnabled } from "@/design/flag";

// The design gallery is a REVIEW-only surface (m13.1 design gate). It's loaded
// lazily and ONLY when the flag is on, so a normal production build splits it
// into a separate chunk that is never requested (the route isn't mounted) —
// invisible by construction. VITE_DESIGN_GALLERY=1 (build) or ?design (runtime)
// enables it.
const DesignGallery = lazy(() =>
  import("@/design/gallery").then((m) => ({ default: m.DesignGallery })),
);

// App — the SPA route table. The AppShell wraps every product surface; the
// flag-gated /design gallery mounts OUTSIDE the shell with its own review chrome.
export function App() {
  const designOn = designGalleryEnabled();

  return (
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
