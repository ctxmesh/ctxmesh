import * as React from "react";

import { cn } from "@/lib/utils";
import { UNKNOWN, UNKNOWN_GLYPH, UNKNOWN_TITLE, type Unknown } from "./meter";

// LifecycleStrip + LifecycleTrack — the Build → Govern → Ship → Improve spine
// (M151 §5.20).
//
// THE STRIP IS A STRIP, NEVER NAVIGATION. It says where a thing IS in its life,
// so it is a list of states, not a set of destinations: no anchors, no buttons,
// nothing focusable, no `nav` landmark. The active stage is marked with
// `aria-current="step"`, which is exactly the "you are here" semantic without
// implying "click to go there". If a stage ever needs to be clickable, it stops
// being this component.
//
// The pine on the active cell is §2.2-legal because pine here means HEALTHY
// MOTION and brand, not status: the strip has no failure state. The only hue it
// ever swaps to is crit, and only on LifecycleTrack, and only for `stopped` —
// which is a state that will not proceed without a person, exactly what crit
// means.
//
// Facts are backend-supplied (§5.20: "never invent one"). A stage whose fact
// the backend cannot answer renders the §7.1 unknown copy — never a blank cell,
// never a plausible-sounding placeholder.

export const LIFECYCLE_STAGES = ["Build", "Govern", "Ship", "Improve"] as const;
export type LifecycleStage = (typeof LIFECYCLE_STAGES)[number];

export interface LifecycleStageCell {
  name: LifecycleStage;
  /**
   * The one-line fact under the stage name. Omit it and the cell renders the
   * §7.1 "not yet known" copy — the type deliberately gives no way to say
   * "unknown" that produces a sentence, so an absent fact cannot be faked.
   */
  fact?: React.ReactNode;
  active?: boolean;
}

/**
 * Class for the bold numbers inside a fact line (§5.20: mono text-faint fact
 * with `text-secondary-foreground` bold figures). Exported so every caller's
 * facts emphasize the same way instead of each page inventing a weight.
 */
export const lifecycleFactNumber = "font-semibold text-secondary-foreground";

export interface LifecycleStripProps {
  stages: LifecycleStageCell[];
  /** Accessible name for the strip region. */
  label?: string;
  className?: string;
}

export function LifecycleStrip({
  stages,
  label = "Lifecycle",
  className,
}: LifecycleStripProps) {
  return (
    <ol
      aria-label={label}
      className={cn(
        // 2×2 below md so four cells never crush to 60px columns on a phone.
        "grid grid-cols-2 overflow-hidden rounded-lg border bg-card md:grid-cols-4",
        className,
      )}
    >
      {stages.map((stage) => (
        <li
          key={stage.name}
          aria-current={stage.active ? "step" : undefined}
          className={cn(
            "relative px-5 py-[17px] border-border",
            // Internal rules only: the card frame draws the outside edges.
            "border-r [&:nth-child(2n)]:border-r-0 md:[&:nth-child(2n)]:border-r md:last:border-r-0",
            "[&:nth-child(-n+2)]:border-b md:[&:nth-child(-n+2)]:border-b-0",
            stage.active && "bg-accent",
          )}
        >
          {stage.active && (
            // The 2px pine bar reads as "the strip is lit here" at a glance;
            // the pine stage name and aria-current carry the same fact for
            // readers who never see it.
            <span
              aria-hidden="true"
              className="absolute inset-x-0 top-0 h-0.5 bg-primary"
            />
          )}
          <div
            className={cn(
              "font-serif text-xl",
              stage.active ? "text-primary" : "text-foreground",
            )}
          >
            {stage.name}
          </div>
          <div className="mt-1 font-mono text-xs text-faint">
            {stage.fact ?? (
              <span className="text-ghost" title={UNKNOWN_TITLE}>
                not yet known
              </span>
            )}
          </div>
        </li>
      ))}
    </ol>
  );
}

export interface LifecycleTrackProps {
  /**
   * The stage the thing is in. `UNKNOWN`/`null` renders the §7.1 dash instead
   * of the bar: a segmented track is a POSITION claim, and a guessed position
   * is the one thing this component must never draw.
   */
  stage: LifecycleStage | Unknown | null;
  /** The whole track goes crit: stopped work will not proceed without a person. */
  stopped?: boolean;
  /** Overrides the label right of the track. Defaults to the stage name. */
  label?: string;
  className?: string;
}

export function LifecycleTrack({
  stage,
  stopped,
  label,
  className,
}: LifecycleTrackProps) {
  const index =
    stage === UNKNOWN || stage == null ? -1 : LIFECYCLE_STAGES.indexOf(stage);

  if (index < 0) {
    return (
      <span
        title={UNKNOWN_TITLE}
        className={cn("font-mono text-xs text-ghost", className)}
      >
        {UNKNOWN_GLYPH}
      </span>
    );
  }

  // A stopped track says so in words as well as in crit — colour is never the
  // only carrier of a state this consequential.
  const text = label ?? (stopped ? "stopped" : (stage as LifecycleStage));
  const name = stopped
    ? `Lifecycle: stopped at ${stage as LifecycleStage}`
    : `Lifecycle: ${stage as LifecycleStage}, stage ${index + 1} of ${LIFECYCLE_STAGES.length}`;

  return (
    <span
      role="img"
      aria-label={name}
      className={cn("inline-flex items-center", className)}
    >
      <span className="relative flex" aria-hidden="true">
        {LIFECYCLE_STAGES.map((s, i) => (
          <span
            key={s}
            className={cn(
              "h-[3px] w-[26px]",
              stopped
                ? "bg-destructive-surface"
                : i <= index
                  ? "bg-primary"
                  : "bg-border-strong",
            )}
          />
        ))}
        {/* The dot marks the CURRENT segment; the ring in the card colour keeps
            it legible where it overlaps the 3px bar. */}
        <span
          className={cn(
            "absolute top-1/2 h-[9px] w-[9px] -translate-x-1/2 -translate-y-1/2 rounded-full ring-2 ring-card",
            stopped ? "bg-destructive" : "bg-primary",
          )}
          style={{ left: `${index * 26 + 13}px` }}
        />
      </span>
      <span
        className={cn(
          "ml-3 whitespace-nowrap font-mono text-xs",
          stopped ? "text-destructive" : "text-secondary-foreground",
        )}
      >
        {text}
      </span>
    </span>
  );
}
