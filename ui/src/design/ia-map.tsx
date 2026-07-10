import { Link } from "react-router-dom";
import { ChevronRight, ExternalLink, LogIn } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { NAV_SECTIONS } from "@/design/console-chrome";
import { MilestoneTag, Note } from "@/design/scaffold";
import { WIREFRAMES } from "@/design/manifest";
import type { Milestone } from "@/design/scaffold";

// The IA map — the full navigation tree of the console rendered as a navigable
// diagram. It reads from the SAME NAV_SECTIONS the real shell uses, so the map
// and the wireframes can never drift. Each leaf links to the surface's
// wireframe (where one exists) and carries its owning milestone.

// Map a nav-item id → the wireframe slug that shows that surface (if any).
const NAV_TO_WIREFRAME: Record<string, string> = {
  dashboard: "dashboard",
  topology: "topology-scale",
  agents: "agents-list",
  tools: "tool-catalog",
  prompts: "prompt-diff",
  evals: "eval",
  traces: "trace-explorer",
  runs: "runs-browser",
  feedback: "feedback-browser",
  cost: "cost",
  providers: "settings-providers",
  registries: "registry-editor",
  routes: "routes-secrets",
  secrets: "routes-secrets",
  settings: "settings-providers",
};

// Sub-pages that hang off a nav item (detail / wizard routes) — shown as the
// second level of the tree so the reviewer sees the full page inventory.
const CHILDREN: Record<string, { label: string; slug?: string; milestone: Milestone }[]> = {
  dashboard: [],
  agents: [
    { label: "New agent · Describe it", slug: "create-describe", milestone: "M14" },
    { label: "New agent · Configure it", slug: "create-configure", milestone: "M14" },
    { label: "Agent detail (tabs + Run)", slug: "agent-detail", milestone: "M14" },
  ],
  tools: [{ label: "Add MCP server (wizard)", slug: "add-mcp", milestone: "M14" }],
  providers: [{ label: "Connect a provider (wizard)", slug: "connect-provider", milestone: "M14" }],
  traces: [{ label: "Trace detail (spans + waterfall)", slug: "trace-explorer", milestone: "M16" }],
  evals: [{ label: "Eval results", slug: "eval", milestone: "M17" }],
  prompts: [{ label: "Version diff", slug: "prompt-diff", milestone: "M17" }],
};

function galleryHref(slug?: string) {
  return slug ? `/design/w/${slug}` : undefined;
}

export function IAMap() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Information architecture</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          The proposed navigation tree for the whole console arc. Each surface
          links to its wireframe and carries the milestone that ships it.
        </p>
      </div>

      <Note>
        This tree is generated from the SAME nav source the app shell renders —
        approve the IA here and the shell follows by construction.
      </Note>

      {/* Pre-auth branch */}
      <section className="rounded-lg border bg-card p-5 shadow-card">
        <div className="mb-3 flex items-center gap-2 text-sm font-semibold text-muted-foreground">
          <LogIn className="h-4 w-4" /> Pre-authentication
        </div>
        <Link
          to="/design/w/login"
          className="flex items-center gap-2 rounded-md border bg-surface-2/40 px-4 py-2.5 text-sm hover:bg-surface-2"
        >
          <span className="flex-1 font-medium">Login</span>
          <MilestoneTag m="M13" />
          <ExternalLink className="h-3.5 w-3.5 text-muted-foreground" />
        </Link>
      </section>

      {/* The console — grouped sidebar sections */}
      <section className="space-y-4">
        {NAV_SECTIONS.map((section) => (
          <div key={section.heading} className="rounded-lg border bg-card p-5 shadow-card">
            <p className="mb-3 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              {section.heading}
            </p>
            <ul className="space-y-1.5">
              {section.items.map((item) => {
                const slug = NAV_TO_WIREFRAME[item.id];
                const href = galleryHref(slug);
                const children = CHILDREN[item.id] ?? [];
                const Icon = item.icon;
                return (
                  <li key={item.id}>
                    <div className="flex items-center gap-2 rounded-md px-2 py-1.5 hover:bg-surface-2/60">
                      <Icon className="h-4 w-4 text-muted-foreground" />
                      {href ? (
                        <Link to={href} className="flex-1 text-sm font-medium hover:text-primary">
                          {item.label}
                        </Link>
                      ) : (
                        <span className="flex-1 text-sm font-medium">{item.label}</span>
                      )}
                      <MilestoneTag m={item.milestone} />
                    </div>
                    {children.length > 0 && (
                      <ul className="ml-6 mt-1 space-y-1 border-l pl-4">
                        {children.map((c) => {
                          const chref = galleryHref(c.slug);
                          return (
                            <li key={c.label} className="flex items-center gap-2 py-1 text-sm">
                              <ChevronRight className="h-3 w-3 text-muted-foreground" />
                              {chref ? (
                                <Link to={chref} className="flex-1 text-muted-foreground hover:text-primary">
                                  {c.label}
                                </Link>
                              ) : (
                                <span className="flex-1 text-muted-foreground">{c.label}</span>
                              )}
                              <MilestoneTag m={c.milestone} />
                            </li>
                          );
                        })}
                      </ul>
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </section>

      {/* Cross-cutting surfaces */}
      <section className="rounded-lg border bg-card p-5 shadow-card">
        <p className="mb-3 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Cross-cutting
        </p>
        <div className="grid gap-2 sm:grid-cols-2">
          {[
            ["⌘K command palette", "shell", "M13"],
            ["RBAC viewer chrome", "agents-list-viewer", "M13"],
            ["dev --ui (local)", "dev-mode", "M18"],
            ["Create-agent entrance (fork)", "create-entrance", "M14"],
          ].map(([label, slug, m]) => (
            <Link
              key={slug}
              to={`/design/w/${slug}`}
              className="flex items-center gap-2 rounded-md border bg-surface-2/40 px-3 py-2 text-sm hover:bg-surface-2"
            >
              <span className="flex-1">{label}</span>
              <Badge variant="secondary" className="text-[9px]">{m}</Badge>
            </Link>
          ))}
        </div>
      </section>

      <p className="text-center text-xs text-muted-foreground">
        {WIREFRAMES.length} wireframes across 6 milestones · IA has{" "}
        {NAV_SECTIONS.reduce((n, s) => n + s.items.length, 0)} top-level destinations.
      </p>
    </div>
  );
}
