import * as React from "react";

import { cn } from "@/lib/utils";

// shadcn/ui Label — token-driven form label. Pairs with Input/Textarea/Select via
// htmlFor. Text color comes from the foreground token; no hardcoded colors.
const Label = React.forwardRef<
  HTMLLabelElement,
  React.ComponentProps<"label">
>(({ className, ...props }, ref) => (
  <label
    ref={ref}
    className={cn(
      "text-sm font-medium leading-none text-foreground peer-disabled:cursor-not-allowed peer-disabled:opacity-70",
      className,
    )}
    {...props}
  />
));
Label.displayName = "Label";

export { Label };
