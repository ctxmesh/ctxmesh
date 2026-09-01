import * as React from "react";
import { ChevronDown, ChevronRight, EyeOff } from "lucide-react";

import { QuantityValue, QuietNote } from "@/components/kit";
import { Badge } from "@/components/ui/badge";
import type { SpanSummary } from "@/lib/api";
import { formatLatency } from "@/lib/format";
import { cn } from "@/lib/utils";

// TraceExplorer — the M16 native span-tree surface (m16.6), re-hued to the
// editorial colour doctrine in M151 (ADR 0128, spec §2.2 / §5.26).
//
// It receives DFS-ordered SpanSummary[] from GET /api/traces/{id}/detail (the
// m16.2 backend sends nestingDepth on each span so the UI never re-walks the
// tree) and renders, per span: a disclosure row indented by depth, an identity
// chip, a waterfall bar plotted against the trace window, the machine facts
// (duration / tokens / cost), and an I/O panel that is redaction-honest.
//
// ── SPAN KIND IS IDENTITY, NOT HEALTH (the M151 fix) ────────────────────────
// This component used to colour rows by KIND: RUN was `bg-primary`, GENERATION
// `bg-info`, TOOL `bg-success`. Three separate doctrine breaches in one map.
// Pine is the brand and interactivity and is NEVER a status (§2.1), so a RUN
// bar painted pine said "this is us, act here" about a fact. `--info` now
// carries the HOLD violet (§2.4), so every model call announced that a person
// must decide. And ok-green means "the system verified it and it is serving"
// (§2.2) — a claim about health, pinned to a word about kind. Meanwhile the
// rows that genuinely needed annotating — `guardrail: pii scan`, `awaiting
// approval: create_refund` — took `bg-muted` and disappeared, which is the
// exact inverse of §5.26's rule that governance renders in the same stream as
// the model and tool calls, VISIBLY annotated.
//
// So the map is now split in two, and that split is the whole design:
//
//   • KIND (RUN / GENERATION / TOOL / EVENT / SPAN) is IDENTITY. It renders in
//     ONE neutral register — the muted tag — and the WORD does the work. A
//     reader learns what a step is by reading it, not by decoding a hue.
//   • STATE is what actually happened, and it owns the semantic hues:
//       failed  → crit    the step will not proceed without a change (§2.2)
//       held    → hold    an approval/consent: a PERSON had to decide (§2.4)
//       guarded → warn    a bound was crossed: a guardrail fired, a retry ran,
//                         the backend flagged the step WARNING (§2.2)
//     A step with no state carries no hue at all, in either the tag column or
//     the waterfall — silence is the honest rendering of "nothing happened
//     here that a person needs to know about".
//
// ── GOVERNANCE IS LOUDER THAN THE ORDINARY, NOT QUIETER (§5.26) ─────────────
// A state-carrying row gets FOUR marks an ordinary row does not: a 2px left
// rule in its hue, a full-row hue-surface→transparent wash, a dot on the rail,
// and a second tag naming the state in words (`APPROVAL`, `GUARDRAIL`,
// `RETRY`, `FAILED`). Ordinary rows carry a transparent left rule of the same
// width so nothing shifts. The waterfall bar takes the hue too, which is what
// makes the governance steps findable in the shape of the trace and not only
// in its text.
//
// ── THE ROOT RUN BAR IS A RULER, NOT A MEASUREMENT ─────────────────────────
// The root span IS the trace window, so its bar is always 100% wide. Filling
// that full-width bar and then printing the duration INSIDE it produced the
// one genuinely unreadable element on the page: low-contrast meta text over a
// saturated fill. Two changes fix it for good — the duration moved OUT of the
// bar into its own mono tabular column (§4.8; it is a machine-owned value and
// belongs beside the other machine-owned values), and the root renders as a
// hollow TRACK (hairline + sunk fill) rather than a fill, because it measures
// the window every other bar is drawn against rather than a step of its own.
//
// ── FIXED TRAILING TRACKS SO THE BARS ARE COMPARABLE ───────────────────────
// Each row is its own grid. With `auto` trailing columns the waterfall column
// was a different width on every row — a generation carrying `41,220 tok`
// squeezed it, a tool with no facts let it grow — so two bars of equal length
// meant two different durations. The duration and facts tracks are therefore
// FIXED rems: the waterfall column is then identical in every row, which is
// the only condition under which a waterfall means anything.
//
// data-testid contract (non-negotiable — the black-box suites key on these):
//   trace-explorer            — root container
//   span-row-{id}             — one disclosure row
//   span-kind-{id}            — the identity chip
//   span-state-{id}           — the state chip (present ONLY when there is one)
//   span-timing-bar-{id}      — the waterfall bar element
//   span-duration-{id}        — the duration cell
//   span-io-toggle-{id}       — the I/O expand button
//   span-tokens-{id}          — the tokens cell (empty when the span has none)
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

