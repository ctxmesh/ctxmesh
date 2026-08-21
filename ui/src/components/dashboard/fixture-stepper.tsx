import * as React from "react";
import { ChevronDown, ChevronRight, FileWarning } from "lucide-react";

import { ApiError, getRunFixture, type RunFixture, type RunFixtureStep } from "@/lib/api";

// FixtureStepper — the O10a console fixture stepper (ADR 0071 §5). For a RECORDED run it shows the
// step timeline joined to its WIRE-EXACT recorded I/O — the SAME bytes `dev --replay` re-serves in CI
// ("what you see is what CI replays"). Read-only; it NEVER re-executes (decoupled from replay), and a
// step the join could not confidently back is shown as a "recording gap", never fabricated I/O.
//
// It is quiet by design: a run with no fixture (not recorded) or one the caller cannot read renders
// nothing (no oracle) — the stepper appears only when there is a real recorded fixture to show, or an
// honest "not configured" note.
//
// data-testid contract:
//   fixture-stepper            — root container (present only when there is a recorded fixture)
//   fixture-stepper-note       — an honest note (not configured / not recorded)
//   fixture-step-row-{i}       — one step disclosure row
//   fixture-step-gap-{i}       — a step's recording-gap marker
//   fixture-step-io-{i}        — a step's expanded I/O panel

type State =
  | { status: "loading" }
  | { status: "ok"; data: RunFixture }
  | { status: "note"; message: string }
  | { status: "hidden" };

export function FixtureStepper({ runId }: { runId: string }) {
  const [state, setState] = React.useState<State>({ status: "loading" });
  const [expanded, setExpanded] = React.useState<Set<number>>(new Set());

  React.useEffect(() => {
    const ctrl = new AbortController();
    setState({ status: "loading" });
    setExpanded(new Set());
    getRunFixture(runId, ctrl.signal)
      .then((data) => setState({ status: "ok", data }))
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return;
        if (err instanceof ApiError && err.status === 501) {
          setState({ status: "note", message: "Fixture viewing is not configured for this install (no object store)." });
        } else if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          setState({ status: "hidden" }); // no access / not found — say nothing (no read oracle)
        } else {
          setState({ status: "note", message: "Could not load the recorded fixture for this run." });
        }
      });
    return () => ctrl.abort();
  }, [runId]);

  if (state.status === "loading" || state.status === "hidden") return null;

  if (state.status === "note") {
    return (
      <p className="text-muted-foreground" data-testid="fixture-stepper-note">
        {state.message}
      </p>
    );
  }

  const { data } = state;
  if (!data.recorded) {
    // A real answer, but this run was not recorded — a discoverability nudge, not clutter.
    return (
      <p className="text-muted-foreground" data-testid="fixture-stepper-note">
        This run was not recorded. Run a record-capable agent in record mode to capture a replayable
        fixture (see <code>dev --replay</code>).
      </p>
    );
  }

  const toggle = (i: number) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(i)) next.delete(i);
      else next.add(i);
      return next;
    });

  return (
    <div className="space-y-2" data-testid="fixture-stepper">
      <div>
        <h3 className="font-semibold">Recorded fixture</h3>
        <p className="text-muted-foreground">
          The wire-exact I/O this run recorded — the SAME bytes <code>dev --replay</code> re-serves in
          CI. What you see here is what a replay reproduces.
        </p>
      </div>
      <ul className="space-y-1">
        {data.steps.map((step, i) => (
          <FixtureStepRow
            key={i}
            index={i}
            step={step}
            open={expanded.has(i)}
            onToggle={() => toggle(i)}
          />
        ))}
      </ul>
    </div>
  );
}

function FixtureStepRow({
  index,
  step,
  open,
  onToggle,
}: {
  index: number;
  step: RunFixtureStep;
  open: boolean;
  onToggle: () => void;
}) {
  const label = `Step ${index + 1} · ${step.kind}${step.toolName ? ` · ${step.toolName}` : ""}`;
  return (
    <li className="rounded-md border">
      <button
        type="button"
        onClick={onToggle}
        data-testid={`fixture-step-row-${index}`}
        className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-surface-2"
      >
        {open ? <ChevronDown className="h-4 w-4 shrink-0" /> : <ChevronRight className="h-4 w-4 shrink-0" />}
        <span className="font-medium">{label}</span>
        {!step.recorded && (
          <span className="ml-auto inline-flex items-center gap-1 text-warning" data-testid={`fixture-step-gap-${index}`}>
            <FileWarning className="h-3.5 w-3.5" aria-hidden />
            recording gap
          </span>
        )}
      </button>
      {open && (
        <div className="border-t px-3 py-2" data-testid={`fixture-step-io-${index}`}>
          {step.recorded ? (
            <>
              <FixtureField label="request" content={formatJSON(step.request)} />
              <FixtureField label="response" content={step.response ?? ""} />
              <p className="mt-1 text-muted-foreground">
                {step.contentType || "—"}
                {step.statusCode ? ` · ${step.statusCode}` : ""}
                {step.callId ? ` · ${step.callId}` : ""}
              </p>
            </>
          ) : (
            <p className="text-muted-foreground">
              Recording gap — no recorded I/O for this step ({step.gapReason || "unknown"}). The
              stepper never shows another step's bytes in its place.
            </p>
          )}
        </div>
      )}
    </li>
  );
}

// FixtureField mirrors the trace-explorer SpanField I/O pane: a label + a verbatim <pre> (or an em
// dash when empty). Fixtures are already credential-free (AssertNoCredentials gates every read).
function FixtureField({ label, content }: { label: string; content: string }) {
  return (
    <div className="mb-3 last:mb-0">
      <p className="mb-1 font-medium uppercase tracking-wide text-muted-foreground">{label}</p>
      {content ? (
        <pre className="max-h-48 overflow-auto rounded-md bg-surface-3 p-3">{content}</pre>
      ) : (
        <p className="text-muted-foreground">—</p>
      )}
    </div>
  );
}

function formatJSON(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}
