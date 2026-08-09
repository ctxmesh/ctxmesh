import * as React from "react";
import { Info } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

// Shared low-fi scaffolding for the wireframe gallery (m13.1). These are
// DESIGN-TIME devices only — annotation callouts, milestone chips, a fake-shell
// wrapper — never shipped in a product surface. They keep each wireframe terse
// and consistent so the user reviews layout + flow, not pixel polish.

export type Milestone =
  | "M13"
  | "M14"
  | "M15"
  | "M16"
  | "M17"
  | "M18"
  | "M47"
  | "M63"
  | "M64"
  | "M66"
  | "M67";

export const MILESTONE_LABEL: Record<Milestone, string> = {
  M13: "Foundation",
  M14: "First agent (the aha)",
  M15: "Fleet management",
  M16: "Observability depth",
  M17: "Authoring depth",
  M18: "Everywhere (dev + OIDC)",
  M47: "Tenancy & quotas",
  M63: "Audit surface",
  M64: "Orchestration I",
  M66: "Guardrails",
  M67: "Workflows",
};

export function MilestoneTag({ m, className }: { m: Milestone; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full border border-primary/30 bg-accent px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-accent-foreground",
        className,
      )}
      title={MILESTONE_LABEL[m]}
    >
      {m}
    </span>
  );
}

// A design annotation — the "why" of a screen, shown only in the gallery.
// Distinct from any product UI so the reviewer never confuses commentary with
// the design itself.
export function Note({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-2 rounded-md border border-info/30 bg-info/5 px-3 py-2 text-xs text-muted-foreground">
      <Info className="mt-0.5 h-3.5 w-3.5 shrink-0 text-info" />
      <span>{children}</span>
    </div>
  );
}

// A titled section inside a wireframe screen (groups related states).
export function WireSection({
  title,
  children,
  aside,
}: {
  title: string;
  children: React.ReactNode;
  aside?: React.ReactNode;
}) {
  return (
    <section className="space-y-3">
      <div className="flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          {title}
        </h3>
        {aside}
      </div>
      {children}
    </section>
  );
}

// A device-frame wrapper so a wireframe reads as a "screen" inside the gallery,
// with an optional caption for the state being shown (e.g. "Empty state").
export function ScreenFrame({
  caption,
  children,
  className,
}: {
  caption?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <figure className="space-y-2">
      {caption && (
        <figcaption className="text-xs font-medium text-muted-foreground">
          {caption}
        </figcaption>
      )}
      <div
        className={cn(
          "overflow-hidden rounded-xl border border-border-strong bg-background shadow-elevated",
          className,
        )}
      >
        {children}
      </div>
    </figure>
  );
}

// A small key/value list used across detail/review wireframes.
export function KeyValue({
  rows,
}: {
  rows: { k: React.ReactNode; v: React.ReactNode }[];
}) {
  return (
    <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2 text-sm">
      {rows.map((r, i) => (
        <React.Fragment key={i}>
          <dt className="text-muted-foreground">{r.k}</dt>
          <dd className="min-w-0 font-medium">{r.v}</dd>
        </React.Fragment>
      ))}
    </dl>
  );
}

// A tabs strip used by detail wireframes (presentational; controlled by parent).
export function WireTabs({
  tabs,
  active,
  onSelect,
}: {
  tabs: string[];
  active: string;
  onSelect: (t: string) => void;
}) {
  return (
    <div className="flex gap-1 border-b">
      {tabs.map((t) => (
        <button
          key={t}
          type="button"
          onClick={() => onSelect(t)}
          className={cn(
            "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
            active === t
              ? "border-primary text-foreground"
              : "border-transparent text-muted-foreground hover:text-foreground",
          )}
        >
          {t}
        </button>
      ))}
    </div>
  );
}

// A read-only chrome banner reused by the viewer/RBAC wireframes.
export function ViewerBanner() {
  return (
    <div className="flex items-center gap-2 border-b border-warning/30 bg-warning/10 px-6 py-2 text-xs text-warning-foreground">
      <Badge variant="warning" className="text-[10px]">
        Read-only
      </Badge>
      <span className="text-muted-foreground">
        You're signed in as a viewer — create / edit / delete are hidden. Ask an
        admin for an editor RoleBinding.
      </span>
    </div>
  );
}