/**
 * What happened at this step — the only thing on the row allowed to carry a
 * hue. Three kinds, mapped 1:1 onto §2.2's meanings; the `label` is the word
 * the tag shows, so the hue is never the only carrier of the message.
 */
type SpanStateKind = "failed" | "held" | "guarded";

interface SpanState {
  kind: SpanStateKind;
  /** ≤ 16 chars — the §4.5 tag budget. */
  label: string;
}

/** "A person had to decide": approval, consent, a human gate (§2.4). */
const HELD_RE =
  /(approval|approve[ds]?|consent|awaiting|requires[_ ]?action|\bheld\b|\bhitl\b|\bhuman\b)/i;
/** "A bound was crossed": a guardrail / moderation / redaction step fired. */
const GUARDRAIL_RE = /(guardrail|guard[_ ]rail|moderation|redact|\bpii\b|\bdlp\b)/i;
/** "Degraded but it recovered": a retry, a backoff. */
const RETRY_RE = /(\bretry\b|\bretried\b|\bretries\b|backoff|back[_ ]off)/i;

/**
 * The state of one span, worst-first. `status`/`level` are the backend's own
 * verdict and always win; the governance categories are then read from the
 * span NAME, because that is where a trace backend records what a step was —
 * there is no structured governance field on SpanSummary to read instead, and
 * inventing one would be a claim the data does not support (§7.1).
 */
function spanState(span: SpanSummary): SpanState | null {
  if (span.status === "error" || span.level === "ERROR") {
    return { kind: "failed", label: "Failed" };
  }
  const name = span.name ?? "";
  if (HELD_RE.test(name)) return { kind: "held", label: "Approval" };
  if (GUARDRAIL_RE.test(name)) return { kind: "guarded", label: "Guardrail" };
  if (RETRY_RE.test(name)) return { kind: "guarded", label: "Retry" };
  // The backend flagged the step but did not name a governance step in it.
  // "Degraded" is the honest word for that, and warn is its hue.
  if (span.level === "WARNING") return { kind: "guarded", label: "Degraded" };
  return null;
}

/** The §5.26 governance treatment: left rule + row wash, one per hue. */
const STATE_ROW: Record<SpanStateKind, string> = {
  failed: "border-l-destructive bg-gradient-to-r from-destructive-surface to-transparent",
  held: "border-l-hold bg-gradient-to-r from-hold-surface to-transparent",
  guarded: "border-l-warning bg-gradient-to-r from-warning-surface to-transparent",
};

const STATE_DOT: Record<SpanStateKind, string> = {
  failed: "bg-destructive",
  held: "bg-hold",
  guarded: "bg-warning",
};

const STATE_TAG: Record<SpanStateKind, "crit" | "hold" | "warn"> = {
  failed: "crit",
  held: "hold",
  guarded: "warn",
};

const STATE_BAR: Record<SpanStateKind, string> = {
  failed: "bg-destructive",
  held: "bg-hold",
  guarded: "bg-warning",
};

/**
 * A step that carries no state. Ink, not hue — `faint` is the tertiary ink that
 * must still be READ, which is exactly what a duration bar is.
 */
const PLAIN_BAR = "bg-faint";

/**
 * The root RUN bar. It is the trace window itself (always 100% wide), so it is
 * drawn as a hollow track: it is the ruler the other bars are measured against,
 * not a measurement.
 */
const ROOT_TRACK = "border border-border-strong bg-surface-2";

/**
 * The §4.4 trailing budget. Fixed rems (never `auto`) so the waterfall track is
 * the same width in every row — see the header note.
 */
const ROW_GRID =
  "grid-cols-[minmax(0,2.2fr)_minmax(0,1.5fr)_4.5rem_5.5rem]";

