import { describe, expect, it } from "vitest";

import { emptyForm, toAgentYAML, validate, type ConfigForm } from "@/lib/config-form";

// Fill only the fields a test cares about on top of the empty form.
function form(overrides: Partial<ConfigForm>): ConfigForm {
  return { ...emptyForm(), ...overrides };
}

describe("config-form validate", () => {
  it("requires name and image", () => {
    const errors = validate(emptyForm());
    expect(errors.name).toBeTruthy();
    expect(errors.image).toBeTruthy();
  });

  it("rejects a non-DNS name", () => {
    const errors = validate(form({ name: "Echo Agent", image: "x:1" }));
    expect(errors.name).toBeTruthy();
  });

  it("accepts a minimal valid form", () => {
    const errors = validate(form({ name: "echo-agent", image: "ghcr.io/x/echo:v1" }));
    expect(Object.keys(errors)).toHaveLength(0);
  });

  it("requires all prompt git fields when the prompt block is on", () => {
    const errors = validate(
      form({ name: "a", image: "b", promptEnabled: true, promptName: "p" }),
    );
    expect(errors.promptRepo).toBeTruthy();
    expect(errors.promptRef).toBeTruthy();
    expect(errors.promptPath).toBeTruthy();
  });

  it("requires an eval threshold in 0..1 and at least one scorer", () => {
    const errors = validate(
      form({
        name: "a",
        image: "b",
        evalEnabled: true,
        evalSuite: "q",
        evalDataset: "d",
        evalThreshold: "9", // out of range
        scorers: [{ name: "", type: "", weight: "" }],
      }),
    );
    expect(errors.evalThreshold).toBeTruthy();
    expect(errors.scorers).toBeTruthy();
  });

  it("rejects a bad budget USD amount", () => {
    const errors = validate(
      form({ name: "a", image: "b", budgetEnabled: true, budgetPerAgentUSD: "ten" }),
    );
    expect(errors.budgetPerAgentUSD).toBeTruthy();
  });
});

