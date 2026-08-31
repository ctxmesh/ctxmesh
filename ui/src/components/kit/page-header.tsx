import * as React from "react";
import { MoreHorizontal, type LucideIcon } from "lucide-react";
import { Link } from "react-router-dom";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Skeleton } from "./skeleton";

// PageHeader — the §4.3 page band, as ONE component, because 43 pages each
// re-laying-out their own header is what produced the measured baseline of
// 202/392 renders that do not fit. Almost all of that is header furniture that
// lays out at full width regardless of viewport. Fixing it here fixes it once.
//
// THE WRAP ORDER (§4.3) is the load-bearing part of this file. As space runs
// out the header must give way in exactly this order:
//
//   1. the actions wrap to their own line, right-aligned;
//   2. the mono meta wraps under the h1;
//   3. only then does the h1 itself give way — truncating (§4.5), never
//      wrapping to a third line.
//
// That order is NOT what a single flat `flex-wrap` row produces, so the row is
// two nested flex contexts and the h1 carries a max-width:
//
//   • OUTER row = [identity group, actions]. Flex line-breaking is greedy over
//     each item's HYPOTHETICAL size, so the only item that can be pushed to a
//     second line is the actions group — step 1, structurally guaranteed rather
//     than hoped for. `ml-auto` keeps it right-aligned on whichever line it
//     lands on.
//   • INNER identity group = [h1, status, meta]. The h1 is capped at
//     `max-w-[28rem]` (§4.5's header rule), so its hypothetical size can never
//     swallow the line: `title(≤28rem) + status` is placed first and `meta`
//     is the item that breaks to the next line — step 2.
//   • Only when even the capped h1 + status cannot share a line does the h1
//     flex-shrink below its cap and clip — step 3. `truncate`/`line-clamp-2`
//     set `overflow:hidden`, which is what lets a flex item shrink past its
//     min-content size at all.
//
// Without the cap the h1's max-content width wins the greedy line-break, the
// h1 clips first, and the specified order inverts — hence the cap is load-
// bearing, not decoration.
//
// ACTION COLLAPSE (§4.3): with more than two actions below `lg`, keep the
// primary and the destructive and fold the rest into a "⋯" menu. That decision
// needs to KNOW which action is which, which an opaque `ReactNode` cannot
// answer — so `actions` takes a structured list (Deviation from §5.17's
// `actions?: ReactNode`, recorded here with its reason). `actionsSlot` remains
// for the rare bespoke control; it is never collapsed. The collapse is pure CSS
// (`hidden lg:inline-flex` / `lg:hidden`) rather than a matchMedia hook: no
// resize listener, no hydration flash, and `display:none` keeps the copy that
// is not showing out of the accessibility tree too.

export interface PageHeaderCrumb {
  label: string;
  /** Router destination. Omit for the trailing (current) crumb. */
  to?: string;
}

export interface PageHeaderAction {
  /** Stable key; falls back to `label`. */
  id?: string;
  label: string;
  onClick?: () => void;
  /** Router destination — renders the action as a link-shaped button. */
  to?: string;
  icon?: LucideIcon;
  variant?: React.ComponentProps<typeof Button>["variant"];
  disabled?: boolean;
  /**
   * Marks THE primary action, the one that survives collapse. Defaults to the
   * first action whose variant is `default` (pine), else the first action —
   * a header never collapses down to nothing.
   */
  primary?: boolean;
}

export interface PageHeaderTab {
  id: string;
  label: string;
  current?: boolean;
}

export interface PageHeaderProps {
  breadcrumb?: PageHeaderCrumb[];
  title: string;
  /** Resource names render mono 22px/600; prose titles render serif 32px. */
  titleMono?: boolean;
  /** A status Tag. Status is never interactive (§2.3) — pass a chip, not a link. */
  status?: React.ReactNode;
  /** Mono meta line: `team-a · v7 · a4f2c1`. */
  meta?: string;
  lede?: React.ReactNode;
  /** Structured so the §4.3 collapse can tell primary/destructive from the rest. */
  actions?: PageHeaderAction[];
  /** Escape hatch for a control the action shape can't express. Never collapsed. */
  actionsSlot?: React.ReactNode;
  tabs?: PageHeaderTab[];
  onTabChange?: (id: string) => void;
  /** Skeleton chrome while the page's own data loads (§5.17). */
  loading?: boolean;
  className?: string;
}

// The collapse breakpoint is `lg` (1024). The classes below are written as
// LITERALS, never composed from a variable — Tailwind's scanner reads source
// text, so a templated `${bp}:hidden` compiles to nothing at all.

function actionKey(a: PageHeaderAction, i: number): string {
  return a.id ?? `${a.label}-${i}`;
}

/**
 * The indices that survive the collapse: the primary and the destructive (§4.3).
 *
 * "The primary" is read in three widening steps — an explicit `primary` flag,
 * else the first pine (`default`) action, else the first non-destructive one.
 * The last step matters: a header of three outline buttons plus a Stop has no
 * declared primary, and collapsing it to `Stop + ⋯` would leave the destructive
 * act as the only thing on screen. Below `lg` the header always shows the most
 * important safe action next to the dangerous one, never the dangerous one alone.
 */
