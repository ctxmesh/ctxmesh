import * as React from "react";

import { cn } from "@/lib/utils";

// quantity.tsx — the ONE place the console decides what a number means (M151).
//
// This module exists because three separate components independently invented
// an "unknown value" primitive during the redesign, each with its own glyph and
// its own title string. Left alone, 43 pages would have inherited three
// different ways of saying "we do not know", which is precisely the failure the
// rule was written to prevent: the reader cannot tell an unmeasured value from
// a measured zero, and the console quietly starts making claims the backend
// never made.
//
// Everything below is the canonical contract. `meter.tsx` re-exports it for
// compatibility, and `quiet-note.tsx` and `tree-table.tsx` consume it.
// The quantity contract, formerly the header of meter.tsx:
// QUANTITY CONTRACT (§7.1), which lifecycle.tsx and pressure-strip.tsx import.
//
// The contract exists because of one rule that outranks every styling note in
// the spec: THE CONSOLE NEVER CLAIMS MORE THAN THE BACKEND CAN ANSWER. A meter
// with no known cap must not draw a bar — a full green bar is a claim that the
// caller is safely inside a limit nobody knows. A pressure strip missing a
// segment must not render it as zero. And unknown and zero must never share a
// glyph: a known zero is a real `0`, an unknown is an em dash with a title.
//
// Nothing about that rule is enforceable by convention, so it is enforced by
// the TYPE SYSTEM instead:
//
//   * `Quantity = number | Unknown | null | undefined` is NOT assignable to
//     `number`. `used / cap`, `v.toLocaleString()`, `a + b` — every arithmetic
//     or formatting path on a raw Quantity is a COMPILE ERROR. The only way
//     through is `isKnown(q)`, which narrows to `number`; every geometry helper
//     in these three files takes `number`, never `Quantity`.
//   * `Unknown` is a `unique symbol`, so it cannot be forged by a backend
//     string, confused with `0`, or smuggled into JSX (a symbol is not a
//     ReactNode — `{quantity}` in a template is also a compile error).
//   * `isKnown` additionally rejects NaN and Infinity. Those are `number` to
//     TypeScript but they are not answers, and `NaN%` widths silently render as
//     zero-width segments — an unknown wearing a known glyph.
//
// So a caller CANNOT hand these components an unknown and get a number drawn.
// The worst they can do is pass a wrong known number, which is a backend bug,
// not a display lie.

/** The one sentinel for "the backend did not answer" (§7.1). */
export const UNKNOWN = Symbol("ctxmesh.unknown");
export type Unknown = typeof UNKNOWN;

/**
 * A number the console may display — or an explicit absence. `null`/`undefined`
 * are accepted as unknown too, because a field missing from a JSON response is
 * exactly "not answered": the ergonomic mapping `count ?? UNKNOWN` and the lazy
 * `data.count` must both land on the honest branch, never on zero.
 */
export type Quantity = number | Unknown | null | undefined;

/** Narrows a Quantity to a real, finite number. The only gate into arithmetic. */
export function isKnown(q: Quantity): q is number {
  return typeof q === "number" && Number.isFinite(q);
}

/** The glyph for an unknown value. Never `0`, never a formatted zero (§7.1). */
export const UNKNOWN_GLYPH = "—";

/** The `title` an unknown value carries so the dash is explainable on hover (§7.1). */
export const UNKNOWN_TITLE =
  "Not recorded for this install — unknown, not zero.";

/** Locale-grouped mono count: `1,024`. Known input only — see the contract above. */
export function formatCount(n: number): string {
  return n.toLocaleString();
}

/**
 * One value, honestly. A known number renders grouped + tabular; an unknown
 * renders the dash with its title. A known zero renders `0` in `text-ghost`
 * (unremarkable, but REAL) — the two never share a glyph.
 */
export function QuantityValue({
  value,
  format = formatCount,
  title = UNKNOWN_TITLE,
  className,
}: {
  value: Quantity;
  /** Formats the KNOWN branch only; it can never be reached by an unknown. */
  format?: (n: number) => string;
  /** Hover text for the unknown branch. */
  title?: string;
  className?: string;
}) {
  if (!isKnown(value)) {
    return (
      <span
        title={title}
        className={cn("font-mono tabular-nums text-ghost", className)}
      >
        {UNKNOWN_GLYPH}
      </span>
    );
  }
  return (
    <span
      className={cn(
        "font-mono tabular-nums",
        value === 0 && "text-ghost",
        className,
      )}
    >
      {format(value)}
    </span>
  );
}

/**
 * The §5.27 QuietNote recipe — the calm "the backend cannot answer this" block
 * these components emit instead of a guessed bar. It is inlined here rather
 * than imported from `kit/quiet-note.tsx` so this trio has no cross-file
 * dependency on a component being built in parallel; re-pointing it at
 * QuietNote later is a one-line change in this file.
 */
export function MeasureNote({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <p
      className={cn(
        "border border-l-2 border-border border-l-ghost bg-surface-2 px-4 py-3 text-sm text-secondary-foreground",
        className,
      )}
    >
      {children}
    </p>
  );
}

/** Screen-reader text for a value inside an aria-label. */
export function speakQuantity(value: Quantity, format = formatCount): string {
  return isKnown(value) ? format(value) : "not recorded";
}

/**
 * The register a KNOWN zero renders in.
 *
 * This is the one sanctioned exception to "never put information in
 * `text-ghost`", and it is worth stating why rather than leaving it as an
 * inconsistency two people will re-litigate. The rule exists because a value
 * whose meaning depends on being *read* must clear contrast. A known zero does
 * not: the glyph itself — `0` rather than `—` — carries the entire meaning, and
 * a reader who cannot resolve the colour loses nothing at all. What recedes is
 * only the emphasis, which is exactly right for a row that is fine.
 *
 * An UNKNOWN value never uses this. It renders in `text-faint`, because there
 * the meaning IS the thing you have to read.
 */
export const ZERO_CLASS = "text-ghost";

/**
 * UnknownValue — the inline "we do not know this" glyph, for a caller that has
 * a plain `number | null | undefined` rather than a `Quantity`.
 *
 * Prefer `QuantityValue` in new code: it makes an unknown impossible to render
 * as a number at COMPILE time, which this cannot.
 */
export function UnknownValue({
  title = UNKNOWN_TITLE,
  className,
}: {
  title?: string;
  className?: string;
}) {
  return (
    <span className={cn("font-mono text-faint", className)} title={title}>
      {UNKNOWN_GLYPH}
    </span>
  );
}

