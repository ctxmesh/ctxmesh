import * as React from "react";
import { Filter, GitBranch, Loader2, RefreshCw, Sparkles } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  CellEntity,
  CellId,
  ClosingNote,
  ConfirmDialog,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuietNote,
  SectionHeader,
  UnknownValue,
  nextStepRank,
  useFocusTrap,
  useToast,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { cn } from "@/lib/utils";
import {
  api,
  ApiError,
  type PromptDiffLine,
  type PromptDiffResponse,
  type PromptVersionSummary,
} from "@/lib/api";
import { RES_PROMPTVERSIONS } from "@/lib/nav";

// PromptsPage — archetype A1 (index/table) plus the diff reader the §6.2
// mapping names: a two-version picker over a side-by-side code well that
// scrolls in its OWN container (§4.6). PageHeader → FilterChipRow → DataTable →
// ClosingNote → the reader.
//
// ── WHAT THE LIST KNOWS, AND WHAT IT REFUSES TO GUESS ───────────────────────
// `PromptVersionSummary` carries name, namespace, ref, promptName, createdAt —
// and NO status of any kind. So this page has no health column and invents
// none. The one thing it can honestly derive is ORDER: within a namespace +
// prompt, the version with the latest recorded `createdAt` is the newest one
// IN THIS LIST. That is a fact about the list, not about production — which
// version an agent actually runs is set on the agent, and the QuietNote above
// the table says so out loud rather than letting "newest" be misread as "live".
//
// A prompt whose versions carry no `createdAt` has no derivable order at all;
// those rows render the honest dash and take no standing, because guessing
// from a version NAME (`v9` > `v8`) would be reading meaning into a string the
// backend never promised was ordered.
//
// ── SORTED BY WHAT IS BLOCKING (§6.1 A1) ────────────────────────────────────
// The page exists so a prompt change can be reviewed before it ships. The row
// that asks something of a person is therefore the newest version of a prompt
// that HAS a predecessor: someone should read what changed. `nextStepRank` puts
// those first; everything else says "Nothing needed" and sinks.
//
// ── THE DIFF READER'S HONEST DEGRADE CONTRACT (non-negotiable) ──────────────
//   200 → render the diff; `resolveMode` is ALWAYS shown explicitly.
//   501 → "prompt resolution is not configured" — a calm QuietNote, NOT an error.
//   404 → "version/ref not found" — a distinct honest state.
//   502 → "resolve failed (retry)" — a real transient error, with a retry.
//   NEVER fabricate a diff, and never render a missing line array as "identical".
//
// data-testid contract:
//   prompts-page                — root container
//   prompt-version-{name}       — each row's entity cell
//   next-step-{name}            — the row's Next step cell
//   prompt-delete-{name}        — the row's delete action (RBAC-gated)
//   prompts-quiet-note          — the "which version is live isn't here" note
//   diff-from-input / diff-to-input / diff-compare-btn — the two-version picker
//   prompt-diff                 — the rendered diff
//   prompt-diff-resolve-mode    — the explicit resolve-mode Tag
//   prompt-diff-lines           — the side-by-side well (its own scroll container)
//   prompt-diff-loading | -not-configured | -not-found | -resolve-failed | -error
//   prompt-diff-unreadable      — a 200 whose body carried no line array

// ---- discriminated state types -----------------------------------------------

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; versions: PromptVersionSummary[]; nextCursor: string }
  | { kind: "error"; message: string; forbidden: boolean };

// DiffState captures the full discriminated outcome for the diff reader.
// Each status-code outcome is DISTINCT — no fabricated diffs at any branch.
type DiffState =
  | { kind: "idle" }
  | { kind: "loading" }
  // 200 — real diff. resolveMode="textual" shown explicitly.
  | { kind: "ready"; diff: PromptDiffResponse; from: string; to: string }
  // 501 — no resolver configured: calm, NOT an error.
  | { kind: "not-configured" }
  // 404 — the from/to ref doesn't exist.
  | { kind: "not-found"; message: string }
  // 502 — resolver found but failed: retryable.
  | { kind: "resolve-failed"; message: string }
  // other errors (network, 403, etc.)
  | { kind: "error"; message: string; forbidden?: boolean };

type DeleteState =
  | { kind: "idle" }
  | { kind: "deleting" }
  | { kind: "error"; message: string };

// ---- deriving order, honestly -------------------------------------------------

/** The stable identity of one version: a name is unique only within a namespace. */
function vkey(v: PromptVersionSummary): string {
  return `${v.namespace}/${v.name}`;
}

/** Milliseconds since epoch, or null when the backend recorded no creation time. */
function stampMs(ts?: string): number | null {
  if (!ts) return null;
  const t = new Date(ts).getTime();
  return Number.isFinite(t) ? t : null;
}

