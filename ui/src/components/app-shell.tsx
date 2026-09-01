import * as React from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
  AlertTriangle,
  ChevronRight,
  FlaskConical,
  LogOut,
  Menu,
  Monitor,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Sun,
  X,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { StopControl, useFocusTrap, useToast } from "@/components/kit";
import type { StopRequest, StopScopeOption } from "@/components/kit";
import { api, type ActiveStop, type StopScopeRequest } from "@/lib/api";
import { logout } from "@/lib/session";
import { useSession } from "@/lib/use-session";
import { useDevMode } from "@/lib/dev-mode";
import {
  buildCrumbs,
  NAV_SECTIONS,
  type NavCountTone,
  type NavItem,
} from "@/lib/nav";
import { CapabilitiesProvider, useCapabilities } from "@/lib/capabilities";
import { NamespaceProvider, useNamespace } from "@/lib/namespace";
import {
  nextThemePreference,
  ThemeProvider,
  THEME_LABEL,
  useTheme,
} from "@/lib/theme";
import { ShellCommandPalette } from "@/components/command-palette-shell";
import { cn } from "@/lib/utils";

// AppShell — the frame every one of the console's surfaces renders inside
// (M151 §4.2). It is the highest-leverage file in the redesign: a measured
// baseline found 202 of 392 renders overflowing, 184 of them at ≤1024px, and the
// frame caused nearly all of that. Two defects did it:
//
//   1. `grid-cols-[16rem_1fr]` at EVERY width — a 256px sidebar took its full
//      share at 360px as readily as at 1440px.
//   2. NO `min-w-0` on the content column. A grid/flex child defaults to
//      `min-width:auto`, i.e. its own min-content width, so one wide table set
//      the width of the entire document and the whole page scrolled sideways.
//
// Both are fixed here, and the fix is structural rather than cosmetic: the
// content column is shrinkable BY CONSTRUCTION (`minmax(0,1fr)` in the template
// AND `min-w-0` on the column itself), so a page can no longer widen the
// document no matter what it renders. Wide artifacts scroll in their own
// containers (§4.6); the body never scrolls sideways.
//
// The breakpoint ladder (§4.2):
//
//   ≥1280  240px sidebar, full labels + counts, section eyebrows · 32px gutter
//   1024–1279  64px icon rail, labels in tooltips, counts as warn/crit dots,
//              expandable by the pin toggle at the rail's foot · 24px gutter
//   768–1023   no rail; the hamburger opens a 288px focus-trapped drawer · 24px
//   <768       same drawer, and the workspace switcher + identity move into its
//              footer · 16px gutter
//
// The top bar is 48px at every width and carries, right to left: the frame stop
// control (§5.23 — present on every page, the ONE element that never collapses),
// sign-out, the theme control, identity, the workspace switcher, and the ⌘K chip.
//
// RBAC-AWARE CHROME (§3, DISPLAY-ONLY per ADR 0011): the shell wraps its content
// in the NamespaceProvider + CapabilitiesProvider so every surface can read the
// selected namespace and gate affordances. Write-only nav items are HIDDEN when
// the caller lacks the right; a probe failure shows an honest banner and leaves
// affordances VISIBLE (never a silently all-disabled console).

// How often the frame re-reads the two live nav counts. Slow on purpose: these
// are ambient signals, not a monitor, and they ride on every page in the app.
const COUNT_REFRESH_MS = 60_000;

// ─────────────────────────────────────────────────────────────────────────────
// Breadcrumbs — the trail that replaced the hard-coded "Control plane" <h1>
// (the trail itself is resolved in lib/nav: it is an IA question, not a layout
// one, and it belongs beside the list it reads).
// ─────────────────────────────────────────────────────────────────────────────

