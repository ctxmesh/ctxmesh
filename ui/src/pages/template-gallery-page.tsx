import * as React from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  BookOpen,
  Check,
  Download,
  GitFork,
  Link2,
  Loader2,
  RefreshCw,
  Search,
  Store,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  ClosingNote,
  CredentialSourceBadge,
  EmptyState,
  ErrorState,
  FilterChipRow,
  ForbiddenInline,
  NextStepLink,
  PageHeader,
  QuantityValue,
  SkeletonCard,
  useFocusTrap,
  useToast,
  VisibilityBadge,
  type FilterChip,
} from "@/components/kit";
import {
  api,
  ApiError,
  type CatalogEntry,
  type McpServerSummary,
  type TemplateEntry,
} from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";

// TemplateGalleryPage — archetype A9, the gallery (M151 spec §6.1/§6.2). Two
// tabs of discoverable things: agent templates (recipes ∪ published agents,
// GET /api/templates) and shared MCP servers (GET /api/catalog). Templates
// carry a Fork/Install CTA; servers carry a Connect CTA. Client-side
// composition — no server union. Discovery-only: no secret is ever rendered.
//
// ── WHY A GALLERY IS NOT A TABLE, AND WHAT THAT COSTS ───────────────────────
// A table gives every value a column, and a column is a WIDTH: it bounds the
// longest name in the set for free. A card has no column, so nothing bounds it
// — which is exactly how this page became the last route failing the fit sweep.
// A published agent named `unbreakablesingletokenagentnamewithnohyphens…` (63
// characters, no hyphen, no space) has a min-content width of ~600px, and the
// origin line carrying it pushed 272px past the card's edge at 1440 and 288px
// at 1280, in both themes, on every render.
//
// The fix is the §4.5 rule applied at the CARD: every line that can carry a
// resource name is a single truncating line with the full value in `title`,
// and every ancestor between the grid cell and that line carries `min-w-0`.
// `truncate` sets `overflow:hidden`, which is what lets a flex/grid child
// shrink below its min-content size at all — without the `min-w-0` chain above
// it, the property has nothing to bite on. A name is NEVER `break-all`-ed
// (§4.5): that turns an identifier into a five-line grey paragraph and makes
// the card taller than the thing it is describing.
//
// ── EQUAL-HEIGHT CARDS, BOUNDED CONTENT (§6.1 A9) ───────────────────────────
// `auto-rows-fr` + `flex flex-col` + a `mt-auto` footer: a card no longer grows
// to fit its longest field, and the CTA sits on the same baseline across the
// row. The description is `line-clamp-2` with the full text in `title`, so the
// 400-word fixture description occupies two lines like every other card.
//
// ── ONE PINE AFFORDANCE PER CARD (§6.1 A9) ──────────────────────────────────
// The CTA is the only interactive pine element on a card. The one card with two
// affordances is the already-forked one (U16), and it still holds the rule: the
// pine element is the "open your fork" link and the Fork button demotes to
// `outline`. Re-forking under a new name stays available — it is just no longer
// the loudest thing on the card.

type ActiveTab = "templates" | "mcp";

// ── The card shell, shared by both tabs ──────────────────────────────────────
// One geometry for every card on the page: the §6.1 A9 anatomy (serif title →
// mono provenance line → 2-line description → Tag row → right-aligned CTA
// footer). It is a local component rather than a kit one because the kit is
// frozen for this task; if a third gallery appears, this is what gets promoted.

/** The A9 grid: `gap-5`, 2-up at `sm`, 3-up at `xl`, equal-height rows. */
const GALLERY_GRID = "grid auto-rows-fr gap-5 sm:grid-cols-2 xl:grid-cols-3";

interface GalleryCardProps {
  testId: string;
  /** The item's name. One line, end-ellipsis, full value in `title` (§4.5). */
  title: string;
  titleTestId?: string;
  /** Machine-owned provenance (origin ref, namespace) — mono, truncated. */
  meta?: React.ReactNode;
  description?: string;
  /** The Tag row: what this thing is and where it came from. Never a hue. */
  tags?: React.ReactNode;
  /** Right-aligned footer — the card's single CTA. */
  footer: React.ReactNode;
}

