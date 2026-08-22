import * as React from "react";

import {
  api,
  ApiError,
  formatRunStep,
  openRunStream,
  type CreateRunRequest,
  type RunAction,
  type RunDetail,
} from "@/lib/api";

// useDurableRun — the shared durable-run engine (ADR 0093). It encapsulates the
// create-or-resume → stream → finalize lifecycle both the Playground (single-shot)
// and the console ChatPanel (conversation-threaded) drive:
//
//   start(args)  → POST /api/runs (createRun)   → openRunStream → finalize via getRun
//   resume(dec)  → POST /api/runs/{id}/resume   → openRunStream → finalize via getRun
//
// The two surfaces used to DUPLICATE this (the Playground had it twice, in onRun and
// onApprove; ChatPanel had a separate synchronous /invoke path). This hook is the single
// engine — every run becomes a first-class, observable durable run.
//
// The hook owns the transient run STATE (status/streamed text/step/id) and the SSE stream
// lifecycle (one cancel per run, torn down on New-run / cancel / unmount — no leak). It does
// NOT own the surface's presentation: the consumer projects the finalized RunDetail (traceId,
// requiresAction, workflow nodes, messages) via the onFinalized callback and reads the live
// stream state for its own rendering. So the Playground keeps its define/run/export chrome and
// ChatPanel keeps its turn list — only the run engine is shared.

// DurableRunStatus is the lifecycle phase of the engine's CURRENT run.
//   idle           — no run in flight (initial / after a reset).
//   creating       — createRun/resumeRun POST is in flight (pre-stream).
//   streaming      — the SSE event stream is open; tokens are arriving live.
//   finalizing     — the stream ended (or paused at requires_action); getRun is in flight.
//   done           — the run reached a terminal/paused state; the finalized detail is available.
//   error          — a non-403 create/stream/finalize failure.
//   forbidden      — a pre-stream 403 (the caller can't run/read this run).
export type DurableRunStatus =
  | "idle"
  | "creating"
  | "streaming"
  | "finalizing"
  | "done"
  | "error"
  | "forbidden";

// DurableRunState is the observable state the consumer renders from.
export interface DurableRunState {
  status: DurableRunStatus;
  // runId is the durable run's id — set once createRun/resumeRun returns, kept across the
  // stream + finalize so cancel/resume target the same run (the "turn stays ONE run" invariant).
  runId?: string;
  // responseText is the live-accumulated stream text (token deltas / the last `message`),
  // replaced by the finalized message content on finalize. The consumer decides how to render it
  // (ChatPanel unwraps the /invoke envelope; the Playground shows it verbatim).
  responseText: string;
  // step is the latest live step-visibility label (M78) — empty until the first `step` event.
  step: string;
  // detail is the finalized RunDetail (traceId, requiresAction, workflow nodes, run status) —
  // present once status is "done". The consumer projects it into its own view.
  detail?: RunDetail;
  // traceId / requiresAction are lifted from the finalized detail for convenience.
  traceId?: string;
  requiresAction?: RunAction;
  // error / forbidden mirror the failure state (status "error" / "forbidden").
  error?: string;
  forbidden: boolean;
}

export interface UseDurableRunOptions {
  // onForbidden fires on a pre-stream 403 (create/resume/stream). The surface uses it to
  // reprobe() its RBAC-aware chrome (a cached "yes" was stale, ADR 0011).
  onForbidden?: (message: string) => void;
  // onFinalized fires exactly once per run when the finalized detail is read (stream close or a
  // requires_action pause). The surface projects the detail into its own view here.
  onFinalized?: (detail: RunDetail) => void;
  // onFinalizeError fires when getRun itself fails (a rare finalize-time error). The surface
  // renders it; forbidden finalize errors also trigger onForbidden.
  onFinalizeError?: (err: unknown) => void;
}

const IDLE: DurableRunState = { status: "idle", responseText: "", step: "", forbidden: false };

// StartArgs mirrors CreateRunRequest — the create-or-resume input. conversationId is optional
// (single-shot Playground omits it; ChatPanel threads it), so ONE hook serves both.
export type StartArgs = CreateRunRequest;

export interface UseDurableRunResult extends DurableRunState {
  // start creates a fresh durable run and streams it to a terminal/paused state.
  start: (args: StartArgs) => Promise<void>;
  // resume re-enters the current run paused in requires_action (POST .../resume) and re-streams
  // it. No decision → a consent connect-and-continue; "approve" | "deny" → a HITL gate.
  resume: (decision?: "approve" | "deny", reason?: string) => Promise<void>;
  // cancel stops the live stream and cancels the run server-side (best-effort), then resets.
  cancel: () => Promise<void>;
  // reset returns the engine to idle (drops the stream) without a server cancel.
  reset: () => void;
  // running is a convenience: a run is mid-flight (creating/streaming/finalizing).
  running: boolean;
}

