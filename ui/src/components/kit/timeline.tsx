import * as React from "react";

import { Skeleton } from "@/components/kit/skeleton";
import { cn } from "@/lib/utils";

// Timeline — the run's story, told once (M151 §5.26; archetype A5).
//
// The doctrine that shapes every decision below: MODEL CALLS, TOOL CALLS,
// GUARDRAILS, APPROVALS AND STOPS ARE ONE STORY. To the person reading a run
// they are not "the run" and "the governance system" — they are what happened,
// in order. So governance never gets its own lane, its own tab, or its own
// panel. It renders in the same spine as everything else, visibly annotated
// with a hue and a wash, exactly where it interrupted the work.
//
// Put a guardrail in a side panel and the reader has to correlate two clocks to
// answer "why did this run stop here?". Put it in the spine and the answer is
// the next line down.
//
// The second doctrine is the title: a step's title is a SENTENCE, not an event
// enum. "A guardrail removed an email address", never `GUARDRAIL_REDACT_PII`.
// The machine words belong in the detail line, inline-mono, where they are
// evidence rather than vocabulary the reader has to learn first.
//
// Tones (§5.26 / §2.2 hue discipline):
//   step        a plain moment                     hollow dot on the card plane
//   done        it completed                       pine dot (this is the ONE
//                                                   place pine annotates, and it
//                                                   is not a status: it is the
//                                                   spine's own progress mark)
//   governance  guardrail / approval gate / stop   warn dot + warn wash
//   hold        it is waiting on a PERSON          hold dot + hold wash
//   failed      it will not proceed                crit dot + crit wash
//
// The wash is a left-anchored fade to transparent, not a filled band: §2.2
// allows exactly two full-bleed semantic surfaces console-wide and this is not
// one of them. It is enough to make a governed step findable when scrolling.
//
// Colour is never the only carrier: each non-plain tone renders a screen-reader
// prefix naming what it is.

export type TimelineTone = "step" | "done" | "governance" | "hold" | "failed";

export interface TimelineStep {
  /** Stable key — the step/span id. */
  id: string;
  /** Mono clock or offset, exactly as it should read: "16:35:02", "+1.2s". */
  time?: string;
  /** A SENTENCE describing what happened. Never an event enum. */
  title: React.ReactNode;
  /**
   * Optional second line (≤64ch): the evidence. Render machine words inside it
   * as inline mono (`<code>` or a `font-mono` span).
   */
  detail?: React.ReactNode;
  /** Right-aligned mono meta — duration, cost, token count. */
  meta?: React.ReactNode;
  tone?: TimelineTone;
}

export interface TimelineProps {
  steps: TimelineStep[];
  /** Render the 5-step loading spine instead of the steps (§7 A5). */
  loading?: boolean;
  /** Accessible name for the list, e.g. "Steps in run 4f2c". */
  label?: string;
  className?: string;
}

/** Dot fill + rule per tone. A plain step is hollow: it is punctuation, not a claim. */
const DOT: Record<TimelineTone, string> = {
  step: "bg-card border-ghost",
  done: "bg-primary border-primary",
  governance: "bg-warning border-warning",
  hold: "bg-hold border-hold",
  failed: "bg-destructive border-destructive",
};

/** The left wash marking a step the reader must not scroll past. */
const WASH: Record<TimelineTone, string | undefined> = {
  step: undefined,
  done: undefined,
  governance: "bg-gradient-to-r from-warning-surface to-transparent",
  hold: "bg-gradient-to-r from-hold-surface to-transparent",
  failed: "bg-gradient-to-r from-destructive-surface to-transparent",
};

/**
 * What each tone MEANS, announced to a screen reader. Colour and a wash are
 * invisible to a reader who cannot see them, and "a person must decide" is the
 * single most important thing this timeline says.
 */
const TONE_ANNOUNCEMENT: Record<TimelineTone, string | undefined> = {
  step: undefined,
  done: "Completed:",
  governance: "Governance step:",
  hold: "Waiting on a person:",
  failed: "Failed:",
};

/** The rail: 22px wide, a 1px rule down its centre line (x = 11px). */
const RAIL_LINE = "pointer-events-none absolute bottom-0 left-[11px] top-0 w-px bg-border-strong";
/** A 9px dot centred on that rule, on the first text line of its step. */
const DOT_BASE = "absolute left-[7px] top-[19px] h-[9px] w-[9px] rounded-full border";
/** Content clears the 22px rail, then takes the step block's own 14px padding. */
const STEP_BLOCK = "py-3.5 pl-[36px] pr-3.5";

export function Timeline({
  steps,
  loading = false,
  label = "Timeline",
  className,
}: TimelineProps) {
  if (loading) return <TimelineSkeleton className={className} />;

  return (
    <ol aria-label={label} className={cn("relative", className)}>
      <span aria-hidden="true" className={RAIL_LINE} />
      {steps.map((step) => {
        const tone = step.tone ?? "step";
        const announce = TONE_ANNOUNCEMENT[tone];
        return (
          <li
            key={step.id}
            className={cn(
              "relative border-b border-border-soft last:border-0",
              WASH[tone],
            )}
          >
            <span aria-hidden="true" className={cn(DOT_BASE, DOT[tone])} />
            <div className={STEP_BLOCK}>
              <div className="flex items-baseline gap-3">
                {step.time ? (
                  <span className="shrink-0 font-mono text-xs tabular-nums text-faint">
                    {step.time}
                  </span>
                ) : null}
                <p className="min-w-0 flex-1 text-[14.5px] leading-5">
                  {announce ? <span className="sr-only">{announce} </span> : null}
                  {step.title}
                </p>
                {step.meta ? (
                  <span className="shrink-0 font-mono text-xs tabular-nums text-secondary-foreground">
                    {step.meta}
                  </span>
                ) : null}
              </div>
              {step.detail ? (
                <p className="mt-1 max-w-[64ch] text-[13.5px] leading-[1.35] text-secondary-foreground">
                  {step.detail}
                </p>
              ) : null}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

/**
 * The loading spine: five dot+bar steps on the real geometry, so the page does
 * not jump when the run's steps arrive (§5.9's shape rule). One labelled busy
 * region, not five — a screen reader hears "Loading" once.
 */
export function TimelineSkeleton({
  steps = 5,
  className,
}: {
  steps?: number;
  className?: string;
}) {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading timeline"
      className={cn("relative", className)}
    >
      <span aria-hidden="true" className={RAIL_LINE} />
      {Array.from({ length: steps }).map((_, i) => (
        <div
          key={i}
          className="relative border-b border-border-soft last:border-0"
        >
          <span
            aria-hidden="true"
            className={cn(DOT_BASE, "bg-surface-2 border-border-soft")}
          />
          <div className={STEP_BLOCK}>
            <div className="flex items-baseline gap-3">
              <Skeleton decorative className="h-3.5 w-12" />
              <Skeleton
                decorative
                className={cn("h-3.5", i % 2 === 0 ? "w-2/5" : "w-3/5")}
              />
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}