function GalleryCard({
  testId,
  title,
  titleTestId,
  meta,
  description,
  tags,
  footer,
}: GalleryCardProps) {
  return (
    <li
      data-testid={testId}
      // Elevation is drawn with rules, not shadows (§2.7).
      className="flex min-w-0 flex-col rounded-lg border border-border bg-card p-5"
    >
      <div className="min-w-0">
        <h3
          className="truncate font-serif text-lg font-medium"
          title={title}
          data-testid={titleTestId}
        >
          {title}
        </h3>
        {meta}
      </div>
      {description && (
        // Prose wraps to two lines and stops; the whole thing rides in `title`
        // (§4.5 prose rule). `line-clamp` also sets overflow:hidden, so a
        // description containing one enormous token cannot widen the card.
        <p
          className="mt-2 line-clamp-2 text-sm text-secondary-foreground"
          title={description}
        >
          {description}
        </p>
      )}
      {tags && <div className="mt-3 flex flex-wrap items-center gap-1.5">{tags}</div>}
      {/* `mt-auto` is what makes the CTA line up across a row of unequal cards. */}
      <div className="mt-auto flex flex-wrap items-center justify-end gap-3 pt-4">
        {footer}
      </div>
    </li>
  );
}

/** The provenance line: machine-owned, mono, one line, full value in `title`. */
function CardMeta({
  children,
  full,
  testId,
}: {
  children: React.ReactNode;
  full: string;
  testId?: string;
}) {
  return (
    <p
      className="mt-1 truncate font-mono text-xs text-faint"
      title={full}
      data-testid={testId}
    >
      {children}
    </p>
  );
}

/** The A9 loading state: six card skeletons in the grid the cards will fill. */
function GallerySkeleton({ label }: { label: string }) {
  return (
    <div className={GALLERY_GRID} aria-label={label}>
      {[0, 1, 2, 3, 4, 5].map((i) => (
        <SkeletonCard key={i} />
      ))}
    </div>
  );
}

// authTypeLabel humanizes an MCP server's auth type for display (H5) — the gallery previously showed
// the raw "oauth" while the owned list showed "OAuth". One place, used on both.
function authTypeLabel(authType: string): string {
  switch (authType) {
    case "oauth":
      return "OAuth";
    case "apikey":
      return "API key";
    case "none":
      return "No auth";
    default:
      return authType;
  }
}

