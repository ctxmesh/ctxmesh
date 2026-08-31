import { Link } from "react-router-dom";

import { cn } from "@/lib/utils";

// NextStepLink — the console's signature element (§5.19). It is the last column
// of every table and it answers the question the row exists to raise: what
// should *I* do about this? Verb-first, ≤22 characters, one per row, describing
// the USER's next action and never the system's state ("Review the stop →", not
// "Stopped"). The `→` is appended by the component so 43 pages cannot each
// decide whether to include it.
//
// Three tones, and the third is the one that matters:
//   default — pine, with the §2.3 resting underline (a pine-surface bottom rule
//             that firms to pine on hover). Same treatment as ResourceLink, so
//             "underline = a destination" stays one promise console-wide.
//   crit    — the target is a failure or a stop. Crit is the ONE hue allowed to
//             be interactive (§2.3); it stays distinguishable from a crit STATUS
//             tag by form — a link is underlined sentence-case sans, a tag is
//             uppercase mono on a tint. A chip is never a link.
//   none    — the literal words "Nothing needed", `text-faint` at normal weight,
//             NO underline, NOT an anchor, NOT focusable. Deviation from the
//             mock, which styled this `ink-4` (1.9:1) and left it an <a>: an
//             inert state must be READABLE (4.8:1) and must not lie about being
//             clickable. Tab order skips it because there is nothing to press.
//
// A tone that is not `none` but was given neither `to` nor `onClick` is a page
// bug, not a link: it renders as honest inert text rather than a dead anchor
// (the same doctrine as ResourceLink's no-detail-page case).

export type NextStepTone = "default" | "crit" | "none";

/** The copy budget from §4.4/§7.2 — the column is never truncated, so the words are. */
export const NEXT_STEP_MAX_CHARS = 22;

/** The words the inert state always renders. Not caller-supplied. */
export const NOTHING_NEEDED = "Nothing needed";

export interface NextStepLinkProps {
  /** Verb-first, ≤22 chars, no trailing arrow. Ignored when tone is "none". */
  label?: string;
  /** Router destination for the next action. */
  to?: string;
  onClick?: () => void;
  tone?: NextStepTone;
  /** Longer context for a screen reader when the row's label alone is terse. */
  ariaLabel?: string;
  className?: string;
  testId?: string;
}

/**
 * The §5.19 sort contract, so every list orders the column the same way: rows
 * that need something sort above rows that do not, and "Nothing needed" sorts
 * last. Use as the primary comparator key on the Next step column.
 */
export function nextStepRank(tone?: NextStepTone): number {
  return tone === "none" ? 1 : 0;
}

// The resting/hover underline per tone. Pine-surface at rest → pine on hover;
// crit-surface at rest → crit on hover. Never a text-decoration underline: the
// bottom rule sits clear of the descenders.
const TONE_CLASS: Record<Exclude<NextStepTone, "none">, string> = {
  default: "text-primary border-b border-accent hover:border-primary",
  crit: "text-destructive border-b border-destructive-surface hover:border-destructive",
};

export function NextStepLink({
  label,
  to,
  onClick,
  tone = "default",
  ariaLabel,
  className,
  testId,
}: NextStepLinkProps) {
  // Inert: nothing is needed on this row. Plain words, no affordance, no focus.
  if (tone === "none") {
    return (
      <span
        className={cn("whitespace-nowrap text-sm font-normal text-faint", className)}
        data-testid={testId}
      >
        {NOTHING_NEEDED}
      </span>
    );
  }

  const text = label ?? "";
  const body = (
    <>
      {text}
      {/* Decoration: the arrow is the link's shape, not information — a screen
          reader hears the verb phrase, not "right arrow". */}
      <span aria-hidden="true"> →</span>
    </>
  );
  const shared = "whitespace-nowrap text-sm font-semibold";

  // A next step with no destination is a page bug. Render honest text rather
  // than an underlined promise that goes nowhere.
  if (!to && !onClick) {
    return (
      <span className={cn(shared, "text-faint", className)} data-testid={testId}>
        {text}
      </span>
    );
  }

  const toneClass = TONE_CLASS[tone];

  if (to) {
    return (
      <Link
        to={to}
        aria-label={ariaLabel}
        onClick={onClick}
        className={cn(shared, toneClass, className)}
        data-testid={testId}
      >
        {body}
      </Link>
    );
  }

  return (
    <button
      type="button"
      aria-label={ariaLabel}
      onClick={onClick}
      className={cn(shared, toneClass, className)}
      data-testid={testId}
    >
      {body}
    </button>
  );
}
