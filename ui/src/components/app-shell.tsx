import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { AlertTriangle, Boxes, FlaskConical, LogOut } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { useToast } from "@/components/kit";
import { logout } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { useDevMode } from "@/lib/dev-mode";
import { NAV_SECTIONS, type NavItem } from "@/lib/nav";
import { CapabilitiesProvider, useCapabilities } from "@/lib/capabilities";
import { NamespaceProvider, useNamespace } from "@/lib/namespace";
import { ShellCommandPalette } from "@/components/command-palette-shell";
import { cn } from "@/lib/utils";

// AppShell — the persistent console layout every re-housed surface renders
// inside (ui-foundation §6). The sidebar renders the APPROVED IA (NAV_SECTIONS,
// lib/nav — the same source the design wireframes use, so shell and gate can't
// drift). The header carries who-am-I (m13.3), a namespace picker (§5), and a
// logout. It renders behind RequireAuth (App.tsx), so a session always exists.
//
// RBAC-AWARE CHROME (§3, DISPLAY-ONLY per ADR 0011): the shell wraps its content
// in the NamespaceProvider + CapabilitiesProvider so every surface can read the
// selected namespace and gate affordances. Write-only nav items are HIDDEN when
// the caller lacks the right; a probe failure shows an honest banner and leaves
// affordances VISIBLE (never a silently all-disabled console).

// humanizeIdentity turns a raw auth principal into a friendly display name + honest identity type.
// A Kubernetes service account (`system:serviceaccount:<ns>:<name>`) shows its short name and
// "Service account" scoped to its namespace; an OIDC user shows their username + a real (non-system)
// group. This replaces the header's raw `system:serviceaccount:...` leak AND the misleading
// hardcoded "Member" chip: the console cannot know the caller's RBAC role (ClusterRoles are bound
// server-side, not carried in the token), so it states the identity TYPE truthfully rather than
// inventing a role. (A real access-tier chip would need the BFF to surface the caller's effective role.)
function humanizeIdentity(
  username: string,
  group: string | undefined,
): { name: string; type: string; context: string | undefined } {
  const sa = username.match(/^system:serviceaccount:([^:]+):(.+)$/);
  if (sa) return { name: sa[2], type: "Service account", context: sa[1] };
  const realGroup = group && !group.startsWith("system:") ? group : undefined;
  return { name: username, type: "User", context: realGroup };
}

// WhoAmIBadge renders the caller's identity (initials avatar + friendly name + identity type) from
// the live session, matching the console-chrome wireframe's header.
function WhoAmIBadge({
  username,
  group,
}: {
  username: string;
  group: string | undefined;
}) {
  const { name, type, context } = humanizeIdentity(username, group);
  const initials = (name || "?").slice(0, 2).toUpperCase();
  return (
    <div className="flex items-center gap-2.5 rounded-md border bg-card px-2.5 py-1">
      <div className="flex h-6 w-6 items-center justify-center rounded-full bg-primary/15 text-[11px] font-semibold text-primary">
        {initials}
      </div>
      <div className="leading-tight">
        <p className="text-xs font-medium" data-testid="whoami-username">
          {name}
        </p>
        {context && <p className="text-[10px] text-muted-foreground">{context}</p>}
      </div>
      <Badge variant="secondary" className="text-[9px]">
        {type}
      </Badge>
    </div>
  );
}

// WorkspaceSwitcher is the header's workspace/namespace selector (ADR 0068 §7).
// Each namespace is shown by its display name (the agents.ctxmesh.ai/display-name
// annotation) falling back to the raw namespace name when no label is set.
// "Workspace" is a UI-only friendly label for what is technically a namespace —
// no API route, DTO, or Go identifier uses that word (ADR 0068 §7 discipline).
// A can't-list-namespaces 403 is shown honestly; "" = all namespaces (default).
function WorkspaceSwitcher() {
  const { namespace, setNamespace, list } = useNamespace();

  const forbidden = list.kind === "forbidden";
  const namespaces = list.kind === "ready" ? list.namespaces : [];

  return (
    <div className="flex items-center gap-2">
      <label
        htmlFor="ns-picker"
        className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground"
      >
        Workspace
      </label>
      <Select
        id="ns-picker"
        aria-label="Workspace"
        value={namespace}
        onChange={(e) => setNamespace(e.target.value)}
        className="h-8 w-44 text-xs"
        title={
          forbidden
            ? "You can't list namespaces — working in all namespaces your RBAC permits."
            : undefined
        }
      >
        <option value="">All workspaces</option>
        {namespaces.map((ns) => (
          <option key={ns.name} value={ns.name}>
            {ns.displayName ?? ns.name}
          </option>
        ))}
      </Select>
    </div>
  );
}

// CapabilityBanner is the honest-failure notice (§3): when the capability probe
// fails (500/network) we CANNOT know the caller's rights, so affordances stay
// VISIBLE (fail-open for DISPLAY) and this non-blocking banner explains that —
// never a silently all-disabled console. It also covers the 403-reprobe path
// (reprobe() re-raises the loading→error cycle).
function CapabilityBanner() {
  const { probeError } = useCapabilities();
  const devMode = useDevMode();
  // In dev mode the capability probe is a cluster surface → it 501s by design; the
  // DevModeBanner already explains that, so this would only add a redundant warning.
  if (devMode || !probeError) return null;
  return (
    <div
      role="status"
      data-testid="capability-banner"
      className="flex items-start gap-2.5 border-b border-warning/40 bg-warning/10 px-8 py-2.5 text-xs text-warning-foreground"
    >
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
      <p>
        Couldn&apos;t determine your permissions — some actions may be shown
        optimistically or hidden. The cluster still enforces access, so a denied
        action will report a clear error.
      </p>
    </div>
  );
}