// ── Template tab ─────────────────────────────────────────────────────────────
type TemplateState =
  | { kind: "loading" }
  | { kind: "ready"; entries: TemplateEntry[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

type TemplateView = "all" | "recipe" | "published";

const TEMPLATE_VIEWS: {
  id: TemplateView;
  label: string;
  match: (e: TemplateEntry) => boolean;
}[] = [
  { id: "all", label: "Everything", match: () => true },
  { id: "recipe", label: "Built-in recipes", match: (e) => e.source === "recipe" },
  { id: "published", label: "Published", match: (e) => e.source === "published" },
];

/**
 * The origin of a template, as the two claims it can actually make: a built-in
 * recipe ships with the platform, and a published agent came from a namespace
 * someone runs. Both are returned untruncated — the card truncates, the `title`
 * carries the whole thing.
 */
function originOf(entry: TemplateEntry): string {
  const provenance = entry.provenance;
  if (provenance === "builtin" || provenance === undefined) return "built-in";
  const base = provenance.originNamespace
    ? `${provenance.originNamespace}/${provenance.originName ?? entry.name}`
    : (provenance.originName ?? entry.name);
  return provenance.version ? `${base} @ ${provenance.version}` : base;
}

/**
 * The §5.18 closing line for the templates tab: the honest split between the
 * two provenance claims, counted from the entries in hand. `/api/templates`
 * returns the whole set in one response, so these counts are FACTS, not a
 * window (which is also why the chips above may carry numbers).
 */
export function templatesClosingLine(entries: TemplateEntry[]): string | null {
  const total = entries.length;
  if (total === 0) return null;
  const recipes = entries.filter((e) => e.source === "recipe").length;
  const published = total - recipes;
  if (recipes === 0) {
    return `All ${total} of these were published from a namespace someone runs — nothing here ships with the platform.`;
  }
  if (published === 0) {
    return `All ${total} of these ship with the platform. Nobody in your org has published an agent yet.`;
  }
  return `${total} templates here: ${recipes} ship with the platform, ${published} were published from a namespace someone runs.`;
}

interface TemplateCardProps {
  entry: TemplateEntry;
  onFork: (entry: TemplateEntry) => void;
  // forkingKey is the unique key of the entry currently being forked (or null if none).
  // This enables a per-entry "Forking…" spinner (U12) instead of a global disable.
  forkingKey: string | null;
  // canFork gates the Fork button for viewers who lack agent-create rights (U10).
  canFork: boolean;
}

function TemplateCard({ entry, onFork, forkingKey, canFork }: TemplateCardProps) {
  const provenance = entry.provenance;
  const origin = originOf(entry);

  // U12: compute the unique key for this entry to check per-entry spinner state.
  const entryKey =
    provenance && provenance !== "builtin" && provenance.originNamespace
      ? `${provenance.originNamespace}/${provenance.originName ?? entry.name}`
      : `recipe/${entry.name}`;
  const isThisEntryForking = forkingKey === entryKey;
  // Block other entries while one is in flight (still a global disable for safety).
  const isAnyForking = forkingKey !== null;

  // U12: recipe uses Download icon + "Install" verb; published uses GitFork + "Fork" verb.
  const isRecipe = entry.source === "recipe";
  const ActionIcon = isRecipe ? Download : GitFork;
  const actionLabel = isRecipe ? "Install" : "Fork";
  const forked = entry.alreadyForkedAs;

  return (
    <GalleryCard
      testId={`template-card-${entry.name}`}
      title={entry.name}
      titleTestId={`template-name-${entry.name}`}
      meta={
        <CardMeta full={`origin: ${origin}`} testId={`template-origin-${entry.name}`}>
          origin: {origin}
        </CardMeta>
      }
      description={entry.description}
      tags={
        <>
          {/* Where it came from, which is the whole question a gallery answers.
              Two claims, two Tag treatments (§5.6): the platform's own recipe is
              a known, declared class (`muted`); a published agent came from
              outside this install's shipped set (`open`, the dashed chip). */}
          <Badge
            variant={isRecipe ? "muted" : "open"}
            data-testid={`template-source-${entry.name}`}
          >
            {isRecipe ? "built-in" : "published"}
          </Badge>
          <VisibilityBadge visibility={entry.visibility} />
          {/* The kind Tag earns its place only when it distinguishes something.
              Every entry in this gallery is an AgentDeployment today, and a Tag
              that reads the same on all 10 cards is decoration, not
              information — so it renders only for anything else. */}
          {entry.kind && entry.kind !== "AgentDeployment" && (
            <Badge variant="muted">{entry.kind}</Badge>
          )}
        </>
      }
      footer={
        <>
          {/* U16: an entry the caller ALREADY forked is pre-marked with a link to
              their fork, so they don't have to attempt a fork to discover it.
              This is the one card with two affordances, and the A9 rule still
              holds: the pine element is this link, and the button below demotes
              to `outline`. */}
          {forked && (
            <NextStepLink
              label="Already forked — open it"
              to={`/agents/${encodeURIComponent(forked.namespace)}/${encodeURIComponent(forked.name)}`}
              ariaLabel={`Open your fork ${forked.namespace}/${forked.name}`}
              testId={`template-already-forked-${entry.name}`}
            />
          )}
          {/* U10: display-gate the Fork button for viewers without create rights */}
          {canFork ? (
            <Button
              size="sm"
              variant={forked ? "outline" : "default"}
              onClick={() => onFork(entry)}
              disabled={isAnyForking}
              title={
                forked
                  ? "You already forked this — forking again creates another copy under a new name"
                  : isAnyForking && !isThisEntryForking
                    ? "Another fork is in progress"
                    : undefined
              }
              data-testid={`fork-template-${entry.name}`}
            >
              {isThisEntryForking ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  {isRecipe ? "Installing…" : "Forking…"}
                </>
              ) : (
                <>
                  <ActionIcon className="mr-1.5 h-3.5 w-3.5" />
                  {actionLabel}
                </>
              )}
            </Button>
          ) : (
            <Button
              size="sm"
              variant="outline"
              disabled
              title="You need agent-create rights to fork"
              data-testid={`fork-template-${entry.name}`}
            >
              <ActionIcon className="mr-1.5 h-3.5 w-3.5" />
              {actionLabel}
            </Button>
          )}
        </>
      }
    />
  );
}

// ── Rename-on-fork dialog (U11) ───────────────────────────────────────────────
// When forking hits a 409 (name collision with a different origin), offer the user
// a prompt to retry with a different name.
interface RenamePromptProps {
  defaultName: string;
  onConfirm: (name: string) => void;
  onCancel: () => void;
}

function RenameOnForkDialog({ defaultName, onConfirm, onCancel }: RenamePromptProps) {
  const [name, setName] = React.useState(defaultName + "-copy");
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onCancel });
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label="Rename fork"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onCancel}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-sm rounded-xl border border-border bg-card p-6 shadow-overlay outline-none"
        data-testid="rename-fork-dialog"
      >
        <h2 className="font-serif text-lg font-medium">Name already taken</h2>
        <p className="mt-1 text-sm text-secondary-foreground">
          An agent named &ldquo;{defaultName}&rdquo; already exists in your namespace with a
          different origin. Choose a different name for your fork.
        </p>
        <div className="mt-3">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="New name for your fork"
            data-testid="rename-fork-input"
            autoFocus
          />
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={onCancel}>Cancel</Button>
          <Button
            onClick={() => onConfirm(name.trim())}
            disabled={!name.trim()}
            data-testid="rename-fork-confirm"
          >
            Fork as {name.trim() || "…"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function TemplatesTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { can } = useCapabilities();
  // U10: display-gate the Fork button for viewers lacking agent-create rights.
  const canFork = can(RES_AGENTS, "create");
  const [state, setState] = React.useState<TemplateState>({ kind: "loading" });
  const [view, setView] = React.useState<TemplateView>("all");
  // U12: forkingEntry tracks the unique key of the entry currently in flight for per-entry spinner.
  const [forkingEntry, setForkingEntry] = React.useState<string | null>(null);
  // U11: rename-on-fork dialog state — shown when a 409 collision is detected.
  const [renameDialog, setRenameDialog] = React.useState<{
    entry: TemplateEntry;
    defaultName: string;
  } | null>(null);

  const load = React.useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });
    api
      .getTemplates(undefined, signal)
      .then((entries) => {
        if (signal?.aborted) return;
        setState({ kind: "ready", entries });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setState({ kind: "forbidden", message: err.message });
          return;
        }
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const c = new AbortController();
    load(c.signal);
    return () => c.abort();
  }, [load]);

  // doFork is called with an optional `localName` override (U11 rename-on-fork).
  async function doFork(entry: TemplateEntry, localName?: string) {
    // Published agent: POST fork.
    const prov = entry.provenance;
    if (!prov || prov === "builtin" || !prov.originNamespace) {
      toast({
        variant: "error",
        title: "Cannot fork",
        description: "This template is missing origin information.",
      });
      return;
    }

    // U12: unique key uses origin ns to avoid same-name collision from different namespaces.
    const key = `${prov.originNamespace}/${prov.originName ?? entry.name}`;
    setForkingEntry(key);
    try {
      const res = await api.forkAgent(
        prov.originNamespace,
        prov.originName ?? entry.name,
        localName,
      );

      // Navigate to the FORK's own coordinates, not the origin's.
      // res.agent carries the fork's namespace + name (the caller's namespace).
      const forkNs = res.agent?.namespace ?? "";
      const forkName = res.agent?.name ?? entry.name;

      if (res.status === "already-forked") {
        // U11: link to the existing fork rather than a generic message.
        toast({
          variant: "info",
          title: "Already forked",
          description: `You already have a fork of ${entry.name}. Opening it now.`,
        });
        navigate(`/agents/${encodeURIComponent(forkNs)}/${encodeURIComponent(forkName)}`);
        return;
      }

      // U9: celebrate resolved refs (tools auto-connected via compose-connect).
      const resolvedCount = res.resolvedRefs?.length ?? 0;
      const hasDangling =
        (res.needsRebinding?.length ?? 0) > 0 ||
        (res.unresolvedRefs?.length ?? 0) > 0;

      if (hasDangling) {
        const items = [
          ...(res.needsRebinding ?? []),
          ...(res.unresolvedRefs ?? []),
        ].join(", ");
        const resolvedNote =
          resolvedCount > 0
            ? ` (${resolvedCount} tool${resolvedCount > 1 ? "s" : ""} connected automatically)`
            : "";
        toast({
          variant: "info",
          title: "Forked — needs attention",
          description: `${entry.name} was forked${resolvedNote} but has dangling references: ${items}. Open the agent to fix them.`,
        });
      } else if (resolvedCount > 0) {
        // U9: the "compounding moment" toast — all tools connected automatically.
        toast({
          variant: "success",
          title: "Forked",
          description: `${entry.name} is now in your namespace — ${resolvedCount} tool${resolvedCount > 1 ? "s" : ""} connected automatically.`,
        });
      } else {
        toast({
          variant: "success",
          title: "Forked",
          description: `${entry.name} is now in your namespace.`,
        });
      }
      navigate(`/agents/${encodeURIComponent(forkNs)}/${encodeURIComponent(forkName)}`);
    } catch (err) {
      const isNotFound = err instanceof ApiError && err.isNotFound;
      const isConflict = err instanceof ApiError && err.status === 409;
      if (isConflict) {
        // U11: 409 — offer rename-on-fork instead of a dead-end error.
        setRenameDialog({ entry, defaultName: entry.name });
      } else {
        const msg = isNotFound
          ? `${entry.name} is no longer discoverable.`
          : err instanceof Error
          ? err.message
          : "fork failed";
        toast({ variant: "error", title: "Fork failed", description: msg });
      }
    } finally {
      setForkingEntry(null);
    }
  }

  async function handleFork(entry: TemplateEntry) {
    // Recipe: pre-fill the create-agent flow via ?recipe=<name>. CreateAgentPage fetches
    // the recipe list and finds the spec by name — avoids a fragile ?spec= blob in the URL
    // that URL-length limits or encoding differences could corrupt (m74 P1-2 fix).
    if (entry.source === "recipe") {
      navigate(`/agents/new?recipe=${encodeURIComponent(entry.name)}`);
      return;
    }
    await doFork(entry);
  }

  const entries = state.kind === "ready" ? state.entries : [];
  const activeView =
    TEMPLATE_VIEWS.find((v) => v.id === view) ?? TEMPLATE_VIEWS[0];
  const visible = entries.filter(activeView.match);
  // The whole set arrives in one response, so a count of it is a fact rather
  // than a window (the FilterChipRow contract's one condition for a number).
  const chips: FilterChip[] = TEMPLATE_VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: entries.filter(v.match).length,
  }));

  return (
    <div className="min-w-0 space-y-4">
      {state.kind === "ready" && entries.length > 0 && (
        <div className="flex flex-wrap items-center gap-3">
          <FilterChipRow
            chips={chips}
            value={view}
            onChange={(id) => setView(id as TemplateView)}
            label="Filter templates"
            className="min-w-0 flex-1"
          />
          <Button
            variant="ghost"
            size="icon"
            onClick={() => load()}
            aria-label="Refresh templates"
            data-testid="templates-refresh"
          >
            <RefreshCw className="h-4 w-4" />
          </Button>
        </div>
      )}

      {state.kind === "loading" && <GallerySkeleton label="Loading templates" />}

      {state.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to browse templates"
          description="Your account can't list the template gallery."
          detail={state.message}
        />
      )}

      {state.kind === "error" && (
        <ErrorState
          title="Couldn't load templates"
          description={state.message}
          onRetry={() => load()}
        />
      )}

      {state.kind === "ready" && entries.length === 0 && (
        <EmptyState
          icon={BookOpen}
          title="No templates yet"
          description="The gallery is empty. Publish an agent from its detail page to seed it — published agents appear here for anyone in scope to fork."
        />
      )}

      {state.kind === "ready" && entries.length > 0 && visible.length === 0 && (
        <EmptyState
          icon={BookOpen}
          intent="filtered"
          title="Nothing matches"
          description={
            view === "recipe"
              ? "No template here ships with the platform. Show everything to see what your org has published."
              : "Nobody in your org has published an agent yet. Show everything to see the built-in recipes."
          }
          totalCount={entries.length}
          countNoun="templates"
          action={{
            label: "Show everything",
            variant: "outline",
            onClick: () => setView("all"),
          }}
        />
      )}

      {state.kind === "ready" && visible.length > 0 && (
        <ul className={GALLERY_GRID} data-testid="template-list">
          {visible.map((e) => {
            // U12: include origin namespace in the key to avoid React key collision for
            // same-named templates published from different namespaces.
            const prov = e.provenance;
            const originNs =
              prov && prov !== "builtin" && prov.originNamespace
                ? prov.originNamespace
                : "builtin";
            const cardKey = `${e.source}/${originNs}/${e.name}`;
            return (
              <TemplateCard
                key={cardKey}
                entry={e}
                onFork={handleFork}
                forkingKey={forkingEntry}
                canFork={canFork}
              />
            );
          })}
        </ul>
      )}

      {state.kind === "ready" && entries.length > 0 && (
        <ClosingNote>{templatesClosingLine(entries)}</ClosingNote>
      )}

      {/* U11: rename-on-fork dialog — shown on 409 collision */}
      {renameDialog && (
        <RenameOnForkDialog
          defaultName={renameDialog.defaultName}
          onConfirm={(newName) => {
            const entry = renameDialog.entry;
            setRenameDialog(null);
            void doFork(entry, newName);
          }}
          onCancel={() => setRenameDialog(null)}
        />
      )}
    </div>
  );
}

