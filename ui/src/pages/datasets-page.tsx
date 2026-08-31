import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ChevronLeft, Database, Filter, Tag } from "lucide-react";
import { useNavigate, useParams } from "react-router-dom";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  UnknownValue,
  nextStepRank,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
} from "@/components/kit";
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

// DatasetsPage — the labeling dataset list + detail surface (m69.3, ADR 0062 Fork 5;
// the list re-housed on the editorial system in M151 as archetype A1).
//
// THE PAGE'S WHOLE POINT IS THAT IT NEVER CLAIMS MORE THAN THE BACKEND CAN ANSWER.
// The dataset store (CONTROLPLANE_DSN) is OPTIONAL. When it is absent the BFF answers
// 501 — and 501 here is CALM, not an error: nothing broke, this install simply has no
// place to keep datasets. The honest rendering is a QuietNote naming what is absent and
// why, never a red error and never an implied zero.
//
// One caveat is load-bearing and is stated in the note rather than hidden: `api.listDatasets`
// currently ABSORBS the 501 and returns `{ items: [] }`, so an empty list is genuinely
// ambiguous between "the store is not configured" and "no datasets have been created".
// The page therefore refuses to claim either, and says so. (The `unavailable` branch below
// is the specced rendering and goes live the moment api.ts stops swallowing the 501.)
//
// List page: name, case count, created-at → a row-click opens the detail page where labels
// are appended. Detail page: draft-head cases + the latest label per case; the author is
// always the server-assigned caller identity (never a client field).
//
// data-testid contract:
//   datasets-page             — list root container
//   datasets-table            — the DataTable (aria-label="Datasets")
//   dataset-row-{name}        — each list row's entity cell
//   datasets-quiet-note       — the calm "cannot be answered" note
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

