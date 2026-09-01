import { cn } from "@/lib/utils";
import {
  MeasureNote,
  QuantityValue,
  UNKNOWN_GLYPH,
  UNKNOWN_TITLE,
  isKnown,
  speakQuantity,
  type Quantity,
} from "./meter";

// PressureStrip — how a team of 1,024 agents is read at a glance (M151 §5.21).
//
// One stacked bar plus a mono legend. The bar is the shape of the pressure; the
// legend is the numbers, because a proportion is not a count and a reader
// deciding whether to intervene needs the count.
//
// Hue discipline (§2.2), and it is the whole point of the component:
//   running  pine     healthy motion — the system doing its job
//   queued   ghost    waiting its turn; not a problem, not an alarm
//   held     violet   A PERSON MUST DECIDE. The one segment that means "you"
//   failed   crit     will not proceed without a change
//   idle     the track itself — off is not a problem and gets no ink
//
// THE UNKNOWN RULE: a member the backend did not answer is dropped from the bar
// AND from the denominator, and the absence is stated in words. It is never
// drawn as a zero-width segment, because a zero-width segment is indistinguish-
// able from "there are none" — the exact conflation §7.1 forbids. The types
// make this the only reachable behavior: `Quantity` cannot be summed or divided
// without `isKnown` narrowing it first (see the contract in meter.tsx).

type MemberKey = "running" | "queued" | "held" | "failed" | "idle";

const MEMBERS: {
  key: MemberKey;
  /** Segment fill. `idle` has none — the track shows through. */
  fill?: string;
  /** Legend chip. `idle`'s chip is the empty track, outlined so it is visible. */
  chip: string;
}[] = [
  { key: "running", fill: "bg-primary", chip: "bg-primary" },
  { key: "queued", fill: "bg-ghost", chip: "bg-ghost" },
  { key: "held", fill: "bg-hold", chip: "bg-hold" },
  { key: "failed", fill: "bg-destructive", chip: "bg-destructive" },
  { key: "idle", chip: "border border-border bg-surface-2" },
];

export interface PressureStripProps {
  /** Integers from the backend only — `UNKNOWN`/`null` for anything it can't answer. */
  running: Quantity;
  queued: Quantity;
  held: Quantity;
  failed: Quantity;
  idle: Quantity;
  /** `mini` (6px × 120px, no legend) serves table cells. */
  size?: "default" | "mini";
  /** Accessible name for the bar. */
  label?: string;
  className?: string;
}

export function PressureStrip({
  running,
  queued,
  held,
  failed,
  idle,
  size = "default",
  label = "Agent pressure",
  className,
}: PressureStripProps) {
  const values: Record<MemberKey, Quantity> = {
    running,
    queued,
    held,
    failed,
    idle,
  };

  // The denominator is the sum of what is KNOWN. An unknown member cannot enter
  // it (isKnown is the only gate), so no segment width is ever inferred from a
  // number nobody supplied.
  const known = MEMBERS.filter((m) => isKnown(values[m.key]));
  const total = known.reduce(
    (sum, m) => sum + Math.max(0, values[m.key] as number),
    0,
  );
  const unknownKeys = MEMBERS.filter((m) => !isKnown(values[m.key])).map(
    (m) => m.key,
  );

  const spoken = MEMBERS.map(
    (m) => `${speakQuantity(values[m.key])} ${m.key}`,
  ).join(", ");
  const a11yName = `${label}: ${spoken}`;

  // Nothing is known: there is no bar to draw at all. Drawing an empty track
  // here would read as "1,024 agents, all idle" — a claim, not an absence.
  const nothingKnown = known.length === 0;

  const note =
    unknownKeys.length > 0 ? (
      <>
        {sentenceList(unknownKeys)}{" "}
        {unknownKeys.length === 1 ? "is" : "are"} not recorded for this install.{" "}
        {nothingKnown
          ? "There is nothing to draw — nothing here is estimated."
          : "Those segments are absent from the bar rather than drawn as zero — nothing here is estimated."}
      </>
    ) : null;

  const bar = nothingKnown ? null : (
    <div
      role="img"
      aria-label={a11yName}
      title={size === "mini" && note ? stripTitle(unknownKeys) : undefined}
      // rounded-sm = 2px (§2.6): a pressure bar is a bar, not a pill.
      className={cn(
        "flex overflow-hidden rounded-sm border bg-surface-2",
        size === "mini" ? "h-1.5 w-[120px]" : "h-2.5 w-full",
      )}
    >
      {MEMBERS.filter((m) => m.fill).map((m) => {
        const v = values[m.key];
        if (!isKnown(v) || v <= 0) return null;
        const share = total > 0 ? (v / total) * 100 : 0;
        return (
          <div
            key={m.key}
            className={cn(
              m.fill,
              // A 4-of-1024 hold is 0.4% — under a pixel at any real width, and
              // it is the segment most likely to need a person. Floor every
              // segment at 3px so nothing that EXISTS can vanish (Deviation
              // from the mock, §5.21).
              share < 1 && "min-w-[3px]",
            )}
            style={{ width: `${share}%` }}
          />
        );
      })}
    </div>
  );

  if (size === "mini") {
    // No legend in a table cell; the counts live in the accessible name, and
    // the unknown disclosure lives in the title (there is no room for prose).
    return nothingKnown ? (
      <span
        title={UNKNOWN_TITLE}
        className={cn("font-mono text-xs text-ghost", className)}
      >
        {UNKNOWN_GLYPH}
      </span>
    ) : (
      <span className={cn("inline-flex items-center", className)}>{bar}</span>
    );
  }

  return (
    <div className={cn("space-y-2", className)}>
      {bar}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-xs text-faint">
        {MEMBERS.map((m) => (
          <span
            key={m.key}
            data-testid={`pressure-${m.key}`}
            className="inline-flex items-center gap-1.5"
          >
            {/* 8px chip, 1px corner — a swatch, not a dot (§5.21). */}
            <span
              aria-hidden="true"
              className={cn("h-2 w-2 rounded-[1px]", m.chip)}
            />
            {m.key}
            <QuantityValue
              value={values[m.key]}
              className={cn(
                // A known zero is a real 0 and stays quiet; an unknown is a
                // dash. Same register, DIFFERENT GLYPH — §7.1.
                isKnown(values[m.key]) && values[m.key] !== 0
                  ? "font-semibold text-foreground"
                  : "text-ghost",
              )}
            />
          </span>
        ))}
      </div>
      {note && <MeasureNote>{note}</MeasureNote>}
    </div>
  );
}

/** "queued", "queued and held", "queued, held and failed" — sentence-initial. */
function sentenceList(keys: string[]): string {
  const capped = keys.map((k, i) => (i === 0 ? cap(k) : k));
  if (capped.length === 1) return capped[0];
  return `${capped.slice(0, -1).join(", ")} and ${capped[capped.length - 1]}`;
}

function cap(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function stripTitle(keys: string[]): string {
  return `${sentenceList(keys)} ${keys.length === 1 ? "is" : "are"} not recorded for this install.`;
}