function fmtCost(usd: number): string {
  return usd < 0.001 ? `$${usd.toFixed(5)}` : `$${usd.toFixed(3)}`;
}

/**
 * A duration is a machine-owned value (§4.8). A real 0ms — an instantaneous
 * event — is a MEASUREMENT and renders as `0ms`; `formatLatency` returns the
 * unknown dash there, and zero and unknown must never share a glyph (§7.1).
 */
function fmtDuration(ms: number): string {
  return ms === 0 ? "0ms" : formatLatency(ms);
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
    <div className="border-b border-border-soft bg-surface-2 px-4 py-3 text-xs">
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
      <p className="mb-1 font-mono text-2xs uppercase tracking-wide text-faint">
        {label}
      </p>
      {redacted ? (
        // Redaction-honest: show the marker, NEVER attempt to reveal. Warn, not
        // crit — a bound was enforced and the run is fine (§2.2).
        <div
          className="flex items-center gap-2 rounded-sm border border-warning/40 bg-warning-surface px-3 py-2 text-secondary-foreground"
          data-testid={`span-field-${fieldId}-redacted`}
        >
          <EyeOff className="h-3.5 w-3.5 shrink-0 text-warning" aria-hidden />
          <span>Redacted by trace-governance policy — timing and cost are still shown.</span>
        </div>
      ) : content ? (
        <pre
          className="max-h-48 overflow-auto rounded-sm bg-surface-3 p-3 font-mono"
          data-testid={`span-field-${fieldId}-content`}
        >
          {content}
        </pre>
      ) : (
        // Nothing was recorded here — the honest absence, never a blank cell.
        <p
          className="font-mono text-faint"
          title="Nothing was recorded for this field."
          data-testid={`span-field-${fieldId}-empty`}
        >
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
    // Calm, never an error and never a zero (§5.27): the trace exists, the
    // backend simply wrote nothing under it.
    return (
      <div data-testid="trace-explorer">
        <QuietNote title="This trace produced no spans.">
          The backend recorded no steps against it, so there is nothing to draw.
          Nothing here is estimated — the record is simply empty.
        </QuietNote>
      </div>
    );
  }

  return (
    <div
      className="overflow-hidden rounded-lg border border-border bg-card"
      data-testid="trace-explorer"
      role="tree"
      aria-label="Trace spans"
    >
      {/* Column heads in the §4.8 mono eyebrow register. Hidden from assistive
          tech: each row already names its own values, so announcing four heads
          per tree would be noise, not orientation. */}
      <div
        aria-hidden="true"
        className={cn(
          "grid items-center gap-3 border-b border-border bg-card px-3 py-2",
          "border-l-2 border-l-transparent font-mono text-2xs uppercase tracking-wide text-faint",
          ROW_GRID,
        )}
      >
        <span>Step</span>
        <span>When it ran</span>
        <span className="text-right">Took</span>
        <span className="text-right">Tokens · cost</span>
      </div>

      {spans.map((span) => {
        // nestingDepth is the m16.2 additive field; fall back to 0 gracefully.
        const depth = span.nestingDepth ?? 0;
        const kind = kindOf(span, depth);
        const state = spanState(span);
        const ioExpanded = expandedIO.has(span.id);
        const hasIO = !!(span.input || span.output || span.inputRedacted || span.outputRedacted);
        const showTokens = span.tokensIn > 0 || span.tokensOut > 0;
        const redacted = span.inputRedacted || span.outputRedacted;
        const label = span.name || span.type || span.id;

        // Waterfall geometry: [leftPct, widthPct] as % of the trace window.
        const leftPct = ((span.startMs - window.start) / window.totalMs) * 100;
        const widthPct = Math.max((span.durationMs / window.totalMs) * 100, 1.5);

        return (
          <React.Fragment key={span.id}>
            <div
              role="treeitem"
              data-testid={`span-row-${span.id}`}
              className={cn(
                "grid items-center gap-3 border-b border-l-2 border-border-soft px-3 py-2 text-sm last:border-b-0",
                ROW_GRID,
                state
                  ? STATE_ROW[state.kind]
                  : "border-l-transparent hover:bg-surface-2/50",
              )}
              aria-expanded={hasIO ? ioExpanded : undefined}
            >
              {/* ── Label column ─────────────────────────────────────────── */}
              {/* `flex-wrap` is load-bearing, not cosmetic: the tags are
                  `whitespace-nowrap` by contract (§5.1), so on a narrow trace
                  column an annotated row — chevron + dot + kind + state + name
                  — cannot shrink and would push its own cell wide. Wrapping
                  lets it drop to a second line instead, which costs a row of
                  height only where the room actually ran out.
                  The state tag and the name wrap as ONE unit (the inner flex
                  below) so the result is deterministically at most two lines:
                  free-wrapping every child let the two tags land on separate
                  lines at 1280 and produced a three-line row. */}
              <div
                className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1"
                style={{ paddingLeft: `${depth * 14}px` }}
              >
                {/* Disclosure chevron */}
                {hasIO ? (
                  <button
                    type="button"
                    onClick={() => toggleIO(span.id)}
                    data-testid={`span-io-toggle-${span.id}`}
                    className="shrink-0 text-faint hover:text-foreground"
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

                {/* The §5.26 dot on the rail. Decoration only — the state tag
                    beside it carries the same message in words. */}
                {state && (
                  <span
                    aria-hidden="true"
                    className={cn(
                      "h-2 w-2 shrink-0 rounded-full",
                      STATE_DOT[state.kind],
                    )}
                  />
                )}

                {/* Redacted icon — warn: a bound was enforced, not a failure. */}
                {redacted && (
                  <EyeOff
                    className="h-3.5 w-3.5 shrink-0 text-warning"
                    aria-label="redacted"
                  />
                )}

                {/* Identity: one neutral register for every kind. */}
                <Badge
                  variant="muted"
                  data-testid={`span-kind-${span.id}`}
                  className="shrink-0"
                >
                  {kind}
                </Badge>

                {/* The state tag and the name travel together — see the wrap
                    note above. */}
                <span className="flex min-w-0 items-center gap-1.5">
                  {/* State: the ONLY hue on the row, and only when there is one. */}
                  {state && (
                    <Badge
                      variant={STATE_TAG[state.kind]}
                      data-testid={`span-state-${span.id}`}
                      className="shrink-0"
                    >
                      {state.label}
                    </Badge>
                  )}

                  {/* A span name is a machine string, so it takes the mono face
                      and end-truncates with the full value in `title` (§4.5). */}
                  <span className="truncate font-mono text-xs" title={label}>
                    {label}
                  </span>
                </span>
              </div>

              {/* ── Waterfall column ─────────────────────────────────────── */}
              {/* A picture of the duration the next column states in words. */}
              <div className="relative h-4" aria-hidden="true">
                <span
                  data-testid={`span-timing-bar-${span.id}`}
                  className={cn(
                    "absolute top-1/2 h-2.5 -translate-y-1/2 rounded-sm",
                    depth === 0
                      ? ROOT_TRACK
                      : state
                        ? STATE_BAR[state.kind]
                        : PLAIN_BAR,
                    redacted && "opacity-40",
                  )}
                  style={{
                    left: `${leftPct}%`,
                    width: `${widthPct}%`,
                  }}
                />
              </div>

              {/* ── Duration ─────────────────────────────────────────────── */}
              {/* Out of the bar and into its own mono tabular track (§4.8) —
                  the fix for the unreadable label over the filled root bar. */}
              <span
                data-testid={`span-duration-${span.id}`}
                className="justify-self-end whitespace-nowrap text-right"
              >
                <QuantityValue
                  value={span.durationMs}
                  format={fmtDuration}
                  className="text-xs"
                />
              </span>

              {/* ── Tokens + cost column ─────────────────────────────────── */}
              {/* Blank, not `0`: a tool call does not have zero tokens, it has
                  no token accounting at all. Inapplicable is a third thing,
                  and it must not be dressed as a measured nought (§7.1). */}
              <div className="flex min-w-0 flex-col items-end gap-0.5 font-mono text-2xs tabular-nums text-faint">
                <span data-testid={`span-tokens-${span.id}`}>
                  {showTokens
                    ? `${(span.tokensIn + span.tokensOut).toLocaleString()} tok`
                    : ""}
                </span>
                <span
                  data-testid={`span-cost-${span.id}`}
                  className="whitespace-nowrap"
                >
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
