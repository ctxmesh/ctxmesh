import * as React from "react";

import { cn } from "@/lib/utils";

// shadcn/ui Card primitives — the base surface every panel is built on
// (M151 §5.4).
//
// Elevation is RULES, not shadows (§2.7): `--shadow-card` now resolves to
// `none`, so the card is defined by `border` on `bg-card` and the old
// `shadow-card` class has been dropped rather than left as a dead no-op. Do not
// reintroduce a `shadow-*` here — shadows survive only for genuinely floating
// layers (popovers, dialogs, toasts).
//
// Padding follows the §4.1 grid: 20px (`p-5`) is the panel/card internal step,
// replacing the old 24px. A panel *with* a header uses <PanelHeader>, which is
// the 16px/20px bordered header band from the mock's `.panel > header`.
const Card = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("rounded-lg border bg-card text-card-foreground", className)}
    {...props}
  />
));
Card.displayName = "Card";

const CardHeader = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("flex flex-col space-y-1.5 p-5", className)}
    {...props}
  />
));
CardHeader.displayName = "CardHeader";

// Serif, weight 500 — never 600. The family carries a 600 but it reads
// bold-mechanical and undoes the editorial voice (§5.4, §3).
const CardTitle = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("font-serif text-lg font-medium tracking-snug", className)}
    {...props}
  />
));
CardTitle.displayName = "CardTitle";

const CardDescription = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("text-sm text-muted-foreground", className)}
    {...props}
  />
));
CardDescription.displayName = "CardDescription";

// The panel-with-header band (§5.4): serif title on the left, mono meta/count
// pushed right, separated from the body by a rule. It marks itself with
// `data-panel-header` so the CardContent that follows restores its own top
// padding — without that, body text would sit flush against the rule.
type PanelHeaderProps = Omit<React.HTMLAttributes<HTMLDivElement>, "title"> & {
  title: React.ReactNode;
  /** Right-aligned mono meta — a count, a timestamp, a scope. */
  meta?: React.ReactNode;
};

const PanelHeader = React.forwardRef<HTMLDivElement, PanelHeaderProps>(
  ({ className, title, meta, children, ...props }, ref) => (
    <div
      ref={ref}
      data-panel-header=""
      className={cn(
        "flex items-center gap-3 border-b border-border px-5 py-4",
        className,
      )}
      {...props}
    >
      <CardTitle>{title}</CardTitle>
      {meta ? (
        <span className="ml-auto font-mono text-xs text-faint">{meta}</span>
      ) : null}
      {children ? (
        <div
          className={cn("flex items-center gap-2", meta ? undefined : "ml-auto")}
        >
          {children}
        </div>
      ) : null}
    </div>
  ),
);
PanelHeader.displayName = "PanelHeader";

const CardContent = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("p-5 pt-0 [[data-panel-header]+&]:pt-5", className)}
    {...props}
  />
));
CardContent.displayName = "CardContent";

const CardFooter = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("flex items-center p-5 pt-0", className)}
    {...props}
  />
));
CardFooter.displayName = "CardFooter";

export {
  Card,
  CardHeader,
  PanelHeader,
  CardFooter,
  CardTitle,
  CardDescription,
  CardContent,
};