// ── MCP Catalog tab ──────────────────────────────────────────────────────────
// T10: cross-checks the catalog against the caller's owned servers to render a
// "Connected ✓" disabled state for entries already in the caller's namespace.
// T11: after a successful Connect, navigates with location.state.highlight so
// McpServersPage can briefly flash the new row.
// T12: card skeletons (not a table skeleton), "Connecting…" label, catalog
// search/filter, consistent "org" vocabulary.

type CatalogState =
  | { kind: "loading" }
  | { kind: "ready"; entries: CatalogEntry[]; ownedKeys: Set<string> }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

/** Frozen empties, so a not-yet-loaded catalog doesn't churn the memos below. */
const NO_ENTRIES: CatalogEntry[] = [];
const NO_KEYS: ReadonlySet<string> = new Set<string>();

type CatalogView = "all" | "new" | "connected";

const CATALOG_VIEWS: { id: CatalogView; label: string }[] = [
  { id: "all", label: "Everything" },
  { id: "new", label: "Not yet connected" },
  { id: "connected", label: "Already connected" },
];

/**
 * The §5.18 closing line for the catalog tab. Every number in it is summed from
 * per-server counts the BFF actually sent — never an estimate.
 */
export function catalogClosingLine(
  entries: CatalogEntry[],
  connected: number,
): string | null {
  const total = entries.length;
  if (total === 0) return null;
  const tools = entries.reduce((n, e) => n + e.toolCount, 0);
  const servers = `${total} server${total === 1 ? "" : "s"}`;
  const toolsPhrase = `${tools} ${tools === 1 ? "tool" : "tools"}`;
  if (connected === 0) {
    return `${servers} discoverable here, exposing ${toolsPhrase} between them. None of them is connected to your namespace yet.`;
  }
  if (connected === total) {
    return `${servers} discoverable here, exposing ${toolsPhrase} between them — and you are already connected to every one.`;
  }
  return `${servers} discoverable here, exposing ${toolsPhrase} between them. ${connected} of them ${connected === 1 ? "is" : "are"} already connected to your namespace.`;
}