function keptActionIndices(actions: PageHeaderAction[]): Set<number> {
  const destructive = actions.findIndex((a) => a.variant === "destructive");
  const primary = [
    actions.findIndex((a) => a.primary),
    actions.findIndex((a) => (a.variant ?? "default") === "default"),
    actions.findIndex((a) => a.variant !== "destructive"),
  ].find((i) => i >= 0);
  const kept = new Set<number>();
  if (primary !== undefined) kept.add(primary);
  if (destructive >= 0) kept.add(destructive);
  return kept;
}

function ActionButton({
  action,
  className,
}: {
  action: PageHeaderAction;
  className?: string;
}) {
  const Icon = action.icon;
  // size="sm" is the 32px control row (§4.2's 32px chrome, and the 32px "⋯"
  // button §4.3 names) with the label lifted back to 13px — an 11px page action
  // reads as meta, not as the thing to do.
  const classes = cn("text-sm", className);
  const body = (
    <>
      {Icon && <Icon className="h-4 w-4" />}
      {action.label}
    </>
  );
  if (action.to && !action.disabled) {
    return (
      <Button asChild variant={action.variant ?? "default"} size="sm" className={classes}>
        <Link to={action.to}>{body}</Link>
      </Button>
    );
  }
  return (
    <Button
      type="button"
      variant={action.variant ?? "default"}
      size="sm"
      className={classes}
      disabled={action.disabled}
      onClick={action.onClick}
    >
      {body}
    </Button>
  );
}

function OverflowMenu({
  items,
  className,
}: {
  items: PageHeaderAction[];
  className?: string;
}) {
  const [open, setOpen] = React.useState(false);
  const rootRef = React.useRef<HTMLDivElement>(null);
  const triggerRef = React.useRef<HTMLButtonElement>(null);
  const menuRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    if (!open) return;
    // Move focus into the menu so the keyboard path is not a dead end.
    menuRef.current?.querySelector<HTMLElement>("[role='menuitem']")?.focus();
    function onPointerDown(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== "Escape") return;
      setOpen(false);
      triggerRef.current?.focus();
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  const itemClass = (a: PageHeaderAction) =>
    cn(
      "flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-sm font-medium",
      "hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
      a.variant === "destructive" ? "text-destructive" : "text-secondary-foreground",
      a.disabled && "pointer-events-none text-ghost",
    );

  return (
    <div ref={rootRef} className={cn("relative", className)}>
      <Button
        ref={triggerRef}
        type="button"
        variant="outline"
        size="icon"
        // 32px icon button (§4.3).
        className="h-8 w-8"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="More actions"
        onClick={() => setOpen((o) => !o)}
      >
        <MoreHorizontal className="h-4 w-4" />
      </Button>
      {open && (
        <div
          ref={menuRef}
          role="menu"
          aria-label="More actions"
          // Elevation is drawn with rules, not shadows (§2.7) — the popover
          // separates from the band by its own hairline on the popover plane.
          className="absolute right-0 z-20 mt-1 min-w-[11rem] rounded-lg border border-border bg-popover p-1"
        >
          {items.map((a, i) =>
            a.to && !a.disabled ? (
              <Link
                key={actionKey(a, i)}
                role="menuitem"
                to={a.to}
                className={itemClass(a)}
                onClick={() => setOpen(false)}
              >
                {a.icon && <a.icon className="h-4 w-4" />}
                {a.label}
              </Link>
            ) : (
              <button
                key={actionKey(a, i)}
                type="button"
                role="menuitem"
                className={itemClass(a)}
                disabled={a.disabled}
                onClick={() => {
                  setOpen(false);
                  a.onClick?.();
                }}
              >
                {a.icon && <a.icon className="h-4 w-4" />}
                {a.label}
              </button>
            ),
          )}
        </div>
      )}
    </div>
  );
}

