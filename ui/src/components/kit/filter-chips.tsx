import * as React from "react";

import { cn } from "@/lib/utils";

// FilterChipRow — the attention-first row above every list (M151 §5.28,
// archetype A1).
//
// These chips are VIEWS, not filters. "Needs a person · Failing · Everything"
// is one question with one answer at a time, which is why this is a radiogroup
// and not a bank of checkboxes: chips that AND together produce empty
// intersections nobody asked for, and they destroy the page's whole premise —
// that the first thing you see is the thing that is blocking.
//
// The counts are the reason the row works, and they carry one hard rule: A
// COUNT IS A FACT FROM THE BACKEND, NEVER A CLIENT-SIDE GUESS. The console
// pages a windowed slice of a much larger set, so counting the rows in hand and
// printing that number would be confidently wrong — and wrong in the direction
// that hides work. A chip whose count the backend did not send therefore shows
// NO number at all: no "0", no "—", nothing. Silence is the honest rendering of
// "we were not told", and unlike a zero it cannot be misread as "nothing here".
// A real 0 from the backend is a real answer and does render.
//
// Keyboard: full radiogroup semantics (arrow keys move AND select, Home/End
// jump, one tab stop for the whole row) — the pattern a screen-reader user
// expects the moment they hear "radio group".

export interface FilterChip {
  /** Stable id — what `onChange` reports and `value` matches. */
  id: string;
  /** Sentence-case label, e.g. "Needs a person". */
  label: string;
  /**
   * The backend's count for this view. Omit when the backend did not send one:
   * the chip then shows no number. NEVER pass a client-side count of the rows
   * currently rendered.
   */
  count?: number;
}

export interface FilterChipRowProps {
  chips: FilterChip[];
  /** The selected chip's id. */
  value: string;
  onChange: (id: string) => void;
  /** Accessible name for the group, e.g. "Filter agents". */
  label?: string;
  className?: string;
}

export function FilterChipRow({
  chips,
  value,
  onChange,
  label = "Filter",
  className,
}: FilterChipRowProps) {
  const refs = React.useRef<Array<HTMLButtonElement | null>>([]);
  const selectedIndex = Math.max(
    0,
    chips.findIndex((c) => c.id === value),
  );

  // Arrow keys move the selection itself (WAI-ARIA radiogroup), so the row can
  // be driven without ever leaving the keyboard.
  function move(to: number) {
    if (chips.length === 0) return;
    const next = (to + chips.length) % chips.length;
    onChange(chips[next].id);
    refs.current[next]?.focus();
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLButtonElement>, index: number) {
    switch (e.key) {
      case "ArrowRight":
      case "ArrowDown":
        e.preventDefault();
        move(index + 1);
        break;
      case "ArrowLeft":
      case "ArrowUp":
        e.preventDefault();
        move(index - 1);
        break;
      case "Home":
        e.preventDefault();
        move(0);
        break;
      case "End":
        e.preventDefault();
        move(chips.length - 1);
        break;
      default:
        break;
    }
  }

  return (
    <div
      role="radiogroup"
      aria-label={label}
      className={cn("flex flex-wrap gap-2", className)}
    >
      {chips.map((chip, i) => {
        const active = chip.id === value;
        // A count of 0 is a real answer and prints; `undefined` prints nothing.
        const hasCount = typeof chip.count === "number";
        return (
          <button
            key={chip.id}
            ref={(el) => {
              refs.current[i] = el;
            }}
            type="button"
            role="radio"
            aria-checked={active}
            // Roving tabindex: the row is ONE tab stop, as a radiogroup must be.
            tabIndex={i === selectedIndex ? 0 : -1}
            onClick={() => onChange(chip.id)}
            onKeyDown={(e) => onKeyDown(e, i)}
            className={cn(
              "inline-flex items-center gap-2 rounded-sm border px-3 py-1.5 text-sm transition-colors",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
              active
                ? // Selection is always pine-family, never a status hue (§2.3).
                  "border-primary bg-accent font-semibold text-primary"
                : "border-border-strong bg-card text-secondary-foreground hover:bg-surface-2",
            )}
          >
            {chip.label}
            {hasCount ? (
              <span
                className={cn(
                  "font-mono text-xs tabular-nums",
                  active ? "text-primary" : "text-faint",
                )}
              >
                {chip.count}
              </span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
