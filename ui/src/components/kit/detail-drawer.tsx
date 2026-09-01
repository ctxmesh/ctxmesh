import * as React from "react";
import { X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useFocusTrap } from "@/components/kit/use-focus-trap";

// DetailDrawer — click-through detail WITHOUT losing list context (kit, m13.1
// → real m13.4; spec §5). A right-side panel that slides over the list: click a
// table/topology row → the drawer opens with that item's detail; close → the
// list is exactly where you left it. Used by the agents list, topology node
// click, tool catalog.
//
// Controlled `open` + `onClose`. Closing paths: the X button, a backdrop click,
// and Esc. Productionized in m13.4: a real focus trap (Tab cycles inside the
// panel, focus returns to the opener on close, Esc handled once via the shared
// useFocusTrap), a labelled dialog (aria-labelledby → the title), sizes, a
// status slot, and a footer actions slot (RBAC-gated by the parent).
//
// Editorial restyle (M151 §5.11): the panel is the ONE surface that legitimately
// floats, so it is the one that keeps a shadow (`shadow-overlay`) — on-page
// elevation is rules, not shadows (§2.7), and `--shadow-card` is now `none`.
// The title carries the two-face rule (§3.1): a drawer almost always names a
// resource, so the title is MONO 600 by default; prose-titled drawers pass
// `mono={false}` and get the serif display face instead.

export interface DetailDrawerProps {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  /** Right-of-title slot for a status badge / phase chip. */
  status?: React.ReactNode;
  /** Sticky footer — primary/secondary actions (omit for viewers). */
  footer?: React.ReactNode;
  /**
   * The title names a RESOURCE (mono 600) rather than reading as prose (serif).
   * Defaults to true — drawers almost always front a K8s object, whose name is
   * a machine string and belongs in the mono face (§3.1, §5.11).
   */
  mono?: boolean;
  /** Drawer width preset. */
  size?: "sm" | "md" | "lg";
  children?: React.ReactNode;
}

const SIZES: Record<NonNullable<DetailDrawerProps["size"]>, string> = {
  sm: "sm:w-[24rem]",
  md: "sm:w-[32rem]",
  lg: "sm:w-[44rem]",
};

export function DetailDrawer({
  open,
  onClose,
  title,
  subtitle,
  status,
  footer,
  mono = true,
  size = "md",
  children,
}: DetailDrawerProps) {
  const titleId = React.useId();
  // The shared trap owns focus-in / focus-return / Tab-cycle / Esc.
  const panelRef = useFocusTrap<HTMLElement>({ active: open, onEscape: onClose });

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-40">
      {/* Backdrop — dims the list but keeps it visible behind the panel. */}
      <div
        className="absolute inset-0 bg-foreground/30 backdrop-blur-[1px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <aside
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className={cn(
          "absolute right-0 top-0 flex h-full w-full max-w-full flex-col border-l bg-card shadow-overlay outline-none",
          SIZES[size],
        )}
      >
        <header className="flex items-start justify-between gap-4 border-b border-border p-6">
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-2">
              <h2
                id={titleId}
                className={cn(
                  "min-w-0 truncate text-lg tracking-snug",
                  // A resource name is a machine string: mono 600. Prose titles
                  // take the serif display face, which never exceeds 500 (§3.2).
                  mono
                    ? "font-mono font-semibold"
                    : "font-serif font-medium",
                )}
              >
                {title}
              </h2>
              {status}
            </div>
            {subtitle && (
              <p className="mt-1 truncate font-mono text-xs text-faint">
                {subtitle}
              </p>
            )}
          </div>
          <Button
            variant="ghost"
            size="icon"
            onClick={onClose}
            aria-label="Close panel"
          >
            <X className="h-4 w-4" />
          </Button>
        </header>

        <div className="flex-1 overflow-y-auto px-6 py-5">{children}</div>

        {footer && (
          <footer className="flex flex-wrap items-center justify-end gap-2 border-t border-border bg-surface-2/40 px-6 py-3">
            {footer}
          </footer>
        )}
      </aside>
    </div>
  );
}