describe("config-form toAgentYAML", () => {
  it("emits only name+image for a minimal form", () => {
    const yaml = toAgentYAML(form({ name: "echo-agent", image: "ghcr.io/x/echo:v1" }));
    expect(yaml).toBe("name: echo-agent\nimage: ghcr.io/x/echo:v1\n");
  });

  it("emits resources and model.route", () => {
    const yaml = toAgentYAML(
      form({
        name: "full-agent",
        image: "ghcr.io/x/full:v1",
        resourcesCpu: "500m",
        resourcesMemory: "256Mi",
        modelRoute: "default-model",
      }),
    );
    expect(yaml).toContain("resources:");
    expect(yaml).toContain("cpu: 500m");
    expect(yaml).toContain("memory: 256Mi");
    expect(yaml).toContain("model:");
    expect(yaml).toContain("route: default-model");
  });

  it("emits scaling.min:1 when keepWarm=true, omits scaling block when false", () => {
    const withWarm = toAgentYAML(
      form({ name: "warm-agent", image: "ghcr.io/x/a:v1", keepWarm: true }),
    );
    expect(withWarm).toContain("scaling:");
    expect(withWarm).toContain("min: 1");
    // max must NOT appear (keepWarm only sets min)
    expect(withWarm).not.toContain("max:");

    const noWarm = toAgentYAML(
      form({ name: "cold-agent", image: "ghcr.io/x/a:v1", keepWarm: false }),
    );
    expect(noWarm).not.toContain("scaling:");
  });

  it("omits the serving default execution model but emits others", () => {
    expect(toAgentYAML(form({ name: "a", image: "b", executionModel: "serving" }))).not.toContain(
      "executionModel",
    );
    expect(toAgentYAML(form({ name: "a", image: "b", executionModel: "job" }))).toContain(
      "executionModel: job",
    );
  });

  it("emits the budget block as numeric YAML", () => {
    const yaml = toAgentYAML(
      form({
        name: "a",
        image: "b",
        budgetEnabled: true,
        budgetPerConversationUSD: "0.50",
        budgetPerAgentUSD: "10",
        budgetSoftThresholdPct: "90",
      }),
    );
    expect(yaml).toContain("budget:");
    expect(yaml).toContain("perConversationUSD: 0.5");
    expect(yaml).toContain("perAgentUSD: 10");
    expect(yaml).toContain("softThresholdPct: 90");
  });

  it("managed runtime: image is optional and emits runtime + systemPrompt + tools (ADR 0013)", () => {
    // A managed agent needs no image (expand resolves the pinned managed ref).
    const errors = validate(form({ name: "support-agent", runtime: "managed" }));
    expect(Object.keys(errors)).toHaveLength(0);

    const yaml = toAgentYAML(
      form({
        name: "support-agent",
        runtime: "managed",
        systemPrompt: "You are a support agent for Acme.",
        tools: ["get_order", "search_docs"],
      }),
    );
    expect(yaml).toContain("runtime: managed");
    // No image emitted when none pinned.
    expect(yaml).not.toContain("\nimage:");
    // System prompt as a literal block scalar.
    expect(yaml).toContain("systemPrompt: |");
    expect(yaml).toContain("You are a support agent for Acme.");
    // Tools as a YAML list — the SAME field expand + generation consume.
    expect(yaml).toContain("tools:");
    expect(yaml).toContain("- get_order");
    expect(yaml).toContain("- search_docs");
  });

  it("custom runtime: image stays required and managed-only fields are omitted", () => {
    // A custom agent (the default) still requires an image.
    expect(validate(form({ name: "a", runtime: "custom" })).image).toBeTruthy();
    // Managed-only fields are NOT serialized on a custom agent (expand rejects
    // them), even if set on the form.
    const yaml = toAgentYAML(
      form({ name: "a", image: "b", runtime: "custom", systemPrompt: "x", tools: ["t"] }),
    );
    expect(yaml).not.toContain("systemPrompt");
    expect(yaml).not.toContain("tools:");
  });

  it("emits eval scorers and prompt.git blocks", () => {
    const yaml = toAgentYAML(
      form({
        name: "a",
        image: "b",
        evalEnabled: true,
        evalSuite: "quality",
        evalDataset: "golden-set",
        evalThreshold: "0.8",
        evalGate: "warn",
        scorers: [{ name: "exact-match", type: "heuristic", weight: "2" }],
        promptEnabled: true,
        promptName: "system-prompt",
        promptRepo: "https://github.com/acme/prompts",
        promptRef: "main",
        promptPath: "prompts/system.txt",
      }),
    );
    expect(yaml).toContain("eval:");
    expect(yaml).toContain("suite: quality");
    expect(yaml).toContain("gate: warn");
    expect(yaml).toContain("- name: exact-match");
    expect(yaml).toContain("weight: 2");
    expect(yaml).toContain("prompt:");
    expect(yaml).toContain("name: system-prompt");
    expect(yaml).toContain("repo: https://github.com/acme/prompts");
    expect(yaml).toContain("path: prompts/system.txt");
  });
});

describe("config-form composition (m18.6)", () => {
  it("serializes role, allowedCallers, and a referenced promptRef", () => {
    const yaml = toAgentYAML(
      form({
        name: "composed",
        runtime: "managed",
        modelRoute: "anthropic",
        role: "reviewer",
        allowedCallers: "orchestrator, auditor",
        promptVersionRef: "system-prompt-v2",
      }),
    );
    expect(yaml).toContain("route: anthropic");
    expect(yaml).toContain("role: reviewer");
    expect(yaml).toContain("allowedCallers:");
    expect(yaml).toContain("- orchestrator");
    expect(yaml).toContain("- auditor");
    expect(yaml).toContain("promptRef: system-prompt-v2");
    // Referencing an existing PromptVersion must NOT emit a git-backed block.
    expect(yaml).not.toContain("git:");
  });

  it("rejects referencing a prompt AND creating a git-backed one", () => {
    const errors = validate(
      form({
        name: "x",
        runtime: "managed",
        promptVersionRef: "existing",
        promptEnabled: true,
        promptName: "new-one",
        promptRepo: "https://github.com/a/b",
        promptRef: "main",
        promptPath: "p.txt",
      }),
    );
    expect(errors.promptVersionRef).toBeTruthy();
  });
});
