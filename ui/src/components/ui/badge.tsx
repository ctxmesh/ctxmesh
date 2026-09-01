import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

// shadcn/ui Badge → the editorial TAG (M151 §5.1).
//
// A tag is an ANNOTATION, not an alarm: uppercase 10px mono on its hue's own
// tint surface, near-square (2px), never filled solid, never interactive. The
// action that resolves a state lives next door in a "Next step" link or a
// button — a tag therefore carries no hover/focus affordance by design (§2.3).
// If a tag sits inside a link, the link owns the affordance.
//
// Two doctrines are load-bearing here:
//   1. The brand is NEVER a status (§2.1). The old `default` variant was a
//      solid pine `bg-primary` pill; solid-filled tags are abolished and
//      `default` now re-points to `progressing` — the pine-TINT chip, which is
//      the one legitimately "system-coloured" state (the machine converging on
//      its own, §2.5). Tint ≠ brand: the form rule (uppercase mono chip vs.
//      sentence-case underlined/filled control) keeps them unambiguous.
//   2. Every recipe is `bg-{hue}-surface text-{hue}` and is correct in BOTH
//      themes with no `dark:` classes — dark swaps the token values, not the
//      utilities (§1.3).
//
// The left column is the canonical vocabulary; the legacy shadcn/M12 names are
// kept as aliases with identical classes so existing call sites compile during
// the migration. New code writes ok / progressing / hold / warn / crit / muted
// / open.
const badgeVariants = cva(
  "inline-flex items-center whitespace-nowrap rounded-sm border border-transparent px-2 py-[3px] font-mono text-2xs font-medium uppercase tracking-wide",
  {
    variants: {
      variant: {
        // --- canonical (§5.1) ---
        /** Verified and serving: Ready, Promoted, Succeeded, "cited". */
        ok: "bg-success-surface text-success",
        /** The machine is converging on its own: Pending, Provisioning, running. */
        progressing: "bg-accent text-accent-foreground",
        /** Awaiting a PERSON: approval, consent, promotion, a held run (§2.4). */
        hold: "bg-hold-surface text-hold",
        /** A bound is near or crossed, or degraded but still serving. */
        warn: "bg-warning-surface text-warning",
        /** Will not proceed without a change: failed, halted, refused. */
        crit: "bg-destructive-surface text-destructive",
        /** Not in motion and not a problem: Draft, Disabled, Paused, idle. */
        muted: "bg-surface-2 text-muted-foreground",
        /** Declared but never exercised: "never called", "no model route". */
        open: "border-dashed border-border-strong bg-transparent text-faint",

        // --- legacy aliases (same classes; do not use in new code) ---
        /** @deprecated alias of `ok`. */
        success: "bg-success-surface text-success",
        /** @deprecated alias of `progressing` — the brand is never a status. */
        default: "bg-accent text-accent-foreground",
        /** @deprecated alias of `hold` (the `--info` slot now carries hold). */
        info: "bg-hold-surface text-hold",
        /** @deprecated alias of `warn`. */
        warning: "bg-warning-surface text-warning",
        /** @deprecated alias of `crit`. */
        destructive: "bg-destructive-surface text-destructive",
        /** @deprecated alias of `muted`. */
        secondary: "bg-surface-2 text-muted-foreground",
        /** @deprecated alias of `open`. */
        outline: "border-dashed border-border-strong bg-transparent text-faint",
      },
    },
    defaultVariants: {
      variant: "progressing",
    },
  },
);

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}

export { Badge, badgeVariants };