// The trail is mono `text-xs text-faint` with pine links (§4.2). Below `md` every
// crumb but the last is hidden: at 360px the current page's name is the only part
// that earns its width, and the trail must never be what pushes the bar wide.
function Breadcrumbs({ pathname }: { pathname: string }) {
  const crumbs = buildCrumbs(pathname);
  return (
    <nav
      aria-label="Breadcrumb"
      data-testid="breadcrumb"
      className="flex min-w-0 flex-1 items-center gap-1.5 font-mono text-xs text-faint"
    >
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1;
        return (
          <React.Fragment key={`${crumb.label}-${i}`}>
            {i > 0 && (
              <ChevronRight
                aria-hidden="true"
                className="hidden h-3 w-3 shrink-0 text-ghost md:block"
              />
            )}
            {crumb.to && !last ? (
              <Link
                to={crumb.to}
                className="hidden shrink-0 text-primary hover:underline md:inline"
              >
                {crumb.label}
              </Link>
            ) : (
              <span
                className={cn(
                  last
                    ? "truncate text-foreground"
                    : "hidden shrink-0 md:inline",
                )}
                aria-current={last ? "page" : undefined}
                title={crumb.label}
              >
                {crumb.label}
              </span>
            )}
          </React.Fragment>
        );
      })}
    </nav>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Identity, workspace, theme, sign-out — the top bar's right cluster
// ─────────────────────────────────────────────────────────────────────────────

// humanizeIdentity turns a raw auth principal into a friendly display name + honest identity type.
// A Kubernetes service account (`system:serviceaccount:<ns>:<name>`) shows its short name and
// "Service account" scoped to its namespace; an OIDC user shows their username + a real (non-system)
// group. This replaces the header's raw `system:serviceaccount:...` leak AND the misleading
// hardcoded "Member" chip: the console cannot know the caller's RBAC role (ClusterRoles are bound
// server-side, not carried in the token), so it states the identity TYPE truthfully rather than
// inventing a role. (A real access-tier chip would need the BFF to surface the caller's effective role.)
function humanizeIdentity(
  username: string,
  group: string | undefined,
): { name: string; type: string; context: string | undefined } {
  const sa = username.match(/^system:serviceaccount:([^:]+):(.+)$/);
  if (sa) return { name: sa[2], type: "Service account", context: sa[1] };
  const realGroup = group && !group.startsWith("system:") ? group : undefined;
  return { name: username, type: "User", context: realGroup };
}

/**
 * The identity chip — first to give way as the bar narrows (§4.2's collapse
 * order), but it degrades rather than disappearing: at 1024–1279 the initials
 * square remains with the full identity in its `title`, so "who am I signed in
 * as" is never a question the console refuses to answer. Below 1024 it moves
 * into the drawer footer, where `variant="full"` renders it again.
 */
function IdentityChip({
  username,
  group,
  variant = "bar",
}: {
  username: string;
  group: string | undefined;
  variant?: "bar" | "full";
}) {
  const { name, type, context } = humanizeIdentity(username, group);
  const initials = (name || "?").slice(0, 2).toUpperCase();
  const full = [name, type, context].filter(Boolean).join(" · ");
  return (
    <div
      className="flex min-w-0 items-center gap-2"
      title={variant === "bar" ? full : undefined}
    >
      <span
        aria-hidden="true"
        className="flex h-7 w-7 shrink-0 items-center justify-center rounded-sm bg-accent font-mono text-2xs font-semibold text-primary"
      >
        {initials}
      </span>
      <span
        className={cn(
          "min-w-0 leading-tight",
          variant === "bar" && "hidden xl:block",
        )}
      >
        <span
          className="block truncate text-xs font-medium"
          data-testid="whoami-username"
        >
          {name}
        </span>
        <span className="block truncate font-mono text-2xs uppercase text-faint">
          {context ? `${type} · ${context}` : type}
        </span>
      </span>
    </div>
  );
}

