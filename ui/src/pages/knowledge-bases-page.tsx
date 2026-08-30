import { useCallback, useEffect, useRef, useState } from "react";
import {
  BookOpen,
  ChevronLeft,
  Upload,
  Play,
  Search,
} from "lucide-react";
import { useNavigate, useParams } from "react-router-dom";

import { DataTable, StatusBadge, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  api,
  ApiError,
  type KBSummary,
  type KBDetail,
  type KBSearchHit,
} from "@/lib/api";

// KnowledgeBasesPage — the KnowledgeBase CR list surface (m68.13, ADR 0061).
//
// Enterprise-RAG demo surface: upload docs → ingest → watch phase → test-query with citations.
// Read-only list (caller-scoped, ADR 0011): each row is a KnowledgeBase CR — its name, phase,
// chunk count, size, and last-ingested timestamp. A KB is authored via YAML/kubectl; the console
// surfaces it for visibility and operator awareness. A row-click navigates to the detail page
// (/knowledgebases/:ns/:name) which includes: document upload, ingest trigger, and test-query panel.
//
// A 403 surfaces as an honest forbidden state (never a fake empty list).
//
// data-testid contract:
//   knowledge-bases-page  — root container
//   kb-table              — the DataTable (aria-label="KnowledgeBases")
//   kb-row-{name}         — each row

