import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Database, Filter, Tag } from "lucide-react";
import { Link, useNavigate, useParams } from "react-router-dom";

import {
  CellEntity,
  ClosingNote,
  DataTable,
  ErrorState,
  FilterChipRow,
  KeyValueList,
  Meter,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  Skeleton,
  SkeletonCard,
  UnknownValue,
  nextStepRank,
  truncateId,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type KeyValueItem,
  type NextStepTone,
} from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
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

// `formatDate` is gone: it answered an absent timestamp with a bare "—" and no
// reason, which is the one dash this milestone forbids — one a reader cannot
// tell apart from a zero. Both surfaces read a timestamp through `formatStamp`
// (present, full ISO in `title`) or `UnknownValue` (absent, with the reason).

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
// Detail page — M151 §6.1 archetype A2
//
// ── THE PAGE'S ONE IDEA: UNLABELLED IS A STATE, NOT AN ABSENCE ─────────────
// A dataset is a queue of judgements waiting to be made, and the only question
// a labeller brings here is "which of these has nobody judged?". So an
// unlabelled case does not render as a case with a blank where a verdict would
// be: it wears the `open` Tag — "declared but never exercised" (§2.5), the one
// Tag variant that carries no semantic hue — and says in words that nobody has
// judged it yet. It is not a pass, it is not a fail, and it is not an error.
// The rail's meter counts it out of the labelled figure and its foot line says
// so, so the number and the rows agree.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ────────────────────────────────────
// `GET /api/datasets/{name}/cases` returns the DRAFT HEAD — the current case
// list with each case's latest label — and nothing else: no label history, no
// per-labeller agreement, no eval scores. `api.listDatasetCases` also absorbs a
// 501 from an install with no dataset store and returns an empty case list, so
// "no cases" here is genuinely ambiguous between "the store isn't configured"
// and "nobody has added one". The empty state refuses to claim either.
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

/**
 * A verdict's Tag variant. `pass` is the only ok-green here — §2.2's rule that
 * green means "verified and serving" and nothing else. `fail` is crit: the case
 * will not pass without a change to the agent. Everything else (flag, partial,
 * an install's own vocabulary) is warn — degraded, not broken.
 *
 * A verdict is never pine, and an ABSENT verdict is never any of these: an
 * unlabelled case gets the hueless `open` Tag, because "nobody has judged it"
 * is not a judgement.
 */
