import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronLeft, Database, Tag } from "lucide-react";
import { useNavigate, useParams } from "react-router-dom";

import { DataTable, type Column, type DataTableError } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  api,
  ApiError,
  type DatasetSummary,
  type DatasetCase,
  type DatasetCasesResponse,
  type AppendLabelRequest,
} from "@/lib/api";

// DatasetsPage — the labeling dataset list + detail surface (m69.3, ADR 0062 Fork 5).
//
// The dataset store (CONTROLPLANE_DSN) is optional: both pages degrade calmly with a
// "not configured" message when the store is absent (501-calm from the BFF). The list
// also degrades calmly when the Langfuse adapter is absent (listDatasets returns empty).
//
// List page: shows name, case count, created-at → clicking a row navigates to the
// detail page where labels can be appended. The store-absent 501 is surfaced as a calm
// "not configured" message, not as an error.
//
// Detail page: lists draft-head cases + the latest label per case. Each case has a
// label form (pass/fail/flag + optional correction + note → POST
// /api/datasets/{name}/cases/{caseId}/labels). The author is always the server-assigned
// caller identity (never a client field).
//
// data-testid contract:
//   datasets-page             — list root container
//   datasets-table            — the DataTable (aria-label="Datasets")
//   dataset-row-{name}        — each list row
//   dataset-detail-page       — detail root container
//   case-row-{id}             — each case card
//   label-form-{caseId}       — the label form for a case
//   label-value-{caseId}      — the value <select> for a case
//   label-submit-{caseId}     — the submit button for a case

// ─── helpers ──────────────────────────────────────────────────────────────────

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

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…`;
}

// ──────────────────────────────────────────────────────────────────────────────
// List page
// ──────────────────────────────────────────────────────────────────────────────

type ListLoadState =
  | { kind: "loading" }
  | { kind: "ready"; items: DatasetSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

export function DatasetsPage() {
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
      .listDatasets(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        // api.listDatasets absorbs 501 (unconfigured store) and returns {items:[]} —
        // so the page always lands on "ready" and shows the teaching empty state.
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
  const items = q ? all.filter((d) => d.name.toLowerCase().includes(q)) : all;

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<DatasetSummary>[] = [
    {
      id: "name",
      header: "Name",
      cell: (d) => <span className="font-medium font-mono">{d.name}</span>,
    },
    {
      id: "cases",
      header: "Cases",
      cell: (d) => (
        <span className="text-sm text-muted-foreground">
          {d.caseCount.toLocaleString()}
        </span>
      ),
    },
    {
      id: "created",
      header: "Created",
      hideOnMobile: true,
      cell: (d) => (
        <span className="text-sm text-muted-foreground">{formatDate(d.createdAt)}</span>
      ),
    },
  ];

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="datasets-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Datasets</h2>
        <p className="text-sm text-muted-foreground">
          Human-labeled eval datasets for the improvement loop (ADR 0062 Fork 5). Add
          traces as cases via the trace view or{" "}
          <span className="font-mono">POST /api/datasets/{"{name}"}/cases/from-run</span>.
        </p>
      </div>

      <DataTable<DatasetSummary>
        columns={columns}
        rows={items}
        rowKey={(d) => d.id}
        loading={loadState.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter datasets by name…"
        ariaLabel="Datasets"
        onRowClick={(d) => navigate(`/datasets/${encodeURIComponent(d.name)}`)}
        empty={{
          icon: Database,
          title: "No datasets",
          description:
            "No labeling datasets yet. Use the 'Add to dataset' action on a trace to add a case, or POST /api/datasets/{name}/cases/from-run directly.",
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
  | { kind: "ready"; data: DatasetCasesResponse }
  | { kind: "error"; message: string };

type LabelFormState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "success" }
  | { kind: "error"; message: string };

interface LabelFormProps {
  datasetName: string;
  caseId: string;
  onSaved: () => void;
}

function LabelForm({ datasetName, caseId, onSaved }: LabelFormProps) {
  const [value, setValue] = useState("pass");
  const [correction, setCorrection] = useState("");
  const [note, setNote] = useState("");
  const [state, setState] = useState<LabelFormState>({ kind: "idle" });

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setState({ kind: "submitting" });
    try {
      const req: AppendLabelRequest = {
        value,
        ...(correction.trim() ? { correction: correction.trim() } : {}),
        ...(note.trim() ? { note: note.trim() } : {}),
      };
      await api.appendLabel(datasetName, caseId, req);
      setState({ kind: "success" });
      // Reset form after success and trigger list reload.
      setValue("pass");
      setCorrection("");
      setNote("");
      onSaved();
    } catch (err) {
      setState({
        kind: "error",
        message: err instanceof Error ? err.message : "label save failed",
      });
    }
  }

  return (
    <form
      data-testid={`label-form-${caseId}`}
      onSubmit={(e) => void onSubmit(e)}
      className="mt-4 space-y-3 border-t pt-4"
    >
      <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">
        Add label
      </p>
      <div className="flex items-center gap-3">
        <Label htmlFor={`label-value-${caseId}`} className="sr-only">
          Verdict
        </Label>
        <select
          id={`label-value-${caseId}`}
          data-testid={`label-value-${caseId}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          disabled={state.kind === "submitting"}
          className="h-9 rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        >
          <option value="pass">pass</option>
          <option value="fail">fail</option>
          <option value="flag">flag</option>
        </select>
        <Button
          type="submit"
          size="sm"
          data-testid={`label-submit-${caseId}`}
          disabled={state.kind === "submitting"}
        >
          <Tag className="h-3.5 w-3.5" />
          {state.kind === "submitting" ? "Saving…" : "Save label"}
        </Button>
      </div>
      <div className="space-y-2">
        <Textarea
          placeholder="Correction (optional) — the ideal expected output for this case"
          value={correction}
          onChange={(e) => setCorrection(e.target.value)}
          disabled={state.kind === "submitting"}
          rows={2}
          className="text-sm font-mono"
        />
        <Textarea
          placeholder="Note (optional) — free-form comment for this label"
          value={note}
          onChange={(e) => setNote(e.target.value)}
          disabled={state.kind === "submitting"}
          rows={1}
          className="text-sm"
        />
      </div>
      {state.kind === "success" && (
        <p className="text-sm text-success">Label saved.</p>
      )}
      {state.kind === "error" && (
        <p className="text-sm text-destructive" role="alert">
          {state.message}
        </p>
      )}
    </form>
  );
}