/**
 * The §4.5 table register for a timestamp: same year → "Aug 28", older →
 * "2025-08-29", with the full ISO value in `title`.
 */
function formatStamp(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  if (d.getFullYear() !== new Date().getFullYear()) return d.toISOString().slice(0, 10);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** Where a version sits in its prompt's history — derived only from real stamps. */
type Standing = "newest" | "superseded" | "unknown";

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
}

interface Decorated {
  version: PromptVersionSummary;
  at: number | null;
  standing: Standing;
  /** How many versions of this prompt the loaded list holds. */
  siblings: number;
  /** The version immediately before this one, when this one is the newest. */
  previous: PromptVersionSummary | null;
  next: NextStep;
}

/**
 * Decorate every version with its standing inside its own prompt's history.
 *
 * Grouping is by namespace + promptName because the diff endpoint is
 * namespace-scoped: two versions in different namespaces are not comparable, so
 * calling one of them the other's predecessor would offer a comparison the
 * backend must refuse.
 */
export function decorate(versions: PromptVersionSummary[]): Decorated[] {
  const groups = new Map<string, PromptVersionSummary[]>();
  for (const v of versions) {
    const key = `${v.namespace}/${v.promptName}`;
    const bucket = groups.get(key);
    if (bucket) bucket.push(v);
    else groups.set(key, [v]);
  }

  const byKey = new Map<string, Decorated>();
  for (const [, members] of groups) {
    // Only versions with a REAL stamp can be ordered. The rest keep the
    // "unknown" standing rather than being ranked by their names.
    const dated = members
      .map((v) => ({ v, at: stampMs(v.createdAt) }))
      .filter((m): m is { v: PromptVersionSummary; at: number } => m.at !== null)
      .sort((a, b) => b.at - a.at);
    const newest = dated[0]?.v ?? null;
    const previous = dated[1]?.v ?? null;

    for (const v of members) {
      const at = stampMs(v.createdAt);
      const standing: Standing =
        at === null || newest === null
          ? "unknown"
          : vkey(v) === vkey(newest)
            ? "newest"
            : "superseded";
      const isNewestWithHistory = standing === "newest" && previous !== null;
      byKey.set(vkey(v), {
        version: v,
        at,
        standing,
        siblings: members.length,
        previous: isNewestWithHistory ? previous : null,
        next: isNewestWithHistory
          ? { label: "Review the change", tone: "default" }
          : { tone: "none" },
      });
    }
  }

  // Preserve the caller's order as the stable base; the page sorts after this.
  return versions.map((v) => byKey.get(vkey(v))!);
}

// ---- the chip views (§5.28): one question, one answer at a time ---------------

const PROMPT_VIEWS = ["all", "needs-you", "superseded"] as const;
type PromptView = (typeof PROMPT_VIEWS)[number];

const PROMPT_VIEW_LABEL: Record<PromptView, string> = {
  all: "Everything",
  "needs-you": "Needs a look",
  superseded: "Superseded",
};

// The chips are BUILT from this union below, so a chip whose id is not a view
// stops compiling instead of silently filtering to nothing.
const PROMPT_VIEW_MATCH: Record<PromptView, (d: Decorated) => boolean> = {
  all: () => true,
  "needs-you": (d) => d.next.tone !== "none",
  superseded: (d) => d.standing === "superseded",
};

const PROMPT_VIEW_EMPTY: Record<
  Exclude<PromptView, "all">,
  { title: string; description: string }
> = {
  "needs-you": {
    title: "No change is waiting to be read",
    description:
      "Every prompt in view is on its first version, so there is nothing to compare it against. Show everything to see them all.",
  },
  superseded: {
    title: "Nothing has been superseded",
    description:
      "Every prompt in view is on its first version. Show everything to see them all.",
  },
};

/**
 * The §5.18 closing line: the honest ratio in words, restating what the table
 * already showed. Counted from the rows in hand, and it says so when the rows
 * in hand are not the whole list.
 */
export function closingLine(rows: Decorated[], complete: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const needs = rows.filter((d) => nextStepRank(d.next.tone) === 0).length;
  if (total === 1) {
    return "The one version here is the first of its prompt, so there is nothing yet to compare it with.";
  }
  const where = complete ? "" : " on this page";
  const more = complete ? "" : " More versions follow.";
  if (needs === 0) {
    return `None of the ${total} versions${where} has a predecessor to be read against. Nothing here needs a person.${more}`;
  }
  const quiet = total - needs;
  return `${needs} of the ${total} versions${where} ${needs === 1 ? "is the newest of its prompt and has" : "are the newest of their prompts and have"} a change to read. The other ${quiet} ${quiet === 1 ? "is history" : "are history"}.${more}`;
}

