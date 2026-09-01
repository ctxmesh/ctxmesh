import { describe, expect, it } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { TraceExplorer } from "@/components/dashboard/trace-explorer";
import type { SpanSummary } from "@/lib/api";

// TraceExplorer — m16.6 tests.
//
// Coverage:
//   • span rows render, indented by nestingDepth.
//   • a depth>0 span is visibly more indented than a depth-0 span.
//   • the waterfall bar position reflects startMs/durationMs (left% + width%).
//   • the I/O expand toggle shows/hides the field content.
//   • a redacted span shows the redacted marker, NEVER the content.
//   • tokens/cost render for spans that carry them; hidden when zero.
//   • kind badges are correct (GENERATION, TOOL, SPAN, RUN).
//   • empty span list → "no spans" state.

function span(over: Partial<SpanSummary>): SpanSummary {
  return {
    id: "s",
    parentId: "",
    type: "SPAN",
    name: "span",
    startMs: 0,
    durationMs: 100,
    model: "",
    tokensIn: 0,
    tokensOut: 0,
    costUSD: 0,
    level: "",
    status: "ok",
    input: "",
    output: "",
    inputRedacted: false,
    outputRedacted: false,
    nestingDepth: 0,
    ...over,
  };
}

// A trace with: root (depth 0), generation child (depth 1), tool span (depth 2),
// a redacted tool span (depth 1), and an event (depth 1).
const SPANS: SpanSummary[] = [
  span({ id: "root",     parentId: "",      type: "SPAN",       name: "run: billing-agent",  startMs: 0,   durationMs: 1840, nestingDepth: 0 }),
  span({ id: "gen",      parentId: "root",  type: "GENERATION", name: "generate response",    startMs: 20,  durationMs: 520,  nestingDepth: 1, model: "claude-sonnet-4", tokensIn: 620, tokensOut: 240, costUSD: 0.003, input: '{"q":1}', output: '{"a":2}' }),
  span({ id: "tool",     parentId: "gen",   type: "SPAN",       name: "tool: get_invoice",   startMs: 560, durationMs: 180,  nestingDepth: 2, tokensIn: 12, tokensOut: 4, costUSD: 0.0001, input: '{"id":4021}', output: '{"status":"shipped"}' }),
  span({ id: "redacted", parentId: "root",  type: "SPAN",       name: "tool: search_docs",   startMs: 760, durationMs: 320,  nestingDepth: 1, inputRedacted: true, outputRedacted: true }),
  span({ id: "event",    parentId: "root",  type: "EVENT",      name: "trace-metadata",      startMs: 0,   durationMs: 0,    nestingDepth: 1 }),
];

