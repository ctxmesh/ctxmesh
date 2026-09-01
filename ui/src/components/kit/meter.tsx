import * as React from "react";

import { cn } from "@/lib/utils";
import {
  formatCount,
  isKnown,
  MeasureNote,
  QuantityValue,
  UNKNOWN_GLYPH,
  type Quantity,
} from "./quantity";

// The quantity contract moved to ./quantity so three components could stop
// inventing it separately (M151). Re-exported here because callers and tests
// already import it from this path, and because a Meter without the contract
// is only half a component.
export * from "./quantity";

/**
 * Where a meter's fill sits relative to its bounds — the MEANING behind the
 * hue, exported so it can be tested without asserting on class strings:
 *   under → pine   (healthy motion, inside the bound)
 *   warn  → amber  (a bound is NEAR or crossed — §2.2's only meaning for amber)
 *   over  → crit   (at/over the cap: it will not proceed without a change)
 */
export type MeterState = "under" | "warn" | "over";

export function meterState(
  used: number,
  cap: number,
  threshold?: Quantity,
): MeterState {
  if (used >= cap) return "over";
  if (isKnown(threshold) && threshold > 0 && used >= threshold) return "warn";
  return "under";
}

const FILL: Record<MeterState, string> = {
  under: "bg-primary",
  warn: "bg-warning",
  over: "bg-destructive",
};

/** Percent of `max` that `v` fills, clamped to the track. Known inputs only. */
function pct(v: number, max: number): number {
  if (!(max > 0)) return 0;
  return Math.max(0, Math.min(100, (v / max) * 100));
}

export interface MeterProps {
  /** The bound's name — the mono key at the left of the label row. */
  label: string;
  used: Quantity;
  cap: Quantity;
  /** Where the alert fires. Drawn as the tick; absent ⇒ no tick, no warn hue. */
  threshold?: Quantity;
  /** Formats every figure (money, counts). Known values only, by construction. */
  format?: (n: number) => string;
  /** Names the thing in the no-cap copy: "No cap is set for this {thing}." */
  thing?: string;
  /**
   * The foot line. Omit it to get the canonical sentence when a tick is drawn
   * ("The tick is where the alert fires. 31% of the cap is left."); pass `null`
   * to suppress the foot entirely; pass a node to say something else.
   */
  foot?: React.ReactNode;
  className?: string;
}

export function Meter({
  label,
  used,
  cap,
  threshold,
  format = formatCount,
  thing,
  foot,
  className,
}: MeterProps) {
  // The label row is one mono string so `$0.19 / $0.50` aligns on the slash
  // (§4.8), and it is the SAME row whether or not a bar can be drawn — the
  // figure the backend does know is never hidden by the figure it doesn't.
  const labelRow = (
    <div className="flex items-baseline justify-between gap-3 font-mono text-xs">
      <span className="truncate text-faint">{label}</span>
      <span className="whitespace-nowrap tabular-nums">
        <QuantityValue value={used} format={format} />
        <span className="text-faint"> / </span>
        <QuantityValue value={cap} format={format} />
      </span>
    </div>
  );

  // NO CAP ⇒ NO BAR (§5.24). A bar without a cap has no denominator, so every
  // width it could draw would be invented. The used figure still shows; the
  // absence is stated in words.
  if (!isKnown(cap) || cap <= 0) {
    return (
      <div className={cn("space-y-2", className)}>
        {labelRow}
        <MeasureNote>
          {thing ? `No cap is set for this ${thing}.` : "No cap is set."}{" "}
          {isKnown(used)
            ? "The figure above is the real usage — there is simply no bound to draw it against."
            : "Neither the usage nor a bound is recorded for this install. Nothing here is estimated."}
        </MeasureNote>
      </div>
    );
  }

  // A known cap but no known usage: the track is drawn empty (the bound is
  // real) and the fill is omitted — an empty bar claims nothing, whereas a
  // zero-width fill labelled 0 would claim the caller has spent nothing.
  if (!isKnown(used)) {
    return (
      <div className={cn("space-y-2", className)}>
        {labelRow}
        <Track cap={cap} threshold={threshold} />
        <MeasureNote>
          Usage against this cap is not recorded for this install. It reads{" "}
          <span className="font-mono">{UNKNOWN_GLYPH}</span> rather than a zero,
          because zero would be a claim we can&rsquo;t make.
        </MeasureNote>
      </div>
    );
  }

  const state = meterState(used, cap, threshold);
  const remaining = Math.max(0, Math.round(((cap - used) / cap) * 100));
  const footNode =
    foot === undefined
      ? isKnown(threshold) && threshold > 0 && threshold <= cap
        ? `The tick is where the alert fires. ${remaining}% of the cap is left.`
        : null
      : foot;

  return (
    <div className={cn("space-y-2", className)}>
      {labelRow}
      <Track
        cap={cap}
        threshold={threshold}
        used={used}
        state={state}
        label={label}
        format={format}
      />
      {footNode !== null && footNode !== undefined && (
        <p className="text-sm text-faint">{footNode}</p>
      )}
    </div>
  );
}

/**
 * The 6px track. Carries the ARIA when a value exists: `role="meter"` with
 * valuenow/valuemax plus a text fallback, so a screen reader gets the numbers
 * and not silence (§5.24). With no known `used` there is no value to announce,
 * so the empty track is decoration and says so.
 */
function Track({
  cap,
  used,
  threshold,
  state,
  label,
  format = formatCount,
}: {
  cap: number;
  used?: number;
  threshold?: Quantity;
  state?: MeterState;
  label?: string;
  format?: (n: number) => string;
}) {
  const tick =
    isKnown(threshold) && threshold > 0 && threshold <= cap
      ? pct(threshold, cap)
      : null;
  const a11y =
    used === undefined
      ? ({ "aria-hidden": true } as const)
      : ({
          role: "meter",
          "aria-label": label,
          "aria-valuemin": 0,
          "aria-valuemax": cap,
          "aria-valuenow": used,
          "aria-valuetext": `${format(used)} of ${format(cap)}`,
        } as const);
  return (
    <div
      {...a11y}
      // rounded-sm = 2px: a meter is a bar, not a pill (§2.6).
      className="relative h-1.5 w-full overflow-visible rounded-sm border bg-surface-2"
    >
      <div className="absolute inset-0 overflow-hidden rounded-sm">
        {used !== undefined && (
          <div
            className={cn("h-full", FILL[state ?? "under"])}
            style={{ width: `${pct(used, cap)}%` }}
          />
        )}
      </div>
      {tick !== null && (
        // 1px hairline overhanging 3px above and below the track, so the alert
        // point is legible even when the fill sits right on it.
        <span
          aria-hidden="true"
          className="absolute -top-[3px] h-3 w-px bg-faint"
          style={{ left: `${tick}%` }}
        />
      )}
    </div>
  );
}
