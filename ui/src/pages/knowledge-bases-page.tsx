import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { BookOpen, Filter, Upload, Play, Search } from "lucide-react";
import { useNavigate, useParams } from "react-router-dom";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  ErrorState,
  FilterChipRow,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  Skeleton,
  SkeletonCard,
  StatusBadge,
  UnknownValue,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type KeyValueItem,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  api,
  ApiError,
  type KBSummary,
  type KBDetail,
  type KBSearchHit,
} from "@/lib/api";

// KnowledgeBasesPage — the KnowledgeBase CR list surface (m68.13, ADR 0061;
// re-housed on the editorial system in M151 as archetype A1).
//
// Enterprise-RAG surface: upload docs → ingest → watch phase → test-query with citations.
// Read-only list (caller-scoped, ADR 0011): each row is a KnowledgeBase CR. A KB is authored
// via YAML/kubectl, so the header carries NO create action — the console does not offer an
// affordance the platform does not have. A row-click navigates to the detail page
// (/knowledgebases/:ns/:name) which includes: document upload, ingest trigger, and test-query.
//
// A1 contract (spec §6.1): PageHeader → FilterChipRow → DataTable → ClosingNote, sorted by
// what is blocking, with a "Next step" column that names the user's next action.
//
// Honesty (spec §7): the four degraded states are four DIFFERENT things here —
//   • first-run empty      → teaching EmptyState ("No knowledge bases")
//   • filtered to nothing  → EmptyState intent="filtered" naming the view + a way back
//   • forbidden            → the DataTable forbidden variant, resource-named
//   • cannot be answered   → a corpus that has never been ingested has NO last-ingested
//                            date; it renders `—` with a reason, never a fabricated date,
//                            and one QuietNote above the table says so once. Chunk and size
//                            go through QuantityValue so a field the BFF did not send reads
//                            `—` rather than a zero the store never claimed.
//
// data-testid contract:
//   knowledge-bases-page  — root container
//   kb-table              — the DataTable (aria-label="KnowledgeBases")
//   kb-quiet-note         — the never-ingested QuietNote

