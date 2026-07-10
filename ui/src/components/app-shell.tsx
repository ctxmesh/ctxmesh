import { NavLink, Outlet, useNavigate } from "react-router-dom";
import {
  Boxes,
  FlaskConical,
  LayoutDashboard,
  LogOut,
  SlidersHorizontal,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/kit";
import { logout } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { cn } from "@/lib/utils";

// AppShell — the persistent layout every surface renders inside (sidebar +
// header + routed <Outlet/>). Composes token utilities only; the three surfaces
// (dashboard m12.5 / config-builder m12.6 / Playground m12.7) mount into the
// outlet. It renders behind RequireAuth (App.tsx), so a session always exists
// here — the header shows who-am-I (username + first group) and a logout action
// (ADR 0012, shell wireframe).
const nav = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/agents", label: "Agents", icon: Boxes, end: false },
  { to: "/config", label: "Config builder", icon: SlidersHorizontal, end: false },
  { to: "/playground", label: "Playground", icon: FlaskConical, end: false },
];

// WhoAmIBadge renders the caller's identity (initials avatar + username + first
// group) from the live session, matching the console-chrome wireframe's header.
function WhoAmIBadge({
  username,
  group,
}: {
  username: string;
  group: string | undefined;
}) {
  const initials = (username || "?").slice(0, 2).toUpperCase();
  return (
    <div className="flex items-center gap-2.5 rounded-md border bg-card px-2.5 py-1">
      <div className="flex h-6 w-6 items-center justify-center rounded-full bg-primary/15 text-[11px] font-semibold text-primary">
        {initials}
      </div>
      <div className="leading-tight">
        <p className="text-xs font-medium" data-testid="whoami-username">
          {username}
        </p>
        {group && (
          <p className="text-[10px] text-muted-foreground">{group}</p>
        )}
      </div>
      <Badge variant="secondary" className="text-[9px]">
        {group ? "Member" : "Authenticated"}
      </Badge>
    </div>
  );
}

export function AppShell() {
  const session = useSession();
  const navigate = useNavigate();
  const { toast } = useToast();

  const onLogout = () => {
    logout();
    toast({ variant: "info", title: "Signed out" });
    navigate("/login", { replace: true });
  };

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
            <div className="flex items-center gap-4">
              {session && (
                <WhoAmIBadge
                  username={session.user.username}
                  group={session.user.groups[0]}
                />
              )}
              <Button
                variant="ghost"
                size="sm"
                onClick={onLogout}
                aria-label="Sign out"
              >
                <LogOut className="h-4 w-4" />
                Sign out
              </Button>
            </div>
          </header>
          <main className="flex-1 p-8">
            <Outlet />
          </main>
        </div>
      </div>
    </div>
  );
}