// formatStamp is the §4.5 table register for a timestamp: same year → "Aug 29",
// older → "2025-08-29", with the full value in `title`.
function formatStamp(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  if (d.getFullYear() !== new Date().getFullYear()) return d.toISOString().slice(0, 10);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
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
  // The store is not configured (501). NOT an error — §7.1's "not configured".
  | { kind: "unavailable" }
  | { kind: "error"; message: string; forbidden: boolean };

interface NextStep {
  label: string;
  tone: NextStepTone;
}

/**
 * The row's next action (§7.2), verb-first and ≤22 characters.
 *
 * A dataset with no cases cannot be labelled, so it needs a person. A dataset that
 * HAS cases needs nothing *from this list* — how many of those cases still want a
 * label is not in the list response, and inventing "12 to label" would be exactly
 * the claim this page exists to refuse. The QuietNote says so out loud.
 */
function datasetNextStep(d: DatasetSummary): NextStep {
  if (typeof d.caseCount !== "number") return { label: "Open the dataset", tone: "default" };
  if (d.caseCount === 0) return { label: "Add the first case", tone: "default" };
  return { label: "", tone: "none" };
}

const DS_VIEWS = ["all", "empty", "populated"] as const;
type DSView = (typeof DS_VIEWS)[number];

export function DatasetsPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [view, setView] = useState<DSView>("all");
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
        setLoadState({ kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 501 is CALM (§7.1): the store is not wired, nothing failed. It renders a
        // QuietNote, never the error state. Unreachable while api.listDatasets
        // absorbs the 501 into an empty list — kept because it is the correct
        // handling and the absorbing is the thing that should change.
        if (err instanceof ApiError && err.isNotImplemented) {
          setLoadState({ kind: "unavailable" });
          return;
        }
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

  // Attention-first (§6.1): datasets that cannot be labelled yet sort above the
  // ones that can, "Nothing needed" last, then alphabetically.
  const sorted = useMemo(
    () =>
      [...all].sort((a, b) => {
        const rank =
          nextStepRank(datasetNextStep(a).tone) - nextStepRank(datasetNextStep(b).tone);
        if (rank !== 0) return rank;
        return a.name.localeCompare(b.name);
      }),
    [all],
  );

  const emptyDatasets = useMemo(
    () => sorted.filter((d) => datasetNextStep(d).tone !== "none"),
    [sorted],
  );

  const q = query.trim().toLowerCase();
  const items = useMemo(() => {
    const byView =
      view === "empty"
        ? emptyDatasets
        : view === "populated"
          ? sorted.filter((d) => datasetNextStep(d).tone === "none")
          : sorted;
    return q ? byView.filter((d) => d.name.toLowerCase().includes(q)) : byView;
  }, [view, q, sorted, emptyDatasets]);

  // The list endpoint answers with every dataset in the caller's namespace in one
  // response (no cursor), so these are the backend's counts, not a tally of rendered rows.
  // Built FROM the view union rather than beside it, so the chips, their order
  // and the type cannot drift apart — a chip whose id is not a view stops
  // compiling instead of silently filtering to nothing.
  const dsViewLabel: Record<DSView, string> = {
    all: "Everything",
    empty: "Waiting for cases",
    populated: "Ready to label",
  };
  const dsViewCount: Record<DSView, number> = {
    all: all.length,
    empty: emptyDatasets.length,
    populated: all.length - emptyDatasets.length,
  };
  const chips: FilterChip[] = DS_VIEWS.map((id) => ({
    id,
    label: dsViewLabel[id],
    count: dsViewCount[id],
  }));
  const viewLabel = chips.find((c) => c.id === view)?.label ?? "Everything";

  const error: DataTableError | null =
    loadState.kind === "error"
      ? {
          message: loadState.message,
          forbidden: loadState.forbidden,
          resource: "datasets",
          onRetry: loadState.forbidden ? undefined : load,
        }
      : null;

  const columns: Column<DatasetSummary>[] = [
    {
      id: "name",
      header: "Dataset",
      priority: 1,
      className: "max-w-[22rem]",
      cell: (d) => (
        <div data-testid={`dataset-row-${d.name}`}>
          <CellEntity name={d.name} namespace={d.namespace} />
        </div>
      ),
    },
    {
      id: "created",
      header: "Created",
      priority: 4,
      cell: (d) =>
        d.createdAt ? (
          <span
            className="whitespace-nowrap font-mono text-xs tabular-nums text-faint"
            title={d.createdAt}
          >
            {formatStamp(d.createdAt)}
          </span>
        ) : (
          <UnknownValue title="No creation time was recorded for this dataset." />
        ),
    },
    {
      id: "cases",
      header: "Cases",
      priority: 3,
      numeric: true,
      // Straight through: a count the BFF did not send arrives as `undefined` and
      // reads `—`, never `0`. Zero and unknown never share a glyph (§7.1).
      cell: (d) => <QuantityValue value={d.caseCount} />,
    },
    {
      id: "next",
      header: "Next step",
      priority: 1,
      cell: (d) => {
        const step = datasetNextStep(d);
        return (
          <NextStepLink
            label={step.label}
            tone={step.tone}
            to={
              step.tone === "none"
                ? undefined
                : `/datasets/${encodeURIComponent(d.name)}`
            }
            ariaLabel={step.tone === "none" ? undefined : `${step.label} in ${d.name}`}
            testId={`dataset-next-${d.name}`}
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
          description: `No dataset is in “${viewLabel}” right now.`,
          action: {
            label: "Show everything",
            variant: "outline",
            onClick: () => setView("all"),
          },
          totalCount: all.length,
          countNoun: "datasets",
        }
      : {
          icon: Database,
          title: "No datasets",
          description:
            "No labeling datasets yet. Use the “Add to dataset” action on a trace to create one from a run.",
        };

  const totalCases = all.reduce(
    (n, d) => n + (typeof d.caseCount === "number" ? d.caseCount : 0),
    0,
  );

  return (
    <div className="mx-auto max-w-6xl space-y-6" data-testid="datasets-page">
      <PageHeader
        title="Datasets"
        loading={loadState.kind === "loading"}
        meta={
          all.length > 0
            ? `${all.length} datasets · ${totalCases.toLocaleString()} cases`
            : undefined
        }
        lede="Human-labeled eval datasets for the improvement loop. Add a trace as a case with the “Add to dataset” action on any trace, then label the cases here."
      />

      {loadState.kind === "unavailable" ? (
        // The backend cannot answer — and that is calm, not a failure (§7.1).
        <div data-testid="datasets-quiet-note">
          <QuietNote title="Datasets aren’t configured on this install.">
            Datasets live in the control-plane store, and this platform has none — set{" "}
            <span className="font-mono">CONTROLPLANE_DSN</span> to enable them. Nothing
            here is estimated and nothing is lost: the list is simply absent, not empty.
          </QuietNote>
        </div>
      ) : (
        <>
          {all.length > 0 && (
            <FilterChipRow
              chips={chips}
              value={view}
              onChange={(id) => setView(id as DSView)}
              label="Filter datasets"
            />
          )}

          {loadState.kind === "ready" && (
            <div data-testid="datasets-quiet-note">
              {all.length === 0 ? (
                <QuietNote title="An empty list means one of two things here.">
                  Datasets live in the control-plane store. An install without{" "}
                  <span className="font-mono">CONTROLPLANE_DSN</span> answers “not
                  configured”, and this list cannot tell that apart from “none created
                  yet” — so it claims neither. Nothing here is estimated.
                </QuietNote>
              ) : (
                <QuietNote title="Label progress isn’t in this list.">
                  The counts above are each dataset’s draft-head size. How many of those
                  cases already carry a human label is known only inside the dataset, so
                  no row claims a labelled ratio. Open one to see and add labels.
                </QuietNote>
              )}
            </div>
          )}

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
            tableClassName="min-w-[36rem]"
            onRowClick={(d) => navigate(`/datasets/${encodeURIComponent(d.name)}`)}
            empty={empty}
          />

          {all.length > 0 && (
            <ClosingNote>
              {emptyDatasets.length === 0
                ? `All ${all.length} datasets hold cases — ${totalCases.toLocaleString()} between them — and need nothing from this list.`
                : `${emptyDatasets.length} of ${all.length} datasets have no cases yet. The other ${all.length - emptyDatasets.length} hold ${totalCases.toLocaleString()} cases between them.`}
            </ClosingNote>
          )}
        </>
      )}
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