// WorkspaceSwitcher is the top bar's workspace/namespace selector (ADR 0068 §7).
// Each namespace is shown by its display name (the agents.ctxmesh.ai/display-name
// annotation) falling back to the raw namespace name when no label is set.
// "Workspace" is a UI-only friendly label for what is technically a namespace —
// no API route, DTO, or Go identifier uses that word (ADR 0068 §7 discipline).
// A can't-list-namespaces 403 is shown honestly; "" = all namespaces (default).
//
// It is the THIRD and last thing to collapse: below `md` it leaves the bar
// entirely and reappears in the drawer footer (`id` keeps the two instances from
// ever sharing a DOM id, since only one of them is ever mounted at a time).
function WorkspaceSwitcher({
  id = "ns-picker",
  className,
}: {
  id?: string;
  className?: string;
}) {
  const { namespace, setNamespace, list } = useNamespace();

  const forbidden = list.kind === "forbidden";
  const namespaces = list.kind === "ready" ? list.namespaces : [];

  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <label
        htmlFor={id}
        className="shrink-0 font-mono text-2xs uppercase text-faint"
      >
        Workspace
      </label>
      <Select
        id={id}
        aria-label="Workspace"
        value={namespace}
        onChange={(e) => setNamespace(e.target.value)}
        className="h-8 w-40 min-w-0 text-xs"
        title={
          forbidden
            ? "You can't list namespaces — working in all namespaces your RBAC permits."
            : undefined
        }
      >
        <option value="">All workspaces</option>
        {namespaces.map((ns) => (
          <option key={ns.name} value={ns.name}>
            {ns.displayName ?? ns.name}
          </option>
        ))}
      </Select>
    </div>
  );
}

const THEME_ICON = { system: Monitor, light: Sun, dark: Moon } as const;

/**
 * The theme control — the reason the designed dark palette is reachable at all.
 *
 * One 32px button cycling system → light → dark → system. A cycle rather than a
 * two-state switch because "follow my device" is a real, distinct choice and the
 * default; a binary toggle would silently pin whatever the OS said at first run.
 * The accessible name states BOTH what is set and what pressing it does, because
 * an icon that is a sun today and a moon tomorrow tells a screen-reader user
 * nothing on its own.
 */
function ThemeControl() {
  const { preference, setPreference } = useTheme();
  const Icon = THEME_ICON[preference];
  const next = nextThemePreference(preference);
  const label = `Theme: ${THEME_LABEL[preference]}. Switch to ${THEME_LABEL[next]}.`;
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      className="h-8 w-8 shrink-0 px-0"
      aria-label={label}
      title={label}
      data-testid="theme-control"
      data-theme-preference={preference}
      onClick={() => setPreference(next)}
    >
      <Icon aria-hidden="true" />
    </Button>
  );
}