function McpCatalogTab() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const [state, setState] = React.useState<CatalogState>({ kind: "loading" });
  const [connectingEntry, setConnectingEntry] = React.useState<string | null>(null);
  const [view, setView] = React.useState<CatalogView>("all");
  // T12 catalog search filter
  const [search, setSearch] = React.useState("");

  const load = React.useCallback((signal?: AbortSignal) => {
    setState({ kind: "loading" });

    // T10: fetch catalog AND owned servers in parallel; cross-check by name+ns.
    Promise.all([
      api.getCatalog(undefined, signal),
      api.listMcpServers(signal),
    ])
      .then(([catalogRes, ownedRes]) => {
        if (signal?.aborted) return;
        const owned = ownedRes.items ?? ownedRes.servers ?? [];
        // Build a key set of the caller's owned servers: "<ns>/<name>"
        const ownedKeys = new Set(owned.map((s: McpServerSummary) => `${s.namespace}/${s.name}`));
        setState({ kind: "ready", entries: catalogRes.entries ?? [], ownedKeys });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setState({ kind: "forbidden", message: err.message });
          return;
        }
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const c = new AbortController();
    load(c.signal);
    return () => c.abort();
  }, [load]);

  async function handleConnect(entry: CatalogEntry) {
    const key = `${entry.namespace}/${entry.name}`;
    setConnectingEntry(key);
    try {
      const res = await api.connectMcpServer(entry.namespace, entry.name);
      if (res.status === "already-connected") {
        toast({
          variant: "info",
          title: "Already connected",
          description: `${entry.name} is already in your namespace.`,
        });
      } else {
        toast({
          variant: "success",
          title: "Connected",
          description: `${entry.name} is now available in your MCP servers.`,
        });
        // T11: pass the new copy's ns/name as a highlight anchor so McpServersPage
        // can flash the new row. The new copy is in the caller's own namespace (from
        // res.namespace) or falls back to the origin namespace if the BFF omits it.
        const connectedNs = res.namespace ?? entry.namespace;
        const connectedName = res.name ?? entry.name;
        navigate("/tools/mcp-servers", {
          state: { highlight: `${connectedNs}/${connectedName}` },
        });
      }
    } catch (err) {
      const msg =
        err instanceof ApiError && err.isNotFound
          ? `${entry.name} is no longer discoverable.`
          : err instanceof Error
          ? err.message
          : "connect failed";
      toast({ variant: "error", title: "Connect failed", description: msg });
    } finally {
      setConnectingEntry(null);
    }
  }

  // Referentially stable while the load state is unchanged, so the memo below
  // is not rebuilt on every render (and eslint can see that it isn't).
  const { entries, ownedKeys } = React.useMemo(
    () =>
      state.kind === "ready"
        ? { entries: state.entries, ownedKeys: state.ownedKeys }
        : { entries: NO_ENTRIES, ownedKeys: NO_KEYS },
    [state],
  );
  const connectedCount = entries.filter((e) =>
    ownedKeys.has(`${e.namespace}/${e.name}`),
  ).length;

  // T12: client-side filter over name/namespace/description, on top of the view.
  const filteredEntries = React.useMemo(() => {
    const inView = entries.filter((e) => {
      if (view === "new") return !ownedKeys.has(`${e.namespace}/${e.name}`);
      if (view === "connected") return ownedKeys.has(`${e.namespace}/${e.name}`);
      return true;
    });
    if (!search.trim()) return inView;
    const q = search.toLowerCase();
    return inView.filter(
      (e) =>
        e.name.toLowerCase().includes(q) ||
        e.namespace.toLowerCase().includes(q) ||
        (e.description ?? "").toLowerCase().includes(q),
    );
  }, [entries, ownedKeys, view, search]);

  const chips: FilterChip[] = CATALOG_VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    // The catalog arrives whole, so these counts describe the whole catalog.
    count:
      v.id === "all"
        ? entries.length
        : v.id === "connected"
          ? connectedCount
          : entries.length - connectedCount,
  }));

  return (
    <div className="min-w-0 space-y-4">
      {state.kind === "ready" && entries.length > 0 && (
        <>
          <div className="flex flex-wrap items-center gap-3">
            <FilterChipRow
              chips={chips}
              value={view}
              onChange={(id) => setView(id as CatalogView)}
              label="Filter shared servers"
              className="min-w-0 flex-1"
            />
            <Button
              variant="ghost"
              size="icon"
              onClick={() => load()}
              aria-label="Refresh shared servers"
              data-testid="mcp-catalog-tab-refresh"
            >
              <RefreshCw className="h-4 w-4" />
            </Button>
          </div>
          {/* T12: catalog search input */}
          <div className="relative max-w-sm">
            <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-faint" />
            <Input
              placeholder="Search servers…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="pl-8"
              data-testid="mcp-catalog-search"
            />
          </div>
        </>
      )}

      {state.kind === "loading" && <GallerySkeleton label="Loading shared servers" />}

      {state.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to browse shared servers"
          description="Your account can't list shared servers."
          detail={state.message}
        />
      )}

      {state.kind === "error" && (
        <ErrorState
          title="Couldn't load shared servers"
          description={state.message}
          onRetry={() => load()}
        />
      )}

      {state.kind === "ready" && entries.length === 0 && (
        <EmptyState
          icon={Store}
          title="No discoverable servers yet"
          description="When servers are shared team-wide, org-wide, or publicly, they appear here for you to connect."
        />
      )}

      {state.kind === "ready" && entries.length > 0 && filteredEntries.length === 0 && (
        <div data-testid="mcp-catalog-no-results">
          <EmptyState
            icon={Store}
            intent="filtered"
            title="Nothing matches"
            description={
              search.trim()
                ? `No shared server matches “${search.trim()}”. Clear the search to see the rest.`
                : view === "connected"
                  ? "You haven't connected any of these servers yet. Show everything to see what is on offer."
                  : "You are already connected to every server here. Show everything to see them."
            }
            totalCount={entries.length}
            countNoun="servers"
            action={{
              label: search.trim() ? "Clear the search" : "Show everything",
              variant: "outline",
              onClick: () => {
                setSearch("");
                setView("all");
              },
            }}
          />
        </div>
      )}

      {state.kind === "ready" && filteredEntries.length > 0 && (
        <ul className={GALLERY_GRID} data-testid="mcp-catalog-tab-list">
          {filteredEntries.map((e) => {
            const entryKey = `${e.namespace}/${e.name}`;
            // T10: cross-check against the caller's owned list by ns+name
            const isConnected = ownedKeys.has(entryKey);
            const isConnecting = connectingEntry === entryKey;
            return (
              <GalleryCard
                key={entryKey}
                testId={`mcp-catalog-entry-${e.name}`}
                title={e.name}
                meta={
                  <CardMeta full={`namespace: ${e.namespace}`}>
                    namespace: {e.namespace}
                  </CardMeta>
                }
                description={e.description}
                tags={
                  <>
                    <VisibilityBadge visibility={e.visibility} />
                    {e.authType && <Badge variant="muted">{authTypeLabel(e.authType)}</Badge>}
                    {/* T8: human-label credentialSource badge */}
                    <CredentialSourceBadge credentialSource={e.credentialSource} name={e.name} />
                    {/* T10: "Connected ✓" Tag for already-owned entries */}
                    {isConnected && (
                      <Badge
                        variant="ok"
                        className="gap-1"
                        data-testid={`mcp-catalog-connected-${e.name}`}
                      >
                        <Check className="h-3 w-3" />
                        Connected
                      </Badge>
                    )}
                  </>
                }
                footer={
                  <>
                    {/* A known count, so it prints as a real figure (§7.1) — a
                        server exposing nothing reads `0`, never a dash. */}
                    <span className="mr-auto text-xs text-faint">
                      <QuantityValue value={e.toolCount} className="text-xs" />{" "}
                      {e.toolCount === 1 ? "tool" : "tools"}
                    </span>
                    {/* T10: disabled state for already-connected; T12: "Connecting…" */}
                    <Button
                      size="sm"
                      onClick={() => handleConnect(e)}
                      disabled={isConnected || isConnecting}
                      data-testid={`connect-mcp-tab-${e.name}`}
                    >
                      {isConnected ? (
                        <>
                          <Check className="mr-1.5 h-3.5 w-3.5" />
                          Connected
                        </>
                      ) : (
                        <>
                          <Link2 className="mr-1.5 h-3.5 w-3.5" />
                          {isConnecting ? "Connecting…" : "Connect"}
                        </>
                      )}
                    </Button>
                  </>
                }
              />
            );
          })}
        </ul>
      )}

      {state.kind === "ready" && entries.length > 0 && (
        <ClosingNote>{catalogClosingLine(entries, connectedCount)}</ClosingNote>
      )}
    </div>
  );
}