export function useDurableRun(options: UseDurableRunOptions = {}): UseDurableRunResult {
  const [state, setState] = React.useState<DurableRunState>(IDLE);
  // The active run's SSE stream canceller — aborts on New-run / cancel / unmount (no leak).
  const streamCancelRef = React.useRef<(() => void) | null>(null);
  // Options can change per render (inline closures); keep the latest in a ref so the long-lived
  // stream callbacks always call the current handlers without re-creating the stream.
  const optsRef = React.useRef(options);
  optsRef.current = options;

  const stopStream = React.useCallback(() => {
    streamCancelRef.current?.();
    streamCancelRef.current = null;
  }, []);

  // Abort an in-flight stream on unmount — the fetch reader keeps reading otherwise (OTH-2).
  React.useEffect(() => () => stopStream(), [stopStream]);

  // finalizeRun reads the structured run state after the stream ends (or pauses at
  // requires_action) — the SSE stream carries tokens but not traceId / requiresAction / nodes,
  // which live on the run object. Mirrors the Playground's finalizeRun.
  const finalizeRun = React.useCallback(async (runId: string, streamed: string) => {
    setState((s) => ({ ...s, status: "finalizing" }));
    let detail: RunDetail;
    try {
      detail = await api.getRun(runId);
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) optsRef.current.onForbidden?.(err.message);
      optsRef.current.onFinalizeError?.(err);
      setState((s) => ({
        ...s,
        status: err instanceof ApiError && err.isForbidden ? "forbidden" : "error",
        error: err instanceof Error ? err.message : "request failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      }));
      return;
    }
    // Prefer the finalized assistant message (the clean, trace-truthful content) over the
    // raw-accumulated stream text; fall back to the streamed text when messages is absent.
    const lastMessage = detail.messages?.length
      ? detail.messages[detail.messages.length - 1].content
      : streamed;
    setState((s) => ({
      ...s,
      status: "done",
      runId,
      responseText: lastMessage,
      detail,
      traceId: detail.traceId,
      requiresAction: detail.requiresAction,
      // A finalized run that FAILED surfaces its error so the surface can show it.
      error: detail.status === "failed" ? detail.error || "The run failed." : undefined,
      forbidden: false,
    }));
    optsRef.current.onFinalized?.(detail);
  }, []);

  // streamRun opens the SSE event stream for an already-created/resumed run and drives it to a
  // terminal/paused state, then finalizes. Shared by start() and resume() so the two paths are
  // byte-identical (the Playground duplicated this).
  const streamRun = React.useCallback(
    (runId: string) => {
      setState((s) => ({ ...s, status: "streaming", runId, responseText: "", step: "" }));
      let acc = "";
      let step = "";
      let finalized = false;
      const finalize = () => {
        if (finalized) return;
        finalized = true;
        stopStream();
        void finalizeRun(runId, acc);
      };
      const cancelStream = openRunStream(runId, {
        onEvent: (kind, data) => {
          if (kind === "token") {
            acc += data;
            setState((s) => ({ ...s, status: "streaming", runId, responseText: acc, step }));
          } else if (kind === "message") {
            acc = data;
            setState((s) => ({ ...s, status: "streaming", runId, responseText: acc, step }));
          } else if (kind === "step") {
            // Live step-visibility (M78, ADR 0071 §4): show "what step is my agent on now".
            // formatRunStep handles BOTH the new JSON metadata and the legacy plain-label Data.
            const label = formatRunStep(data);
            if (label) {
              step = label;
              setState((s) => ({ ...s, status: "streaming", runId, responseText: acc, step }));
            }
          } else if (kind === "state" && data === "requires_action") {
            // requires_action is NOT terminal, so the stream stays open — stop it and finalize.
            finalize();
          }
        },
        onClose: finalize,
        onError: (message, status) => {
          finalized = true;
          setState((s) => ({ ...s, status: "error", error: message, forbidden: false }));
          void status;
        },
        onForbidden: (message) => {
          finalized = true;
          optsRef.current.onForbidden?.(message);
          setState((s) => ({ ...s, status: "forbidden", error: message, forbidden: true }));
        },
      });
      streamCancelRef.current = cancelStream;
    },
    [finalizeRun, stopStream],
  );

  const start = React.useCallback(
    async (args: StartArgs) => {
      stopStream();
      setState({ status: "creating", responseText: "", step: "", forbidden: false });
      let runId: string;
      try {
        runId = (await api.createRun(args)).id;
      } catch (err) {
        const forbidden = err instanceof ApiError && err.isForbidden;
        if (forbidden) optsRef.current.onForbidden?.(err.message);
        setState({
          status: forbidden ? "forbidden" : "error",
          responseText: "",
          step: "",
          error: err instanceof Error ? err.message : "run failed",
          forbidden,
        });
        return;
      }
      streamRun(runId);
    },
    [stopStream, streamRun],
  );

  const resume = React.useCallback(
    async (decision?: "approve" | "deny", reason?: string) => {
      const runId = state.runId;
      if (!runId) return;
      stopStream();
      // A deny is terminal — no re-stream; the consumer projects "denied" itself.
      try {
        await api.resumeRun(runId, decision, reason);
      } catch (err) {
        const forbidden = err instanceof ApiError && err.isForbidden;
        if (forbidden) optsRef.current.onForbidden?.(err.message);
        setState((s) => ({
          ...s,
          status: forbidden ? "forbidden" : "error",
          error: err instanceof Error ? err.message : "resume failed",
          forbidden,
        }));
        return;
      }
      if (decision === "deny") {
        // A denied run is finalized without a re-stream; read its terminal state.
        void finalizeRun(runId, "");
        return;
      }
      streamRun(runId);
    },
    [state.runId, stopStream, streamRun, finalizeRun],
  );

  const cancel = React.useCallback(async () => {
    const runId = state.runId;
    stopStream();
    if (runId) {
      try {
        await api.cancelRun(runId);
      } catch {
        // best-effort — the run may already be terminal; the engine resets regardless.
      }
    }
    setState(IDLE);
  }, [state.runId, stopStream]);

  const reset = React.useCallback(() => {
    stopStream();
    setState(IDLE);
  }, [stopStream]);

  return {
    ...state,
    start,
    resume,
    cancel,
    reset,
    running:
      state.status === "creating" ||
      state.status === "streaming" ||
      state.status === "finalizing",
  };
}