function SignOutButton({
  onLogout,
  variant = "bar",
}: {
  onLogout: () => void;
  variant?: "bar" | "full";
}) {
  return (
    <Button
      variant="ghost"
      size="sm"
      onClick={onLogout}
      aria-label="Sign out"
      className={cn("shrink-0", variant === "bar" && "h-8")}
    >
      <LogOut aria-hidden="true" />
      <span className={cn(variant === "bar" && "hidden xl:inline")}>
        Sign out
      </span>
    </Button>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// The frame stop control (§4.2, §5.23)
// ─────────────────────────────────────────────────────────────────────────────

/**
 * The kill switch, on every page, last in the bar, and the one element that
 * never collapses at any width.
 *
 * The frame knows two scopes honestly: the selected workspace and the fleet. A
 * page that knows more — an agent's detail page, a team — passes its own scopes
 * to its own StopControl; the frame does not guess. Impact counts are omitted
 * rather than invented: GET /api/kills reports what IS stopped, not what WOULD
 * be, so the dialog says "not reported" instead of claiming a blast radius the
 * backend never told us (§5.23's rule, and the reason the kit refuses to render
 * an unreported count as 0).
 */
function FrameStopControl({ onStopped }: { onStopped: () => void }) {
  const { namespace, list } = useNamespace();
  const devMode = useDevMode();
  const { toast } = useToast();

  const workspaceLabel =
    (list.kind === "ready"
      ? list.namespaces.find((n) => n.name === namespace)?.displayName
      : undefined) ?? namespace;

  const scopes: StopScopeOption[] = [
    ...(namespace
      ? [{ kind: "workspace" as const, name: workspaceLabel || namespace }]
      : []),
    { kind: "fleet" as const },
  ];

  async function onStop(req: StopRequest) {
    const body: StopScopeRequest =
      req.scope === "workspace"
        ? { level: "namespace", namespace, reason: req.reason }
        : { level: "fleet", reason: req.reason };
    await api.stopScope(body);
    toast({
      variant: "info",
      title: req.scope === "fleet" ? "Everything is stopped" : `${namespace} is stopped`,
      description:
        "New runs are refused and queued work is held. Nothing is discarded.",
    });
    onStopped();
  }

  return (
    <StopControl
      scopes={scopes}
      onStop={onStop}
      disabled={devMode}
      disabledReason={
        devMode
          ? "Stopping is a cluster control — the local dev loop has no cluster to stop."
          : undefined
      }
    />
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Live nav counts
// ─────────────────────────────────────────────────────────────────────────────

export interface NavCounts {
  /** Runs waiting on a person in the selected workspace. */
  approvals?: number;
  /** Active stops that reach the selected workspace. */
  stops?: number;
}

/** Stops that touch the selected scope: a fleet stop reaches everything. */
function stopsReaching(stops: ActiveStop[], namespace: string): number {
  if (!namespace) return stops.length;
  return stops.filter((s) => s.level === "fleet" || s.namespace === namespace)
    .length;
}

/**
 * The two live numbers the sidebar renders.
 *
 * Both are UNDEFINED until a backend answers, and they go back to undefined the
 * moment one stops answering — never 0. A `0` beside Approvals means "nobody is
 * waiting", and the console may only say that when it has been told.
 *
 * The approvals queue is namespace-scoped (the endpoint 400s without one), so
 * with "All workspaces" selected there is no count to show — an honest gap
 * rather than a cluster-wide number the endpoint cannot produce.
 */
function useNavCounts(): { counts: NavCounts; refresh: () => void } {
  const { namespace } = useNamespace();
  const { can } = useCapabilities();
  const devMode = useDevMode();
  const [counts, setCounts] = React.useState<NavCounts>({});
  const [nonce, setNonce] = React.useState(0);
  const canApprovals = can("workflows", "list");

  React.useEffect(() => {
    // In dev mode (`ctxmesh dev --ui`, ADR 0021) these are cluster surfaces that
    // 501 by design. The dev banner already explains that; polling them would
    // only fill the console with failed requests.
    if (devMode) {
      setCounts({});
      return;
    }
    const controller = new AbortController();
    let alive = true;

    const load = () => {
      if (namespace && canApprovals) {
        api
          .listApprovals(namespace, controller.signal)
          .then((items) => {
            if (!alive || !Array.isArray(items)) return;
            setCounts((c) => ({ ...c, approvals: items.length }));
          })
          .catch(() => {
            if (alive) setCounts((c) => ({ ...c, approvals: undefined }));
          });
      } else {
        setCounts((c) => ({ ...c, approvals: undefined }));
      }
      api
        .listStops(controller.signal)
        .then((stops) => {
          if (!alive) return;
          setCounts((c) => ({ ...c, stops: stopsReaching(stops, namespace) }));
        })
        .catch(() => {
          if (alive) setCounts((c) => ({ ...c, stops: undefined }));
        });
    };

    load();
    const timer = window.setInterval(load, COUNT_REFRESH_MS);
    return () => {
      alive = false;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [namespace, canApprovals, devMode, nonce]);

  const refresh = React.useCallback(() => setNonce((n) => n + 1), []);
  return { counts, refresh };
}

// ─────────────────────────────────────────────────────────────────────────────
// The nav itself
// ─────────────────────────────────────────────────────────────────────────────

const COUNT_TONE_TEXT: Record<NavCountTone, string> = {
  quiet: "text-faint",
  waiting: "text-warning font-semibold",
  stopped: "text-destructive font-semibold",
};

const COUNT_TONE_DOT: Record<NavCountTone, string> = {
  quiet: "bg-faint",
  waiting: "bg-warning",
  stopped: "bg-destructive",
};

const COUNT_TONE_WORDS: Record<NavCountTone, string> = {
  quiet: "",
  waiting: "waiting on a person",
  stopped: "in force",
};

/**
 * A count beside a nav label: right-aligned, mono, tabular — faint when it is
 * just a magnitude, warn when things are waiting on a person, crit when work is
 * stopped (§4.2). A KNOWN zero renders as `0` in ghost (§4.5: zero and unknown
 * must not share a glyph); an unknown renders nothing at all, because a nav
 * badge that says 0 when nobody asked the backend is exactly the cosmetic
 * authority this system forbids.
 */
function NavCountValue({
  value,
  tone,
  className,
}: {
  value: number | undefined;
  tone: NavCountTone;
  className?: string;
}) {
  if (value === undefined) return null;
  const words = COUNT_TONE_WORDS[tone];
  const title = (value === 0 ? `None ${words}` : `${value} ${words}`).trim();
  return (
    <span
      className={cn(
        "ml-auto shrink-0 font-mono text-xs tabular-nums",
        value === 0 ? "text-ghost" : COUNT_TONE_TEXT[tone],
        className,
      )}
      title={title}
    >
      {value.toLocaleString()}
      <span className="sr-only"> {words}</span>
    </span>
  );
}

/**
 * The 64px rail has no room for a number, so a non-zero warn/crit count becomes
 * a 6px dot (§4.2) — and a zero shows nothing, because there is no such thing as
 * a dot that means "none". The number itself stays reachable in the item's
 * tooltip, so the rail loses the glance, never the fact.
 */
function NavCountDot({
  value,
  tone,
}: {
  value: number | undefined;
  tone: NavCountTone;
}) {
  if (value === undefined || value === 0 || tone === "quiet") return null;
  return (
    <span
      aria-hidden="true"
      className={cn(
        "absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full xl:hidden",
        COUNT_TONE_DOT[tone],
      )}
    />
  );
}

// NavItemLink routes to a re-housed surface (item.route) or, for a not-yet-built
// destination, to its milestone placeholder route (/soon/<id>). The distinction
// keeps the full approved IA walkable without pulling later features forward.
//
// Active state (§4.2): pine text at 600, an `accent` ground, and a 2px pine bar
// on the leading edge. Pine is the interactive colour and never a status, so an
// active item reads as "you are here", never as "this is healthy".
function NavItemLink({
  item,
  expanded,
  count,
}: {
  item: NavItem;
  expanded: boolean;
  count: number | undefined;
}) {
  const to = item.route ?? `/soon/${item.id}`;
  const Icon = item.icon;
  const end = item.route === "/";
  const tone = item.count?.tone ?? "quiet";
  // In the rail the label lives in the tooltip, and a pending count belongs there
  // too — a bare dot says "something", and something is not a number.
  const railTitle =
    count === undefined || count === 0
      ? item.label
      : `${item.label} — ${count} ${COUNT_TONE_WORDS[tone]}`.trim();

  return (
    <NavLink
      to={to}
      end={end}
      title={expanded ? undefined : railTitle}
      className={({ isActive }) =>
        cn(
          "group relative mb-0.5 flex items-center gap-3 rounded-md py-2 text-sm transition-colors",
          expanded ? "px-3" : "justify-center px-2 xl:justify-start xl:px-3",
          isActive
            ? "bg-accent font-semibold text-primary"
            : "text-muted-foreground hover:bg-surface-2 hover:text-foreground",
        )
      }
    >
      {({ isActive }) => (
        <>
          {isActive && (
            <span
              aria-hidden="true"
              className="absolute inset-y-0 left-0 w-0.5 rounded-sm bg-primary"
            />
          )}
          <Icon aria-hidden="true" className="h-4 w-4 shrink-0" />
          <span className={cn("truncate", !expanded && "hidden xl:inline")}>
            {item.label}
          </span>
          {item.count && (
            <>
              <NavCountValue
                value={count}
                tone={tone}
                className={cn(!expanded && "hidden xl:inline")}
              />
              {!expanded && <NavCountDot value={count} tone={tone} />}
            </>
          )}
        </>
      )}
    </NavLink>
  );
}

// NavSections renders the grouped IA. Write-only items (config-builder,
// Playground, the operator-only queues) are HIDDEN when the caller lacks the
// gating right (RBAC-aware chrome, §3) — a viewer sees a read-only nav by
// construction.
function NavSections({
  expanded,
  counts,
}: {
  expanded: boolean;
  counts: NavCounts;
}) {
  const { can } = useCapabilities();

  const visible = (it: NavItem): boolean =>
    !it.requiresCapability ||
    can(it.requiresCapability.resource, it.requiresCapability.verb);

  return (
    <>
      {NAV_SECTIONS.map((section, sectionIndex) => {
        const items = section.items.filter(visible);
        if (items.length === 0) return null;
        // A one-item section whose item is its own name (Home) needs no eyebrow —
        // it would print the word twice, once in each register.
        const eyebrow =
          items.length === 1 && items[0].label === section.heading
            ? null
            : section.heading;
        return (
          <div key={section.heading} className="mb-3">
            {eyebrow && (
              <p
                className={cn(
                  "px-3 pb-1 pt-2 font-mono text-2xs uppercase text-faint",
                  !expanded && "hidden xl:block",
                )}
              >
                {eyebrow}
              </p>
            )}
            {/* In the rail the eyebrow has no room, so the grouping is carried by
                a hairline instead of a word — the sections still read as sections. */}
            {!expanded && sectionIndex > 0 && (
              <div
                aria-hidden="true"
                className="mx-2 mb-2 mt-1 border-t border-border-soft xl:hidden"
              />
            )}
            {items.map((it) => (
              <NavItemLink
                key={it.id}
                item={it}
                expanded={expanded}
                count={it.count ? counts[it.count.source] : undefined}
              />
            ))}
          </div>
        );
      })}
    </>
  );
}

// The wordmark: serif, `ctx` in ink and `mesh` in italic pine (§4.2). In the
// 64px rail only `ctx` survives — enough to know whose console this is, and the
// one place the brand is allowed to take space it hasn't earned.
function Wordmark({ expanded }: { expanded: boolean }) {
  return (
    <Link
      to="/"
      className="flex min-w-0 items-center font-serif text-xl tracking-snug text-foreground"
    >
      ctx
      <span className={cn("italic text-primary", !expanded && "hidden xl:inline")}>
        mesh
      </span>
    </Link>
  );
}

function Sidebar({
  expanded,
  onToggleRail,
  counts,
}: {
  expanded: boolean;
  onToggleRail: () => void;
  counts: NavCounts;
}) {
  return (
    <aside className="sticky top-0 hidden h-screen min-w-0 flex-col border-r bg-card lg:flex">
      {/* 20px of padding is right beside a 240px sidebar and wrong inside a 64px
          rail: it leaves 24px of content box, and the serif "ctx" needs 29. The
          rail centres the mark in 8px gutters instead — measured, not guessed
          (the visual sweep flagged this as a 5px self-overflow at 1024). */}
      <div
        className={cn(
          "flex h-12 shrink-0 items-center border-b",
          expanded ? "px-5" : "justify-center px-2 xl:justify-start xl:px-5",
        )}
      >
        <Wordmark expanded={expanded} />
      </div>
      <nav
        aria-label="Sections"
        className="min-h-0 flex-1 overflow-y-auto px-2 py-3"
      >
        <NavSections expanded={expanded} counts={counts} />
      </nav>
      {/* The pin toggle exists only in the rail range: at ≥1280 the sidebar is
          always full, so a control offering to expand it would be a lie. */}
      <div className="shrink-0 border-t p-2 xl:hidden">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onToggleRail}
          aria-pressed={expanded}
          aria-label={expanded ? "Collapse the sidebar" : "Expand the sidebar"}
          title={expanded ? "Collapse the sidebar" : "Expand the sidebar"}
          className={cn("h-8 w-full", !expanded && "px-0")}
        >
          {expanded ? (
            <PanelLeftClose aria-hidden="true" />
          ) : (
            <PanelLeftOpen aria-hidden="true" />
          )}
          <span className={cn(!expanded && "hidden")}>Collapse</span>
        </Button>
      </div>
    </aside>
  );
}

/**
 * The overlay drawer below 1024 — the sidebar's replacement, not its absence
 * (the mocks specified nothing below 880px and simply dropped the nav).
 *
 * Focus-trapped through the kit's `useFocusTrap`, so Tab cycles inside it and
 * Escape closes it; it is mounted only while open, which is what keeps its
 * workspace switcher from ever being a second element with the same label as the
 * bar's. Below `md` its footer carries the switcher, identity and sign-out —
 * everything the bar gave up on the way down.
 */
function NavDrawer({
  onClose,
  counts,
  onLogout,
  session,
  devMode,
}: {
  onClose: () => void;
  counts: NavCounts;
  onLogout: () => void;
  session: ReturnType<typeof useSession>;
  devMode: boolean;
}) {
  const ref = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });
  return (
    <div className="fixed inset-0 z-40 lg:hidden" data-testid="nav-drawer">
      <div
        aria-hidden="true"
        onClick={onClose}
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
      />
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label="Navigation"
        tabIndex={-1}
        className="absolute inset-y-0 left-0 flex w-72 max-w-[85vw] flex-col border-r bg-card shadow-overlay"
      >
        <div className="flex h-12 shrink-0 items-center justify-between border-b px-5">
          <Wordmark expanded />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-8 w-8 px-0"
            aria-label="Close the navigation"
            onClick={onClose}
          >
            <X aria-hidden="true" />
          </Button>
        </div>
        <nav
          aria-label="Sections (menu)"
          className="min-h-0 flex-1 overflow-y-auto px-2 py-3"
        >
          <NavSections expanded counts={counts} />
        </nav>
        <div className="shrink-0 space-y-3 border-t p-3">
          <WorkspaceSwitcher id="drawer-ns-picker" className="md:hidden" />
          {session && (
            <IdentityChip
              username={session.user.username}
              group={session.user.groups[0]}
              variant="full"
            />
          )}
          {!devMode && <SignOutButton onLogout={onLogout} variant="full" />}
        </div>
      </div>
    </div>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Honest banners
// ─────────────────────────────────────────────────────────────────────────────

// CapabilityBanner is the honest-failure notice (§3): when the capability probe
// fails (500/network) we CANNOT know the caller's rights, so affordances stay
// VISIBLE (fail-open for DISPLAY) and this non-blocking banner explains that —
// never a silently all-disabled console. It also covers the 403-reprobe path
// (reprobe() re-raises the loading→error cycle).
function CapabilityBanner() {
  const { probeError } = useCapabilities();
  const devMode = useDevMode();
  // In dev mode the capability probe is a cluster surface → it 501s by design; the
  // DevModeBanner already explains that, so this would only add a redundant warning.
  if (devMode || !probeError) return null;
  return (
    <div
      role="status"
      data-testid="capability-banner"
      className="flex shrink-0 items-start gap-2.5 border-b border-warning/40 bg-warning-surface px-4 py-2.5 text-xs text-foreground md:px-6 xl:px-8"
    >
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
      <p>
        Couldn&apos;t determine your permissions — some actions may be shown
        optimistically or hidden. The cluster still enforces access, so a denied
        action will report a clear error.
      </p>
    </div>
  );
}

// DevModeBanner is the honest "you're on the local loop" notice (ADR 0021): under
// `ctxmesh dev --ui` the console runs against Docker Compose with NO cluster, so
// the fleet/providers/topology/RBAC surfaces are unavailable (served as calm 501s) and
// there is no login. This persistent banner names that plainly so a dev never reads the
// missing cluster surfaces as a broken console. Shown only when the BFF confirms devMode.
function DevModeBanner() {
  const devMode = useDevMode();
  if (!devMode) return null;
  return (
    <div
      role="status"
      data-testid="dev-mode-banner"
      className="flex shrink-0 items-start gap-2.5 border-b bg-accent px-4 py-2.5 text-xs text-foreground md:px-6 xl:px-8"
    >
      <FlaskConical className="mt-0.5 h-4 w-4 shrink-0 text-primary" />
      <p>
        <span className="font-semibold">Dev mode</span> — running against your
        local <code className="font-mono">ctxmesh dev</code> loop, no
        cluster. Define, config-preview, and run work here; fleet, providers,
        topology, and RBAC are cluster features and aren&apos;t available.
      </p>
    </div>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// The frame
// ─────────────────────────────────────────────────────────────────────────────

// ShellChrome is the layout — split out so it renders INSIDE the namespace +
// capability providers (it reads both).
function ShellChrome() {
  const session = useSession();
  const devMode = useDevMode();
  const navigate = useNavigate();
  const location = useLocation();
  const { toast } = useToast();
  const [drawerOpen, setDrawerOpen] = React.useState(false);
  const [railExpanded, setRailExpanded] = React.useState(false);
  const { counts, refresh } = useNavCounts();

  // A nav click inside the drawer navigates AND must dismiss it; leaving it open
  // over the page it just routed to is the classic mobile-nav bug.
  React.useEffect(() => {
    setDrawerOpen(false);
  }, [location.pathname]);

  const onLogout = React.useCallback(() => {
    logout();
    toast({ variant: "info", title: "Signed out" });
    navigate("/login", { replace: true });
  }, [navigate, toast]);

  return (
    <div className="min-h-screen bg-background text-foreground">
      {/*
        The fit fix, in one line. `minmax(0,1fr)` is the load-bearing half: a bare
        `1fr` is `minmax(auto,1fr)`, whose auto floor is the column's MIN-CONTENT
        width — so a wide table inside it would set the width of the grid, the
        grid would set the width of the body, and the whole document would scroll
        sideways. The column's own `min-w-0` says the same thing again to the
        flex context inside it. Below `lg` there is no sidebar column at all.
      */}
      <div
        className={cn(
          "grid",
          railExpanded
            ? "lg:grid-cols-[15rem_minmax(0,1fr)]"
            : "lg:grid-cols-[4rem_minmax(0,1fr)]",
          "xl:grid-cols-[15rem_minmax(0,1fr)]",
        )}
      >
        <Sidebar
          expanded={railExpanded}
          onToggleRail={() => setRailExpanded((v) => !v)}
          counts={counts}
        />

        <div className="flex min-h-screen min-w-0 flex-col">
          <header className="sticky top-0 z-30 flex h-12 shrink-0 items-center gap-3 border-b bg-background px-4 md:px-6 xl:px-8">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-8 w-8 shrink-0 px-0 lg:hidden"
              aria-label="Open the navigation"
              aria-expanded={drawerOpen}
              onClick={() => setDrawerOpen(true)}
            >
              <Menu aria-hidden="true" />
            </Button>

            <Breadcrumbs pathname={location.pathname} />

            <div className="flex shrink-0 items-center gap-2">
              {/* ⌘K command palette — the global navigator + the discoverable
                  header chip that opens it. Mounted here, inside the Namespace +
                  Capabilities providers, so it RBAC-filters exactly like the nav
                  and shares the shell's sign-out flow. The chip collapses with
                  the width; the ⌘K binding never does. */}
              <ShellCommandPalette onLogout={onLogout} />
              <WorkspaceSwitcher className="hidden md:flex" />
              {session && (
                <div className="hidden min-w-0 lg:flex">
                  <IdentityChip
                    username={session.user.username}
                    group={session.user.groups[0]}
                  />
                </div>
              )}
              <ThemeControl />
              {/* No session to sign out of in dev mode (no login wall, ADR 0021). */}
              {!devMode && (
                <span className="hidden lg:inline-flex">
                  <SignOutButton onLogout={onLogout} />
                </span>
              )}
              <FrameStopControl onStopped={refresh} />
            </div>
          </header>
          <DevModeBanner />
          <CapabilityBanner />
          <main className="min-w-0 flex-1 px-4 py-6 md:px-6 xl:px-8 xl:py-8">
            <Outlet />
          </main>
        </div>
      </div>

      {drawerOpen && (
        <NavDrawer
          onClose={() => setDrawerOpen(false)}
          counts={counts}
          onLogout={onLogout}
          session={session}
          devMode={devMode}
        />
      )}
    </div>
  );
}

export function AppShell() {
  return (
    <ThemeProvider>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <ShellChrome />
        </CapabilitiesProvider>
      </NamespaceProvider>
    </ThemeProvider>
  );
}