// ── Page ─────────────────────────────────────────────────────────────────────

/** The lede is tab-aware: it says what THIS tab offers, in an operator's words. */
const TAB_LEDE: Record<ActiveTab, string> = {
  templates:
    "Built-in recipes you can install, and agents your org has published for you to fork into your own namespace.",
  mcp: "MCP servers shared across your org. Connect one to make its tools available in your namespace — its credentials stay with whoever published it.",
};

export function TemplateGalleryPage() {
  const [searchParams] = useSearchParams();
  const initialTab: ActiveTab = searchParams.get("tab") === "mcp" ? "mcp" : "templates";
  const [activeTab, setActiveTab] = React.useState<ActiveTab>(initialTab);

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Gallery"
        lede={TAB_LEDE[activeTab]}
        // The tab strip is the kit's (§5.17): APG roving tabindex, the 2px pine
        // underline for the current tab, and its own overflow scroller. The
        // second tab keeps the m76.1 taxonomy label — "Shared servers", never
        // "MCP servers", which is the OWNED list one nav item away.
        tabs={[
          { id: "templates", label: "Templates", current: activeTab === "templates" },
          { id: "mcp", label: "Shared servers", current: activeTab === "mcp" },
        ]}
        onTabChange={(id) => setActiveTab(id as ActiveTab)}
      />

      {activeTab === "templates" && <TemplatesTab />}
      {activeTab === "mcp" && <McpCatalogTab />}
    </div>
  );
}
