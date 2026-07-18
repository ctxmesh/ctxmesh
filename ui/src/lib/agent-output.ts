// extractAgentOutput unwraps the managed-agent /invoke envelope
// ({agent, output, steps, tools_called, consent_required}, m25.9) to the human answer
// the chat should render. A non-envelope body (a custom agent, or non-JSON) falls back
// to the raw string — the chat never shows a bare JSON blob to the user.
export function extractAgentOutput(raw: string): string {
  const trimmed = (raw ?? "").trim();
  if (!trimmed) return "";
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (parsed && typeof parsed === "object" && "output" in parsed) {
      const out = (parsed as { output: unknown }).output;
      if (typeof out === "string") return out;
    }
  } catch {
    // not JSON — a plain-text answer; fall through to the raw string.
  }
  return trimmed;
}