// formatBytes renders a byte count as a human-readable string (e.g. "1.2 MB").
function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`;
}

// `formatDate` is gone: it answered an absent timestamp with a bare "—" and no
// reason, which is the one thing this milestone's honesty rules forbid — a dash
// a reader cannot tell apart from a zero. Both surfaces now read a timestamp
// through `formatStamp` (present) or `UnknownValue` (absent, with the reason in
// its title).

// formatStamp is the §4.5 table register for a timestamp: same year → "Aug 29",
// older → "2025-08-29". The full ISO string always rides along in `title`, so the
// compact form never loses information.
function formatStamp(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  if (d.getFullYear() !== new Date().getFullYear()) return d.toISOString().slice(0, 10);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

// ──────────────────────────────────────────────────────────────────────────────
// List page
// ──────────────────────────────────────────────────────────────────────────────

type ListLoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: KBSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

/** Attention-first order (§6.1 A1): what is blocking sorts above what is fine. */
const ATTENTION: Record<StatusTone, number> = {
  failed: 0,
  waiting: 1,
  progressing: 2,
  ready: 3,
  draft: 4,
};

interface NextStep {
  label: string;
  tone: NextStepTone;
}

/**
 * The row's next action, verb-first and ≤22 characters (§7.2). It describes what
 * the USER does, never what the system is: "Fix the ingestion", not "Failed".
 * A corpus the list has nothing to ask of returns the inert `none` tone, which
 * NextStepLink renders as "Nothing needed" and the sort puts last.
 */
function kbNextStep(kb: KBSummary): NextStep {
  const phase = (kb.phase ?? "").toLowerCase();
  if (!kb.embeddingRoute) return { label: "Set an embedding route", tone: "default" };
  if (phase === "failed") return { label: "Fix the ingestion", tone: "crit" };
  if (phase === "budgetexceeded") return { label: "Raise the ingest cap", tone: "crit" };
  if (kb.documentCount === 0) return { label: "Upload a document", tone: "default" };
  if (phase === "partiallyingested") return { label: "Finish the ingestion", tone: "default" };
  const tone = resolveStatus(phase === "ready", kb.phase).tone;
  // Still converging on its own — the machine is working, nobody is blocked.
  if (tone === "progressing") return { label: "", tone: "none" };
  if (kb.chunkCount === 0) return { label: "Start the ingestion", tone: "default" };
  return { label: "", tone: "none" };
}

const KB_VIEWS = ["all", "attention", "serving"] as const;
type KBView = (typeof KB_VIEWS)[number];

export function KnowledgeBasesPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [view, setView] = useState<KBView>("all");
  const [loadState, setLoadState] = useState<ListLoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listKnowledgeBases(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const all = useMemo(
    () => (loadState.kind === "ready" ? loadState.items : []),
    [loadState],
  );

  // Attention-first sort (§6.1): rows that need a person above rows that do not,
  // "Nothing needed" last, then by how bad the state is, then by name.
  const sorted = useMemo(() => {
    return [...all].sort((a, b) => {
      const sa = kbNextStep(a);
      const sb = kbNextStep(b);
      const rank = nextStepRank(sa.tone) - nextStepRank(sb.tone);
      if (rank !== 0) return rank;
      const ta = resolveStatus(a.phase === "Ready", a.phase).tone;
      const tb = resolveStatus(b.phase === "Ready", b.phase).tone;
      if (ATTENTION[ta] !== ATTENTION[tb]) return ATTENTION[ta] - ATTENTION[tb];
      return a.name.localeCompare(b.name);
    });
  }, [all]);

  const needsSomeone = useMemo(
    () => sorted.filter((kb) => kbNextStep(kb).tone !== "none"),
    [sorted],
  );

  const q = query.trim().toLowerCase();
  const items = useMemo(() => {
    const byView =
      view === "attention"
        ? needsSomeone
        : view === "serving"
          ? sorted.filter((kb) => kbNextStep(kb).tone === "none")
          : sorted;
    return q ? byView.filter((kb) => kb.name.toLowerCase().includes(q)) : byView;
  }, [view, q, sorted, needsSomeone]);

  // The list endpoint returns every KnowledgeBase the caller can see in one
  // response (no cursor), so these counts are the backend's complete answer for
  // this scope — not a count of the rows that happen to be rendered.
  // Built FROM the view union rather than beside it, so the chips, their order
  // and the type cannot drift apart — a chip whose id is not a view stops
  // compiling instead of silently filtering to nothing.
  const kbViewLabel: Record<KBView, string> = {
    all: "Everything",
    attention: "Needs a person",
    serving: "Serving",
  };
  const kbViewCount: Record<KBView, number> = {
    all: all.length,
    attention: needsSomeone.length,
    serving: all.length - needsSomeone.length,
  };
  const chips: FilterChip[] = KB_VIEWS.map((id) => ({
    id,
    label: kbViewLabel[id],
    count: kbViewCount[id],
  }));
  const viewLabel = chips.find((c) => c.id === view)?.label ?? "Everything";

  // Corpora that have never run an ingestion. Their last-ingested is ABSENT, not
  // zero and not "now" — the cell renders a dash and this note says why, once.
  const neverIngested = items.filter((kb) => !kb.lastIngestedAt).length;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "knowledge bases",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const kbPath = (kb: KBSummary) =>
    `/knowledgebases/${encodeURIComponent(kb.namespace)}/${encodeURIComponent(kb.name)}`;

  const columns: Column<KBSummary>[] = [
    {
      id: "name",
      header: "Knowledge base",
      priority: 1,
      className: "max-w-[20rem]",
      cell: (kb) => <CellEntity name={kb.name} namespace={kb.namespace} />,
    },
    {
      id: "embeddingRoute",
      header: "Embedding route",
      priority: 4,
      className: "max-w-[14rem]",
      cell: (kb) =>
        kb.embeddingRoute ? (
          <span
            className="block truncate font-mono text-xs text-faint"
            title={kb.embeddingRoute}
          >
            {kb.embeddingRoute}
          </span>
        ) : (
          // Declared but never wired — the `open` tag, not a status hue (§2.5).
          <Badge variant="open">no route</Badge>
        ),
    },
    {
      id: "lastIngested",
      header: "Last ingested",
      priority: 4,
      cell: (kb) =>
        kb.lastIngestedAt ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums text-faint"
            title={kb.lastIngestedAt}
          >
            {formatStamp(kb.lastIngestedAt)}
          </span>
        ) : (
          <UnknownValue title="Never ingested — this corpus has no ingestion date, so none is shown." />
        ),
    },
    {
      id: "phase",
      header: "State",
      priority: 1,
      cell: (kb) => <StatusBadge ready={kb.phase === "Ready"} phase={kb.phase} />,
    },
    {
      id: "chunks",
      header: "Chunks",
      priority: 3,
      numeric: true,
      // Passed straight through: a field the BFF did not send arrives as
      // `undefined` and QuantityValue renders `—`, never a zero (§7.1).
      cell: (kb) => <QuantityValue value={kb.chunkCount} />,
    },
    {
      id: "size",
      header: "Size",
      priority: 3,
      numeric: true,
      cell: (kb) => <QuantityValue value={kb.sizeBytes} format={formatBytes} />,
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      cell: (kb) => {
        const step = kbNextStep(kb);
        return (
          <NextStepLink
            label={step.label}
            tone={step.tone}
            to={step.tone === "none" ? undefined : kbPath(kb)}
            ariaLabel={step.tone === "none" ? undefined : `${step.label} for ${kb.name}`}
            testId={`kb-next-${kb.name}`}
          />
        );
      },
    },
  ];

  const empty: EmptyStateProps =
    view !== "all" && all.length > 0
      ? {
          intent: "filtered",
          icon: Filter,
          title: "Nothing in this view",
          description: `No knowledge base is in “${viewLabel}” right now.`,
          action: {
            label: "Show everything",
            variant: "outline",
            onClick: () => setView("all"),
          },
          totalCount: all.length,
          countNoun: "knowledge bases",
        }
      : {
          icon: BookOpen,
          title: "No knowledge bases",
          description:
            "No knowledge bases yet. A knowledge base is a managed RAG corpus your agents retrieve from — apply a KnowledgeBase resource, upload documents, and they are chunked, embedded, and searchable.",
        };

  const totalChunks = all.reduce(
    (n, kb) => n + (typeof kb.chunkCount === "number" ? kb.chunkCount : 0),
    0,
  );
  const serving = all.length - needsSomeone.length;

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="knowledge-bases-page">
      <PageHeader
        title="Knowledge bases"
        loading={loadState.kind === "loading"}
        meta={
          all.length > 0
            ? `${all.length} corpora · ${totalChunks.toLocaleString()} chunks`
            : undefined
        }
        lede="Managed RAG corpora your agents retrieve from. Each one is chunked, embedded into a pgvector store, and authored as a KnowledgeBase resource — so this list reads it, and never invents what it has not been told."
      />

      {all.length > 0 && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as KBView)}
          label="Filter knowledge bases"
        />
      )}

      {neverIngested > 0 && (
        <div data-testid="kb-quiet-note">
          <QuietNote title="Some corpora have never been ingested.">
            {neverIngested === 1 ? "One" : neverIngested} of the {items.length} listed
            here has no ingestion behind it, so “Last ingested” reads{" "}
            <span className="font-mono">—</span> rather than a date. The chunk and size
            figures are what the store actually holds. Nothing here is estimated — the
            missing date is simply absent.
          </QuietNote>
        </div>
      )}

      <DataTable<KBSummary>
        columns={columns}
        rows={items}
        rowKey={(kb) => `${kb.namespace}/${kb.name}`}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter knowledge bases by name…"
        ariaLabel="KnowledgeBases"
        tableClassName="min-w-[44rem]"
        onRowClick={(kb) => navigate(kbPath(kb))}
        empty={empty}
      />

      {all.length > 0 && (
        <ClosingNote>
          {needsSomeone.length === 0
            ? `All ${all.length} corpora are serving ${totalChunks.toLocaleString()} chunks and need nothing.`
            : `${needsSomeone.length} of ${all.length} corpora are waiting on a person. The other ${serving} are serving ${totalChunks.toLocaleString()} chunks between them.`}
        </ClosingNote>
      )}
    </div>
  );
}

// ──────────────────────────────────────────────────────────────────────────────
// Detail page
// ──────────────────────────────────────────────────────────────────────────────

type DetailLoadState =
  | { kind: "loading" }
  | { kind: "ready"; kb: KBDetail }
  // `forbidden` is carried separately because a permission boundary is a
  // different STATE from a failure (§7): one is calm and names the role to ask
  // for, the other offers a retry.
  | { kind: "error"; message: string; forbidden: boolean };

type UploadState =
  | { kind: "idle" }
  | { kind: "uploading" }
  | { kind: "success"; ref: string }
  | { kind: "error"; message: string };

type IngestState =
  | { kind: "idle" }
  | { kind: "running" }
  | { kind: "success"; runId: string }
  | { kind: "error"; message: string };

type SearchState =
  | { kind: "idle" }
  | { kind: "searching" }
  | { kind: "results"; hits: KBSearchHit[] }
  | { kind: "error"; message: string }
  | { kind: "unavailable" };

// KBDetailPage — the KnowledgeBase detail surface (m68.13, ADR 0061; re-housed
// on the editorial system in M151 as archetype A2). Route: /knowledgebases/:ns/:name
//
// ── THE PAGE'S ONE IDEA: A CORPUS IS A PIPELINE, NOT A RECORD ──────────────
// Everything a reader does here is one sequence — put documents in, ingest
// them, ask a question and see what comes back — so the main column IS that
// sequence, in that order, and the rail holds the facts the sequence produced.
// The old page interleaved a stat grid, a condition list and three action
// panels in one flat column, which made "what do I do next" a scanning problem.
//
// ── A NEVER-RUN INGESTION IS ABSENT, NOT ZERO (§7.1) ───────────────────────
// This is the honesty rule the surface exists to demonstrate, and the two
// halves of it live one row apart in the rail:
//
//   • "Last ingested" on a corpus that has never ingested is UNKNOWN. It reads
//     `—` with the reason in its title — never a zero, and above all never
//     "now": `new Date(undefined)` is the current time, and a fabricated
//     ingestion date on a retrieval corpus is a lie an operator would act on.
//   • Documents / Chunks / Size are KNOWN ZEROS when the store answers zero,
//     and they render as a real `0`. Zero is a measurement; unknown is the
//     absence of one, and the two never share a glyph.
//
// One QuietNote in the Ingest panel says which of the two a reader is looking
// at, once, rather than leaving the dash to be guessed at.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ────────────────────────────────────
// `GET /api/knowledgebases/{name}` returns the spec, the counts and the
// conditions. It returns NO ingest cost, NO per-document status and NO query
// history — so none is drawn. A 501 from the search proxy is CALM (the install
// has no token service), never an error: a QuietNote, never a red banner.
//
// data-testid contract:
//   kb-detail-page        — root container (ready state)
//   kb-detail-loading     — the loading state
//   kb-detail-error       — the generic error state
//   kb-detail-header      — the page band (name / phase / route)
//   kb-never-ingested     — the never-ingested QuietNote
//   upload-input          — the file input
//   upload-submit         — the upload button
//   upload-result         — success/error message after upload
//   ingest-button         — the ingest trigger
//   ingest-result         — the ingest outcome message
//   query-input           — the test-query text input
//   query-topk            — the topK input
//   query-submit          — the search button
//   query-results         — the results container
//   query-hit-{i}         — one result hit row (0-indexed)
//   query-unavailable     — the 501 calm state

export function KBDetailPage() {
  // No `useNavigate` here any more: the way back is the PageHeader breadcrumb,
  // which is a real link (right-clickable, middle-clickable) rather than a
  // button that calls navigate() — and it renders in every state, including the
  // ones where the old back button did not.
  const { ns, name } = useParams<{ ns: string; name: string }>();

  const [loadState, setLoadState] = useState<DetailLoadState>({ kind: "loading" });
  const [uploadState, setUploadState] = useState<UploadState>({ kind: "idle" });
  const [uploadFile, setUploadFile] = useState<File | null>(null);
  const [ingestState, setIngestState] = useState<IngestState>({ kind: "idle" });
  const [queryText, setQueryText] = useState("");
  const [queryTopK, setQueryTopK] = useState(5);
  const [searchState, setSearchState] = useState<SearchState>({ kind: "idle" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    if (!ns || !name) return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .getKnowledgeBase(name, ns, controller.signal)
      .then((kb) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", kb });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "failed to load KB",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, [ns, name]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  async function onUpload() {
    if (!uploadFile || !name || !ns) return;
    setUploadState({ kind: "uploading" });
    try {
      const result = await api.uploadKBDocument(name, uploadFile, ns);
      setUploadState({ kind: "success", ref: result.documentRef });
      setUploadFile(null);
    } catch (err) {
      setUploadState({
        kind: "error",
        message: err instanceof Error ? err.message : "upload failed",
      });
    }
  }

  async function onIngest() {
    if (!name || !ns) return;
    setIngestState({ kind: "running" });
    try {
      const result = await api.ingestKB(name, ns);
      setIngestState({ kind: "success", runId: result.runId });
      // Reload KB status after a brief delay so the phase flips to Ingesting.
      setTimeout(() => void load(), 1000);
    } catch (err) {
      setIngestState({
        kind: "error",
        message: err instanceof Error ? err.message : "ingestion failed",
      });
    }
  }

  async function onSearch() {
    if (!name || !ns || !queryText.trim()) return;
    setSearchState({ kind: "searching" });
    try {
      const result = await api.searchKnowledgeBase(name, { query: queryText.trim(), topK: queryTopK }, ns);
      setSearchState({ kind: "results", hits: result.results });
    } catch (err) {
      if (err instanceof ApiError && err.isNotImplemented) {
        setSearchState({ kind: "unavailable" });
      } else {
        setSearchState({
          kind: "error",
          message: err instanceof Error ? err.message : "search failed",
        });
      }
    }
  }

  const crumbs = [
    { label: "Knowledge bases", to: "/knowledgebases" },
    { label: name ?? "" },
  ];

  // §7 A2 loading: the header band instantly, a SkeletonCard where each action
  // panel will be, kv-line bars in the rail. The root testid is NOT set here —
  // `kb-detail-page` marks the ready surface, so a test that waits for it waits
  // for the real page rather than resolving against the skeleton.
  if (loadState.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6" data-testid="kb-detail-loading">
        <PageHeader
          breadcrumb={crumbs}
          title={name ?? "Knowledge base"}
          titleMono
          loading
        />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <div className="min-w-0 space-y-5">
            <SkeletonCard />
            <SkeletonCard />
          </div>
          <Card className="min-w-0">
            <PanelHeader title="What it holds" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label="Loading the corpus facts">
                {[0, 1, 2, 3, 4, 5].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  if (loadState.kind === "error") {
    // A permission boundary replaces the page under the header — calm,
    // resource-named, never the raw RBAC string (M100 UI99-403).
    if (loadState.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader breadcrumb={crumbs} title={name ?? ""} titleMono />
          <ForbiddenInline
            title={`You don't have permission to view ${name}.`}
            resource="knowledge bases"
            detail={loadState.message}
          />
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid="kb-detail-error">
        <PageHeader breadcrumb={crumbs} title={name ?? ""} titleMono />
        <ErrorState
          title="The knowledge base didn't load."
          description="Nothing has changed about the corpus itself — only this page failed to read it."
          detail={loadState.message}
          onRetry={load}
        />
      </div>
    );
  }

  const { kb } = loadState;

  // The load-bearing distinction on this page. A corpus with no ingestion
  // behind it has no ingestion DATE — that is unknown. Its counts may still be
  // a real, measured zero. The two are rendered differently on purpose.
  const neverIngested = !kb.lastIngestedAt;

  // What it holds. Every figure passes through QuantityValue, so a field the
  // BFF did not send arrives as `undefined` and reads `—`, while a store that
  // answered zero reads `0` (§7.1).
  const holdings: KeyValueItem[] = [
    { key: "Documents", value: <QuantityValue value={kb.documentCount} />, mono: false },
    { key: "Chunks", value: <QuantityValue value={kb.chunkCount} />, mono: false },
    {
      key: "Size",
      value: <QuantityValue value={kb.sizeBytes} format={formatBytes} />,
      mono: false,
    },
    {
      key: "Last ingested",
      value: kb.lastIngestedAt ? (
        <span className="tabular-nums" title={kb.lastIngestedAt}>
          {formatStamp(kb.lastIngestedAt)}
        </span>
      ) : (
        // The dash WITH its reason — never a zero, and never today's date.
        <UnknownValue title="This corpus has never been ingested, so it has no ingestion date. The date is absent — it is not zero, and it is not now." />
      ),
      mono: false,
    },
  ];

  // The record: how the corpus is configured, as declared.
  const record: KeyValueItem[] = [
    { key: "Corpus", value: kb.name, title: kb.name },
    { key: "Workspace", value: kb.namespace, absent: "not recorded" },
    {
      key: "Embedding route",
      value: kb.embeddingRoute ? (
        <span className="block truncate" title={kb.embeddingRoute}>
          {kb.embeddingRoute}
        </span>
      ) : undefined,
      absent: "not set",
      title:
        "No embedding route is set, so nothing can be embedded and nothing can be retrieved.",
    },
    { key: "Source", value: kb.sourceType, absent: "not recorded" },
    { key: "Chunk size", value: <QuantityValue value={kb.chunkSize} />, mono: false },
    { key: "Chunk overlap", value: <QuantityValue value={kb.chunkOverlap} />, mono: false },
    { key: "Splitter", value: kb.chunkSplitter, absent: "not recorded" },
    {
      key: "Ingestion run",
      value: kb.ingestionRunRef,
      absent: "none recorded",
      title:
        "No ingestion run is recorded against this corpus — nothing has run, or the run predates the field.",
    },
  ];

  return (
    <div className="min-w-0 space-y-6" data-testid="kb-detail-page">
      <div data-testid="kb-detail-header">
        <PageHeader
          breadcrumb={crumbs}
          title={kb.name}
          titleMono
          status={<StatusBadge ready={kb.phase === "Ready"} phase={kb.phase} />}
          meta={`${kb.namespace} · ${kb.embeddingRoute || "no embedding route"}`}
          lede={kb.displayName}
        />
      </div>

      {/* §4.7 hub grid: the pipeline on the left, the facts it produced in the
          300px rail, which stacks UNDER the main column below `lg`. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          {/* 1 — put documents in */}
          <Card className="min-w-0">
            <PanelHeader title="Add a document" />
            <CardContent className="space-y-4">
              <p className="max-w-[64ch] text-sm text-secondary-foreground">
                Uploading puts the file in the corpus&rsquo;s durable bucket. It is
                not searchable until an ingestion chunks, embeds and indexes it —
                that is the next panel, and it is a separate step on purpose.
              </p>
              <div className="flex flex-wrap items-center gap-3">
                <Input
                  type="file"
                  data-testid="upload-input"
                  className="max-w-sm"
                  onChange={(e) => {
                    setUploadFile(e.target.files?.[0] ?? null);
                    setUploadState({ kind: "idle" });
                  }}
                  disabled={uploadState.kind === "uploading"}
                />
                <Button
                  data-testid="upload-submit"
                  onClick={() => void onUpload()}
                  disabled={!uploadFile || uploadState.kind === "uploading"}
                >
                  <Upload className="h-4 w-4" />
                  {uploadState.kind === "uploading" ? "Uploading…" : "Upload"}
                </Button>
              </div>
              {uploadState.kind === "success" && (
                <p className="text-sm text-success" data-testid="upload-result">
                  Uploaded:{" "}
                  <span className="break-all font-mono text-xs">
                    {uploadState.ref}
                  </span>
                </p>
              )}
              {uploadState.kind === "error" && (
                <p
                  className="text-sm text-destructive"
                  role="alert"
                  data-testid="upload-result"
                >
                  {uploadState.message}
                </p>
              )}
            </CardContent>
          </Card>

          {/* 2 — ingest them */}
          <Card className="min-w-0">
            <PanelHeader title="Ingest" />
            <CardContent className="space-y-4">
              <p className="max-w-[64ch] text-sm text-secondary-foreground">
                An ingestion run chunks, embeds and indexes every document in the
                corpus. The corpus moves to{" "}
                <span className="font-mono text-xs">Ingesting</span>, then to{" "}
                <span className="font-mono text-xs">Ready</span> when it finishes.
              </p>
              {/* Said once, where a reader meets the dash's consequence: this
                  corpus retrieves nothing yet. */}
              {neverIngested && (
                <div data-testid="kb-never-ingested">
                  <QuietNote title="This corpus has never been ingested.">
                    No ingestion has run against it, so it has no ingestion date —
                    the rail reads{" "}
                    <span className="font-mono">&mdash;</span> rather than a date,
                    because a fabricated one is worse than none. The document and
                    chunk figures beside it are what the store actually holds, so a{" "}
                    <span className="font-mono">0</span> there is a measurement,
                    not a guess. Until an ingestion runs, a query against this
                    corpus returns nothing.
                  </QuietNote>
                </div>
              )}
              <Button
                data-testid="ingest-button"
                onClick={() => void onIngest()}
                disabled={ingestState.kind === "running"}
              >
                <Play className="h-4 w-4" />
                {ingestState.kind === "running" ? "Starting…" : "Start ingestion"}
              </Button>
              {ingestState.kind === "success" && (
                <p className="text-sm text-success" data-testid="ingest-result">
                  Ingestion run started:{" "}
                  <span className="break-all font-mono text-xs">
                    {ingestState.runId}
                  </span>
                </p>
              )}
              {ingestState.kind === "error" && (
                <p
                  className="text-sm text-destructive"
                  role="alert"
                  data-testid="ingest-result"
                >
                  {ingestState.message}
                </p>
              )}
            </CardContent>
          </Card>

          {/* 3 — ask it something */}
          <Card className="min-w-0">
            <PanelHeader title="Test a query" />
            <CardContent className="space-y-4">
              <p className="max-w-[64ch] text-sm text-secondary-foreground">
                Runs a retrieval query against the corpus&rsquo;s pgvector index and
                shows what an agent would get back — the ranked chunks with their
                citations, so a wrong answer can be traced to the passage that
                caused it.
              </p>
              <div className="flex flex-wrap items-center gap-3">
                <Input
                  data-testid="query-input"
                  className="min-w-[12rem] flex-1"
                  placeholder="Ask what an agent would ask…"
                  value={queryText}
                  onChange={(e) => setQueryText(e.target.value)}
                  disabled={searchState.kind === "searching"}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") void onSearch();
                  }}
                />
                <div className="flex items-center gap-2">
                  <label
                    htmlFor="query-topk"
                    className="whitespace-nowrap font-mono text-2xs uppercase tracking-wide text-faint"
                  >
                    Top K
                  </label>
                  <Input
                    id="query-topk"
                    data-testid="query-topk"
                    type="number"
                    min={1}
                    max={50}
                    className="w-16 tabular-nums"
                    value={queryTopK}
                    onChange={(e) => setQueryTopK(Number(e.target.value) || 5)}
                    disabled={searchState.kind === "searching"}
                  />
                </div>
                <Button
                  data-testid="query-submit"
                  onClick={() => void onSearch()}
                  disabled={!queryText.trim() || searchState.kind === "searching"}
                >
                  <Search className="h-4 w-4" />
                  {searchState.kind === "searching" ? "Searching…" : "Search"}
                </Button>
              </div>

              {/* 501 is CALM: the install has no token service, nothing broke. */}
              {searchState.kind === "unavailable" && (
                <div data-testid="query-unavailable">
                  <QuietNote title="Test queries aren't configured on this install.">
                    Retrieval runs through the token service, and this install has
                    no <span className="font-mono text-xs">TOKEN_SERVICE_URL</span>{" "}
                    set. The corpus itself is fine and agents that reach it through
                    a configured install still retrieve from it — there is simply
                    nothing here to run the query against.
                  </QuietNote>
                </div>
              )}

              {searchState.kind === "error" && (
                <p className="text-sm text-destructive" role="alert">
                  {searchState.message}
                </p>
              )}

              {searchState.kind === "results" && (
                <div data-testid="query-results">
                  {searchState.hits.length === 0 ? (
                    <QuietNote title="Nothing matched.">
                      The index answered, and no chunk in this corpus was close
                      enough to the query to return. That is a real answer, not a
                      failure — try different wording, or a larger Top K.
                    </QuietNote>
                  ) : (
                    <>
                      <SectionHeader
                        title={`${searchState.hits.length} passage${searchState.hits.length === 1 ? "" : "s"} came back`}
                        lede="Ranked by similarity. This is exactly what an agent would be handed."
                        as="h3"
                      />
                      {/* A wide artifact scrolls inside its OWN container so the
                          page never scrolls sideways (§4.6). */}
                      <div className="mt-3 max-h-[28rem] overflow-y-auto rounded-md border border-border">
                        <ul>
                          {searchState.hits.map((hit, i) => (
                            <li
                              key={i}
                              data-testid={`query-hit-${i}`}
                              className="border-b border-border-soft p-3 last:border-0"
                            >
                              <div className="flex flex-wrap items-baseline gap-2">
                                <span
                                  className="min-w-0 truncate font-mono text-xs text-faint"
                                  title={`${hit.documentRef}#${hit.chunkIndex}`}
                                >
                                  {hit.documentRef}#{hit.chunkIndex}
                                </span>
                                {hit.truncated && (
                                  // Declared, not a state: the `open` Tag, which
                                  // carries no semantic hue (§2.5).
                                  <Badge variant="open">truncated</Badge>
                                )}
                                <span className="ml-auto whitespace-nowrap font-mono text-xs tabular-nums">
                                  {(hit.score * 100).toFixed(1)}%
                                </span>
                              </div>
                              <p className="mt-1 whitespace-pre-wrap break-words text-sm leading-relaxed">
                                {hit.content}
                              </p>
                            </li>
                          ))}
                        </ul>
                      </div>
                    </>
                  )}
                </div>
              )}
            </CardContent>
          </Card>

          {/* What the controller has to say about it. Conditions carry prose
              messages, so they stay in the main column — the rail is kv-lists
              and meters only (§4.7). */}
          {kb.conditions.length > 0 && (
            <Card className="min-w-0">
              <PanelHeader
                title="What the controller says"
                meta={`${kb.conditions.length} condition${kb.conditions.length === 1 ? "" : "s"}`}
              />
              <CardContent>
                <ul>
                  {kb.conditions.map((c) => (
                    <li
                      key={c.type}
                      className="border-b border-border-soft py-3 last:border-0"
                    >
                      <div className="flex flex-wrap items-baseline gap-2">
                        {/* The TAG says the state, in the console's own ≤16-char
                            vocabulary (§4.5); the condition TYPE is a machine
                            identifier and stays verbatim in the mono face, so it
                            is still the string an operator greps `kubectl` for.
                            Putting the identifier IN the tag broke both rules at
                            once: a 22-character uppercase label, and a vocabulary
                            nobody could learn. */}
                        <Badge
                          variant={
                            c.status === "True"
                              ? "ok"
                              : c.status === "False"
                                ? "crit"
                                : // Not yet determined — the machine is still
                                  // converging, which is progressing, not idle.
                                  "progressing"
                          }
                        >
                          {c.status === "True"
                            ? "Met"
                            : c.status === "False"
                              ? "Not met"
                              : "Undecided"}
                        </Badge>
                        <span className="min-w-0 truncate font-mono text-xs" title={c.type}>
                          {c.type}
                        </span>
                        {c.reason && (
                          <span className="font-mono text-xs text-faint">
                            {c.reason}
                          </span>
                        )}
                        {c.lastTransitionTime && (
                          <span
                            className="ml-auto whitespace-nowrap font-mono text-xs tabular-nums text-faint"
                            title={c.lastTransitionTime}
                          >
                            {formatStamp(c.lastTransitionTime)}
                          </span>
                        )}
                      </div>
                      {c.message && (
                        <p className="mt-1 max-w-[64ch] text-sm text-secondary-foreground">
                          {c.message}
                        </p>
                      )}
                    </li>
                  ))}
                </ul>
              </CardContent>
            </Card>
          )}
        </div>

        <div className="min-w-0 space-y-5">
          <Card className="min-w-0">
            <PanelHeader title="What it holds" />
            <CardContent>
              <KeyValueList items={holdings} />
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <KeyValueList items={record} />
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  );
}