interface CaseCardProps {
  datasetName: string;
  c: DatasetCase;
  onLabelSaved: () => void;
}

function CaseCard({ datasetName, c, onLabelSaved }: CaseCardProps) {
  return (
    <Card data-testid={`case-row-${c.id}`} className="space-y-0">
      <CardHeader className="pb-3">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span className="font-mono text-xs text-muted-foreground truncate max-w-xs">
            {c.id}
          </span>
          <div className="flex flex-wrap gap-1">
            {c.tags &&
              Object.entries(c.tags).map(([k, v]) => (
                <Badge key={k} variant="secondary" className="text-xs font-mono">
                  {k}={v}
                </Badge>
              ))}
          </div>
        </div>
        {c.sourceTraceId && (
          <a
            href={`/traces/${encodeURIComponent(c.sourceTraceId)}`}
            className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
          >
            Source trace: {c.sourceTraceId}
          </a>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-1">Input</p>
          <p className="text-sm font-mono whitespace-pre-wrap break-words rounded bg-muted px-2 py-1.5">
            {truncate(c.input, 400)}
          </p>
        </div>
        {c.expected && (
          <div>
            <p className="text-xs font-medium text-muted-foreground mb-1">Expected</p>
            <p className="text-sm font-mono whitespace-pre-wrap break-words rounded bg-muted px-2 py-1.5">
              {truncate(c.expected, 400)}
            </p>
          </div>
        )}
        {c.latestLabel && (
          <div className="rounded-md border bg-card/60 px-3 py-2 space-y-1">
            <div className="flex items-center gap-2">
              <Badge
                variant={
                  c.latestLabel.value === "pass"
                    ? "success"
                    : c.latestLabel.value === "fail"
                      ? "destructive"
                      : "warning"
                }
                className="text-xs"
              >
                {c.latestLabel.value}
              </Badge>
              <span className="text-xs text-muted-foreground">
                by {c.latestLabel.author} · {formatDate(c.latestLabel.createdAt)}
              </span>
            </div>
            {c.latestLabel.correction && (
              <p className="text-xs font-mono text-muted-foreground">
                Correction: {truncate(c.latestLabel.correction, 200)}
              </p>
            )}
            {c.latestLabel.note && (
              <p className="text-xs text-muted-foreground">{c.latestLabel.note}</p>
            )}
          </div>
        )}
        <LabelForm datasetName={datasetName} caseId={c.id} onSaved={onLabelSaved} />
      </CardContent>
    </Card>
  );
}

export function DatasetDetailPage() {
  const navigate = useNavigate();
  const { name = "" } = useParams<{ name: string }>();
  const [loadState, setLoadState] = useState<DetailLoadState>({ kind: "loading" });
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    if (!name) return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setLoadState({ kind: "loading" });
    api
      .listDatasetCases(name, controller.signal)
      .then((data) => {
        if (controller.signal.aborted) return;
        setLoadState({ kind: "ready", data });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // Note: api.listDatasetCases absorbs 501 (unconfigured) and returns empty cases.
        // Only real errors (5xx, 4xx) land here.
        setLoadState({
          kind: "error",
          message: err instanceof Error ? err.message : "failed to load dataset",
        });
      });
  }, [name]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="dataset-detail-page">
      {/* Back link */}
      <Button variant="ghost" size="sm" onClick={() => navigate("/datasets")}>
        <ChevronLeft className="h-4 w-4" />
        Back to Datasets
      </Button>

      {/* Header */}
      <div className="space-y-1">
        <h2 className="text-2xl font-semibold tracking-tight font-mono">{name}</h2>
        <p className="text-sm text-muted-foreground">
          Draft-head cases with their latest human labels. Labels are append-only — a new
          label supersedes the previous one (displayed here); the history is preserved.
        </p>
      </div>

      {loadState.kind === "loading" && (
        <p className="text-sm text-muted-foreground">Loading cases…</p>
      )}

      {loadState.kind === "error" && (
        <div
          className="rounded-lg border border-destructive/40 bg-destructive/5 p-6 text-sm text-destructive"
          role="alert"
        >
          {loadState.message}
        </div>
      )}

      {loadState.kind === "ready" && loadState.data.cases.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">No cases</CardTitle>
            <CardDescription>
              No cases in this dataset yet. Add a trace as a case from the trace view or
              via{" "}
              <span className="font-mono">
                POST /api/datasets/{name}/cases/from-run
              </span>
              .
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {loadState.kind === "ready" &&
        loadState.data.cases.map((c) => (
          <CaseCard
            key={c.id}
            datasetName={name}
            c={c}
            onLabelSaved={load}
          />
        ))}
    </div>
  );
}
