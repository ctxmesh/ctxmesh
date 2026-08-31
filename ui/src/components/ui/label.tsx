import * as React from "react";

import { cn } from "@/lib/utils";

// shadcn/ui Label — the token-driven form label (M151 §5.3). Pairs with
// Input/Textarea/Select via htmlFor.
//
// Two forms:
//   variant="default" — `text-sm font-medium`, the ordinary stacked form label.
//   variant="key"     — the mono kv-key used in compact form grids, matching
//                       the kv-lists on detail pages so a form and a read-only
//                       record read as the same object.
//
// When the labelled control is a `peer` and disabled, the label drops to
// `text-ghost` — §2.3 sanctions `text-ghost` on controls for disabled only.
// Text color comes from tokens; no hardcoded colors.
type LabelProps = React.ComponentProps<"label"> & {
  variant?: "default" | "key";
};

const LABEL_VARIANTS: Record<NonNullable<LabelProps["variant"]>, string> = {
  default: "text-sm font-medium text-foreground",
  key: "font-mono text-2xs uppercase tracking-wide text-faint",
};

const Label = React.forwardRef<HTMLLabelElement, LabelProps>(
  ({ className, variant = "default", ...props }, ref) => (
    <label
      ref={ref}
      className={cn(
        "leading-none",
        LABEL_VARIANTS[variant],
        "peer-disabled:cursor-not-allowed peer-disabled:text-ghost",
        className,
      )}
      {...props}
    />
  ),
);
Label.displayName = "Label";

// FieldError — the message line under an invalid field (§5.3). Mono, so it
// reads as machine feedback rather than prose, and `role="alert"` so a screen
// reader announces it when it appears. The field itself carries `aria-invalid`
// (which turns its border destructive) and should point at this node with
// `aria-describedby`.
//
// Deviation from §5.3, recorded: the spec asks for a 12px line, but the §3.2
// type scale has no 12px step (xs = 11, sm = 13). `text-xs` is the mono meta
// register and is used here; inventing a 12px size would break the scale.
const FieldError = React.forwardRef<
  HTMLParagraphElement,
  React.ComponentProps<"p">
>(({ className, children, ...props }, ref) => {
  if (!children) return null;
  return (
    <p
      ref={ref}
      role="alert"
      className={cn("font-mono text-xs text-destructive", className)}
      {...props}
    >
      {children}
    </p>
  );
});
FieldError.displayName = "FieldError";

export { Label, FieldError };