// DevModeBanner is the honest "you're on the local loop" notice (ADR 0021): under
// `agentry dev --ui` the console runs against Docker Compose with NO cluster, so
// the fleet/providers/topology/RBAC surfaces are unavailable (served as calm 501s) and
// there is no login. This persistent banner names that plainly so a dev never reads the
// missing cluster surfaces as a broken console. Shown only when the BFF confirms devMode.
function DevModeBanner() {
  const devMode = useDevMode();
  if (!devMode) return null;
  return (
    <div
      role="status"
      data-testid="dev-mode-banner"
      className="flex items-start gap-2.5 border-b border-primary/40 bg-primary/10 px-8 py-2.5 text-xs text-foreground"
    >
      <FlaskConical className="mt-0.5 h-4 w-4 shrink-0 text-primary" />
      <p>
        <span className="font-semibold">Dev mode</span> — running against your
        local <code className="font-mono">agentry dev</code> loop, no
        cluster. Define, config-preview, and run work here; fleet, providers,
        topology, and RBAC are cluster features and aren&apos;t available.
      </p>
    </div>
  );
}

// Sidebar renders the grouped IA. Write-only items (config-builder, Playground,
// and later Platform surfaces) are HIDDEN when the caller lacks the gating right
// (RBAC-aware chrome, §3) — a viewer sees a read-only nav by construction.
function Sidebar() {
  const { can } = useCapabilities();

  const visible = (it: NavItem): boolean =>
    !it.requiresCapability || can(it.requiresCapability.resource, it.requiresCapability.verb);

  return (
    <aside className="sticky top-0 h-screen overflow-y-auto border-r bg-card">
      <div className="flex h-16 items-center gap-2 border-b px-6">
        <div className="flex h-8 w-8 items-center justify-center rounded-md bg-primary text-primary-foreground">
          <Boxes className="h-5 w-5" />
        </div>
        <span className="text-base font-semibold tracking-tight">
          agentry
        </span>
      </div>
      <nav className="flex flex-col gap-1 p-3">
        {NAV_SECTIONS.map((section) => {
          const items = section.items.filter(visible);
          if (items.length === 0) return null;
          return (
            <div key={section.heading} className="mb-2">
              <p className="px-3 pb-1 pt-2 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/70">
                {section.heading}
              </p>
              {items.map((it) => (
                <NavItemLink key={it.id} item={it} />
              ))}
            </div>
          );
        })}
      </nav>
    </aside>
  );
}

// NavItemLink routes to a re-housed surface (item.route) or, for a not-yet-built
// destination, to its milestone placeholder route (/soon/<id>). The distinction
// keeps the full approved IA walkable without pulling later features forward.
function NavItemLink({ item }: { item: NavItem }) {
  const to = item.route ?? `/soon/${item.id}`;
  const Icon = item.icon;
  const end = item.route === "/";
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        cn(
          "mb-0.5 flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
          isActive
            ? "bg-accent text-accent-foreground"
            : "text-muted-foreground hover:bg-accent/60 hover:text-foreground",
        )
      }
    >
      <Icon className="h-4 w-4" />
      {item.label}
    </NavLink>
  );
}

// ShellChrome is the layout — split out so it renders INSIDE the namespace +
// capability providers (it reads both).
function ShellChrome() {
  const session = useSession();
  const devMode = useDevMode();
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
        <Sidebar />

        <div className="flex min-h-screen flex-col">
          <header className="flex h-16 items-center justify-between border-b bg-card/50 px-8 backdrop-blur">
            <h1 className="whitespace-nowrap text-lg font-semibold tracking-tight">
              Control plane
            </h1>
            <div className="flex items-center gap-4">
              {/* ⌘K command palette — the global navigator + the discoverable
                  header chip that opens it (m13.6b). Mounted here, inside the
                  Namespace + Capabilities providers, so it RBAC-filters exactly
                  like the nav and shares the shell's sign-out flow. */}
              <ShellCommandPalette onLogout={onLogout} />
              <WorkspaceSwitcher />
              {session && (
                <WhoAmIBadge
                  username={session.user.username}
                  group={session.user.groups[0]}
                />
              )}
              {/* No session to sign out of in dev mode (no login wall, ADR 0021). */}
              {!devMode && (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={onLogout}
                  aria-label="Sign out"
                >
                  <LogOut className="h-4 w-4" />
                  Sign out
                </Button>
              )}
            </div>
          </header>
          <DevModeBanner />
          <CapabilityBanner />
          <main className="flex-1 p-8">
            <Outlet />
          </main>
        </div>
      </div>
    </div>
  );
}

export function AppShell() {
  return (
    <NamespaceProvider>
      <CapabilitiesProvider>
        <ShellChrome />
      </CapabilitiesProvider>
    </NamespaceProvider>
  );
}