// formatBytes renders a byte count as a human-readable string (e.g. "1.2 MB").
function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`;
}

// formatDate renders an ISO8601 timestamp as a short local date string.
function formatDate(ts?: string): string {
  if (!ts) return "—";
  try {
    return new Date(ts).toLocaleDateString(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
    });
  } catch {
    return ts;
  }
}

// ──────────────────────────────────────────────────────────────────────────────
// List page
// ──────────────────────────────────────────────────────────────────────────────

type ListLoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: KBSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function KnowledgeBasesPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
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

  const all = loadState.kind === "ready" ? loadState.items : [];
  const q = query.trim().toLowerCase();
  const items = q ? all.filter((kb) => kb.name.toLowerCase().includes(q)) : all;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "knowledge bases",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<KBSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (kb) => <span className="font-medium">{kb.name}</span>,
    },
    {
      id: "phase",
      header: "Phase",
      cell: (kb) => <StatusBadge ready={kb.phase === "Ready"} phase={kb.phase} />,
    },
    {
      id: "chunks",
      header: "Chunks",
      hideOnMobile: true,
      cell: (kb) => (
        <span className="text-sm text-muted-foreground">{kb.chunkCount.toLocaleString()}</span>
      ),
    },
    {
      id: "size",
      header: "Size",
      hideOnMobile: true,
      cell: (kb) => (
        <span className="text-sm text-muted-foreground">{formatBytes(kb.sizeBytes)}</span>
      ),
    },
    {
      id: "lastIngested",
      header: "Last ingested",
      hideOnMobile: true,
      cell: (kb) => (
        <span className="text-sm text-muted-foreground">{formatDate(kb.lastIngestedAt)}</span>
      ),
    },
    {
      id: "embeddingRoute",
      header: "Embedding route",
      hideOnMobile: true,
      cell: (kb) => (
        <span className="text-sm text-muted-foreground font-mono">{kb.embeddingRoute}</span>
      ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="knowledge-bases-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Knowledge Bases</h2>
        <p className="text-sm text-muted-foreground">
          Managed RAG corpora — upload documents, ingest and index them, then test retrieval with
          citations. Each KnowledgeBase is backed by a pgvector store and authored via YAML/kubectl.
        </p>
      </div>

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
        onRowClick={(kb) => navigate(`/knowledgebases/${encodeURIComponent(kb.namespace)}/${encodeURIComponent(kb.name)}`)}
        empty={{
          icon: BookOpen,
          title: "No knowledge bases",
          description:
            "No knowledge bases yet. A knowledge base is a managed RAG corpus your agents retrieve from — upload documents and they are chunked, embedded, and searchable.",
        }}
      />
    </div>
  );
}

// ──────────────────────────────────────────────────────────────────────────────
// Detail page
// ──────────────────────────────────────────────────────────────────────────────

type DetailLoadState =
  | { kind: "loading" }
  | { kind: "ready"; kb: KBDetail }
  | { kind: "error"; message: string };

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

// KBDetailPage — the KnowledgeBase detail surface (m68.13, ADR 0061).
//
// Shows the KB's spec + status + conditions, a document upload panel, an ingest trigger,
// and the test-query panel that proxies to /v1/knowledge/search and renders ranked chunks
// with citations (documentRef#chunkIndex + score + content).
//
// data-testid contract:
//   kb-detail-page        — root container
//   kb-detail-header      — the KB name + phase header
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
  const navigate = useNavigate();
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

  if (loadState.kind === "loading") {
    return (
      <div className="mx-auto max-w-4xl" data-testid="kb-detail-page">
        <p className="text-sm text-muted-foreground">Loading knowledge base…</p>
      </div>
    );
  }

  if (loadState.kind === "error") {
    return (
      <div className="mx-auto max-w-4xl" data-testid="kb-detail-page">
        <Button variant="ghost" size="sm" onClick={() => navigate("/knowledgebases")}>
          <ChevronLeft className="h-4 w-4" />
          Back to Knowledge Bases
        </Button>
        <p className="text-sm text-destructive mt-4">{loadState.message}</p>
      </div>
    );
  }

  const { kb } = loadState;

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="kb-detail-page">
      {/* Back link */}
      <Button variant="ghost" size="sm" onClick={() => navigate("/knowledgebases")}>
        <ChevronLeft className="h-4 w-4" />
        Back to Knowledge Bases
      </Button>

      {/* Header */}
      <div data-testid="kb-detail-header" className="flex items-start gap-4">
        <div className="flex-1 space-y-1">
          <div className="flex items-center gap-3">
            <h2 className="text-2xl font-semibold tracking-tight font-mono">{kb.name}</h2>
            <StatusBadge ready={kb.phase === "Ready"} phase={kb.phase} />
          </div>
          {kb.displayName && (
            <p className="text-sm text-muted-foreground">{kb.displayName}</p>
          )}
          <p className="text-xs text-muted-foreground">
            Embedding route: <span className="font-mono">{kb.embeddingRoute}</span>
            {" · "}
            Source: <span className="font-mono">{kb.sourceType}</span>
            {" · "}
            Chunking: {kb.chunkSize} tokens / {kb.chunkOverlap} overlap / {kb.chunkSplitter} splitter
          </p>
        </div>
      </div>

      {/* Status summary */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Status</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-muted-foreground">Documents</dt>
              <dd className="font-medium">{kb.documentCount.toLocaleString()}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Chunks</dt>
              <dd className="font-medium">{kb.chunkCount.toLocaleString()}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Size</dt>
              <dd className="font-medium">{formatBytes(kb.sizeBytes)}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Last ingested</dt>
              <dd className="font-medium">{formatDate(kb.lastIngestedAt)}</dd>
            </div>
          </dl>
          {kb.conditions.length > 0 && (
            <div className="mt-4 space-y-1">
              {kb.conditions.map((c) => (
                <div key={c.type} className="flex items-center gap-2 text-xs">
                  <Badge
                    variant={c.status === "True" ? "success" : c.status === "False" ? "destructive" : "secondary"}
                    className="text-xs"
                  >
                    {c.type}
                  </Badge>
                  {c.reason && <span className="text-muted-foreground">{c.reason}</span>}
                  {c.message && <span className="text-muted-foreground truncate max-w-md">{c.message}</span>}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Document upload */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Upload document</CardTitle>
          <CardDescription>
            Add a document to the KB&apos;s durable bucket. Trigger ingestion after uploading to
            chunk, embed, and index the document for retrieval.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center gap-3">
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
              Uploaded: <span className="font-mono">{uploadState.ref}</span>
            </p>
          )}
          {uploadState.kind === "error" && (
            <p className="text-sm text-destructive" role="alert" data-testid="upload-result">
              {uploadState.message}
            </p>
          )}
        </CardContent>
      </Card>

      {/* Ingest trigger */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Ingest</CardTitle>
          <CardDescription>
            Start an ingestion run to chunk, embed, and index the KB&apos;s documents. The KB
            transitions to <span className="font-mono">Ingesting</span> and then{" "}
            <span className="font-mono">Ready</span> when complete.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
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
              Ingestion run started: <span className="font-mono">{ingestState.runId}</span>
            </p>
          )}
          {ingestState.kind === "error" && (
            <p className="text-sm text-destructive" role="alert" data-testid="ingest-result">
              {ingestState.message}
            </p>
          )}
        </CardContent>
      </Card>

      {/* Test-query panel */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Test query</CardTitle>
          <CardDescription>
            Run a retrieval query against the KB&apos;s pgvector index. Results show ranked chunks
            with citations — document reference, chunk index, and similarity score. Requires the
            token-service to be configured (TOKEN_SERVICE_URL).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center gap-3">
            <Input
              data-testid="query-input"
              className="flex-1"
              placeholder="Enter a query, e.g. 'how to configure rate limits'"
              value={queryText}
              onChange={(e) => setQueryText(e.target.value)}
              disabled={searchState.kind === "searching"}
              onKeyDown={(e) => {
                if (e.key === "Enter") void onSearch();
              }}
            />
            <div className="flex items-center gap-2">
              <label htmlFor="query-topk" className="text-xs text-muted-foreground whitespace-nowrap">
                Top K
              </label>
              <Input
                id="query-topk"
                data-testid="query-topk"
                type="number"
                min={1}
                max={50}
                className="w-16"
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

          {searchState.kind === "unavailable" && (
            <p className="text-sm text-muted-foreground" data-testid="query-unavailable">
              Test-query is not available — set TOKEN_SERVICE_URL to enable KB retrieval.
            </p>
          )}

          {searchState.kind === "error" && (
            <p className="text-sm text-destructive" role="alert">
              {searchState.message}
            </p>
          )}

          {searchState.kind === "results" && (
            <div className="space-y-3" data-testid="query-results">
              {searchState.hits.length === 0 ? (
                <p className="text-sm text-muted-foreground">No results for this query.</p>
              ) : (
                searchState.hits.map((hit, i) => (
                  <div
                    key={i}
                    data-testid={`query-hit-${i}`}
                    className="rounded-md border p-3 space-y-1"
                  >
                    <div className="flex items-center gap-2 text-xs text-muted-foreground">
                      <span className="font-mono">
                        {hit.documentRef}#{hit.chunkIndex}
                      </span>
                      <span className="ml-auto font-medium text-foreground">
                        {(hit.score * 100).toFixed(1)}%
                      </span>
                      {hit.truncated && (
                        <Badge variant="secondary" className="text-xs">truncated</Badge>
                      )}
                    </div>
                    <p className="text-sm leading-relaxed whitespace-pre-wrap">{hit.content}</p>
                  </div>
                ))
              )}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
