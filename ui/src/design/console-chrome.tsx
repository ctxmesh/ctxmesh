import * as React from "react";
import { Boxes, ChevronRight, Command, Search } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { NAV_SECTIONS } from "@/lib/nav";
import type { NavItem, NavSection } from "@/lib/nav";

// ConsoleChrome — the canonical console SHELL used by every content wireframe so
// the reviewer sees each surface in its real home (sidebar IA + who-am-I header
// + cmd-K affordance). It renders the PROPOSED information architecture — the
// SAME NAV_SECTIONS the REAL app shell (components/app-shell) consumes, imported
// from lib/nav so the map, the wireframes, and the shipped shell can never drift
// (m13.5 re-housing).

export { NAV_SECTIONS };
export type { NavItem, NavSection };

export interface ConsoleChromeProps {
  /** The nav item id to render active. */
  active: string;
  /** Who-am-I identity for the header. */
  user?: { name: string; groups: string[]; persona: string };
  /** Read-only viewer variant — write-only nav is hidden, persona = viewer. */
  viewer?: boolean;
  /** dev --ui variant — a "local" badge + reduced nav (M18). */
  devMode?: boolean;
  /** Optional banner slot above the content (viewer / dev notices). */
  banner?: React.ReactNode;
  /** The page title in the header. */
  title: React.ReactNode;
  /** Right-of-title header actions. */
  headerActions?: React.ReactNode;
  children: React.ReactNode;
}

// In dev --ui (M18) the surface is reduced to run / logs / trace — no cluster.
const DEV_ALLOWED = new Set(["agents", "traces", "runs"]);

export function ConsoleChrome({
  active,
  user,
  viewer = false,
  devMode = false,
  banner,
  title,
  headerActions,
  children,
}: ConsoleChromeProps) {
  const identity = user ?? {
    name: viewer ? "casey.viewer" : "alex.dev",
    groups: viewer ? ["viewers"] : ["dev-team", "system:authenticated"],
    persona: viewer ? "Viewer" : "Editor",
  };

  return (
    <div className="grid h-[46rem] grid-cols-[15rem_1fr] overflow-hidden bg-background text-foreground">
      {/* Sidebar */}
      <aside className="flex flex-col border-r bg-card">
        <div className="flex h-14 items-center gap-2.5 border-b px-4">
          <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-sm">
            <Boxes className="h-5 w-5" />
          </div>
          <span className="text-sm font-semibold tracking-tight">agentry</span>
          {devMode && (
            <Badge variant="warning" className="ml-auto text-[9px]">
              dev
            </Badge>
          )}
        </div>

        {/* cmd-K trigger — the search entrance to everything. */}
        <div className="p-3">
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md border bg-surface-2/60 px-3 py-2 text-xs text-muted-foreground transition-colors hover:bg-surface-2"
          >
            <Search className="h-3.5 w-3.5" />
            <span className="flex-1 text-left">Search…</span>
            <kbd className="flex items-center gap-0.5 rounded border bg-card px-1 py-0.5 text-[9px]">
              <Command className="h-2.5 w-2.5" />K
            </kbd>
          </button>
        </div>

        <nav className="flex-1 overflow-y-auto px-3 pb-3">
          {NAV_SECTIONS.map((section) => {
            const items = section.items.filter((it) => {
              if (viewer && it.requiresCapability) return false;
              if (devMode && !DEV_ALLOWED.has(it.id)) return false;
              return true;
            });
            if (items.length === 0) return null;
            return (
              <div key={section.heading} className="mb-3">
                <p className="px-2 pb-1 pt-2 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/70">
                  {section.heading}
                </p>
                {items.map((it) => {
                  const isActive = it.id === active;
                  const Icon = it.icon;
                  return (
                    <div
                      key={it.id}
                      className={cn(
                        "mb-0.5 flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm font-medium transition-colors",
                        isActive
                          ? "bg-accent text-accent-foreground"
                          : "text-muted-foreground hover:bg-surface-2 hover:text-foreground",
                      )}
                    >
                      <Icon className="h-4 w-4" />
                      {it.label}
                    </div>
                  );
                })}
              </div>
            );
          })}
        </nav>
      </aside>

      {/* Main column */}
      <div className="flex min-w-0 flex-col">
        <header className="flex h-14 shrink-0 items-center justify-between border-b bg-card/60 px-6 backdrop-blur">
          <div className="flex items-center gap-2 text-sm">
            <span className="font-semibold tracking-snug">{title}</span>
          </div>
          <div className="flex items-center gap-4">
            {headerActions}
            {/* who-am-I */}
            <div className="flex items-center gap-2.5 rounded-md border bg-card px-2.5 py-1">
              <div className="flex h-6 w-6 items-center justify-center rounded-full bg-primary/15 text-[11px] font-semibold text-primary">
                {identity.name.slice(0, 2).toUpperCase()}
              </div>
              <div className="leading-tight">
                <p className="text-xs font-medium">{identity.name}</p>
                <p className="text-[10px] text-muted-foreground">
                  {identity.groups[0]}
                </p>
              </div>
              <Badge
                variant={viewer ? "warning" : "secondary"}
                className="text-[9px]"
              >
                {identity.persona}
              </Badge>
            </div>
          </div>
        </header>

        {banner}

        <main className="min-w-0 flex-1 overflow-y-auto bg-background p-6">
          {children}
        </main>
      </div>
    </div>
  );
}

// A breadcrumb device reused across detail wireframes.
export function Breadcrumb({ parts }: { parts: string[] }) {
  return (
    <nav className="mb-4 flex items-center gap-1.5 text-xs text-muted-foreground">
      {parts.map((p, i) => (
        <React.Fragment key={i}>
          {i > 0 && <ChevronRight className="h-3 w-3" />}
          <span className={i === parts.length - 1 ? "text-foreground" : ""}>
            {p}
          </span>
        </React.Fragment>
      ))}
    </nav>
  );
}
