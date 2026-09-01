import * as React from "react";

import { cn } from "@/lib/utils";

// shadcn/ui Input — the token-driven text field (M151 §5.3).
//
// Three things here are deliberate and must not be "simplified" back:
//   1. `bg-card`, not `bg-background`. Fields sit on the card plane: on the
//      paper page a white field reads as "fillable", and in dark, card lifts
//      above paper the same way.
//   2. `border-input`, not `border-border`. The card and page grounds differ
//      by only ~1.03:1, so the field's border is the ONLY thing identifying it
//      as a control — `--input` is therefore held to WCAG 1.4.11 3:1 while
//      decorative rules stay hairlines.
//   3. No `shadow-sm`. Elevation is rules, not shadows (§2.7).
//
// Error state (§5.3): set `aria-invalid` on the field — the border turns
// destructive on its own. Pair it with <FieldError> from ui/label.tsx for the
// message line. No hardcoded colors; every surface composes this rather than
// styling <input> directly.
const Input = React.forwardRef<HTMLInputElement, React.ComponentProps<"input">>(
  ({ className, type, ...props }, ref) => (
    <input
      type={type}
      ref={ref}
      className={cn(
        "flex h-9 w-full rounded-md border border-input bg-card px-3 py-1 text-sm transition-colors",
        "placeholder:text-ghost",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
        "aria-[invalid=true]:border-destructive",
        "disabled:cursor-not-allowed disabled:border-border disabled:opacity-50",
        className,
      )}
      {...props}
    />
  ),
);
Input.displayName = "Input";

export { Input };