// ---- main page ----------------------------------------------------------------

export function PromptsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [query, setQuery] = React.useState("");
  const [view, setView] = React.useState<PromptView>("all");
  const [diffState, setDiffState] = React.useState<DiffState>({ kind: "idle" });
  const [fromKey, setFromKey] = React.useState("");
  const [toKey, setToKey] = React.useState("");
  const [deleteTarget, setDeleteTarget] =
    React.useState<PromptVersionSummary | null>(null);
  const [deleteState, setDeleteState] = React.useState<DeleteState>({
    kind: "idle",
  });
  const [showNewForm, setShowNewForm] = React.useState(false);
  const readerRef = React.useRef<HTMLDivElement>(null);

  const { can } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();

  const canCreate = can(RES_PROMPTVERSIONS, "create");
  const canDelete = can(RES_PROMPTVERSIONS, "delete");

  const load = React.useCallback(
    (signal?: AbortSignal) => {
      setPage({ kind: "loading" });
      api
        .listPromptVersions({ namespace: shellNs || undefined }, signal)
        .then((res) => {
          if (signal?.aborted) return;
          setPage({ kind: "ready", versions: res.items, nextCursor: res.nextCursor });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          setPage({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
            forbidden: err instanceof ApiError && err.isForbidden,
          });
        });
    },
    [shellNs],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const all = React.useMemo(
    () => (page.kind === "ready" ? page.versions : []),
    [page],
  );
  // The loaded window IS the whole list only when no cursor follows it — the one
  // condition under which counting the rows in hand is a fact, not a guess.
  const complete = page.kind === "ready" && page.nextCursor === "";

  const decorated = React.useMemo(() => decorate(all), [all]);

  // Attention-first (§6.1 A1): a change waiting to be read sorts above history,
  // then newest first, then by name. A version with no recorded stamp cannot be
  // placed in time, so it sorts after the ones that can.
  const sorted = React.useMemo(
    () =>
      [...decorated].sort(
        (a, b) =>
          nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
          (b.at ?? -Infinity) - (a.at ?? -Infinity) ||
          a.version.name.localeCompare(b.version.name),
      ),
    [decorated],
  );

  const q = query.trim().toLowerCase();
  const visible = React.useMemo(() => {
    const inView = sorted.filter(PROMPT_VIEW_MATCH[view]);
    return q
      ? inView.filter(
          (d) =>
            d.version.name.toLowerCase().includes(q) ||
            d.version.promptName.toLowerCase().includes(q),
        )
      : inView;
  }, [sorted, view, q]);

  const chips: FilterChip[] = PROMPT_VIEWS.map((id) => ({
    id,
    label: PROMPT_VIEW_LABEL[id],
    // A count only when the loaded window provably IS the whole list.
    count: complete ? sorted.filter(PROMPT_VIEW_MATCH[id]).length : undefined,
  }));

  const byKey = React.useMemo(
    () => new Map(decorated.map((d) => [vkey(d.version), d] as const)),
    [decorated],
  );
  const fromVersion = fromKey ? (byKey.get(fromKey)?.version ?? null) : null;
  const toVersion = toKey ? (byKey.get(toKey)?.version ?? null) : null;
  // The diff endpoint is namespace-scoped by contract (`?from=` names a version
  // in the SAME namespace), so a cross-namespace pair is refused here with a
  // reason rather than sent off to earn a 404.
  const sameNamespace =
    fromVersion !== null &&
    toVersion !== null &&
    fromVersion.namespace === toVersion.namespace;

  // Prime the picker once, with the first pair the table says is worth reading.
  // Priming CLAIMS nothing: no request is made and the reader stays idle until
  // Compare (or a row's "Review the change") is pressed — it only spares the
  // reader from operating two empty selects to reach the page's whole point.
  const primedRef = React.useRef(false);
  React.useEffect(() => {
    if (primedRef.current) return;
    const first = sorted.find((d) => d.previous !== null);
    if (!first?.previous) return;
    primedRef.current = true;
    setFromKey(vkey(first.previous));
    setToKey(vkey(first.version));
  }, [sorted]);

  const runDiff = React.useCallback(
    async (from: PromptVersionSummary, to: PromptVersionSummary) => {
      setDiffState({ kind: "loading" });
      try {
        const result = await api.promptVersionDiff(
          to.namespace,
          to.name,
          from.name,
        );
        if (result === null) {
          // 501 — no resolver configured. Calm state, NOT an error.
          setDiffState({ kind: "not-configured" });
          return;
        }
        setDiffState({ kind: "ready", diff: result, from: from.name, to: to.name });
      } catch (err) {
        if (err instanceof ApiError) {
          if (err.isForbidden) {
            setDiffState({ kind: "error", message: err.message, forbidden: true });
            return;
          }
          if (err.isNotFound) {
            // 404 — version/ref not found. Distinct from every other branch.
            setDiffState({ kind: "not-found", message: err.message });
            return;
          }
          if (err.status === 502) {
            // 502 — the resolver was found and the resolve call failed. Retryable.
            setDiffState({ kind: "resolve-failed", message: err.message });
            return;
          }
          setDiffState({ kind: "error", message: err.message });
          return;
        }
        setDiffState({
          kind: "error",
          message: err instanceof Error ? err.message : "diff request failed",
        });
      }
    },
    [],
  );

  // The row's Next step drives the reader: it sets the pair AND fetches, so the
  // link never leaves the user in front of a picker they have to re-operate.
  const reviewChange = React.useCallback(
    (d: Decorated) => {
      if (!d.previous) return;
      setFromKey(vkey(d.previous));
      setToKey(vkey(d.version));
      const el = readerRef.current;
      if (el && typeof el.scrollIntoView === "function") {
        el.scrollIntoView({ behavior: "smooth", block: "start" });
      }
      void runDiff(d.previous, d.version);
    },
    [runDiff],
  );

  async function handleDelete() {
    if (!deleteTarget) return;
    setDeleteState({ kind: "deleting" });
    try {
      await api.removePromptVersion(deleteTarget.namespace, deleteTarget.name);
      setDeleteState({ kind: "idle" });
      setDeleteTarget(null);
      toast({ variant: "success", title: `Deleted ${deleteTarget.name}` });
      load();
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? err.message
          : err instanceof Error
            ? err.message
            : "delete failed";
      setDeleteState({ kind: "error", message: msg });
    }
  }

  const error: DataTableError | null =
    page.kind === "error"
      ? {
          message: page.message,
          forbidden: page.forbidden,
          resource: "prompt versions",
          onRetry: page.forbidden ? undefined : () => load(),
        }
      : null;

  // §4.4 resource-list budget, in visual order. Version, Standing and Next step
  // are priority 1 and survive every width.
  const columns: Column<Decorated>[] = [
    {
      id: "version",
      header: "Version",
      priority: 1,
      className: "max-w-[20rem]",
      cell: (d) => (
        <div data-testid={`prompt-version-${d.version.name}`}>
          <CellEntity name={d.version.name} namespace={d.version.namespace} />
        </div>
      ),
    },
    {
      id: "prompt",
      header: "Prompt",
      priority: 3,
      className: "max-w-[16rem]",
      cell: (d) => (
        <span
          className="block truncate font-mono text-xs text-secondary-foreground"
          title={d.version.promptName}
        >
          {d.version.promptName}
        </span>
      ),
    },
    {
      id: "ref",
      header: "Ref",
      priority: 4,
      cell: (d) => <CellId id={d.version.ref} />,
    },
    {
      id: "created",
      header: "Created",
      priority: 4,
      cell: (d) =>
        d.version.createdAt ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums text-faint"
            title={d.version.createdAt}
          >
            {formatStamp(d.version.createdAt)}
          </span>
        ) : (
          <UnknownValue title="No creation time was recorded for this version — unknown, not old." />
        ),
    },
    {
      id: "standing",
      header: "Standing",
      priority: 1,
      className: "w-[9rem]",
      // Where the version sits in its own prompt's history — a fact about THIS
      // list, never a claim about what production is serving. `unknown` is a
      // real third answer, not a dressed-up "superseded".
      cell: (d) =>
        d.standing === "unknown" ? (
          <UnknownValue title="Without a recorded creation time this list cannot say where the version sits in its prompt's history." />
        ) : (
          <span
            className={cn(
              "whitespace-nowrap text-sm",
              d.standing === "newest" ? "font-medium" : "text-faint",
            )}
            title={
              d.standing === "newest"
                ? `The most recently created of the ${d.siblings} version${d.siblings === 1 ? "" : "s"} of this prompt in this list. Which version an agent runs is set on the agent.`
                : "A newer version of this prompt exists in this list."
            }
          >
            {d.standing === "newest" ? "newest" : "superseded"}
          </span>
        ),
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4) — it is the page's point.
      priority: 1,
      className: "w-[12rem]",
      cell: (d) => (
        <NextStepLink
          label={d.next.label}
          tone={d.next.tone}
          onClick={d.next.tone === "none" ? undefined : () => reviewChange(d)}
          ariaLabel={
            d.previous
              ? `Review what changed between ${d.previous.name} and ${d.version.name}`
              : undefined
          }
          testId={`next-step-${d.version.name}`}
        />
      ),
    },
  ];

  const chipEmptied = all.length > 0 && visible.length === 0 && view !== "all";
  const empty: EmptyStateProps = chipEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: PROMPT_VIEW_EMPTY[view as Exclude<PromptView, "all">].title,
        description: PROMPT_VIEW_EMPTY[view as Exclude<PromptView, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: complete ? all.length : undefined,
        countNoun: "versions",
      }
    : {
        icon: GitBranch,
        title: "No prompt versions yet",
        description:
          "A prompt version is one reviewable revision of an agent's instructions. Roll a new one here and an agent pinned to that prompt picks it up — so a prompt change is a change you can read before it ships.",
        action: canCreate
          ? {
              label: "New version",
              icon: Sparkles,
              onClick: () => setShowNewForm(true),
            }
          : undefined,
      };

  const closing = page.kind === "ready" ? closingLine(sorted, complete) : null;
  const metaLine =
    page.kind === "ready"
      ? complete
        ? `${all.length} version${all.length === 1 ? "" : "s"}`
        : `${all.length} on this page`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="prompts-page">
      <PageHeader
        title="Prompts"
        meta={metaLine}
        loading={page.kind === "loading"}
        lede="Reusable, versioned instructions you can attach to an agent. Sorted by what is waiting to be read: the newest version of each prompt sits on top, with the change it introduced one click away."
        actionsSlot={
          <>
            <Button
              variant="ghost"
              size="icon"
              onClick={() => load()}
              aria-label="Refresh prompts"
              data-testid="prompts-refresh"
            >
              <RefreshCw className="h-4 w-4" />
            </Button>
            {canCreate && (
              <Button
                size="sm"
                className="text-sm"
                onClick={() => setShowNewForm(true)}
                data-testid="prompts-new-btn"
              >
                <Sparkles className="h-4 w-4" />
                New version
              </Button>
            )}
          </>
        }
      />

      {all.length > 0 && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as PromptView)}
          label="Filter prompt versions"
          className="min-w-0"
        />
      )}

      {/* The backend-cannot-answer state for this table (§7 A1): the list carries
          no binding data, so "newest" is a fact about the list and nothing more. */}
      {page.kind === "ready" && all.length > 0 && (
        <div data-testid="prompts-quiet-note">
          <QuietNote title="Which version is live isn’t in this list.">
            The prompt registry records what each version <em>is</em>, not which
            agent runs it — that binding is set on the agent, under{" "}
            <span className="font-mono">Prompt version</span>. “Newest” below
            therefore means the most recently created version in this list, and
            never “the one in production”. Nothing here is estimated — the
            binding is simply absent.
          </QuietNote>
        </div>
      )}

      <DataTable<Decorated>
        columns={columns}
        rows={visible}
        rowKey={(d) => vkey(d.version)}
        loading={page.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter versions by name or prompt…"
        ariaLabel="Prompt versions"
        tableClassName="min-w-[52rem]"
        onRowClick={(d) => (d.previous ? reviewChange(d) : undefined)}
        rowActions={
          canDelete
            ? (d) => (
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-faint hover:text-destructive"
                  onClick={() => setDeleteTarget(d.version)}
                  data-testid={`prompt-delete-${d.version.name}`}
                >
                  Delete
                </Button>
              )
            : undefined
        }
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {/* ── The diff reader ─────────────────────────────────────────────────
          A two-version picker over a side-by-side code well. The well owns its
          own horizontal scroll (§4.6): a long prompt line widens the WELL, and
          never the page. */}
      {page.kind === "ready" && all.length > 0 && (
        <section ref={readerRef} className="min-w-0 space-y-4">
          <SectionHeader
            title="Compare two versions"
            lede="A line-by-line diff of the resolved prompt text. Both versions must live in the same namespace — the resolver compares within one, not across."
          />

          <div className="grid min-w-0 gap-4 sm:grid-cols-2">
            <div className="min-w-0 space-y-1.5">
              <Label htmlFor="diff-from">From (the older version)</Label>
              <select
                id="diff-from"
                data-testid="diff-from-input"
                value={fromKey}
                onChange={(e) => setFromKey(e.target.value)}
                className="h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <option value="">Choose a version…</option>
                {all.map((v) => (
                  <option key={vkey(v)} value={vkey(v)}>
                    {v.name} · {v.namespace}
                  </option>
                ))}
              </select>
            </div>
            <div className="min-w-0 space-y-1.5">
              <Label htmlFor="diff-to">To (the newer version)</Label>
              <select
                id="diff-to"
                data-testid="diff-to-input"
                value={toKey}
                onChange={(e) => setToKey(e.target.value)}
                className="h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <option value="">Choose a version…</option>
                {all.map((v) => (
                  <option key={vkey(v)} value={vkey(v)}>
                    {v.name} · {v.namespace}
                  </option>
                ))}
              </select>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-3">
            <Button
              size="sm"
              disabled={!sameNamespace || diffState.kind === "loading"}
              onClick={() => {
                if (fromVersion && toVersion && sameNamespace) {
                  void runDiff(fromVersion, toVersion);
                }
              }}
              data-testid="diff-compare-btn"
            >
              {diffState.kind === "loading" ? (
                <>
                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  Comparing…
                </>
              ) : (
                "Compare"
              )}
            </Button>
            {fromVersion && toVersion && !sameNamespace && (
              <p className="text-sm text-faint">
                Those two versions are in different namespaces, so the resolver
                can’t compare them.
              </p>
            )}
          </div>

          {diffState.kind === "idle" && (
            <p className="text-sm text-faint">
              Nothing is compared yet. Press Compare, or use “Review the change”
              on any row above.
            </p>
          )}

          {diffState.kind !== "idle" && (
            <DiffOutput
              state={diffState}
              onRetry={() => {
                if (fromVersion && toVersion && sameNamespace) {
                  void runDiff(fromVersion, toVersion);
                }
              }}
            />
          )}
        </section>
      )}

      {showNewForm && (
        <NewPromptVersionForm
          onClose={() => setShowNewForm(false)}
          onCreated={() => {
            setShowNewForm(false);
            load();
          }}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          open
          destructive
          title={`Delete ${deleteTarget.name}?`}
          description={`This will delete the prompt version "${deleteTarget.name}" in namespace "${deleteTarget.namespace}". This cannot be undone.`}
          confirmLabel={deleteState.kind === "deleting" ? "Deleting…" : "Delete"}
          busy={deleteState.kind === "deleting"}
          onConfirm={handleDelete}
          onCancel={() => {
            setDeleteTarget(null);
            setDeleteState({ kind: "idle" });
          }}
          impact={
            deleteState.kind === "error" ? (
              <p className="text-sm text-destructive" role="alert">
                {deleteState.message}
              </p>
            ) : undefined
          }
        />
      )}
    </div>
  );
}