describe("TraceExplorer (m16.6)", () => {
  it("renders a row for every span", () => {
    render(<TraceExplorer spans={SPANS} />);
    for (const s of SPANS) {
      expect(screen.getByTestId(`span-row-${s.id}`)).toBeInTheDocument();
    }
  });

  it("indents deeper spans — depth-2 tool span has more padding than depth-1 gen", () => {
    render(<TraceExplorer spans={SPANS} />);
    const rowRoot = screen.getByTestId("span-row-root");
    const rowGen  = screen.getByTestId("span-row-gen");
    const rowTool = screen.getByTestId("span-row-tool");

    const indent = (row: HTMLElement) => {
      const el = row.querySelector<HTMLElement>("[style]");
      return parseInt((el?.style.paddingLeft ?? "0").replace("px", ""), 10);
    };

    expect(indent(rowGen)).toBeGreaterThan(indent(rowRoot));
    expect(indent(rowTool)).toBeGreaterThan(indent(rowGen));
  });

  it("waterfall bar left/width reflect startMs/durationMs relative to the trace window", () => {
    render(<TraceExplorer spans={SPANS} />);
    // Trace window: min(startMs)=0, max(startMs+durationMs)=1840 → totalMs=1840.
    // gen: startMs=20, durationMs=520 → leftPct=20/1840*100≈1.09%, widthPct=520/1840*100≈28.26%.
    const genBar = screen.getByTestId("span-timing-bar-gen");
    const style = genBar.getAttribute("style") ?? "";
    // Extract left and width percentages.
    const leftMatch = style.match(/left:\s*([\d.]+)%/);
    const widthMatch = style.match(/width:\s*([\d.]+)%/);
    const left = parseFloat(leftMatch?.[1] ?? "0");
    const width = parseFloat(widthMatch?.[1] ?? "0");
    // The root starts at left=0%.
    const rootBar = screen.getByTestId("span-timing-bar-root");
    const rootStyle = rootBar.getAttribute("style") ?? "";
    const rootLeft = parseFloat((rootStyle.match(/left:\s*([\d.]+)%/) ?? ["", "0"])[1]);

    expect(left).toBeGreaterThan(rootLeft); // gen starts after root (startMs=20 > 0).
    expect(width).toBeGreaterThan(0);
    expect(width).toBeLessThan(100);
  });

  it("I/O toggle expands and collapses the content panel", () => {
    render(<TraceExplorer spans={SPANS} />);
    // "gen" span has non-empty input/output — it gets a real toggle button.
    const toggle = screen.getByTestId("span-io-toggle-gen");
    // Initially collapsed — content not in DOM.
    expect(screen.queryByTestId("span-field-input-gen-content")).toBeNull();

    fireEvent.click(toggle);
    // Now expanded — content visible.
    expect(screen.getByTestId("span-field-input-gen-content")).toBeInTheDocument();
    expect(screen.getByTestId("span-field-input-gen-content")).toHaveTextContent('{"q":1}');

    // Click again to collapse.
    fireEvent.click(toggle);
    expect(screen.queryByTestId("span-field-input-gen-content")).toBeNull();
  });

  it("shows a redacted marker for a redacted span — NEVER the content", () => {
    render(<TraceExplorer spans={SPANS} />);
    const toggle = screen.getByTestId("span-io-toggle-redacted");
    fireEvent.click(toggle);

    // Redacted markers are present.
    expect(screen.getByTestId("span-field-input-redacted-redacted")).toBeInTheDocument();
    expect(screen.getByTestId("span-field-output-redacted-redacted")).toBeInTheDocument();
    // Raw content elements are absent.
    expect(screen.queryByTestId("span-field-input-redacted-content")).toBeNull();
    expect(screen.queryByTestId("span-field-output-redacted-content")).toBeNull();
  });

  it("renders tokens and cost for spans that carry them", () => {
    render(<TraceExplorer spans={SPANS} />);
    // gen has tokensIn=620, tokensOut=240, costUSD=0.003.
    expect(screen.getByTestId("span-tokens-gen")).toHaveTextContent("860 tok");
    expect(screen.getByTestId("span-cost-gen")).toHaveTextContent("$0.003");
  });

  it("hides tokens/cost for spans with zero values", () => {
    render(<TraceExplorer spans={SPANS} />);
    // root has tokensIn=0, tokensOut=0, costUSD=0.
    expect(screen.getByTestId("span-tokens-root")).toHaveTextContent("");
    expect(screen.getByTestId("span-cost-root")).toHaveTextContent("");
  });

  it("shows the correct kind badge for each span type", () => {
    render(<TraceExplorer spans={SPANS} />);
    expect(screen.getByTestId("span-kind-root")).toHaveTextContent("RUN");
    expect(screen.getByTestId("span-kind-gen")).toHaveTextContent("GENERATION");
    expect(screen.getByTestId("span-kind-tool")).toHaveTextContent("TOOL");
    expect(screen.getByTestId("span-kind-event")).toHaveTextContent("EVENT");
  });

  it("renders the empty state when spans is an empty array", () => {
    render(<TraceExplorer spans={[]} />);
    expect(screen.getByTestId("trace-explorer")).toHaveTextContent("no spans");
  });
});

// ── M151: kind is identity, state is hue (ADR 0128 §2.1/§2.2, spec §5.26) ────
//
// The regression these lock down is the one this component shipped with: span
// KIND was the taxonomy that carried colour (RUN → bg-primary, GENERATION →
// bg-info, TOOL → bg-success) while the rows that actually needed annotating —
// a guardrail, a held approval, a failure — took bg-muted and disappeared.

/** Every hue the doctrine defines. None of them may land on an identity chip. */
const SEMANTIC = [
  "bg-primary",
  "bg-info",
  "bg-hold",
  "bg-success",
  "bg-warning",
  "bg-destructive",
  "text-primary",
  "text-info",
  "text-hold",
  "text-success",
  "text-warning",
  "text-destructive",
];

const GOVERNED: SpanSummary[] = [
  span({ id: "root",      type: "SPAN",       name: "run: billing-agent",             startMs: 0,    durationMs: 4000, nestingDepth: 0 }),
  span({ id: "gen",       type: "GENERATION", name: "generate response",              startMs: 10,   durationMs: 900,  nestingDepth: 1, tokensIn: 10, tokensOut: 5 }),
  span({ id: "guardrail", type: "EVENT",      name: "guardrail: pii scan",            startMs: 950,  durationMs: 40,   nestingDepth: 2, level: "WARNING" }),
  span({ id: "approval",  type: "EVENT",      name: "awaiting approval: create_refund", startMs: 1000, durationMs: 1800, nestingDepth: 1 }),
  span({ id: "failtool",  type: "SPAN",       name: "tool: post_message",             startMs: 2900, durationMs: 120,  nestingDepth: 1, status: "error", level: "ERROR" }),
  span({ id: "retry",     type: "EVENT",      name: "retry 1/1",                      startMs: 3030, durationMs: 90,   nestingDepth: 2, level: "WARNING" }),
  span({ id: "plain",     type: "SPAN",       name: "tool: get_invoice",              startMs: 3200, durationMs: 300,  nestingDepth: 1 }),
];

