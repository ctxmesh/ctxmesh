import * as React from "react";
import { Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Wizard — the multi-step primitive that kills the M12 giant form (kit, m13.1;
// spec §5). Every creation flow (connect-provider, add-MCP, create-agent) is a
// Wizard: a labelled step rail with progress, a body slot per step, and a
// footer with Back / Next / Finish. The LAST step is conventionally a review.
//
// Controlled: the parent owns `current` and validation (`canProceed`) so it can
// gate a step until inputs are valid, run async work on Next (probe a provider,
// generate a config), and render the review from collected state. The Wizard
// only renders chrome + the active step's `content`.

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
  className,
}: WizardProps) {
  const isLast = current === steps.length - 1;
  const step = steps[current];

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
                disabled={i > current}
                onClick={() => i < current && onStepChange(i)}
                className={cn(
                  "flex w-full items-start gap-3 rounded-md px-3 py-2 text-left transition-colors",
                  active && "bg-accent",
                  !active && done && "hover:bg-surface-2",
                  i > current && "opacity-50",
                )}
              >
                <span
                  className={cn(
                    "mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs font-semibold",
                    done && "bg-success text-success-foreground",
                    active && "bg-primary text-primary-foreground",
                    !done && !active && "border bg-card text-muted-foreground",
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
                    <span className="block text-xs text-muted-foreground">
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
        <div className="mb-1 flex items-center justify-between text-xs text-muted-foreground">
          <span>
            Step {current + 1} of {steps.length}
          </span>
          <span>{step.title}</span>
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-surface-2">
          <div
            className="h-full rounded-full bg-primary transition-all"
            style={{ width: `${((current + 1) / steps.length) * 100}%` }}
          />
        </div>
      </div>

      {/* Body + footer. */}
      <div className="flex min-h-[20rem] flex-col">
        <div className="flex-1">{step.content}</div>
        <div className="mt-8 flex items-center justify-between border-t pt-5">
          <div>
            {onCancel && (
              <Button variant="ghost" onClick={onCancel} disabled={busy}>
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
    </div>
  );
}
