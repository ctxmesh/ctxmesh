import fs from "node:fs";
import path from "node:path";

// The BFF's run event stream (GET /api/runs/{id}/events) exactly as its handler writes it.
// The Go test TestRunEventsWire_MatchesGolden regenerates this file from the real handler and
// fails when the handler's bytes drift from it, so a console test fed these bytes is fed what
// the server sends, never what the client hopes for. The run pauses for consent at seq 5, is
// resumed, and completes at seq 11.
export const RUN_EVENTS_GOLDEN = fs.readFileSync(
  path.resolve(__dirname, "../../../sdk/run-events.golden.sse"),
  "utf8",
);

// The seq of the `state: requires_action` frame in the golden run.
export const GOLDEN_REQUIRES_ACTION_SEQ = 5;

// The text the golden run's tokens add up to.
export const GOLDEN_TEXT =
  "Calling echo_tool.\nMOCK_TOOL_OK echo_tool returned:\nFIXTURE_ECHO: ping\n Done.";

// framesOf splits stream bytes into their frames, each with its seq and event name, every frame
// keeping its terminating blank line, so a slice of them is still the server's exact bytes.
function framesOf(bytes: string): { seq: number; event: string; bytes: string }[] {
  return bytes
    .split(/(?<=\n\n)/)
    .filter((f) => f.trim() !== "")
    .map((frame) => ({
      seq: Number(/^id: (\d+)$/m.exec(frame)?.[1] ?? 0),
      event: /^event: (.*)$/m.exec(frame)?.[1] ?? "",
      bytes: frame,
    }));
}

function sliceFrames(bytes: string, after: number, through: number): string {
  return framesOf(bytes)
    .filter((f) => f.seq > after && f.seq <= through)
    .map((f) => f.bytes)
    .join("");
}

// golden is the run's frames with after < seq <= through: golden(0, 5) is the stream as a client
// holds it when the run pauses, golden(5) is what the server sends a client resuming with
// Last-Event-ID: 5.
export function golden(after = 0, through = Number.POSITIVE_INFINITY): string {
  return sliceFrames(RUN_EVENTS_GOLDEN, after, through);
}

// The supervisor's stream when a sub-agent it delegated to pauses for consent: the run waits, the
// stream names the paused sub-run in a `descendant-requires-action` frame, and after the sub-run is
// resumed the supervisor is woken and answers. TestRunEventsWire_DescendantPauseMatchesGolden
// regenerates it, and the paused sub-run's detail, from the real handlers.
export const RUN_EVENTS_DESCENDANT_GOLDEN = fs.readFileSync(
  path.resolve(__dirname, "../../../sdk/run-events-descendant.golden.sse"),
  "utf8",
);

// GET /api/runs/{sub} for the paused sub-run, as the handler served it.
export const RUN_DESCENDANT_PAUSED: {
  id: string;
  agent: string;
  rootRunId: string;
  requiresAction: { kind: string; servers?: string[] };
} = JSON.parse(
  fs.readFileSync(path.resolve(__dirname, "../../../sdk/run-descendant-paused.golden.json"), "utf8"),
);

// The seq of the frame that names the paused sub-run.
export const DESCENDANT_PAUSE_SEQ =
  framesOf(RUN_EVENTS_DESCENDANT_GOLDEN).find((f) => f.event === "descendant-requires-action")?.seq ?? 0;

// The supervisor's answer once the sub-run is resumed.
export const DESCENDANT_ANSWER = "The researcher found 3 sources.";

// descendantGolden is the supervisor's frames with after < seq <= through, as golden() is.
export function descendantGolden(after = 0, through = Number.POSITIVE_INFINITY): string {
  return sliceFrames(RUN_EVENTS_DESCENDANT_GOLDEN, after, through);
}

// chunked cuts bytes into reads of n bytes, so a parser sees frames split across reads the way
// a network delivers them.
export function chunked(bytes: string, n: number): string[] {
  const out: string[] = [];
  for (let i = 0; i < bytes.length; i += n) out.push(bytes.slice(i, i + n));
  return out;
}

// The supervisor's stream when it delegates to two sub-agents at once and both pause for consent
// before either is resumed: two `descendant-requires-action` frames back to back, then, once both
// are resumed, the supervisor's answer. TestRunEventsWire_FanOutPauseMatchesGolden regenerates it,
// and both paused sub-runs' details (in the order they paused), from the real handlers.
export const RUN_EVENTS_FANOUT_GOLDEN = fs.readFileSync(
  path.resolve(__dirname, "../../../sdk/run-events-fanout.golden.sse"),
  "utf8",
);

// GET /api/runs/{sub} for each paused sub-run, as the handler served it.
export const RUN_FANOUT_PAUSED: {
  id: string;
  agent: string;
  rootRunId: string;
  requiresAction: { kind: string; servers?: string[] };
}[] = JSON.parse(
  fs.readFileSync(path.resolve(__dirname, "../../../sdk/run-fanout-paused.golden.json"), "utf8"),
);

// The seqs of the frames that name the paused sub-runs, in stream order.
export const FANOUT_PAUSE_SEQS = framesOf(RUN_EVENTS_FANOUT_GOLDEN)
  .filter((f) => f.event === "descendant-requires-action")
  .map((f) => f.seq);

// The supervisor's answer once both sub-runs are resumed.
export const FANOUT_ANSWER = "Sources found and the summary is sent.";

// fanOutGolden is the supervisor's frames with after < seq <= through, as golden() is.
export function fanOutGolden(after = 0, through = Number.POSITIVE_INFINITY): string {
  return sliceFrames(RUN_EVENTS_FANOUT_GOLDEN, after, through);
}
