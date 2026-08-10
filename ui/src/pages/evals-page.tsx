import * as React from "react";
import {
  CheckCircle,
  ChevronRight,
  Loader2,
  RefreshCw,
  ShieldCheck,
  TestTube2,
  XCircle,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  Wizard,
  useToast,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type EvalCondition,
  type EvalGatedMetricResponse,
  type EvalScore,
  type EvalSuiteDetail,
  type EvalSuiteResults,
  type EvalSuiteSummary,
} from "@/lib/api";
import { RES_EVALSUITES } from "@/lib/nav";

// EvalsPage — the EvalSuite builder + results browser (m17.12).
// Three surfaces in one page:
//   1. List — all EvalSuites for the current namespace.
//   2. Builder wizard — create a new EvalSuite (dataset ref + scorers + gate).
//   3. Results browser — honest results from GET .../results:
//        • conditions = the controller's gate outcome (always present)
//        • scores only when scoresAvailable=true (Langfuse wired)
//        • when false, scoresUnavailableReason shown calmly — NEVER fabricated
//
// RBAC: list/read is open; create/delete gated on evalsuites/create+delete.

// ---- discriminated state types -----------------------------------------------

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; suites: EvalSuiteSummary[] }
  | { kind: "empty" }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

type ResultsState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; results: EvalSuiteResults }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

type DeleteState =
  | { kind: "idle" }
  | { kind: "deleting" }
  | { kind: "error"; message: string };

// ---- wizard step indices ------------------------------------------------------

const STEP_DATASET = 0;
const STEP_SCORERS = 1;
const STEP_GATE = 2;
const STEP_REVIEW = 3;

type BuilderState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "done"; suite: EvalSuiteDetail }
  | { kind: "error"; message: string; forbidden?: boolean };

// ---- main page ----------------------------------------------------------------

// MetricState holds the eval-gated metric load state (PRD §5, ADR 0062
// governance #2). "unavailable" (501) is a calm degrade — the endpoint requires
// a wired cluster; "error" is a real failure. Neither is fabricated.
type MetricState =
  | { kind: "loading" }
  | { kind: "ready"; data: EvalGatedMetricResponse }
  | { kind: "unavailable" }
  | { kind: "error" };

// TARGET_PERCENT is the PRD §5 quality-discipline target: > 50% of production
// deploys must be gated by an EvalSuite. Shown as a visual indicator on the card.
const TARGET_PERCENT = 50;

