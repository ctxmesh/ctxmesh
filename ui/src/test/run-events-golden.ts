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

// goldenFrames splits the golden bytes into their frames, each with its seq, every frame keeping
// its terminating blank line, so a slice of them is still the server's exact bytes.
export function goldenFrames(): { seq: number; bytes: string }[] {
  return RUN_EVENTS_GOLDEN.split(/(?<=\n\n)/)
    .filter((f) => f.trim() !== "")
    .map((bytes) => ({ seq: Number(/^id: (\d+)$/m.exec(bytes)?.[1] ?? 0), bytes }));
}

// golden is the run's frames with after < seq <= through: golden(0, 5) is the stream as a client
// holds it when the run pauses, golden(5) is what the server sends a client resuming with
// Last-Event-ID: 5.
export function golden(after = 0, through = Number.POSITIVE_INFINITY): string {
  return goldenFrames()
    .filter((f) => f.seq > after && f.seq <= through)
    .map((f) => f.bytes)
    .join("");
}

// chunked cuts bytes into reads of n bytes, so a parser sees frames split across reads the way
// a network delivers them.
export function chunked(bytes: string, n: number): string[] {
  const out: string[] = [];
  for (let i = 0; i < bytes.length; i += n) out.push(bytes.slice(i, i + n));
  return out;
}