describe("TraceExplorer — kind is identity, state is hue (M151)", () => {
  it("draws every kind chip in ONE neutral register — no semantic hue on identity", () => {
    render(<TraceExplorer spans={SPANS} />);
    const registers = new Set<string>();
    for (const s of SPANS) {
      const chip = screen.getByTestId(`span-kind-${s.id}`);
      registers.add(chip.className);
      for (const hue of SEMANTIC) {
        expect(chip.className.split(/\s+/)).not.toContain(hue);
      }
    }
    // One register, literally: RUN, GENERATION, TOOL, EVENT and SPAN are told
    // apart by the WORD, never by the colour.
    expect(registers.size).toBe(1);
  });

  it("annotates governance and failure with the state chip its hue means", () => {
    render(<TraceExplorer spans={GOVERNED} />);

    // A guardrail crossed a bound → warn.
    expect(screen.getByTestId("span-state-guardrail")).toHaveTextContent("Guardrail");
    expect(screen.getByTestId("span-state-guardrail").className).toContain("text-warning");
    // A retry: degraded, but it recovered → warn.
    expect(screen.getByTestId("span-state-retry")).toHaveTextContent("Retry");
    expect(screen.getByTestId("span-state-retry").className).toContain("text-warning");
    // A person had to decide → hold, and ONLY here.
    expect(screen.getByTestId("span-state-approval")).toHaveTextContent("Approval");
    expect(screen.getByTestId("span-state-approval").className).toContain("text-hold");
    // It will not proceed without a change → crit.
    expect(screen.getByTestId("span-state-failtool")).toHaveTextContent("Failed");
    expect(screen.getByTestId("span-state-failtool").className).toContain("text-destructive");
  });

  it("gives ordinary steps no state chip and no hue at all", () => {
    render(<TraceExplorer spans={GOVERNED} />);
    for (const id of ["root", "gen", "plain"]) {
      expect(screen.queryByTestId(`span-state-${id}`)).toBeNull();
      const bar = screen.getByTestId(`span-timing-bar-${id}`);
      for (const hue of SEMANTIC) {
        expect(bar.className.split(/\s+/)).not.toContain(hue);
      }
    }
  });

  it("makes a governance row MORE visible than an ordinary one, not less", () => {
    render(<TraceExplorer spans={GOVERNED} />);
    // The §5.26 treatment: a hue left rule + a row wash on the governance row…
    const held = screen.getByTestId("span-row-approval");
    expect(held.className).toContain("border-l-hold");
    expect(held.className).toContain("from-hold-surface");
    // …and its waterfall bar carries the hue too, so it is findable in the
    // SHAPE of the trace and not only in its text.
    expect(screen.getByTestId("span-timing-bar-approval").className).toContain("bg-hold");
    // An ordinary row keeps the same rule WIDTH (nothing shifts) with no hue.
    expect(screen.getByTestId("span-row-plain").className).toContain("border-l-transparent");
  });

  it("prints the duration beside the bar, never inside it", () => {
    render(<TraceExplorer spans={GOVERNED} />);
    // The root bar spans the whole window; its label used to be painted over
    // that fill in low-contrast meta text. The bar now carries no text at all…
    const rootBar = screen.getByTestId("span-timing-bar-root");
    expect(rootBar).toHaveTextContent("");
    // …and the duration is a mono cell of its own.
    expect(screen.getByTestId("span-duration-root")).toHaveTextContent("4.00s");
    expect(screen.getByTestId("span-duration-guardrail")).toHaveTextContent("40ms");
  });

  it("draws the root RUN bar as a hollow track — it is the ruler, not a step", () => {
    render(<TraceExplorer spans={GOVERNED} />);
    const rootBar = screen.getByTestId("span-timing-bar-root");
    expect(rootBar.className).toContain("border-border-strong");
    // A child step IS a measurement, so it is a fill.
    expect(screen.getByTestId("span-timing-bar-plain").className).toContain("bg-faint");
  });

  it("renders a measured 0ms as `0ms`, never as the unknown dash", () => {
    render(
      <TraceExplorer
        spans={[span({ id: "instant", name: "trace-metadata", type: "EVENT", durationMs: 0 })]}
      />,
    );
    expect(screen.getByTestId("span-duration-instant")).toHaveTextContent("0ms");
    expect(screen.getByTestId("span-duration-instant")).not.toHaveTextContent("—");
  });
});
