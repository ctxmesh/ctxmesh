import * as React from "react";

import { cn } from "@/lib/utils";

// Select — a token-driven native <select>. The config-builder uses it for
// bounded enums (executionModel, eval gate) where the CRD schema fixes the
// allowed values; a native select keeps the dependency surface small (no Radix
// Select) and M151 §5.3 keeps that decision. Same field surface as Input:
// `bg-card`, `border-input`, no shadow, `aria-invalid` drives the error border.
// No hardcoded colors.
const Select = React.forwardRef<
  HTMLSelectElement,
  React.ComponentProps<"select">
>(({ className, children, ...props }, ref) => (
  <select
    ref={ref}
    className={cn(
      "flex h-9 w-full rounded-md border border-input bg-card px-3 py-1 text-sm transition-colors",
      "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
      "aria-[invalid=true]:border-destructive",
      "disabled:cursor-not-allowed disabled:border-border disabled:opacity-50",
      className,
    )}
    {...props}
  >
    {children}
  </select>
));
Select.displayName = "Select";

export { Select };
