import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

// shadcn/ui Button — a design-token primitive. All colors/radii come from the
// theme tokens (bg-primary, ring, etc.); no hardcoded values. Surfaces compose
// this rather than styling <button> directly.
//
// M151 §5.2: geometry is unchanged (h-9 / h-8 / h-10 / 9×9 icon) but rounded-md
// now resolves to 3px, not 12px (§2.6), and the label firms up to font-semibold
// — a hairline-and-serif language needs button text that holds its own against
// a 1px rule. Elevation is drawn with rules, not shadows (§2.7), so every
// `shadow` / `shadow-sm` is gone; only genuinely floating layers keep one.
//
// Colour follows §2.3: pine is the ONLY bold colour and it means "you can act
// here". Pine controls deepen to brand-2 on hover/press (in dark the token
// brightens instead — same class, different value); outlined controls sharpen
// their rule; ghost/secondary shift one plane. Crit is the single hue that may
// fill a control, and only for destructive acts.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-semibold transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default:
          "bg-primary text-primary-foreground hover:bg-brand-2 active:bg-brand-2 active:ring-2 active:ring-ring active:ring-offset-2",
        destructive:
          "bg-destructive text-destructive-foreground hover:bg-destructive/90",
        outline:
          "border border-border-strong bg-card text-secondary-foreground hover:bg-accent hover:text-accent-foreground",
        secondary: "bg-secondary text-secondary-foreground hover:bg-surface-3",
        ghost: "hover:bg-surface-2 hover:text-foreground",
        link: "text-primary underline-offset-2 hover:underline",
      },
      size: {
        default: "h-9 px-4 py-2",
        sm: "h-8 rounded-md px-3 text-xs",
        lg: "h-10 rounded-md px-8",
        icon: "h-9 w-9",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  },
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, asChild = false, ...props }, ref) => {
    const Comp = asChild ? Slot : "button";
    return (
      <Comp
        className={cn(buttonVariants({ variant, size, className }))}
        ref={ref}
        {...props}
      />
    );
  },
);
Button.displayName = "Button";

export { Button, buttonVariants };
