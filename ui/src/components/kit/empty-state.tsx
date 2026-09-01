import * as React from "react";
import type { LucideIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// EmptyState — the TEACHING empty primitive (kit, m13.1; restyled M151 §5.8).
// Every empty screen says what the surface is FOR and what to do next, with a
// CTA — never a blank panel or a terse "no results". This is a core
// anti-"rudimentary" defense: a first-run user should always be told the next
// action ("No providers connected → Connect one").
//
// The three intents are three DIFFERENT TRUTHS and must never render alike
// (M151 §7 — honest degraded states):
//   • "teaching"    — nothing exists yet (first run). Dashed fence + pine chip:
//                     "a thing belongs here, you make the first one." Leads
//                     with the creating CTA.
//   • "filtered"    — things exist, your view excluded them. Dashed fence +
//                     neutral chip, "Clear filter", and — when the caller knows
//                     it from the backend — how many exist unfiltered.
//   • "unavailable" — this install has no backend that can answer (§7.1). NOT
//                     an error and NOT a zero: a solid, calm frame, no hue, and
//                     copy that says the value is ABSENT rather than nought.
//                     (Inline, among content, that note is QuietNote §5.27;
//                     this intent is the whole-surface case.)
//
// Style (M151 §5.8 / §2.7): rules, never shadows — elevation is drawn with
// lines. Heading is serif at weight 500; the icon chip is a 48px rounded-lg
// square (--radius is 3px now, so "rounded" means near-square).

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
  intent?: "teaching" | "filtered" | "unavailable";
  /**
   * `filtered` only: how many rows exist WITHOUT the filter. An empty view is
   * far less alarming once the user can see the data is still there — "8 agents
   * exist here, your filter excluded them all" (§7, empty-filtered). Pass ONLY
   * a real unfiltered count from the backend; never a client-side guess, and
   * never 0 (a true 0 is the `teaching` case, not a filtered one).
   */
  totalCount?: number;
  /** Plural noun for `totalCount`, e.g. "agents". Defaults to "items". */
  countNoun?: string;
  className?: string;
}

export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  secondaryAction,
  intent = "teaching",
  totalCount,
  countNoun = "items",
  className,
}: EmptyStateProps) {
  const unavailable = intent === "unavailable";

  // The absent-backend state must never be silent: even with no copy from the
  // caller it says the value is ABSENT, so a surface can't degrade into
  // implying zero (§7.1 — "Nothing here is estimated").
  const resolvedDescription =
    description ??
    (unavailable
      ? "This install has no backend that can answer it, so there is nothing to show. Nothing here is estimated — the value is simply absent."
      : undefined);

  // "How many exist unfiltered" — rendered only when the caller actually knows
  // it (a real count > 0), because an invented number is exactly the claim this
  // component exists to avoid.
  const showCount =
    intent === "filtered" && typeof totalCount === "number" && totalCount > 0;
  const one = totalCount === 1;
  const noun = one ? countNoun.replace(/s$/, "") : countNoun;

  return (
    <div
      // Announce the empty surface as a labelled region so a screen-reader user
      // lands on "what this is for + the next action", not silent whitespace.
      role="region"
      aria-label={title}
      className={cn(
        "flex flex-col items-center justify-center rounded-lg px-6 py-14 text-center",
        // A dashed fence says "something belongs here". An absent backend is
        // NOT a placeholder for anything the user can create, so it gets a
        // solid, quiet frame instead.
        unavailable
          ? "border border-border bg-surface-2/40"
          : "border border-dashed border-border-strong bg-card",
        className,
      )}
    >
      {Icon && (
        <div
          className={cn(
            "mb-4 flex h-12 w-12 items-center justify-center rounded-lg",
            intent === "teaching"
              ? "bg-accent text-accent-foreground"
              : "bg-surface-2 text-muted-foreground",
          )}
        >
          <Icon className="h-6 w-6" />
        </div>
      )}
      <h3 className="font-serif text-lg font-medium">{title}</h3>
      {resolvedDescription && (
        <p className="mt-1.5 max-w-md text-sm text-muted-foreground">
          {resolvedDescription}
        </p>
      )}
      {showCount && (
        <p className="mt-2 text-xs text-faint">
          <span className="font-mono">{totalCount}</span> {noun}{" "}
          {one ? "exists" : "exist"} here — your filter excluded{" "}
          {one ? "it" : "them all"}.
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
