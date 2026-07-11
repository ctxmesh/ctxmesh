import * as React from "react";
import { Link } from "react-router-dom";
import { ChevronDown, ExternalLink, EyeOff } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { buttonVariants } from "@/components/ui/button";
import { ForbiddenInline } from "@/components/kit";
import { api, ApiError, type SpanSummary, type TraceDetailResponse } from "@/lib/api";

// RunInspector — the NATIVE, on-theme run summary for one trace (first-agent-flow
// §5, m14.11). It reads GET /api/traces/{id}/detail (a FLAT span list + rollup)
// and builds the tree/waterfall CLIENT-side: key on id, tree from parentId, plot
// each span at startMs for width durationMs against the trace's own span. This is
// the M14 SUMMARY — a clean span tree + a lightweight waterfall + a span-detail
// view — NOT the full M16 forensics explorer. "Open in Langfuse" stays a link-out
// (the shipped GET /api/traces/{id} embed route, resolved server-side).
//
// Redaction-honest (M11): a span whose input/output was scrubbed carries the
// *Redacted flags; the detail view shows a MARKER over the span's structure
// (name/timing/tokens/cost stay visible), never a blank field and never an
// attempt to reveal. A `status: "error"` (Level ERROR) span shows the error dot.

// spanKind classifies a flat span into a display kind for its color/label: the
// GENERATION observations are LLM calls; a SPAN named like a tool call is a tool
// span (the aha — its tokens/cost are visible); the root (no parent) is the run.
type SpanKind = "run" | "llm" | "tool" | "internal";

const KIND_DOT: Record<SpanKind, string> = {
  run: "bg-primary",
  llm: "bg-info",
  tool: "bg-success",
  internal: "bg-muted-foreground",
};

function classifySpan(span: SpanSummary, isRoot: boolean): SpanKind {
  if (isRoot) return "run";
  if (span.type === "GENERATION") return "llm";
  // A non-generation SPAN that isn't the root is a tool/step call. The tool span
  // is the one that carries the aha — surface it as a distinct "tool" kind.
  if (span.type === "SPAN") return "tool";
  return "internal";
}

// TreeNode is a flat span plus its computed depth (indent) — the tree is walked
// depth-first so children render under their parent in the (indented) list.
interface TreeNode {
  span: SpanSummary;
  depth: number;
  kind: SpanKind;
}

// buildTree turns the FLAT parentId-linked spans into a depth-ordered list. It is
// defensive against a cyclic/missing parent (a span whose parentId names no known
// span, or a cycle) — such spans are treated as roots so nothing is dropped and
// the walk always terminates (the aha must never crash on odd trace data).
function buildTree(spans: SpanSummary[]): TreeNode[] {
  const byId = new Map<string, SpanSummary>();
  for (const s of spans) byId.set(s.id, s);

  const childrenOf = new Map<string, SpanSummary[]>();
  const roots: SpanSummary[] = [];
  for (const s of spans) {
    const hasParent = s.parentId !== "" && byId.has(s.parentId) && s.parentId !== s.id;
    if (hasParent) {
      const list = childrenOf.get(s.parentId) ?? [];
      list.push(s);
      childrenOf.set(s.parentId, list);
    } else {
      roots.push(s);
    }
  }
  // Children ordered by start time so the waterfall reads left-to-right in-list.
  const byStart = (a: SpanSummary, b: SpanSummary) => a.startMs - b.startMs;
  roots.sort(byStart);

  const isSingleRoot = roots.length === 1;
  const out: TreeNode[] = [];
  const seen = new Set<string>();
  const walk = (span: SpanSummary, depth: number, forceRoot: boolean) => {
    if (seen.has(span.id)) return; // cycle guard — never revisit.
    seen.add(span.id);
    out.push({ span, depth, kind: classifySpan(span, forceRoot) });
    const kids = (childrenOf.get(span.id) ?? []).slice().sort(byStart);
    for (const k of kids) walk(k, depth + 1, false);
  };
  for (const r of roots) walk(r, 0, isSingleRoot);
  // Any span not reachable from a root (orphaned by a cycle) still renders.
  for (const s of spans) if (!seen.has(s.id)) walk(s, 0, false);
  return out;
}