function Tabs({
  tabs,
  onTabChange,
}: {
  tabs: PageHeaderTab[];
  onTabChange?: (id: string) => void;
}) {
  const listRef = React.useRef<HTMLDivElement>(null);
  const currentIndex = Math.max(
    0,
    tabs.findIndex((t) => t.current),
  );

  // APG roving tabindex + automatic activation. Inactive tabs are tabIndex -1,
  // so arrow handling is not optional — without it they'd be unreachable.
  function onKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    const keys = ["ArrowRight", "ArrowLeft", "Home", "End"];
    if (!keys.includes(e.key)) return;
    e.preventDefault();
    let next = currentIndex;
    if (e.key === "ArrowRight") next = (currentIndex + 1) % tabs.length;
    else if (e.key === "ArrowLeft") next = (currentIndex - 1 + tabs.length) % tabs.length;
    else if (e.key === "Home") next = 0;
    else next = tabs.length - 1;
    onTabChange?.(tabs[next].id);
    const els = listRef.current?.querySelectorAll<HTMLElement>("[role='tab']");
    els?.[next]?.focus();
  }

  return (
    // Own-container scrolling (§4.6): the tab strip scrolls, the page never does.
    <div className="mt-4 overflow-x-auto">
      <div
        ref={listRef}
        role="tablist"
        className="flex min-w-max items-end gap-1"
        onKeyDown={onKeyDown}
      >
        {tabs.map((t, i) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            aria-selected={!!t.current}
            tabIndex={i === currentIndex ? 0 : -1}
            onClick={() => onTabChange?.(t.id)}
            className={cn(
              "whitespace-nowrap border-b-2 px-4 py-2 text-sm font-medium transition-colors",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              t.current
                ? "border-primary text-primary"
                : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>
    </div>
  );
}

export function PageHeader({
  breadcrumb,
  title,
  titleMono,
  status,
  meta,
  lede,
  actions,
  actionsSlot,
  tabs,
  onTabChange,
  loading,
  className,
}: PageHeaderProps) {
  const list = actions ?? [];
  const collapsible = list.length > 2;
  // Computed on render rather than memoised: a memo would need `actions` to be
  // referentially stable, which a page building its action list inline cannot
  // promise — and the work is a handful of findIndex calls over ≤5 items.
  const kept = collapsible ? keptActionIndices(list) : null;
  const overflow = kept ? list.filter((_, i) => !kept.has(i)) : [];
  const hasActions = list.length > 0 || actionsSlot != null;

  return (
    <header
      className={cn(
        "border-b border-border bg-card px-6 pt-5",
        tabs && tabs.length > 0 ? "pb-0" : "pb-5",
        className,
      )}
    >
      {breadcrumb && breadcrumb.length > 0 && (
        <nav aria-label="Breadcrumb" className="mb-2">
          <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-1 font-mono text-xs text-faint">
            {breadcrumb.map((c, i) => {
              const last = i === breadcrumb.length - 1;
              return (
                <li key={`${c.label}-${i}`} className="flex items-center gap-x-1.5">
                  {i > 0 && (
                    // The separator is pure decoration — ghost, and never read out.
                    <span aria-hidden="true" className="text-ghost">
                      /
                    </span>
                  )}
                  {c.to && !last ? (
                    <Link to={c.to} className="text-primary hover:underline">
                      {c.label}
                    </Link>
                  ) : (
                    <span aria-current={last ? "page" : undefined}>{c.label}</span>
                  )}
                </li>
              );
            })}
          </ol>
        </nav>
      )}

      {loading ? (
        // §5.17 skeleton: a 32×240 title bar + one lede line. One busy region,
        // bars marked decorative, so a screen reader hears "Loading" once.
        <div role="status" aria-busy="true" aria-label="Loading">
          <Skeleton decorative className="h-8 w-60" />
          <Skeleton decorative className="mt-3 h-4 w-96 max-w-full" />
        </div>
      ) : (
        <>
          {/* OUTER row — the only item that can break to a second line is the
              actions group (§4.3 step 1). */}
          <div className="flex flex-wrap items-end gap-x-3 gap-y-2">
            {/* INNER identity group — h1 / status / meta (§4.3 steps 2 and 3). */}
            <div className="flex min-w-0 flex-wrap items-end gap-x-3 gap-y-1">
              <h1
                // The full string is always recoverable even when the visible
                // one is clipped (§4.5).
                title={title}
                className={cn(
                  "max-w-[28rem]",
                  titleMono
                    ? // Resource name: one line, end-ellipsis. A 63-char K8s
                      // name clips; it never wraps and never break-alls.
                      "truncate font-mono text-xl font-semibold tracking-snug"
                    : // Prose title: serif display, weight 500 (600 in this
                      // family reads bold-mechanical), clamped so §4.3's
                      // "never a third line" holds for long titles too.
                      "line-clamp-2 font-serif text-3xl font-medium",
                )}
              >
                {title}
              </h1>
              {status}
              {meta && <p className="font-mono text-xs text-faint">{meta}</p>}
            </div>

            {hasActions && (
              <div className="ml-auto flex shrink-0 items-center gap-2">
                {list.map((a, i) => (
                  <ActionButton
                    key={actionKey(a, i)}
                    action={a}
                    // Folded actions are display:none below lg — out of the
                    // layout AND out of the accessibility tree, so the "⋯"
                    // menu is not a duplicate announcement.
                    className={kept && !kept.has(i) ? "hidden lg:inline-flex" : undefined}
                  />
                ))}
                {overflow.length > 0 && (
                  <OverflowMenu items={overflow} className="lg:hidden" />
                )}
                {actionsSlot}
              </div>
            )}
          </div>

          {lede && (
            <p className="mt-2 max-w-[64ch] text-md text-muted-foreground">{lede}</p>
          )}
        </>
      )}

      {tabs && tabs.length > 0 && <Tabs tabs={tabs} onTabChange={onTabChange} />}
    </header>
  );
}