function verdictVariant(value: string): "ok" | "crit" | "warn" {
  const v = value.trim().toLowerCase();
  if (v === "pass") return "ok";
  if (v === "fail") return "crit";
  return "warn";
}

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
      className="space-y-3 border-t border-border pt-4"
    >
      <p className="font-mono text-2xs uppercase tracking-wide text-faint">
        Add a label
      </p>
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor={`label-value-${caseId}`} className="sr-only">
          Verdict
        </Label>
        <select
          id={`label-value-${caseId}`}
          data-testid={`label-value-${caseId}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          disabled={state.kind === "submitting"}
          className="h-9 rounded-sm border border-input bg-background px-3 py-1 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
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
          className="font-mono text-sm"
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

/** One case: what was asked, what was expected, how it was judged (or that it
 *  has not been), and the form that judges it. */
function CaseCard({ datasetName, c, onLabelSaved }: CaseCardProps) {
  const label = c.latestLabel;
  const tags = Object.entries(c.tags ?? {});

  return (
    <Card className="min-w-0" data-testid={`case-row-${c.id}`}>
      <PanelHeader
        title={
          <span className="font-mono text-sm font-semibold" title={c.id}>
            {truncateId(c.id)}
          </span>
        }
        meta={c.createdAt ? formatStamp(c.createdAt) : undefined}
      >
        {label ? (
          <Badge variant={verdictVariant(label.value)}>{label.value}</Badge>
        ) : (
          // Declared but never exercised — the one Tag that carries no hue,
          // because "nobody has judged this" is not a judgement (§2.5).
          <Badge variant="open">unlabelled</Badge>
        )}
      </PanelHeader>
      <CardContent className="space-y-4">
        {(tags.length > 0 || c.sourceTraceId) && (
          <div className="flex flex-wrap items-center gap-2">
            {tags.map(([k, v]) => (
              <Badge key={k} variant="muted">
                {k}={v}
              </Badge>
            ))}
            {c.sourceTraceId && (
              <Link
                to={`/traces/${encodeURIComponent(c.sourceTraceId)}`}
                title={c.sourceTraceId}
                className="border-b border-accent font-mono text-xs text-primary hover:border-primary"
              >
                Source trace: {truncateId(c.sourceTraceId)}
              </Link>
            )}
          </div>
        )}

        {/* Machine text in a code well: it keeps its own line breaks and scrolls
            inside its own frame rather than widening the page (§4.5/§4.6). */}
        <div>
          <p className="mb-1 font-mono text-2xs uppercase tracking-wide text-faint">
            Input
          </p>
          <div className="max-h-44 overflow-auto rounded-md bg-surface-3 p-3">
            <pre className="whitespace-pre-wrap break-words font-mono text-xs">
              {truncate(c.input, 400)}
            </pre>
          </div>
        </div>

        {c.expected && (
          <div>
            <p className="mb-1 font-mono text-2xs uppercase tracking-wide text-faint">
              Expected
            </p>
            <div className="max-h-44 overflow-auto rounded-md bg-surface-3 p-3">
              <pre className="whitespace-pre-wrap break-words font-mono text-xs">
                {truncate(c.expected, 400)}
              </pre>
            </div>
          </div>
        )}

        {label ? (
          <div className="rounded-md border border-border bg-surface-2 px-3 py-2">
            <p className="text-sm text-faint">
              Judged {label.value} by {label.author} ·{" "}
              <span
                className="font-mono tabular-nums"
                title={label.createdAt}
              >
                {formatStamp(label.createdAt)}
              </span>
            </p>
            {label.correction && (
              <p className="mt-1 break-words font-mono text-xs text-secondary-foreground">
                Correction: {truncate(label.correction, 200)}
              </p>
            )}
            {label.note && (
              <p className="mt-1 text-sm text-secondary-foreground">
                {label.note}
              </p>
            )}
          </div>
        ) : (
          // An unlabelled case is a REAL state, and it says so — a blank here
          // would read as a rendering gap rather than as work to be done.
          <QuietNote title="Nobody has judged this case yet.">
            It is in the dataset and waiting for a verdict. Until someone gives
            it one it counts as neither a pass nor a fail — it is unjudged, and
            the labelled figure in the rail leaves it out rather than counting it
            against the agent.
          </QuietNote>
        )}

        <LabelForm
          datasetName={datasetName}
          caseId={c.id}
          onSaved={onLabelSaved}
        />
      </CardContent>
    </Card>
  );
}

export function DatasetDetailPage() {
  // No `useNavigate` here: the way back is the PageHeader breadcrumb, a real
  // link rather than a button that calls navigate() — and it renders in every
  // state, including the ones the old back button did not cover.
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

  const crumbs = [{ label: "Datasets", to: "/datasets" }, { label: name }];
  const lede =
    "Draft-head cases with their latest human labels. Labels are append-only — a new label supersedes the one shown here, and the history is kept.";

  if (loadState.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6" data-testid="dataset-detail-loading">
        <PageHeader breadcrumb={crumbs} title={name} titleMono loading />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <div className="min-w-0 space-y-5">
            <SkeletonCard />
            <SkeletonCard />
          </div>
          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label="Loading the dataset facts">
                {[0, 1, 2, 3].map((i) => (
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
    return (
      <div className="min-w-0 space-y-6" data-testid="dataset-detail-error">
        <PageHeader breadcrumb={crumbs} title={name} titleMono />
        <ErrorState
          title="The dataset didn't load."
          description="Nothing has changed about the cases themselves — only this page failed to read them."
          detail={loadState.message}
          onRetry={load}
        />
      </div>
    );
  }

  const { data } = loadState;
  const cases = data.cases;
  const labelled = cases.filter((c) => c.latestLabel).length;
  const unlabelled = cases.length - labelled;

  const record: KeyValueItem[] = [
    { key: "Dataset", value: name, title: name },
    {
      key: "Dataset id",
      value: data.datasetId ? (
        <span title={data.datasetId}>{truncateId(data.datasetId)}</span>
      ) : undefined,
      absent: "not recorded",
      title: "This response carried no dataset id.",
    },
    { key: "Cases", value: <QuantityValue value={cases.length} />, mono: false },
    {
      key: "Unlabelled",
      // A real, counted zero renders `0` — the whole dataset is judged. It is
      // never the same glyph as a figure we do not have (§7.1).
      value: <QuantityValue value={unlabelled} />,
      mono: false,
    },
  ];

  return (
    <div className="min-w-0 space-y-6" data-testid="dataset-detail-page">
      <PageHeader
        breadcrumb={crumbs}
        title={name}
        titleMono
        meta={
          cases.length > 0
            ? `${cases.length} case${cases.length === 1 ? "" : "s"} · ${labelled} labelled`
            : undefined
        }
        lede={lede}
      />

      {/* §4.7 hub grid: the queue of judgements on the left, how far it has got
          in the 300px rail, which stacks UNDER the main column below `lg`. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          {cases.length === 0 ? (
            <QuietNote title="No cases">
              This dataset holds nothing to judge yet — and this page cannot tell
              you which of two things that means. An install with no dataset
              store answers the same way as a dataset nobody has added to, so
              rather than pick one, it says so: add a run as a case from its
              trace, or{" "}
              <span className="break-all font-mono text-xs">
                POST /api/datasets/{name}/cases/from-run
              </span>
              .
            </QuietNote>
          ) : (
            <>
              <SectionHeader
                title="The cases"
                lede={
                  unlabelled === 0
                    ? "Every case here has a verdict. A new label supersedes the one shown."
                    : `${unlabelled} of ${cases.length} have no verdict yet. Judging one appends a label; it never overwrites the last.`
                }
              />
              {cases.map((c) => (
                <CaseCard
                  key={c.id}
                  datasetName={name}
                  c={c}
                  onLabelSaved={load}
                />
              ))}
            </>
          )}
        </div>

        <div className="min-w-0 space-y-5">
          {cases.length > 0 && (
            <Card className="min-w-0">
              <PanelHeader title="How much is judged" />
              <CardContent>
                <Meter
                  label="Cases labelled"
                  used={labelled}
                  cap={cases.length}
                  thing="dataset"
                  foot={
                    unlabelled === 0
                      ? "Every case in this dataset has been judged."
                      : `${unlabelled} still unjudged. Unlabelled is a state of its own — it counts as neither a pass nor a fail.`
                  }
                />
              </CardContent>
            </Card>
          )}

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
