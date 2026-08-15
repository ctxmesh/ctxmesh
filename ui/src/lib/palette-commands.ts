import { Globe, LogOut } from "lucide-react";

import type { CommandItem } from "@/components/kit";
import type { NamespaceSummary } from "@/lib/api";
import { NAV_SECTIONS, type NavItem } from "@/lib/nav";

// palette-commands - the pure command-model for the shell's cmd-K palette
// (m13.6b). It builds the grouped CommandItem[] the kit's CommandPalette renders,
// from live app state (RBAC allow-map, namespace list, session actions). Kept
// side-effect-free and component-free so it is unit-testable in isolation and so
// the palette and the sidebar nav share ONE visibility rule (they can't drift).

export interface BuildCommandsArgs {
  /** The RBAC gate - same `can(resource, verb)` the Sidebar uses (ADR 0011). */
  can: (resource: string, verb: string) => boolean;
  /** Route to a path (react-router navigate). */
  navigate: (to: string) => void;
  /** Namespaces the caller can switch to (from the namespace picker's list).
   *  Each entry carries the raw name (used as the scope key) and an optional
   *  displayName (ADR 0068 §7 — friendly label, UI-only). */
  namespaces: NamespaceSummary[];
  /** The currently-selected namespace ("" = all namespaces). */
  currentNamespace: string;
  /** Select a namespace (re-scopes the list + capability probe). */
  setNamespace: (ns: string) => void;
  /** The shell's sign-out flow (logout + toast + redirect). */
  onLogout: () => void;
}

// The same visibility rule the app-shell Sidebar applies: a capability-gated surface
// is shown only when the caller may perform its gating verb. Read-open surfaces have
// no `requiresCapability` and are always shown.
function isVisible(it: NavItem, can: BuildCommandsArgs["can"]): boolean {
  return !it.requiresCapability || can(it.requiresCapability.resource, it.requiresCapability.verb);
}

// buildCommands assembles the grouped command list. Navigate commands mirror the
// RBAC-filtered nav (built surfaces route to their real path; not-yet-shipped IA
// destinations route to their honest /soon placeholder, labelled with the owning
// milestone). Actions cover only what genuinely exists in M13: switch namespace
// (as sub-commands) and sign out. (No theme toggle - the production shell has
// none; the design gallery's is gallery-only.)
export function buildCommands({
  can,
  navigate,
  namespaces,
  currentNamespace,
  setNamespace,
  onLogout,
}: BuildCommandsArgs): CommandItem[] {
  const navigateCmds: CommandItem[] = NAV_SECTIONS.flatMap((section) =>
    section.items
      .filter((it) => isVisible(it, can))
      .map((it) => {
        const to = it.route ?? `/soon/${it.id}`;
        const isPlaceholder = !it.route;
        return {
          id: `nav-${it.id}`,
          group: "Navigate",
          label: it.label,
          icon: it.icon,
          // Placeholders carry their milestone as a hint so the palette stays
          // honest about what does / doesn't exist yet.
          hint: isPlaceholder ? it.milestone : undefined,
          keywords: `${section.heading} ${it.id} go to`,
          onRun: () => navigate(to),
        };
      }),
  );

  const actionCmds: CommandItem[] = [];

  // Namespace switching as sub-commands - one keystroke from inside the palette,
  // never re-offering the current scope. Labels use the display name (friendly
  // label, ADR 0068 §7) falling back to the raw namespace name.
  for (const ns of namespaces) {
    if (ns.name === currentNamespace) continue;
    const label = ns.displayName ?? ns.name;
    actionCmds.push({
      id: `ns-${ns.name}`,
      group: "Switch workspace",
      label,
      // Include the raw name as a keyword so the palette matches even if the
      // user types the namespace name rather than the display label.
      keywords: `workspace switch scope ${ns.name}`,
      icon: Globe,
      onRun: () => setNamespace(ns.name),
    });
  }
  // "All workspaces" is always a reachable scope (the honest default).
  if (currentNamespace !== "") {
    actionCmds.push({
      id: "ns-all",
      group: "Switch workspace",
      label: "All workspaces",
      icon: Globe,
      keywords: "workspace switch scope all cluster namespace",
      onRun: () => setNamespace(""),
    });
  }

  actionCmds.push({
    id: "action-signout",
    group: "Actions",
    label: "Sign out",
    icon: LogOut,
    keywords: "logout log out exit session",
    onRun: onLogout,
  });

  return [...navigateCmds, ...actionCmds];
}