// EvalGatedStatCard renders the PRD §5 ">50% of production deploys gated by an
// EvalSuite" quality metric as a compact stat card (M69, ADR 0062 governance #2).
// Shows gated/total (percent%) with a clear label and a >50% target indicator.
// Degrades to "…" while loading; shows "—" on error (no fabricated numbers).
function EvalGatedStatCard({ metric }: { metric: MetricState }) {
  const isLoading = metric.kind === "loading";
  const isError = metric.kind === "error";
  const data = metric.kind === "ready" ? metric.data : undefined;

  const meetsTarget = data !== undefined && data.percent > TARGET_PERCENT;
  const percentStr = data !== undefined ? `${data.percent.toFixed(1)}%` : "—";
  const subStr =
    data !== undefined ? `${data.gated}/${data.total} deployments` : "";

  return (
    <Card data-testid="eval-gated-stat">
      <CardContent className="p-4">
        <div className="flex items-center justify-between">
          <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            Eval-gated deploys
          </p>
          <ShieldCheck
            className={`h-4 w-4 shrink-0 ${
              meetsTarget ? "text-success" : "text-muted-foreground"
            }`}
          />
        </div>
        <p
          className={`mt-2 text-2xl font-semibold tracking-tight tabular-nums ${
            isLoading || isError ? "text-muted-foreground" : ""
          }`}
          data-testid="eval-gated-percent"
        >
          {isLoading ? "…" : percentStr}
        </p>
        <p className="mt-0.5 h-4 text-xs text-muted-foreground" data-testid="eval-gated-sub">
          {isLoading ? "" : (isError ? "Couldn't load metric" : subStr)}
        </p>
        {/* PRD §5 target indicator: >50% threshold */}
        {data !== undefined && (
          <p
            className={`mt-1 text-xs font-medium ${
              meetsTarget ? "text-success" : "text-muted-foreground"
            }`}
            data-testid="eval-gated-target"
          >
            {meetsTarget
              ? `✓ Above ${TARGET_PERCENT}% target`
              : `Target: >${TARGET_PERCENT}% (PRD §5)`}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

export function EvalsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [showBuilder, setShowBuilder] = React.useState(false);
  const [expandedSuite, setExpandedSuite] = React.useState<string | null>(null);
  const [resultsMap, setResultsMap] = React.useState<Map<string, ResultsState>>(
    new Map(),
  );
  const [deleteTarget, setDeleteTarget] = React.useState<EvalSuiteSummary | null>(
    null,
  );
  const [deleteState, setDeleteState] = React.useState<DeleteState>({ kind: "idle" });
  // PRD §5 eval-gated metric — live snapshot of how many deploys are eval-gated.
  const [metric, setMetric] = React.useState<MetricState>({ kind: "loading" });

  const { can } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();

  const canCreate = can(RES_EVALSUITES, "create");
  const canDelete = can(RES_EVALSUITES, "delete");

  const load = React.useCallback(
    (signal?: AbortSignal) => {
      setPage({ kind: "loading" });
      setMetric({ kind: "loading" });

      // Load EvalSuite list.
      api
        .listEvalSuites({ namespace: shellNs || undefined }, signal)
        .then((res) => {
          if (signal?.aborted) return;
          if (res.items.length === 0) {
            setPage({ kind: "empty" });
          } else {
            setPage({ kind: "ready", suites: res.items });
          }
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          if (err instanceof ApiError) {
            if (err.isForbidden) {
              setPage({ kind: "forbidden", message: err.message });
              return;
            }
            setPage({ kind: "error", message: err.message });
            return;
          }
          setPage({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
          });
        });

      // Load eval-gated metric (PRD §5, ADR 0062 governance #2).
      api
        .evalGatedMetric({ namespace: shellNs || undefined, signal })
        .then((data) => {
          if (signal?.aborted) return;
          setMetric({ kind: "ready", data });
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          // 501 = endpoint not yet wired (dev substrate) — calm degrade.
          if (err instanceof ApiError && err.status === 501) {
            setMetric({ kind: "unavailable" });
            return;
          }
          setMetric({ kind: "error" });
        });
    },
    [shellNs],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  function loadResults(suite: EvalSuiteSummary) {
    const key = `${suite.namespace}/${suite.name}`;
    setResultsMap((m) => new Map(m).set(key, { kind: "loading" }));
    api
      .evalSuiteResults(suite.namespace, suite.name)
      .then((results) => {
        setResultsMap((m) => new Map(m).set(key, { kind: "ready", results }));
      })
      .catch((err: unknown) => {
        if (err instanceof ApiError) {
          if (err.isForbidden) {
            setResultsMap((m) =>
              new Map(m).set(key, { kind: "forbidden", message: err.message }),
            );
            return;
          }
          setResultsMap((m) =>
            new Map(m).set(key, { kind: "error", message: err.message }),
          );
          return;
        }
        setResultsMap((m) =>
          new Map(m).set(key, {
            kind: "error",
            message: err instanceof Error ? err.message : "failed to load results",
          }),
        );
      });
  }

  function handleRowExpand(suite: EvalSuiteSummary) {
    const key = `${suite.namespace}/${suite.name}`;
    if (expandedSuite === key) {
      setExpandedSuite(null);
    } else {
      setExpandedSuite(key);
      const existing = resultsMap.get(key);
      if (!existing || existing.kind === "error") {
        loadResults(suite);
      }
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    setDeleteState({ kind: "deleting" });
    try {
      await api.removeEvalSuite(deleteTarget.namespace, deleteTarget.name);
      setDeleteState({ kind: "idle" });
      setDeleteTarget(null);
      toast({
        variant: "success",
        title: `Deleted ${deleteTarget.name}`,
      });
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

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="evals-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Evals</h2>
          <p className="text-sm text-muted-foreground">
            Build and run eval suites. Results show the controller's gate
            outcome; scores only when Langfuse is wired.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="icon"
            onClick={() => load()}
            aria-label="Refresh evals"
            data-testid="evals-refresh"
          >
            <RefreshCw className="h-4 w-4" />
          </Button>
          {canCreate && (
            <Button
              size="sm"
              onClick={() => setShowBuilder(true)}
              data-testid="evals-new-btn"
            >
              New eval suite
            </Button>
          )}
        </div>
      </div>

      {/* PRD §5 eval-gated metric — live snapshot (ADR 0062 governance #2).
          Hides when the endpoint is unavailable (dev substrate, 501). */}
      {metric.kind !== "unavailable" && (
        <EvalGatedStatCard metric={metric} />
      )}

      {page.kind === "loading" && <SkeletonTable rows={4} />}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to view eval suites"
          description="Reading eval suites requires list permission."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load eval suites"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "empty" && (
        <EmptyState
          icon={TestTube2}
          title="No eval suites yet"
          description="Create an eval suite to define a dataset + scorers + gate for your agents."
          action={
            canCreate
              ? {
                  label: "New eval suite",
                  onClick: () => setShowBuilder(true),
                  variant: "default",
                }
              : undefined
          }
        />
      )}

      {page.kind === "ready" && (
        <div className="rounded-lg border bg-card shadow-card divide-y" data-testid="eval-suite-list">
          {page.suites.map((suite) => {
            const key = `${suite.namespace}/${suite.name}`;
            const isExpanded = expandedSuite === key;
            const results = resultsMap.get(key);
            return (
              <div key={key} data-testid={`eval-suite-${suite.name}`}>
                <div className="flex items-center gap-4 px-4 py-3">
                  <TestTube2 className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <div className="min-w-0 flex-1 space-y-0.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-sm font-medium">
                        {suite.name}
                      </span>
                      <Badge variant="secondary" className="text-[10px]">
                        {suite.namespace}
                      </Badge>
                      {suite.gate && (
                        <Badge variant="outline" className="text-[10px]">
                          gate: {suite.gate}
                          {suite.threshold !== undefined
                            ? ` ≥ ${suite.threshold}`
                            : ""}
                        </Badge>
                      )}
                      <Badge
                        variant={suite.ready ? "secondary" : "warning"}
                        className="text-[10px]"
                      >
                        {suite.ready ? "ready" : "not ready"}
                      </Badge>
                    </div>
                    <p className="font-mono text-xs text-muted-foreground">
                      dataset: {suite.datasetRef}
                    </p>
                    {suite.scorers.length > 0 && (
                      <p className="text-xs text-muted-foreground">
                        scorers: {suite.scorers.join(", ")}
                      </p>
                    )}
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => handleRowExpand(suite)}
                      data-testid={`eval-results-${suite.name}`}
                    >
                      {isExpanded ? "Hide results" : "View results"}
                      <ChevronRight
                        className={`ml-1 h-3.5 w-3.5 transition-transform ${
                          isExpanded ? "rotate-90" : ""
                        }`}
                      />
                    </Button>
                    {canDelete && (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(suite)}
                        data-testid={`eval-delete-${suite.name}`}
                      >
                        Delete
                      </Button>
                    )}
                  </div>
                </div>
                {isExpanded && (
                  <div className="border-t bg-muted/30 px-4 py-4">
                    <ResultsPanel results={results ?? { kind: "loading" }} suiteName={suite.name} />
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {showBuilder && (
        <EvalBuilderWizard
          onClose={() => setShowBuilder(false)}
          onCreated={() => {
            setShowBuilder(false);
            load();
          }}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          open
          title={`Delete ${deleteTarget.name}?`}
          description={`This will delete the eval suite "${deleteTarget.name}" in namespace "${deleteTarget.namespace}". This cannot be undone.`}
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

// ---- ResultsPanel ------------------------------------------------------------
// Renders the honest results from GET .../results:
//   • conditions → the controller's gate outcome (always)
//   • scoresAvailable=true → render scores (real numbers only)
//   • scoresAvailable=false → render scoresUnavailableReason calmly
// NEVER fabricate scores.

interface ResultsPanelProps {
  results: ResultsState;
  suiteName: string;
}

function ResultsPanel({ results, suiteName }: ResultsPanelProps) {
  if (results.kind === "idle") return null;

  if (results.kind === "loading") {
    return (
      <div
        className="flex items-center gap-2 py-2 text-sm text-muted-foreground"
        data-testid={`eval-results-loading-${suiteName}`}
      >
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading results…
      </div>
    );
  }

  if (results.kind === "forbidden") {
    return (
      <p
        className="text-sm text-muted-foreground"
        data-testid={`eval-results-forbidden-${suiteName}`}
      >
        Not allowed to view results for this suite.
      </p>
    );
  }

  if (results.kind === "error") {
    return (
      <p
        className="text-sm text-destructive"
        role="alert"
        data-testid={`eval-results-error-${suiteName}`}
      >
        Failed to load results: {results.message}
      </p>
    );
  }

  const { conditions, scoresAvailable, scores, scoresUnavailableReason } =
    results.results;

  return (
    <div className="space-y-4" data-testid={`eval-results-panel-${suiteName}`}>
      {/* Gate outcome from conditions */}
      <div>
        <h4 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-2">
          Gate outcome
        </h4>
        {conditions.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No conditions reported yet.
          </p>
        ) : (
          <div className="space-y-2">
            {conditions.map((c: EvalCondition, i: number) => (
              <ConditionRow key={i} condition={c} />
            ))}
          </div>
        )}
      </div>

      {/* Scores — only when scoresAvailable=true */}
      <div>
        <h4 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-2">
          Scores
        </h4>
        {scoresAvailable ? (
          scores && scores.length > 0 ? (
            <div className="space-y-1">
              {scores.map((s: EvalScore, i: number) => (
                <ScoreRow key={i} score={s} />
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">
              No scores returned from scorers.
            </p>
          )
        ) : (
          // scoresAvailable=false — surface reason CALMLY, never fabricate
          <p
            className="text-sm text-muted-foreground"
            data-testid={`eval-scores-unavailable-${suiteName}`}
          >
            {scoresUnavailableReason
              ? scoresUnavailableReason
              : "Scores unavailable."}
          </p>
        )}
      </div>
    </div>
  );
}

function ConditionRow({ condition }: { condition: EvalCondition }) {
  const isTrue = condition.status === "True";
  const isFalse = condition.status === "False";
  return (
    <div className="flex items-start gap-2 text-sm">
      {isTrue ? (
        <CheckCircle className="mt-0.5 h-4 w-4 shrink-0 text-success" />
      ) : isFalse ? (
        <XCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
      ) : (
        <Loader2 className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
      )}
      <div>
        <span className="font-mono text-xs font-medium">{condition.type}</span>
        {condition.reason && (
          <span className="ml-2 text-xs text-muted-foreground">
            ({condition.reason})
          </span>
        )}
        {condition.message && (
          <p className="text-xs text-muted-foreground">{condition.message}</p>
        )}
      </div>
    </div>
  );
}

function ScoreRow({ score }: { score: EvalScore }) {
  const display =
    score.value !== undefined
      ? String(score.value)
      : score.stringValue !== undefined
      ? score.stringValue
      : "—";
  return (
    <div className="flex items-center gap-2 text-sm">
      <span className="font-mono text-xs text-muted-foreground w-32 truncate">
        {score.scorer}
      </span>
      <span className="font-mono text-xs font-medium">{display}</span>
    </div>
  );
}

// ---- EvalBuilderWizard -------------------------------------------------------
// A 4-step wizard: dataset ref → scorers → gate/threshold → review + create.
// The wizard calls createEvalSuite on finish. A 403 surfaces honestly.

interface EvalBuilderWizardProps {
  onClose: () => void;
  onCreated: () => void;
}

function EvalBuilderWizard({ onClose, onCreated }: EvalBuilderWizardProps) {
  const [step, setStep] = React.useState(STEP_DATASET);
  const [datasetRef, setDatasetRef] = React.useState("");
  const [scorersInput, setScorersInput] = React.useState("");
  const [gate, setGate] = React.useState("");
  const [threshold, setThreshold] = React.useState("");
  const [name, setName] = React.useState("");
  const [namespace, setNamespace] = React.useState("");
  const [builderState, setBuilderState] = React.useState<BuilderState>({
    kind: "idle",
  });

  const { can, reprobe } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();

  React.useEffect(() => {
    if (shellNs) setNamespace(shellNs);
  }, [shellNs]);

  const scorers = React.useMemo(
    () =>
      scorersInput
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean),
    [scorersInput],
  );

  const canProceed = React.useMemo(() => {
    if (step === STEP_DATASET) return datasetRef.trim().length > 0;
    if (step === STEP_SCORERS) return scorers.length > 0;
    if (step === STEP_GATE) return true; // gate is optional
    if (step === STEP_REVIEW) return false; // handled by finish
    return true;
  }, [step, datasetRef, scorers]);

  async function handleCreate() {
    setBuilderState({ kind: "submitting" });
    try {
      const thresholdNum = threshold.trim()
        ? parseFloat(threshold.trim())
        : undefined;
      const created = await api.createEvalSuite({
        name: name.trim() || undefined,
        namespace: namespace.trim() || undefined,
        datasetRef: datasetRef.trim(),
        scorers,
        gate: gate.trim() || undefined,
        threshold: thresholdNum,
      });
      setBuilderState({ kind: "done", suite: created });
      toast({
        variant: "success",
        title: `Created ${created.name}`,
        description: "Eval suite is created and will be reconciled.",
      });
      onCreated();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setBuilderState({
            kind: "error",
            message: `Not allowed: ${err.message}`,
            forbidden: true,
          });
          return;
        }
        setBuilderState({ kind: "error", message: err.message });
        return;
      }
      setBuilderState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
      });
    }
  }

  const busy = builderState.kind === "submitting";
  const canBuild = can(RES_EVALSUITES, "create");

  const steps: WizardStep[] = [
    {
      id: "dataset",
      title: "Dataset",
      description: "Specify the evaluation dataset reference",
      content: (
        <div className="space-y-4" data-testid="eval-builder-dataset-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-dataset-ref">
              Dataset ref <span className="text-destructive">*</span>
            </Label>
            <Input
              id="eval-dataset-ref"
              value={datasetRef}
              onChange={(e) => setDatasetRef(e.target.value)}
              placeholder="my-dataset or gs://bucket/eval.jsonl"
              data-testid="eval-dataset-ref-input"
            />
            <p className="text-xs text-muted-foreground">
              A name or URI pointing to the evaluation dataset.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-name">
              Suite name{" "}
              <span className="text-muted-foreground">(auto-generated)</span>
            </Label>
            <Input
              id="eval-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-eval-suite"
              data-testid="eval-name-input"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-namespace">Namespace</Label>
            <Input
              id="eval-namespace"
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
              placeholder="default"
              data-testid="eval-namespace-input"
            />
          </div>
        </div>
      ),
    },
    {
      id: "scorers",
      title: "Scorers",
      description: "Choose the scorers to run",
      content: (
        <div className="space-y-4" data-testid="eval-builder-scorers-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-scorers">
              Scorers <span className="text-destructive">*</span>
            </Label>
            <Input
              id="eval-scorers"
              value={scorersInput}
              onChange={(e) => setScorersInput(e.target.value)}
              placeholder="exact-match, bleu, rouge"
              data-testid="eval-scorers-input"
            />
            <p className="text-xs text-muted-foreground">
              Comma-separated scorer names.
            </p>
          </div>
          {scorers.length > 0 && (
            <div className="flex flex-wrap gap-1">
              {scorers.map((s) => (
                <Badge key={s} variant="secondary" className="text-[10px]">
                  {s}
                </Badge>
              ))}
            </div>
          )}
        </div>
      ),
    },
    {
      id: "gate",
      title: "Gate",
      description: "Optional pass/fail gate and threshold",
      content: (
        <div className="space-y-4" data-testid="eval-builder-gate-step">
          <div className="space-y-1.5">
            <Label htmlFor="eval-gate">
              Gate condition{" "}
              <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="eval-gate"
              value={gate}
              onChange={(e) => setGate(e.target.value)}
              placeholder="exact-match"
              data-testid="eval-gate-input"
            />
            <p className="text-xs text-muted-foreground">
              The scorer whose value is compared against the threshold.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eval-threshold">
              Threshold{" "}
              <span className="text-muted-foreground">(optional, 0–1)</span>
            </Label>
            <Input
              id="eval-threshold"
              type="number"
              min="0"
              max="1"
              step="0.01"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
              placeholder="0.8"
              data-testid="eval-threshold-input"
            />
          </div>
        </div>
      ),
    },
    {
      id: "review",
      title: "Review",
      description: "Confirm the eval suite definition",
      review: true,
      content: (
        <div className="space-y-4" data-testid="eval-builder-review-step">
          <div className="rounded-lg border bg-card/50 p-4 space-y-2">
            <ReviewRow label="Name" value={name || "(auto-generated)"} />
            <ReviewRow label="Namespace" value={namespace || "default"} />
            <ReviewRow label="Dataset" value={datasetRef} mono />
            <ReviewRow
              label="Scorers"
              value={scorers.join(", ") || "—"}
            />
            {gate && (
              <ReviewRow
                label="Gate"
                value={`${gate}${threshold ? ` ≥ ${threshold}` : ""}`}
              />
            )}
          </div>
          {builderState.kind === "error" && (
            <p
              className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
              role="alert"
              data-testid="eval-builder-error"
            >
              {builderState.message}
            </p>
          )}
          {!canBuild && (
            <p className="text-sm text-muted-foreground">
              You don't have permission to create eval suites.
            </p>
          )}
        </div>
      ),
    },
  ];

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      data-testid="eval-builder"
    >
      <div className="w-full max-w-2xl rounded-xl bg-background shadow-xl">
        <div className="p-6">
          <h3 className="text-lg font-semibold">New eval suite</h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Define a dataset, scorers, and optional gate to evaluate your agents.
          </p>
        </div>
        <div className="px-6 pb-6">
          <Wizard
            steps={steps}
            current={step}
            onStepChange={setStep}
            canProceed={step === STEP_REVIEW ? canBuild && !busy : canProceed}
            busy={busy}
            onFinish={handleCreate}
            finishLabel={busy ? "Creating…" : "Create"}
            nextLabel="Next"
            onCancel={onClose}
          />
        </div>
      </div>
    </div>
  );
}

function ReviewRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="flex items-start gap-2">
      <span className="text-sm font-medium text-muted-foreground w-24 shrink-0">
        {label}
      </span>
      <span className={`text-sm ${mono ? "font-mono" : ""}`}>{value}</span>
    </div>
  );
}