// traceWindow is the [start, end] the waterfall plots against — the min start to
// the max end across all spans (so a child that overruns the root still fits).
function traceWindow(spans: SpanSummary[]): { start: number; span: number } {
  if (spans.length === 0) return { start: 0, span: 1 };
  let start = Infinity;
  let end = -Infinity;
  for (const s of spans) {
    start = Math.min(start, s.startMs);
    end = Math.max(end, s.startMs + s.durationMs);
  }
  const span = Math.max(end - start, 1); // never divide by zero.
  return { start, span };
}

function fmtCost(usd: number): string {
  if (usd === 0) return "$0.000";
  return usd < 0.001 ? `$${usd.toFixed(5)}` : `$${usd.toFixed(3)}`;
}

type State =
  | { kind: "loading" }
  | { kind: "ingesting" } // 404 → Langfuse ingestion lag; polling, not a failure
  | { kind: "ready"; detail: TraceDetailResponse }
  | { kind: "error"; message: string; forbidden: boolean };

// ingestBackoffMs is the poll schedule for a freshly-completed run whose trace has
// not landed in Langfuse yet (~20s lag). ~50s total before declaring not-found.
const ingestBackoffMs = [1500, 2000, 3000, 4000, 5000, 6000, 8000, 10000, 12000];

export function RunInspector({ traceId }: { traceId: string }) {
  const [state, setState] = React.useState<State>({ kind: "loading" });
  // The selected span id — defaults to the first tool span (the aha) if present,
  // else the root, so opening the inspector lands on something meaningful.
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  // The Langfuse link-out target, resolved lazily (the embed route). Absent →
  // the button is simply not shown; it is never the primary surface.
  const [langfuseUrl, setLangfuseUrl] = React.useState<string | null>(null);

  React.useEffect(() => {
    const controller = new AbortController();
    setState({ kind: "loading" });
    setSelectedId(null);

    // Langfuse ingestion lags a run by ~20s, so a just-completed run's trace 404s
    // briefly. Poll with backoff and show a calm "still landing" state instead of a
    // hard "trace not found" error (m18.7 — the shakedown run-inspector bug).
    let timer: ReturnType<typeof setTimeout> | undefined;
    const attempt = (i: number) => {
      api
        .traceDetail(traceId, controller.signal)
        .then((detail) => {
          if (controller.signal.aborted) return;
          setState({ kind: "ready", detail });
        })
        .catch((err: unknown) => {
          if (controller.signal.aborted) return;
          if (err instanceof ApiError && err.isNotFound && i < ingestBackoffMs.length) {
            setState({ kind: "ingesting" });
            timer = setTimeout(() => attempt(i + 1), ingestBackoffMs[i]);
            return;
          }
          const forbidden = err instanceof ApiError && err.isForbidden;
          setState({
            kind: "error",
            message:
              err instanceof ApiError && err.isNotFound
                ? "The trace hasn't appeared yet — it may still be ingesting. Try reopening the run in a moment."
                : err instanceof Error
                  ? err.message
                  : "couldn't load the run",
            forbidden,
          });
        });
    };
    attempt(0);

    return () => {
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [traceId]);

  // Resolve the Langfuse link-out in the background (best-effort — a failure just
  // hides the button; the native summary is the primary surface).
  React.useEffect(() => {
    const controller = new AbortController();
    setLangfuseUrl(null);
    api
      .traceLink(traceId, controller.signal)
      .then((res) => {
        if (!controller.signal.aborted) setLangfuseUrl(res.url);
      })
      .catch(() => {
        /* link-out is optional — swallow. */
      });
    return () => controller.abort();
  }, [traceId]);

  const nodes = React.useMemo(
    () => (state.kind === "ready" ? buildTree(state.detail.spans) : []),
    [state],
  );
  const window = React.useMemo(
    () => (state.kind === "ready" ? traceWindow(state.detail.spans) : { start: 0, span: 1 }),
    [state],
  );

  // Default selection: the first tool span, else the root, else the first node.
  const effectiveSelected = React.useMemo(() => {
    if (selectedId) return selectedId;
    const tool = nodes.find((n) => n.kind === "tool");
    if (tool) return tool.span.id;
    return nodes[0]?.span.id ?? null;
  }, [selectedId, nodes]);

  if (state.kind === "loading") {
    return (
      <div
        className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground shadow-card"
        data-testid="run-inspector-loading"
      >
        Loading the run…
      </div>
    );
  }

  if (state.kind === "ingesting") {
    return (
      <div
        className="flex h-40 flex-col items-center justify-center gap-1 rounded-lg border bg-card text-sm text-muted-foreground shadow-card"
        data-testid="run-inspector-ingesting"
      >
        <span>The trace is still landing…</span>
        <span className="text-xs">Traces take ~20s to ingest after a run completes.</span>
      </div>
    );
  }

  if (state.kind === "error") {
    return state.forbidden ? (
      <ForbiddenInline
        title="Not allowed to read this run"
        description="Your account can't read traces in this cluster."
        detail={state.message}
      />
    ) : (
      <div
        className="flex h-40 items-center justify-center rounded-lg border bg-card text-sm text-destructive shadow-card"
        role="alert"
        data-testid="run-inspector-error"
      >
        Couldn't load the run: {state.message}
      </div>
    );
  }

  const { rollup } = state.detail;
  const active = nodes.find((n) => n.span.id === effectiveSelected) ?? null;

  return (
    <div className="space-y-3" data-testid="run-inspector">
      <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
        {/* ── Span tree + waterfall ─────────────────────────────────────── */}
        <div className="overflow-hidden rounded-lg border bg-card shadow-card">
          <div className="flex items-center justify-between border-b bg-surface-2/60 px-4 py-2 text-xs text-muted-foreground">
            <span data-testid="run-inspector-title">
              {rollup.name || "Run"} · {rollup.spanCount} span
              {rollup.spanCount === 1 ? "" : "s"}
            </span>
            <span className="tabular-nums">
              {Math.round(rollup.latencyMs)}ms · {rollup.tokens.toLocaleString()} tok ·{" "}
              {fmtCost(rollup.costUSD)}
            </span>
          </div>
          {nodes.length === 0 ? (
            <p className="px-4 py-8 text-center text-sm text-muted-foreground">
              This run produced no spans.
            </p>
          ) : (
            <div role="tree" aria-label="Run spans">
              {nodes.map((node) => {
                const s = node.span;
                const isSel = s.id === effectiveSelected;
                const leftPct = ((s.startMs - window.start) / window.span) * 100;
                const widthPct = Math.max((s.durationMs / window.span) * 100, 1.5);
                const redacted = s.inputRedacted || s.outputRedacted;
                return (
                  <button
                    key={s.id}
                    type="button"
                    role="treeitem"
                    onClick={() => setSelectedId(s.id)}
                    data-testid={`span-row-${s.id}`}
                    className={`grid w-full grid-cols-[15rem_1fr] items-center gap-2 border-b border-border/50 px-3 py-2 text-left transition-colors last:border-0 ${
                      isSel ? "bg-accent" : "hover:bg-surface-2/60"
                    }`}
                  >
                    <span
                      className="flex min-w-0 items-center gap-1.5 truncate text-sm"
                      style={{ paddingLeft: `${node.depth * 14}px` }}
                    >
                      {node.kind === "run" ? (
                        <ChevronDown className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                      ) : (
                        <span className="w-3.5 shrink-0" />
                      )}
                      {s.status === "error" && (
                        <span
                          className="h-2 w-2 shrink-0 rounded-full bg-destructive"
                          data-testid={`span-error-dot-${s.id}`}
                          aria-label="error"
                        />
                      )}
                      {redacted && (
                        <EyeOff
                          className="h-3.5 w-3.5 shrink-0 text-warning"
                          data-testid={`span-redacted-icon-${s.id}`}
                          aria-label="redacted"
                        />
                      )}
                      <span className={`h-2 w-2 shrink-0 rounded-full ${KIND_DOT[node.kind]}`} />
                      <span className="truncate">{s.name || s.type || s.id}</span>
                    </span>
                    <span className="relative h-4">
                      <span
                        className={`absolute top-1/2 h-2.5 -translate-y-1/2 rounded-sm ${KIND_DOT[node.kind]} ${
                          redacted ? "opacity-40" : ""
                        }`}
                        style={{ left: `${leftPct}%`, width: `${widthPct}%` }}
                      />
                      <span className="absolute right-1 top-1/2 -translate-y-1/2 text-[10px] tabular-nums text-muted-foreground">
                        {s.durationMs}ms
                      </span>
                    </span>
                  </button>
                );
              })}
            </div>
          )}
        </div>

        {/* ── Span detail ───────────────────────────────────────────────── */}
        <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="span-detail">
          {active ? (
            <SpanDetail node={active} />
          ) : (
            <p className="text-sm text-muted-foreground">Select a span to inspect it.</p>
          )}
        </div>
      </div>

      {/* Action row: View full trace (m16.7) + optional Langfuse link-out. */}
      <div className="flex flex-wrap gap-2">
        {/* "View full trace" navigates to the native trace page (m16.7). */}
        <Link
          to={`/traces/${encodeURIComponent(traceId)}`}
          data-testid="view-full-trace"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          View full trace
        </Link>

        {/* Langfuse stays a LINK-OUT (never the primary surface). */}
        {langfuseUrl && (
          <a
            href={langfuseUrl}
            target="_blank"
            rel="noreferrer"
            data-testid="open-in-langfuse"
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            <ExternalLink className="h-4 w-4" />
            Open in Langfuse
          </a>
        )}
      </div>
    </div>
  );
}

// SpanDetail renders one selected span: identity + timing + (for a generation)
// tokens/cost, then input/output — or a REDACTED marker over the structure when
// the content was scrubbed (never a blank, never a reveal attempt).
function SpanDetail({ node }: { node: TreeNode }) {
  const s = node.span;
  const showTokens = s.tokensIn > 0 || s.tokensOut > 0;
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className={`h-2.5 w-2.5 shrink-0 rounded-full ${KIND_DOT[node.kind]}`} />
        <p className="truncate text-sm font-medium">{s.name || s.type || s.id}</p>
        {s.status === "error" && (
          <Badge variant="destructive" className="text-[10px]">
            error
          </Badge>
        )}
      </div>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
        <KV k="Kind" v={node.kind} />
        <KV k="Duration" v={`${s.durationMs}ms`} />
        {s.model && <KV k="Model" v={<span className="font-mono text-xs">{s.model}</span>} />}
        {showTokens && <KV k="Tokens" v={`${s.tokensIn} in / ${s.tokensOut} out`} />}
        <KV k="Cost" v={<span className="tabular-nums">{fmtCost(s.costUSD)}</span>} />
        {s.level && s.level !== "DEFAULT" && <KV k="Level" v={s.level} />}
      </dl>

      <SpanField
        label="Input"
        content={s.input}
        redacted={s.inputRedacted}
        testid={`span-input-${s.id}`}
      />
      <SpanField
        label="Output"
        content={s.output}
        redacted={s.outputRedacted}
        testid={`span-output-${s.id}`}
      />
    </div>
  );
}

function KV({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="text-right">{v}</dd>
    </>
  );
}

// SpanField shows one input/output block. When redacted it renders a MARKER over
// the field's place (the redaction-honest rule) — never the raw content, never a
// blank pretending the field is empty. When simply absent (not redacted) it shows
// a muted em-dash. Otherwise the persisted (already-redacted) content, verbatim.
function SpanField({
  label,
  content,
  redacted,
  testid,
}: {
  label: string;
  content: string;
  redacted: boolean;
  testid: string;
}) {
  return (
    <div>
      <p className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </p>
      {redacted ? (
        <div
          className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-xs text-muted-foreground"
          data-testid={`${testid}-redacted`}
        >
          <EyeOff className="h-3.5 w-3.5 shrink-0 text-warning" />
          <span>Redacted by trace-governance policy — timing and cost are still shown.</span>
        </div>
      ) : content ? (
        <pre
          className="max-h-40 overflow-auto rounded-md bg-surface-3 p-3 text-xs"
          data-testid={testid}
        >
          {content}
        </pre>
      ) : (
        <p className="text-xs text-muted-foreground" data-testid={`${testid}-empty`}>
          —
        </p>
      )}
    </div>
  );
}
