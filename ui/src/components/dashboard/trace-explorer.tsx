import * as React from "react";
import { ChevronDown, ChevronRight, EyeOff } from "lucide-react";

import type { SpanSummary } from "@/lib/api";

// TraceExplorer — the M16 native span-tree surface (m16.6). It receives DFS-
// ordered SpanSummary[] from GET /api/traces/{id}/detail (the m16.2 backend
// now sends nestingDepth on each span so the UI never has to re-walk the tree).
//
// For each span it renders:
//   • a disclosure row indented by nestingDepth (14px per level).
//   • a kind badge (SPAN/GENERATION/EVENT; tool spans have a distinct "tool" mark).
//   • a timing waterfall bar: the span's [startMs, startMs+durationMs] plotted
//     against the trace window (min start → max end across all spans). The bar
//     position/width is CSS % so the layout is responsive and pixel-free.
//   • per-span tokens and cost (tabular-nums, hidden when both are zero).
//   • an I/O expand toggle — shows the persisted (already-redacted) input/output;
//     when inputRedacted/outputRedacted the panel shows an honest marker ONLY,
//     NEVER the content (redaction-honest, ADR from M11).
//
// data-testid contract (non-negotiable):
//   trace-explorer            — root container
//   span-row-{id}             — one disclosure row
//   span-kind-{id}            — the kind badge
//   span-timing-bar-{id}      — the waterfall bar element
//   span-io-toggle-{id}       — the I/O expand button
//   span-tokens-{id}          — the tokens cell (may be empty string when 0)
//   span-cost-{id}            — the cost cell

// ── helpers ──────────────────────────────────────────────────────────────────

/** Tool spans are SPAN-typed but carry a name that starts with "tool:" or
 *  "tool_". Classify defensively — false negatives are fine (still a SPAN). */
function isToolSpan(span: SpanSummary, isRoot: boolean): boolean {
  if (isRoot) return false;
  if (span.type !== "SPAN") return false;
  const n = span.name.toLowerCase();
  return n.startsWith("tool:") || n.startsWith("tool_") || n.startsWith("tool ");
}

type KindLabel = "RUN" | "GENERATION" | "TOOL" | "EVENT" | "SPAN";

function kindOf(span: SpanSummary, depth: number): KindLabel {
  if (depth === 0) return "RUN";
  if (span.type === "GENERATION") return "GENERATION";
  if (span.type === "EVENT") return "EVENT";
  if (isToolSpan(span, depth === 0)) return "TOOL";
  return "SPAN";
}

const KIND_BADGE_VARIANT: Record<KindLabel, string> = {
  RUN: "bg-primary/15 text-primary",
  GENERATION: "bg-info/15 text-info",
  TOOL: "bg-success/15 text-success",
  EVENT: "bg-muted text-muted-foreground",
  SPAN: "bg-muted text-muted-foreground",
};

const WATERFALL_COLOR: Record<KindLabel, string> = {
  RUN: "bg-primary",
  GENERATION: "bg-info",
  TOOL: "bg-success",
  EVENT: "bg-muted-foreground",
  SPAN: "bg-muted-foreground",
};

function fmtCost(usd: number): string {
  if (usd === 0) return "$0.000";
  return usd < 0.001 ? `$${usd.toFixed(5)}` : `$${usd.toFixed(3)}`;
}

/** The trace-level window: min start → max end across all spans. */
function traceWindow(spans: SpanSummary[]): { start: number; totalMs: number } {
  if (spans.length === 0) return { start: 0, totalMs: 1 };
  let start = Infinity;
  let end = -Infinity;
  for (const s of spans) {
    start = Math.min(start, s.startMs);
    end = Math.max(end, s.startMs + s.durationMs);
  }
  return { start, totalMs: Math.max(end - start, 1) };
}

// ── sub-components ────────────────────────────────────────────────────────────

function SpanIOExpanded({
  span,
  expanded,
}: {
  span: SpanSummary;
  expanded: boolean;
}) {
  if (!expanded) return null;
  return (
    <div className="col-span-full border-t border-border/40 bg-surface-2/40 px-4 py-3 text-xs">
      <SpanField
        label="Input"
        content={span.input}
        redacted={span.inputRedacted}
        fieldId={`input-${span.id}`}
      />
      <SpanField
        label="Output"
        content={span.output}
        redacted={span.outputRedacted}
        fieldId={`output-${span.id}`}
      />
    </div>
  );
}

function SpanField({
  label,
  content,
  redacted,
  fieldId,
}: {
  label: string;
  content: string;
  redacted: boolean;
  fieldId: string;
}) {
  return (
    <div className="mb-3 last:mb-0">
      <p className="mb-1 font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </p>
      {redacted ? (
        // Redaction-honest: show the marker, NEVER attempt to reveal.
        <div
          className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-muted-foreground"
          data-testid={`span-field-${fieldId}-redacted`}
        >
          <EyeOff className="h-3.5 w-3.5 shrink-0 text-warning" aria-hidden />
          <span>Redacted by trace-governance policy — timing and cost are still shown.</span>
        </div>
      ) : content ? (
        <pre
          className="max-h-48 overflow-auto rounded-md bg-surface-3 p-3"
          data-testid={`span-field-${fieldId}-content`}
        >
          {content}
        </pre>
      ) : (
        <p className="text-muted-foreground" data-testid={`span-field-${fieldId}-empty`}>
          —
        </p>
      )}
    </div>
  );
}