// ---- DiffOutput ---------------------------------------------------------------
// Each degrade branch is DISTINCT, and none of them draws a diff.

interface DiffOutputProps {
  state: DiffState;
  onRetry: () => void;
}

function DiffOutput({ state, onRetry }: DiffOutputProps) {
  if (state.kind === "loading") {
    return (
      <p
        className="flex items-center gap-2 text-sm text-faint"
        data-testid="prompt-diff-loading"
      >
        <Loader2 className="h-4 w-4 animate-spin" />
        Comparing…
      </p>
    );
  }

  // 501 — no resolver configured. Calm, and NOT an error: nothing broke, this
  // install simply has no way to fetch the prompt text (§7.1).
  if (state.kind === "not-configured") {
    return (
      <div data-testid="prompt-diff-not-configured">
        <QuietNote title="Prompt resolution is not configured on this install.">
          Comparing two versions means fetching each one’s text from its git
          pointer, and no resolver is wired up here — ask your operator to enable
          the textual resolver. The versions above are real and unaffected.
          Nothing here is estimated: the diff is simply absent, not empty.
        </QuietNote>
      </div>
    );
  }

  // 404 — one of the two versions (or its git pointer) does not resolve.
  if (state.kind === "not-found") {
    return (
      <div
        className="rounded-md border border-border bg-surface-2 px-4 py-3"
        data-testid="prompt-diff-not-found"
        role="alert"
      >
        <p className="text-sm font-medium">Version or ref not found</p>
        <p className="mt-1 font-mono text-xs text-faint">{state.message}</p>
      </div>
    );
  }

  // 502 — the resolver was found and the resolve call failed. Degraded but
  // retryable, which is exactly what warn means (§2.2).
  if (state.kind === "resolve-failed") {
    return (
      <div
        className="flex flex-wrap items-center gap-3 rounded-md border border-warning bg-warning-surface px-4 py-3"
        data-testid="prompt-diff-resolve-failed"
      >
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-warning">Resolve failed</p>
          <p className="mt-1 font-mono text-xs text-faint">{state.message}</p>
        </div>
        <Button size="sm" variant="outline" onClick={onRetry}>
          Retry
        </Button>
      </div>
    );
  }

  // Everything else (network, 403, …).
  if (state.kind === "error") {
    return (
      <p
        className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
        role="alert"
        data-testid="prompt-diff-error"
      >
        {state.message}
      </p>
    );
  }

  if (state.kind !== "ready") return null;

  const { diff, from, to } = state;
  // A 200 whose body carried no line array is NOT "no differences" — that would
  // be the one lie this reader exists to avoid. Say what happened instead.
  if (!Array.isArray(diff.lines)) {
    return (
      <div data-testid="prompt-diff-unreadable">
        <QuietNote title="The resolver answered, and the console couldn’t read its diff.">
          The response carried no line-by-line diff, so there is nothing to
          render. Nothing here is estimated — an unreadable answer is not the
          same as an identical pair of versions, and this reader will not show
          one as the other.
        </QuietNote>
      </div>
    );
  }

  const rows = pairLines(diff.lines);

  return (
    <div className="min-w-0 space-y-2" data-testid="prompt-diff">
      {/* resolveMode is ALWAYS stated: this is a TEXTUAL line diff, never a
          semantic one, and a reader must not infer more from it than that. */}
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="muted" data-testid="prompt-diff-resolve-mode">
          resolve mode: {diff.resolveMode}
        </Badge>
        <span className="font-mono text-xs text-faint">
          {from} → {to}
        </span>
      </div>
      {rows.length === 0 ? (
        <p className="text-sm text-faint">
          No differences found — the two versions resolve to identical text.
        </p>
      ) : (
        // The well, and its OWN scroll container (§4.6): the inner grid carries
        // the min-width, so a long prompt line scrolls HERE and can never widen
        // the document.
        <div
          className="min-w-0 max-w-full overflow-x-auto rounded-lg border bg-surface-3"
          data-testid="prompt-diff-lines"
        >
          <div className="min-w-[44rem]">
            {/* The column heads name the two versions. They stay in the version's
                OWN casing — a resource name is a machine string, and the
                eyebrow register's uppercase would quietly misspell it. */}
            <div className="grid grid-cols-2 border-b border-border">
              <div className="truncate border-r border-border px-3 py-1.5 font-mono text-2xs font-medium tracking-wide text-faint">
                {from}
              </div>
              <div className="truncate px-3 py-1.5 font-mono text-2xs font-medium tracking-wide text-faint">
                {to}
              </div>
            </div>
            {rows.map((row, i) => (
              <div key={i} className="grid grid-cols-2">
                <DiffCell line={row.left} side="left" />
                <DiffCell line={row.right} side="right" />
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

/** One rendered row of the side-by-side well. A missing side has no counterpart. */
interface DiffRow {
  left?: PromptDiffLine;
  right?: PromptDiffLine;
}

/**
 * Turn the resolver's unified line diff into side-by-side rows.
 *
 * This is a re-presentation, not an interpretation: a context line belongs to
 * both sides, a run of removals pairs with the run of additions that follows
 * it, and a side with no counterpart renders BLANK. The blank is the honest
 * rendering of "the unified diff had no line here" — it is never filled in with
 * the neighbouring text to make the columns look balanced.
 */
export function pairLines(lines: PromptDiffLine[]): DiffRow[] {
  const rows: DiffRow[] = [];
  let i = 0;
  while (i < lines.length) {
    if (lines[i].op === " ") {
      rows.push({ left: lines[i], right: lines[i] });
      i++;
      continue;
    }
    const start = i;
    const removed: PromptDiffLine[] = [];
    while (i < lines.length && lines[i].op === "-") removed.push(lines[i++]);
    const added: PromptDiffLine[] = [];
    while (i < lines.length && lines[i].op === "+") added.push(lines[i++]);
    if (i === start) {
      // An op outside the contract. Show it on both sides rather than drop it —
      // a line the console cannot classify is still a line the resolver sent.
      rows.push({ left: lines[i], right: lines[i] });
      i++;
      continue;
    }
    const height = Math.max(removed.length, added.length);
    for (let k = 0; k < height; k++) {
      rows.push({ left: removed[k], right: added[k] });
    }
  }
  return rows;
}

/**
 * One side of one row.
 *
 * Change is marked by the gutter glyph and a 2px rule in the semantic hue, with
 * the text itself in ordinary ink. Filling every changed line with a crit/ok
 * surface would make a routine prompt edit read as a page full of alarms —
 * §2.2's "annotation, not alarm", applied where it is most tempting to break.
 */
function DiffCell({ line, side }: { line?: PromptDiffLine; side: "left" | "right" }) {
  const kind =
    line === undefined
      ? "absent"
      : line.op === "+"
        ? "add"
        : line.op === "-"
          ? "del"
          : "context";
  return (
    <div
      className={cn(
        "border-l-2 px-3 py-0.5 font-mono text-xs leading-relaxed",
        side === "left" && "border-r border-r-border",
        kind === "add" && "border-l-success",
        kind === "del" && "border-l-destructive",
        kind === "context" && "border-l-transparent",
        // A side with no counterpart takes a plane shift, not a hue: it is an
        // absence, and an absence is not a state.
        kind === "absent" && "border-l-transparent bg-surface-2",
      )}
      data-testid={`diff-line-${kind}`}
    >
      {line === undefined ? (
        <span className="sr-only">no corresponding line</span>
      ) : (
        <>
          <span
            aria-hidden="true"
            className={cn(
              "mr-2 inline-block w-2 select-none",
              kind === "add" && "text-success",
              kind === "del" && "text-destructive",
              kind === "context" && "text-ghost",
            )}
          >
            {line.op === " " ? " " : line.op}
          </span>
          <span className="whitespace-pre-wrap break-words">{line.content}</span>
        </>
      )}
    </div>
  );
}

// ---- NewPromptVersionForm ----------------------------------------------------

interface NewPromptVersionFormProps {
  onClose: () => void;
  onCreated: () => void;
}

type FormState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "error"; message: string };

function NewPromptVersionForm({ onClose, onCreated }: NewPromptVersionFormProps) {
  const [ref, setRef] = React.useState("");
  const [promptName, setPromptName] = React.useState("");
  const [name, setName] = React.useState("");
  const [namespace, setNamespace] = React.useState("");
  const [content, setContent] = React.useState("");
  const [formState, setFormState] = React.useState<FormState>({ kind: "idle" });

  const { namespace: shellNs } = useNamespace();
  const { reprobe } = useCapabilities();
  const { toast } = useToast();
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  React.useEffect(() => {
    if (shellNs) setNamespace(shellNs);
  }, [shellNs]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!ref.trim() || !promptName.trim()) return;
    setFormState({ kind: "submitting" });
    try {
      const created = await api.createPromptVersion({
        name: name.trim() || undefined,
        namespace: namespace.trim() || undefined,
        ref: ref.trim(),
        promptName: promptName.trim(),
        content: content.trim() || undefined,
      });
      toast({ variant: "success", title: `Created ${created.name}` });
      onCreated();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setFormState({ kind: "error", message: `Not allowed: ${err.message}` });
          return;
        }
        setFormState({ kind: "error", message: err.message });
        return;
      }
      setFormState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
      });
    }
  }

  const busy = formState.kind === "submitting";

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label="New prompt version"
      data-testid="new-prompt-version-form"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative flex max-h-[85vh] w-full max-w-lg flex-col overflow-y-auto rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h3 className="font-serif text-lg font-medium tracking-snug">
          New prompt version
        </h3>
        <p className="mt-1 text-sm text-faint">
          One reviewable revision of a prompt, pointing at the git ref its text
          lives at.
        </p>
        <form onSubmit={handleSubmit} className="mt-4 space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="pv-prompt-name">
              Prompt name <span className="text-destructive">*</span>
            </Label>
            <Input
              id="pv-prompt-name"
              value={promptName}
              onChange={(e) => setPromptName(e.target.value)}
              placeholder="my-system-prompt"
              required
              data-testid="pv-prompt-name-input"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pv-ref">
              Ref <span className="text-destructive">*</span>
            </Label>
            <Input
              id="pv-ref"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              placeholder="v1 or abc1234"
              required
              data-testid="pv-ref-input"
            />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="pv-name">Name (optional)</Label>
              <Input
                id="pv-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="auto-generated"
                data-testid="pv-name-input"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pv-namespace">Namespace</Label>
              <Input
                id="pv-namespace"
                value={namespace}
                onChange={(e) => setNamespace(e.target.value)}
                placeholder="default"
                data-testid="pv-namespace-input"
              />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pv-content">Content (optional)</Label>
            {/* A prompt body is code-shaped: it goes in the sunk well register,
                mono and resizable, not in a prose field (§4.5). */}
            <textarea
              id="pv-content"
              className="min-h-[80px] w-full resize-y rounded-md border border-input bg-surface-3 px-3 py-2 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={content}
              onChange={(e) => setContent(e.target.value)}
              placeholder="You are a helpful assistant…"
              data-testid="pv-content-input"
            />
          </div>
          {formState.kind === "error" && (
            <p
              className="rounded-md border border-destructive bg-destructive-surface px-3 py-2 text-sm text-destructive"
              role="alert"
              data-testid="pv-form-error"
            >
              {formState.message}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !ref.trim() || !promptName.trim()}>
              {busy ? (
                <>
                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  Creating…
                </>
              ) : (
                "Create"
              )}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
