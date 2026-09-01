import * as React from "react";

import { cn } from "@/lib/utils";

// shadcn/ui Textarea — the multi-line field (the config-builder's prompt input).
// Same token surface as Input (M151 §5.3): `bg-card` because fields sit on the
// card plane, `border-input` because that border is the only thing that reads
// as a control against near-identical card/page grounds, and no shadow because
// elevation is rules (§2.7). Set `aria-invalid` for the error state; no
// hardcoded colors.
const Textarea = React.forwardRef<
  HTMLTextAreaElement,
  React.ComponentProps<"textarea">
>(({ className, ...props }, ref) => (
  <textarea
    ref={ref}
    className={cn(
      "flex min-h-20 w-full rounded-md border border-input bg-card px-3 py-2 text-sm transition-colors",
      "placeholder:text-ghost",
      "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
      "aria-[invalid=true]:border-destructive",
      "disabled:cursor-not-allowed disabled:border-border disabled:opacity-50",
      className,
    )}
    {...props}
  />
));
Textarea.displayName = "Textarea";

export { Textarea };