// ── TraceExplorer ─────────────────────────────────────────────────────────────

export interface TraceExplorerProps {
  /** DFS-ordered spans from GET /api/traces/{id}/detail (m16.2). */
  spans: SpanSummary[];
}

export function TraceExplorer({ spans }: TraceExplorerProps) {
  const [expandedIO, setExpandedIO] = React.useState<Set<string>>(new Set());

  const window = React.useMemo(() => traceWindow(spans), [spans]);

  function toggleIO(id: string) {
    setExpandedIO((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  if (spans.length === 0) {
    return (
      <div
        className="flex h-32 items-center justify-center rounded-lg border bg-card text-sm text-muted-foreground"
        data-testid="trace-explorer"
      >
        This trace produced no spans.
      </div>
    );
  }

  return (
    <div
      className="overflow-hidden rounded-lg border bg-card shadow-card"
      data-testid="trace-explorer"
      role="tree"
      aria-label="Trace spans"
    >
      {spans.map((span) => {
        // nestingDepth is the m16.2 additive field; fall back to 0 gracefully.
        const depth = span.nestingDepth ?? 0;
        const kind = kindOf(span, depth);
        const ioExpanded = expandedIO.has(span.id);
        const hasIO = !!(span.input || span.output || span.inputRedacted || span.outputRedacted);
        const showTokens = span.tokensIn > 0 || span.tokensOut > 0;

        // Waterfall geometry: [leftPct, widthPct] as % of the trace window.
        const leftPct = ((span.startMs - window.start) / window.totalMs) * 100;
        const widthPct = Math.max((span.durationMs / window.totalMs) * 100, 1.5);

        return (
          <React.Fragment key={span.id}>
            <div
              role="treeitem"
              data-testid={`span-row-${span.id}`}
              className="grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)_auto] items-center gap-2 border-b border-border/50 px-3 py-2 text-sm last:border-0 hover:bg-surface-2/50"
              aria-expanded={hasIO ? ioExpanded : undefined}
            >
              {/* ── Label column ─────────────────────────────────────────── */}
              <div
                className="flex min-w-0 items-center gap-1.5"
                style={{ paddingLeft: `${depth * 14}px` }}
              >
                {/* Disclosure chevron */}
                {hasIO ? (
                  <button
                    type="button"
                    onClick={() => toggleIO(span.id)}
                    data-testid={`span-io-toggle-${span.id}`}
                    className="shrink-0 text-muted-foreground hover:text-foreground"
                    aria-label={ioExpanded ? "Collapse I/O" : "Expand I/O"}
                  >
                    {ioExpanded ? (
                      <ChevronDown className="h-3.5 w-3.5" />
                    ) : (
                      <ChevronRight className="h-3.5 w-3.5" />
                    )}
                  </button>
                ) : (
                  // Placeholder to preserve alignment when no I/O
                  <span
                    className="w-3.5 shrink-0"
                    data-testid={`span-io-toggle-${span.id}`}
                  />
                )}

                {/* Error indicator */}
                {span.status === "error" && (
                  <span
                    className="h-2 w-2 shrink-0 rounded-full bg-destructive"
                    aria-label="error"
                  />
                )}

                {/* Redacted icon */}
                {(span.inputRedacted || span.outputRedacted) && (
                  <EyeOff
                    className="h-3.5 w-3.5 shrink-0 text-warning"
                    aria-label="redacted"
                  />
                )}

                {/* Kind badge */}
                <span
                  data-testid={`span-kind-${span.id}`}
                  className={`shrink-0 rounded px-1 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${KIND_BADGE_VARIANT[kind]}`}
                >
                  {kind}
                </span>

                <span className="truncate text-foreground">
                  {span.name || span.type || span.id}
                </span>
              </div>

              {/* ── Waterfall column ─────────────────────────────────────── */}
              <div className="relative h-4">
                <span
                  data-testid={`span-timing-bar-${span.id}`}
                  className={`absolute top-1/2 h-2.5 -translate-y-1/2 rounded-sm ${WATERFALL_COLOR[kind]} ${
                    span.inputRedacted || span.outputRedacted ? "opacity-40" : ""
                  }`}
                  style={{
                    left: `${leftPct}%`,
                    width: `${widthPct}%`,
                  }}
                />
                <span className="absolute right-1 top-1/2 -translate-y-1/2 text-[10px] tabular-nums text-muted-foreground">
                  {span.durationMs}ms
                </span>
              </div>

              {/* ── Tokens + cost column ─────────────────────────────────── */}
              <div className="flex shrink-0 flex-col items-end gap-0.5 text-[11px] tabular-nums text-muted-foreground">
                <span data-testid={`span-tokens-${span.id}`}>
                  {showTokens ? `${span.tokensIn + span.tokensOut} tok` : ""}
                </span>
                <span data-testid={`span-cost-${span.id}`}>
                  {span.costUSD > 0 ? fmtCost(span.costUSD) : ""}
                </span>
              </div>
            </div>

            {/* I/O expand panel — full-width row beneath the disclosure row */}
            <SpanIOExpanded span={span} expanded={ioExpanded} />
          </React.Fragment>
        );
      })}
    </div>
  );
}
