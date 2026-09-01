import * as React from "react";
import { Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { ConfirmDialog } from "@/components/kit/confirm-dialog";

// Wizard — the multi-step primitive that kills the M12 giant form (kit, m13.1 →
// real m13.4; spec §5). Every creation flow (connect-provider, add-MCP,
// create-agent) is a Wizard: a labelled step rail with progress, a body slot
// per step, and a footer with Back / Next / Finish. The LAST step is
// conventionally a review.
//
// Controlled: the parent owns `current` and validation (`canProceed`) so it can
// gate a step until inputs are valid, run async work on Next (probe a provider,
// generate a config), and render the review from collected state. The Wizard
// renders chrome + the active step's `content`.
//
// Productionized in m13.4:
//   • Per-step gating — Next/Finish disabled unless `canProceed` (unchanged
//     API); tested explicitly.
//   • Focus management — on step change, focus moves to the step panel (which
//     is labelled by the step title) so keyboard/AT users follow the flow.
//   • aria-current="step" on the active rail item; the panel is a labelled
//     region wired to the step heading.
//   • Esc-guard on dirty state — when `dirty`, Esc (and Cancel) route through a
//     discard-confirm instead of dropping unsaved input on the floor.

export interface WizardStep {
  id: string;
  title: string;
  /** Optional one-line hint under the title in the rail. */
  description?: string;
  content: React.ReactNode;
  /** Marks this as the review/summary step (styled as the finish line). */
  review?: boolean;
}

export interface WizardProps {
  steps: WizardStep[];
  current: number;
  onStepChange: (index: number) => void;
  /** Gate forward navigation — false disables Next on the current step. */
  canProceed?: boolean;
  /** Show a spinner on the Next/Finish button (async probe/generate). */
  busy?: boolean;
  onFinish?: () => void;
  finishLabel?: string;
  nextLabel?: string;
  /** Cancel out of the whole flow (closes a dialog / returns to the list). */
  onCancel?: () => void;
  /** When true, Cancel/Esc route through a discard-confirmation guard. */
  dirty?: boolean;
  className?: string;
}

export function Wizard({
  steps,
  current,
  onStepChange,
  canProceed = true,
  busy = false,
  onFinish,
  finishLabel = "Create",
  nextLabel = "Continue",
  onCancel,
  dirty = false,
  className,
}: WizardProps) {
  const isLast = current === steps.length - 1;
  const step = steps[current];
  const panelRef = React.useRef<HTMLDivElement>(null);
  const headingId = React.useId();
  const [confirmDiscard, setConfirmDiscard] = React.useState(false);

  // Focus the step panel on step change so AT/keyboard users follow the flow.
  // The panel is tabIndex=-1 + aria-labelledby the step heading.
  React.useEffect(() => {
    panelRef.current?.focus();
  }, [current]);

  // Esc-guard: if dirty, ask before discarding; otherwise cancel straight out.
  const requestCancel = React.useCallback(() => {
    if (!onCancel) return;
    if (dirty) setConfirmDiscard(true);
    else onCancel();
  }, [dirty, onCancel]);

  React.useEffect(() => {
    if (!onCancel) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" && !confirmDiscard) {
        e.preventDefault();
        requestCancel();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onCancel, confirmDiscard, requestCancel]);

  return (
    <div className={cn("grid gap-8 md:grid-cols-[15rem_1fr]", className)}>
      {/* Step rail — progress + labels; past steps are clickable (go back). */}
      <ol className="hidden space-y-1 md:block">
        {steps.map((s, i) => {
          const done = i < current;
          const active = i === current;
          return (
            <li key={s.id}>
              <button
                type="button"
                aria-current={active ? "step" : undefined}
                disabled={i > current}
                onClick={() => i < current && onStepChange(i)}
                className={cn(
                  "flex w-full items-start gap-3 rounded-md px-3 py-2 text-left transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed",
                  active && "bg-accent",
                  !active && done && "hover:bg-surface-2",
                  i > current && "opacity-50",
                )}
              >
                <span
                  className={cn(
                    "mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full font-mono text-xs font-semibold",
                    done && "bg-success text-success-foreground",
                    active && "bg-primary text-primary-foreground",
                    !done && !active && "border bg-card text-faint",
                  )}
                >
                  {done ? <Check className="h-3.5 w-3.5" /> : i + 1}
                </span>
                <span className="min-w-0">
                  <span
                    className={cn(
                      "block text-sm font-medium",
                      active ? "text-accent-foreground" : "text-foreground",
                    )}
                  >
                    {s.title}
                  </span>
                  {s.description && (
                    <span className="block text-xs text-faint">
                      {s.description}
                    </span>
                  )}
                </span>
              </button>
            </li>
          );
        })}
      </ol>

      {/* Mobile progress: a compact "Step n of m" bar. */}
      <div className="md:hidden">
        <div className="mb-1 flex items-center justify-between text-xs text-faint">
          <span className="font-mono">
            Step {current + 1} of {steps.length}
          </span>
          <span className="font-medium text-foreground">{step.title}</span>
        </div>
        {/* A meter, not a pip — §1.4 puts meters/mini-bars on rounded-sm. */}
        <div className="h-1.5 overflow-hidden rounded-sm bg-surface-2">
          <div
            className="h-full rounded-sm bg-primary transition-all"
            style={{ width: `${((current + 1) / steps.length) * 100}%` }}
          />
        </div>
      </div>

      {/* Body + footer. */}
      <div className="flex min-h-[20rem] flex-col">
        <div
          ref={panelRef}
          role="group"
          aria-labelledby={headingId}
          tabIndex={-1}
          className="flex-1 outline-none"
        >
          {/* Screen-reader step heading (visual title lives in the rail). */}
          <h3 id={headingId} className="sr-only">
            {step.title} — step {current + 1} of {steps.length}
          </h3>
          {step.content}
        </div>
        <div className="mt-8 flex items-center justify-between border-t pt-5">
          <div>
            {onCancel && (
              <Button variant="ghost" onClick={requestCancel} disabled={busy}>
                Cancel
              </Button>
            )}
          </div>
          <div className="flex items-center gap-2">
            {current > 0 && (
              <Button
                variant="outline"
                onClick={() => onStepChange(current - 1)}
                disabled={busy}
              >
                Back
              </Button>
            )}
            {isLast ? (
              <Button onClick={onFinish} disabled={!canProceed || busy}>
                {busy ? "Working…" : finishLabel}
              </Button>
            ) : (
              <Button
                onClick={() => onStepChange(current + 1)}
                disabled={!canProceed || busy}
              >
                {busy ? "Working…" : nextLabel}
              </Button>
            )}
          </div>
        </div>
      </div>

      {/* Dirty-state discard guard. */}
      <ConfirmDialog
        open={confirmDiscard}
        onCancel={() => setConfirmDiscard(false)}
        onConfirm={() => {
          setConfirmDiscard(false);
          onCancel?.();
        }}
        title="Discard your changes?"
        description="You have unsaved input in this wizard. Leaving now discards it."
        confirmLabel="Discard"
      />
    </div>
  );
}
