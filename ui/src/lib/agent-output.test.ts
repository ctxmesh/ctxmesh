import { describe, expect, it } from "vitest";

import { extractAgentOutput } from "./agent-output";

describe("extractAgentOutput", () => {
  it("unwraps the managed-agent envelope to the output text", () => {
    const raw = JSON.stringify({
      agent: "sk-agent",
      output: "Here are the **2 environments** in your account.",
      steps: 2,
      tools_called: ["list_environments"],
      consent_required: [],
    });
    expect(extractAgentOutput(raw)).toBe("Here are the **2 environments** in your account.");
  });

  it("never leaks the envelope's structural fields", () => {
    const raw = JSON.stringify({ agent: "x", output: "hi", steps: 1, tools_called: [] });
    const out = extractAgentOutput(raw);
    expect(out).not.toContain("tools_called");
    expect(out).not.toContain("steps");
    expect(out).not.toContain("agent");
  });

  it("falls back to the raw string for a plain-text (non-JSON) answer", () => {
    expect(extractAgentOutput("just text")).toBe("just text");
  });

  it("falls back to the raw string when JSON has no string output", () => {
    expect(extractAgentOutput(JSON.stringify({ foo: "bar" }))).toBe('{"foo":"bar"}');
    expect(extractAgentOutput(JSON.stringify({ output: 42 }))).toBe('{"output":42}');
  });

  it("returns empty for empty input", () => {
    expect(extractAgentOutput("")).toBe("");
    expect(extractAgentOutput("   ")).toBe("");
  });
});
