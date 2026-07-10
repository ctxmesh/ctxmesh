import * as React from "react";
import type { LucideIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// EmptyState — the TEACHING empty primitive (kit, m13.1). Every empty screen
// says what the surface is FOR and what to do next, with a CTA — never a blank
// panel or a terse "no results". This is a core anti-"rudimentary" defense:
// a first-run user should always be told the next action ("No providers
// connected → Connect one").
//
// Two intents:
//   • "teaching"  (default) — first-run / nothing-created-yet; leads with a CTA.
//   • "filtered"  — a search/filter matched nothing; leads with "clear filter".

export interface EmptyStateAction {
  label: string;
  onClick?: () => void;
  icon?: LucideIcon;
  variant?: React.ComponentProps<typeof Button>["variant"];
}

export interface EmptyStateProps {
  icon?: LucideIcon;
  title: string;
  /** One or two sentences: what this surface is for + what to do next. */
  description?: React.ReactNode;
  /** Primary CTA — the single most likely next action. */
  action?: EmptyStateAction;
  /** Optional secondary action (e.g. "Learn more", "Import"). */
  secondaryAction?: EmptyStateAction;
  intent?: "teaching" | "filtered";
  className?: string;
}

export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  secondaryAction,
  intent = "teaching",
  className,
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center rounded-lg border border-dashed bg-card/40 px-6 py-14 text-center",
        className,
      )}
    >
      {Icon && (
        <div
          className={cn(
            "mb-4 flex h-12 w-12 items-center justify-center rounded-xl",
            intent === "teaching"
              ? "bg-accent text-accent-foreground"
              : "bg-surface-2 text-muted-foreground",
          )}
        >
          <Icon className="h-6 w-6" />
        </div>
      )}
      <h3 className="text-lg font-semibold tracking-snug">{title}</h3>
      {description && (
        <p className="mt-1.5 max-w-md text-sm text-muted-foreground">
          {description}
        </p>
      )}
      {(action || secondaryAction) && (
        <div className="mt-6 flex items-center gap-3">
          {action && (
            <Button variant={action.variant ?? "default"} onClick={action.onClick}>
              {action.icon && <action.icon className="h-4 w-4" />}
              {action.label}
            </Button>
          )}
          {secondaryAction && (
            <Button
              variant={secondaryAction.variant ?? "ghost"}
              onClick={secondaryAction.onClick}
            >
              {secondaryAction.icon && <secondaryAction.icon className="h-4 w-4" />}
              {secondaryAction.label}
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
