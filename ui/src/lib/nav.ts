import {
  Bell,
  BookOpen,
  Boxes,
  Building2,
  CheckSquare,
  Coins,
  Database,
  FlaskConical,
  GitBranch,
  GitFork,
  House,
  KeyRound,
  Library,
  MessagesSquare,
  Network,
  OctagonX,
  PlugZap,
  ScrollText,
  Share2,
  Shield,
  ShieldCheck,
  SlidersHorizontal,
  Tags,
  TestTube2,
  Users,
  Waypoints,
  Wrench,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

// nav.ts — the ONE source of truth for the console's information architecture
// (ui-foundation §6 re-housing). Both the REAL app shell (components/app-shell)
// and the DESIGN wireframes (design/console-chrome re-exports NAV_SECTIONS from
// here) read this list, so the approved IA and the shipped shell can never drift
// — approve the IA at the design gate and the shell follows by construction.
//
// This module is gallery-free (no design-only imports) so the production shell
// can consume it without pulling the review-only wireframes into the main chunk.
//
// Milestone tags mark which arc milestone SHIPS each surface. Only surfaces that
// exist TODAY carry a `route`; the rest render a PlaceholderPage ("arrives in
// M<n>") so the full IA is walkable without pulling later features forward.

// Milestone is duplicated from design/scaffold to keep this module independent of
// the design gallery (the wireframes' own tag type). It is display-only.
export type Milestone =
  | "M13"
  | "M14"
  | "M15"
  | "M16"
  | "M17"
  | "M18"
  | "M47"
  | "M63"
  | "M64"
  | "M66"
  | "M67"
  | "M68"
  | "M69"
  | "M70"
  | "M73"
  | "M74"
  | "M76"
  | "M112"
  | "M151";

// The golden CRD resources the console probes capabilities for — the plural
// names the BFF's SelfSubjectAccessReview uses (internal/bff/identity.go). A nav
// item or action gates on `capabilities.allowed[resource][verb]`.
export const RES_AGENTS = "agentdeployments";
export const RES_ROUTES = "modelroutes";
export const RES_SECRETS = "secretbindings";
// The CORE-group Secret, reported by the BFF as its own synthetic capability.
// Distinct from RES_SECRETS above, which is the SecretBinding CRD.
export const RES_CORE_SECRETS = "secrets";
export const RES_REGISTRIES = "agentregistries";
export const RES_MEMORY = "memorybindings";
export const RES_SCALING = "agentscalingpolicies";
export const RES_EVALSUITES = "evalsuites";
export const RES_PROMPTVERSIONS = "promptversions";
export const RES_TENANTS = "tenants";
// RES_AUDITLOGS gates the operator-only Audit surface (ADR 0056). It is a virtual
// resource: only the operator persona's ClusterRole grants `list auditlogs`, so the
// nav item hides for developer/viewer chrome (display-only; the API still enforces).
export const RES_AUDITLOGS = "auditlogs";
// RES_GUARDRAIL is the plural resource name for GuardrailPolicies (m66.10, ADR 0059).
export const RES_GUARDRAIL = "guardrailpolicies";
// RES_ALERTPOLICIES gates the Alerts feed (M70, ADR 0063 D2). The caller-scoped SSAR
// authorizes against `list alertpolicies` — the same resource the CRD path enforced.
export const RES_ALERTPOLICIES = "alertpolicies";
// RES_KNOWLEDGEBASES gates the Knowledge Bases nav item (M99 C2): a persona that can't
// `list knowledgebases` (e.g. developer) must not see a nav item that then 403s. The BFF
// probes it in the golden set; display-only, the API still enforces.
export const RES_KNOWLEDGEBASES = "knowledgebases";
// RES_LOGS gates the agent-detail **Logs** tab (M100 UI99-logs): a persona who can't
// `get pods/log` (the core-group subresource the live-log tail needs) must not see a tab that
// then 403s. It is a SYNTHETIC capability key the BFF probes as `get pods/log`; display-only,
// the API still enforces on the logs SSE endpoint. Verb is "get".
export const RES_LOGS = "logs";

/**
 * A live signal the shell may render as a count beside a nav item.
 *
 * A count is DATA, not decoration, so the IA declares which backend answers it
 * rather than letting the shell invent one. A source the install cannot answer
 * renders NOTHING — never a `0` (M151 §7.1: zero and unknown must not share a
 * glyph). `approvals` = GET /api/approvals (namespace-scoped, needs `list
 * workflows`); `stops` = GET /api/kills (the active scoped stops, ADR 0126).
 */
export type NavCountSource = "approvals" | "stops";

/**
 * What the number MEANS, which is what fixes its colour (M151 §2, §4.2):
 * `waiting` = items are sitting on a person (warn), `stopped` = work is halted
 * right now (crit), `quiet` = a plain magnitude (faint). Pine is never a status,
 * so a count is never rendered in the brand colour.
 */
export type NavCountTone = "quiet" | "waiting" | "stopped";

export interface NavCount {
  source: NavCountSource;
  tone: NavCountTone;
}

export interface NavItem {
  id: string;
  label: string;
  icon: LucideIcon;
  milestone: Milestone;
  /**
   * The router path this nav item routes to. Present only for surfaces that
   * exist in the current build; absent items render a PlaceholderPage keyed on
   * the milestone. `/` for the index (Home).
   */
  route?: string;
  /**
   * When set, this destination is capability-gated — hidden from any chrome whose
   * caller isn't `allowed[requiresCapability.resource][requiresCapability.verb]`
   * (display-only, ADR 0011). Read-open destinations omit it and are always shown.
   * The COMMON case is a write affordance hidden from a viewer (verb "create"/"update").
   * But a `list` (read) verb is also valid here as a deliberate OPERATOR-ONLY
   * *visibility* gate — e.g. Audit (`list auditlogs`, M63): a read-only page whose
   * data spans users/namespaces, so it's shown only to the operator persona, exactly
   * like MCP approvals. Renamed from `requiresWrite` (m76.5, H4) because the field
   * now also carries read-verb gates — the old name misled.
   */
  requiresCapability?: { resource: string; verb: string };
  /** A live backend count the shell renders right-aligned beside the label. */
  count?: NavCount;
}

export interface NavSection {
  heading: string;
  items: NavItem[];
}

// ── THE APPROVED IA (M151, spec §4.2) ───────────────────────────────────────
//
// Six sections — Home / Agents / Library / Govern / Activity / Admin — replacing
// the seven-section Overview / Build / Tools / Observe / Platform / Advanced
// arrangement, which had grown by accretion: "Build" mixed a provider
// connection with a chat playground, "Platform" held four unrelated policy CRDs,
// and "Advanced" was a shelf for anything object-shaped.
//
// The sections answer six different questions, and each destination belongs to
// exactly the one it answers:
//
//   Home      — what needs me right now?
//   Agents    — the things that run.
//   Library   — the reusable material they draw on (tools, knowledge, prompts,
//               workflows, templates). Nothing here runs by itself.
//   Govern    — who is blocked, what is enforced, what was recorded. DELIBERATELY
//               THE FATTEST SECTION: governance is the product's thesis, so it is
//               not a footnote under "Platform".
//   Activity  — what happened and what it cost.
//   Admin     — the install: credentials, routing, tenancy.
//
// Two placements are worth their reasons. REGISTRIES sits in Govern, not with
// the other CRDs: a registry is the allow-list of what an agent may pull, which
// is a control, not plumbing. PROVIDERS sits in Admin because it is credential
// handling; its first-run prominence is carried by the Home checklist
// (FIRST_RUN_CHECKLIST below), which is where m49.2's onboarding-order fix
// actually lives — the checklist links straight to /providers/connect.
export const NAV_SECTIONS: NavSection[] = [
  {
    heading: "Home",
    items: [
      {
        // The fleet sentence + the queues that need a person (spec §6.1 A11).
        // Route "/" is unchanged; only the name is (it was "Dashboard").
        id: "home",
        label: "Home",
        icon: House,
        milestone: "M13",
        route: "/",
      },
    ],
  },
  {
    heading: "Agents",
    items: [
      {
        id: "agents",
        label: "Agents",
        icon: Boxes,
        milestone: "M13",
        route: "/agents",
      },
      {
        // Agent Teams (M64, ADR 0057) — the orchestration rosters: a supervisor + summonable
        // sub-agents + a spawn budget. Read-open (the API is the RBAC gate).
        id: "teams",
        label: "Teams",
        icon: Waypoints,
        milestone: "M64",
        route: "/teams",
      },
      {
        // The Playground — running an agent is a create/invoke-shaped op; a
        // viewer's chrome hides it (they still get an honest 403 if they reach
        // it directly). Gated on create agentdeployments (the run path).
        id: "playground",
        label: "Playground",
        icon: FlaskConical,
        milestone: "M13",
        route: "/playground",
        requiresCapability: { resource: RES_AGENTS, verb: "create" },
      },
      // "New agent" is NOT a nav item (m25 S8) — it lives as the primary action
      // (top-right button) ON the Agents page, next to the list it creates into.
    ],
  },
  {
    // Library — the material agents draw on. Everything here is composed INTO an
    // agent; nothing here runs on its own. The old "Tools" section collapses in
    // here (m23.7's grouping survives, one level up).
    heading: "Library",
    items: [
      {
        // Template Gallery (m74.6) — agent templates (recipes ∪ published agents)
        // and the MCP catalog in two tabs. Read-open; Fork/Connect are gated
        // server-side by agent creation rights.
        id: "gallery",
        label: "Gallery",
        icon: Library,
        milestone: "M74",
        route: "/gallery",
      },
      {
        // MCP Servers LIST page (m25 S10) — every registered BYO MCP server, with
        // "Add MCP server" as an in-page CTA rather than a second nav item.
        id: "mcp-servers",
        label: "MCP servers",
        icon: Wrench,
        milestone: "M14",
        route: "/tools/mcp-servers",
      },
      {
        // Tool catalog (m17.10) — the merged curated + user-added + pending
        // catalog; the bind-time picker, not the discovery surface (m76.1).
        id: "tool-catalog",
        label: "Tool catalog",
        icon: BookOpen,
        milestone: "M17",
        route: "/tools/catalog",
      },
      {
        // KnowledgeBases (m68.13, ADR 0061) — managed RAG corpora. GATED on
        // `list knowledgebases` (M99 C2) so a persona that can't list them never
        // sees a nav item that 403s; the API still enforces.
        id: "knowledgebases",
        label: "Knowledge bases",
        icon: Database,
        milestone: "M68",
        route: "/knowledgebases",
        requiresCapability: { resource: RES_KNOWLEDGEBASES, verb: "list" },
      },
      {
        // PromptVersion list + side-by-side diff (m17.12). Read-open.
        id: "prompts",
        label: "Prompts",
        icon: GitBranch,
        milestone: "M17",
        route: "/prompts",
      },
      {
        // Workflows (m67.9, ADR 0060) — declarative graphs of agent invocations.
        // Read-open (RBAC gate at the API server, ADR 0011).
        id: "workflows",
        label: "Workflows",
        icon: GitFork,
        milestone: "M67",
        route: "/workflows",
      },
      {
        // The config-builder — the raw agent.yaml → CRD-apply surface. It is a
        // hand-authoring path, so it sits with the other material rather than in
        // the agent-creation flow ("New agent" is the ONE primary create entry,
        // m23.7 / audit B3). A WRITE surface, hidden from a viewer's nav.
        id: "config",
        label: "Config builder",
        icon: SlidersHorizontal,
        milestone: "M13",
        route: "/config",
        requiresCapability: { resource: RES_AGENTS, verb: "create" },
      },
    ],
  },
  {
    // Govern — the fattest section on purpose (M151 approved direction). Ordered
    // by urgency: the two human queues, then the kill switch, then the standing
    // controls, then the record.
    heading: "Govern",
    items: [
      {
        // Approvals (V5/V15) — the namespace-scoped queue of runs paused on
        // plan_approval OR a mid-run approval step. Gated on `list workflows`.
        // Its count is the console's "someone is waiting on you" signal.
        id: "approvals",
        label: "Approvals",
        icon: CheckSquare,
        milestone: "M112",
        route: "/approvals",
        requiresCapability: { resource: "workflows", verb: "list" },
        count: { source: "approvals", tone: "waiting" },
      },
      {
        // MCP approval queue (m17.9). Operator-only: gated on update agentregistries.
        id: "mcp-approvals",
        label: "MCP approvals",
        icon: ShieldCheck,
        milestone: "M17",
        route: "/tools/approvals",
        requiresCapability: { resource: RES_REGISTRIES, verb: "update" },
      },
      {
        // Stops (ADR 0126, spec §5.23/§6.2) — the scoped kill switch's landing
        // surface: what is stopped, why, by whom, and how to lift it. The frame's
        // StopControl creates them; this is where they are read. The PAGE shipped
        // in m151.7 (spec §6.2 gap 2), so the item now walks to the real surface
        // instead of /soon/stops — and the COUNT is live from GET /api/kills,
        // because "a scope is halted right now" is the one fact the frame must
        // not sit on.
        id: "stops",
        label: "Stops",
        icon: OctagonX,
        milestone: "M151",
        route: "/stops",
        count: { source: "stops", tone: "stopped" },
      },
      {
        // GuardrailPolicies (m66.10, ADR 0059) — PII scanning, deny-lists, the
        // optional LLM judge, per-user rate limits. Read-open.
        id: "guardrails",
        label: "Guardrails",
        icon: Shield,
        milestone: "M66",
        route: "/guardrails",
      },
      {
        // EvalSuite builder + results browser (m17.12).
        id: "evals",
        label: "Evals",
        icon: TestTube2,
        milestone: "M17",
        route: "/evals",
      },
      {
        // Datasets (M69) — human-labeled eval cases for the improvement loop
        // (ADR 0062 Fork 5). Read-open.
        id: "datasets",
        label: "Datasets",
        icon: Tags,
        milestone: "M69",
        route: "/datasets",
      },
      {
        // Registries (M15) — the allow-list of images/tools an agent may pull.
        // It reads as plumbing but it is a CONTROL, which is why it moved out of
        // the old "Advanced" shelf into Govern.
        id: "registries",
        label: "Registries",
        icon: Users,
        milestone: "M15",
        route: "/registries",
      },
      {
        // Audit (M63, ADR 0056) — the compliance trail. OPERATOR-ONLY: the trail
        // spans users and namespaces, so it is gated on `list auditlogs`.
        id: "audit",
        label: "Audit",
        icon: ScrollText,
        milestone: "M63",
        route: "/audit",
        requiresCapability: { resource: RES_AUDITLOGS, verb: "list" },
      },
    ],
  },
  {
    // Activity — what happened, what it cost, what it looks like. Runs IS the
    // trace list: each row drills into the native trace explorer at /traces/:id
    // (m16.7), so there is deliberately no standalone Traces destination.
    heading: "Activity",
    items: [
      {
        id: "runs",
        label: "Runs",
        icon: MessagesSquare,
        milestone: "M16",
        route: "/runs",
      },
      {
        // Alerts (M70, ADR 0063 D2) — fired AlertPolicy conditions. Gated on
        // `list alertpolicies` so a caller without that RBAC sees an honest 403
        // rather than an empty list.
        id: "alerts",
        label: "Alerts",
        icon: Bell,
        milestone: "M70",
        route: "/alerts",
        requiresCapability: { resource: RES_ALERTPOLICIES, verb: "list" },
      },
      {
        id: "cost",
        label: "Cost",
        icon: Coins,
        milestone: "M16",
        route: "/cost",
      },
      {
        // My Shares (V13) — the caller's own live share links, and where they are
        // revoked. Caller-scoped by the BFF, so read-open.
        id: "my-shares",
        label: "My shares",
        icon: Share2,
        milestone: "M112",
        route: "/my-shares",
      },
      {
        // Topology (m15.13) — the grouped/searchable live graph of what is
        // wired to what. It is a VIEW of activity, not a thing you create.
        id: "topology",
        label: "Topology",
        icon: Network,
        milestone: "M15",
        route: "/topology",
      },
    ],
  },
  {
    // Admin — the install itself. Credentials, model routing, tenancy: rarely
    // touched, consequential when touched, and never in the way of daily work.
    heading: "Admin",
    items: [
      {
        // Providers — the model home and the first step of the journey. Its
        // first-run prominence is carried by the Home checklist below (which
        // links to /providers/connect), not by nav position.
        id: "providers",
        label: "Providers",
        icon: PlugZap,
        milestone: "M14",
        route: "/providers",
      },
      {
        id: "routes",
        label: "Model routes",
        icon: GitBranch,
        milestone: "M15",
        route: "/routes",
      },
      {
        id: "secrets",
        label: "Secret bindings",
        icon: KeyRound,
        milestone: "M15",
        route: "/secrets",
      },
      {
        // Tenants (M47) — cluster-scoped namespace groupings with compute + model
        // quotas. Read-only for everyone; operators manage them (RBAC enforced at
        // the API server, ADR 0011).
        id: "tenants",
        label: "Tenants",
        icon: Building2,
        milestone: "M47",
        route: "/tenants",
      },
    ],
  },
];

// NAV_ITEMS is the flat list (every section's items) — handy for route/lookup.
export const NAV_ITEMS: NavItem[] = NAV_SECTIONS.flatMap((s) => s.items);

// navRoute resolves a nav item's router path by id — the single lookup so any
// route the console references stays anchored to the IA source of truth above. It
// throws on an unknown id (a nav rename then fails loudly at module-load in tests,
// not silently at runtime).
export function navRoute(id: string): string {
  const item = NAV_ITEMS.find((i) => i.id === id);
  if (!item?.route) {
    throw new Error(`navRoute: no routed nav item with id "${id}"`);
  }
  return item.route;
}

// navSectionOf names the section a nav id belongs to — the breadcrumb trail's
// middle crumb (M151 §4.2), resolved from the IA rather than re-declared in the
// shell. Returns undefined for an id the IA does not carry.
export function navSectionOf(id: string): string | undefined {
  return NAV_SECTIONS.find((s) => s.items.some((i) => i.id === id))?.heading;
}

// FirstRunStep is one guided step in the Home first-run checklist. `doneKey`
// maps to Home's live setup signals (provider/agent/run); `to` is derived
// from the nav surface the step drives so the checklist can't drift from the IA.
export interface FirstRunStep {
  label: string;
  to: string;
  doneKey: "provider" | "agent" | "run";
  /** What the step actually does, for someone who has never done it. */
  blurb: string;
}

// FIRST_RUN_CHECKLIST — Home's guided "get started" steps (m18.10), co-located
// with NAV_SECTIONS (m54.4) so the steps + the IA are the ONE source of truth
// reviewed together. Each `to` derives from the nav route it drives (the
// connect/new suffixes are the action affordances ON those surfaces) — a nav route
// change follows automatically instead of leaving a stale hardcoded path.
export const FIRST_RUN_CHECKLIST: FirstRunStep[] = [
  {
    label: "Connect a provider",
    to: `${navRoute("providers")}/connect`,
    doneKey: "provider",
    blurb: "Paste a key once. It is validated server-side and never reaches the browser.",
  },
  {
    label: "Create an agent",
    to: `${navRoute("agents")}/new`,
    doneKey: "agent",
    blurb: "Describe what you want in a sentence, then review the config before it applies.",
  },
  // The Playground — the taught run surface. Was /agents (a list, not a run
  // affordance); m49.4 UX-review P1.
  {
    label: "Run your agent",
    to: navRoute("playground"),
    doneKey: "run",
    blurb: "Send it a message and watch the trace, before anything reaches production.",
  },
];

// ── The breadcrumb trail (M151 §4.2) ────────────────────────────────────────

export interface ShellCrumb {
  label: string;
  /** A router destination. The LAST crumb never has one — you are already there. */
  to?: string;
}

/** Longest-prefix first, so /tools/mcp-servers wins over a hypothetical /tools. */
const ROUTED_ITEMS: NavItem[] = NAV_ITEMS.filter(
  (i): i is NavItem & { route: string } => Boolean(i.route) && i.route !== "/",
).sort((a, b) => (b.route ?? "").length - (a.route ?? "").length);

function humanizeSegment(seg: string): string {
  const words = decodeURIComponent(seg).replace(/-/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/**
 * The trail for a pathname, resolved from the IA rather than hand-maintained:
 * Home → the owning section → the destination → whatever the page added to the
 * URL (a namespace/name pair, a run id).
 *
 * The section crumb is deliberately NOT a link: a section is a grouping, not a
 * destination, and a breadcrumb that navigates nowhere is worse than one that
 * plainly does not offer to.
 */
export function buildCrumbs(pathname: string): ShellCrumb[] {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  if (path === "/" || path === "") return [{ label: "Home" }];

  const home: ShellCrumb = { label: "Home", to: "/" };

  // A not-yet-built IA destination: /soon/<nav id>.
  const soon = /^\/soon\/([^/]+)$/.exec(path);
  if (soon) {
    const item = NAV_ITEMS.find((i) => i.id === soon[1]);
    if (item) {
      const section = navSectionOf(item.id);
      return [
        home,
        ...(section && section !== item.label ? [{ label: section }] : []),
        { label: item.label },
      ];
    }
    return [home, { label: humanizeSegment(soon[1]) }];
  }

  const match = ROUTED_ITEMS.find(
    (i) => path === i.route || path.startsWith(`${i.route}/`),
  );
  if (!match || !match.route) {
    // A route the IA does not name (a not-found, a deep link to a surface reached
    // only from another page). Say what the URL says rather than inventing a home
    // for it.
    const segments = path.split("/").filter(Boolean);
    return [home, { label: humanizeSegment(segments[0] ?? "") }];
  }

  const section = navSectionOf(match.id);
  const tail = path.slice(match.route.length).split("/").filter(Boolean);
  const crumbs: ShellCrumb[] = [home];
  // "Agents › Agents" is not a trail, it is a stutter: when a section and its
  // destination share a name, the section crumb is dropped and the destination
  // (which is the one that navigates) is kept.
  if (section && section !== match.label) crumbs.push({ label: section });
  crumbs.push(
    tail.length > 0 ? { label: match.label, to: match.route } : { label: match.label },
  );
  if (tail.length > 0) {
    crumbs.push({ label: tail.map(decodeURIComponent).join("/") });
  }
  return crumbs;
}

/**
 * May this caller connect a provider?
 *
 * Connecting writes TWO objects: the core Secret holding the key, and the
 * SecretBinding that points at it. The console used to ask only about the
 * binding, so a caller with binding-create and no Secret-create was shown the
 * full wizard, typed their API key, and got `forbidden: not allowed to create
 * Secret` — the refusal arriving only once the credential was already in flight.
 *
 * A permission check that asks about less than the operation needs is worse than
 * none: it converts an honest up-front "you can't do this" into a late failure
 * with the user's secret in the request body. The rule lives here so the two
 * pages that gate on it (Providers, Connect a provider) cannot drift apart.
 */
// canConnectProvider was REMOVED (M160). It asked `secretbindings.create && secrets.create`
// while the connect handler upserts a THIRD object, a ModelRoute, and asked only about
// `create` against an upsert path — so a caller could see an enabled button and hit a denial
// after the Secret was already written. The question now lives with the handler that performs
// the writes (internal/bff/flows.go) and reaches the UI as canFlow("connectProvider"), so a
// permission conjunction can no longer drift from the operation it describes.

