import { lazy, Suspense } from "react";
import { Route, Routes, useParams } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { AgentsPage } from "@/pages/agents-page";
import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { DashboardPage } from "@/pages/dashboard-page";
import { LoginPage } from "@/pages/login-page";
import { PlaceholderPage } from "@/pages/placeholder-page";
import { PlaygroundPage } from "@/pages/playground-page";
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
            <Route path="config" element={<ConfigBuilderPage />} />
            <Route path="playground" element={<PlaygroundPage />} />
            {/* Not-yet-built IA destinations (Topology, Tools, Traces, … ,
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
