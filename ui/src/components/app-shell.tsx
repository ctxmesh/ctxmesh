import { NavLink, Outlet } from "react-router-dom";
import { Boxes, LayoutDashboard, SlidersHorizontal, FlaskConical } from "lucide-react";

import { cn } from "@/lib/utils";

// AppShell — the persistent layout every surface renders inside (sidebar +
// header + routed <Outlet/>). Composes token utilities only; the three surfaces
// (dashboard m12.5 / config-builder m12.6 / Playground m12.7) mount into the
// outlet. Nav entries for those surfaces are present but their routes land on
// the foundation placeholder until each surface ships.
const nav = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/agents", label: "Agents", icon: Boxes, end: false },
  { to: "/config", label: "Config builder", icon: SlidersHorizontal, end: false },
  { to: "/playground", label: "Playground", icon: FlaskConical, end: false },
];

export function AppShell() {
  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="grid grid-cols-[16rem_1fr]">
        <aside className="sticky top-0 h-screen border-r bg-card">
          <div className="flex h-16 items-center gap-2 border-b px-6">
            <div className="flex h-8 w-8 items-center justify-center rounded-md bg-primary text-primary-foreground">
              <Boxes className="h-5 w-5" />
            </div>
            <span className="text-base font-semibold tracking-tight">
              agent-engine
            </span>
          </div>
          <nav className="flex flex-col gap-1 p-3">
            {nav.map(({ to, label, icon: Icon, end }) => (
              <NavLink
                key={to}
                to={to}
                end={end}
                className={({ isActive }) =>
                  cn(
                    "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                    isActive
                      ? "bg-accent text-accent-foreground"
                      : "text-muted-foreground hover:bg-accent/60 hover:text-foreground",
                  )
                }
              >
                <Icon className="h-4 w-4" />
                {label}
              </NavLink>
            ))}
          </nav>
        </aside>

        <div className="flex min-h-screen flex-col">
          <header className="flex h-16 items-center justify-between border-b bg-card/50 px-8 backdrop-blur">
            <h1 className="text-lg font-semibold tracking-tight">
              Control plane
            </h1>
            <span className="text-xs text-muted-foreground">
              M12 UI foundation
            </span>
          </header>
          <main className="flex-1 p-8">
            <Outlet />
          </main>
        </div>
      </div>
    </div>
  );
}
