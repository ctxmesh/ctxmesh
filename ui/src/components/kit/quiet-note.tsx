import * as React from "react";

import { cn } from "@/lib/utils";

// QuietNote — the "this install has no backend that can answer it" block
// (M151 §5.27, copy patterns §7.1).
//
// This is the component that keeps the console HONEST. Everywhere a number is
// conceptually meaningful but unknowable here — per-trace cost with no trace
// backend, a dataset store that was never configured, a figure that is only
// attributed when a run closes — the surface renders this note INSTEAD of a
// value. Never a zero, never an estimate, never an error.
//
// Three properties are load-bearing and must not be "improved":
//
//   1. IT IS NOT AN ERROR. No icon, no hue, no `role="alert"`, no crit border.
//      Nothing broke: the platform is simply not wired to answer. Dressing an
//      unconfigured capability as a failure teaches operators to ignore real
//      failures, and sends them debugging a system that is working.
//   2. IT IS NOT A ZERO. A zero is a claim ("we measured, it was nought"); an
//      absent backend supports no claim at all. Zero and unknown never share a
//      glyph — see `UnknownValue` below for the inline form.
//   3. THE REGISTER IS CALM. A hairline frame, one 2px ghost rule on the left
//      to mark it as an aside, the sunk plane behind it. It recedes; it does
//      not compete with the data it sits beside.
//
// Canonical body (§7.1, the pattern every caller should follow):
//   "{Capability} isn't configured. {What the visible numbers DO cover}.
//    {What configuring it needs}. Nothing here is estimated — the {value} is
//    simply absent."

export interface QuietNoteProps {
  /**
   * Optional serif head — one short sentence naming what is missing, e.g.
   * "Per-trace cost isn't configured."
   */
  title?: React.ReactNode;
  /** The explanation: what IS covered, what configuring it would need. */
  children?: React.ReactNode;
  className?: string;
}

export function QuietNote({ title, children, className }: QuietNoteProps) {
  return (
    <div
      // `note` is the ARIA role for an aside/parenthetical — deliberately NOT
      // `alert` or `status`: this must never interrupt, and it must never be
      // announced as a problem.
      role="note"
      className={cn(
        "border border-border border-l-2 border-l-ghost bg-surface-2 px-4 py-3",
        className,
      )}
    >
      {title ? (
        <p className="font-serif text-md font-medium">{title}</p>
      ) : null}
      {children ? (
        <div
          className={cn(
            "text-sm text-secondary-foreground",
            title ? "mt-1" : undefined,
          )}
        >
          {children}
        </div>
      ) : null}
    </div>
  );
}

// UnknownValue and its title moved to ./quantity in M151: three components had
// each invented their own dash-and-title, so 43 pages would have inherited
// three different ways of saying "we do not know". Re-exported here because
// this is where a reader looks for it, and because QuietNote and UnknownValue
// are two halves of the same idea — one for a panel, one for a cell.
export { UnknownValue, UNKNOWN_TITLE as UNKNOWN_VALUE_TITLE } from "./quantity";
