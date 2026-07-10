// config-form — the config-builder's form model, client-side validation, and
// agent.yaml serializer. It mirrors the simplified agent.yaml schema the engine's
// `expand` core consumes (cmd/agent-engine/expand.go → internal/expand): name,
// image, executionModel, resources, scaling, model.route, budget, eval, prompt.
//
// The client-side validation is a FRIENDLY mirror of the server's rules (the BFF
// re-validates via the same expand core, so the server is the source of truth) —
// it catches obvious mistakes before a round-trip. The serializer emits the exact
// simplified schema; the BFF expands it (no second mapping).

export type ExecutionModel = "serving" | "eventing" | "job";
export type EvalGate = "block" | "warn";

// ScorerForm is one eval scorer row. weight is a string in the form (empty →
// omitted, so the CRD default of 1 applies).
export interface ScorerForm {
  name: string;
  type: string;
  weight: string;
}

// ConfigForm is the full editable state of the config-builder. Every field is a
// string (form-native); the serializer coerces + omits empties so the emitted
// agent.yaml only carries what the user set — matching the CLI's optional-field
// behavior. `eval` and `prompt` are opt-in blocks (enabled flags) because they
// are multi-field and the expand schema requires all sub-fields once present.
export interface ConfigForm {
  name: string;
  image: string;
  executionModel: ExecutionModel;
  modelRoute: string;
  resourcesCpu: string;
  resourcesMemory: string;
  scalingMin: string;
  scalingMax: string;

  budgetEnabled: boolean;
  budgetPerConversationUSD: string;
  budgetPerAgentUSD: string;
  budgetSoftThresholdPct: string;

  promptEnabled: boolean;
  promptName: string;
  promptRepo: string;
  promptRef: string;
  promptPath: string;

  evalEnabled: boolean;
  evalSuite: string;
  evalDataset: string;
  evalThreshold: string;
  evalGate: EvalGate;
  scorers: ScorerForm[];
}

// emptyForm is the initial config-builder state.
export function emptyForm(): ConfigForm {
  return {
    name: "",
    image: "",
    executionModel: "serving",
    modelRoute: "",
    resourcesCpu: "",
    resourcesMemory: "",
    scalingMin: "",
    scalingMax: "",
    budgetEnabled: false,
    budgetPerConversationUSD: "",
    budgetPerAgentUSD: "",
    budgetSoftThresholdPct: "",
    promptEnabled: false,
    promptName: "",
    promptRepo: "",
    promptRef: "",
    promptPath: "",
    evalEnabled: false,
    evalSuite: "",
    evalDataset: "",
    evalThreshold: "",
    evalGate: "block",
    scorers: [{ name: "", type: "heuristic", weight: "" }],
  };
}

// FieldErrors maps a form field id to its validation message. Empty → valid.
export type FieldErrors = Record<string, string>;

// dns1123 mirrors the DNS-label shape CRD names must take (used for name and the
// eval/prompt resource names). Kept lenient — the API server is the final judge.
const dns1123 = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
// decimalUSD mirrors the CRD BudgetSpec pattern ^[0-9]+(\.[0-9]{1,6})?$.
const decimalUSD = /^[0-9]+(\.[0-9]{1,6})?$/;
// threshold01 mirrors the EvalSuite threshold shape (a 0..1 decimal string).
const threshold01 = /^[01](\.[0-9]{1,4})?$/;

// validate returns the field-error map for a form. It mirrors expand.go's
// required-field + shape checks so the user sees issues before submitting.
export function validate(form: ConfigForm): FieldErrors {
  const errors: FieldErrors = {};

  if (!form.name.trim()) {
    errors.name = "Name is required.";
  } else if (!dns1123.test(form.name.trim())) {
    errors.name = "Name must be a DNS label (lowercase alphanumeric and dashes).";
  } else if (form.name.trim().length > 44) {
    // Matches the AgentDeployment name-length CEL rule (revision-name budget).
    errors.name = "Name must be at most 44 characters.";
  }

  if (!form.image.trim()) {
    errors.image = "Image is required.";
  }

  if (form.scalingMin.trim() && !isNonNegInt(form.scalingMin)) {
    errors.scalingMin = "Min replicas must be a non-negative integer.";
  }
  if (form.scalingMax.trim() && !isPosInt(form.scalingMax)) {
    errors.scalingMax = "Max replicas must be a positive integer.";
  }

  if (form.budgetEnabled) {
    if (form.budgetPerConversationUSD.trim() && !decimalUSD.test(form.budgetPerConversationUSD.trim())) {
      errors.budgetPerConversationUSD = "Use a decimal amount, e.g. 0.50.";
    }
    if (form.budgetPerAgentUSD.trim() && !decimalUSD.test(form.budgetPerAgentUSD.trim())) {
      errors.budgetPerAgentUSD = "Use a decimal amount, e.g. 10.00.";
    }
    if (form.budgetSoftThresholdPct.trim()) {
      const pct = Number(form.budgetSoftThresholdPct);
      if (!Number.isInteger(pct) || pct < 1 || pct > 99) {
        errors.budgetSoftThresholdPct = "Threshold must be an integer 1–99.";
      }
    }
  }

  if (form.promptEnabled) {
    if (!form.promptName.trim()) errors.promptName = "Prompt name is required.";
    if (!form.promptRepo.trim()) errors.promptRepo = "Git repo is required.";
    if (!form.promptRef.trim()) errors.promptRef = "Git ref is required.";
    if (!form.promptPath.trim()) errors.promptPath = "Git path is required.";
  }

  if (form.evalEnabled) {
    if (!form.evalSuite.trim()) errors.evalSuite = "Eval suite name is required.";
    if (!form.evalDataset.trim()) errors.evalDataset = "Dataset ref is required.";
    if (!form.evalThreshold.trim()) {
      errors.evalThreshold = "Threshold is required.";
    } else if (!threshold01.test(form.evalThreshold.trim())) {
      errors.evalThreshold = "Threshold must be a 0..1 decimal, e.g. 0.8.";
    }
    const named = form.scorers.filter((s) => s.name.trim() || s.type.trim());
    if (named.length === 0) {
      errors.scorers = "Add at least one scorer.";
    } else {
      named.forEach((s, i) => {
        if (!s.name.trim()) errors[`scorer.${i}.name`] = "Scorer name is required.";
        if (!s.type.trim()) errors[`scorer.${i}.type`] = "Scorer type is required.";
      });
    }
  }

  return errors;
}

function isNonNegInt(s: string): boolean {
  return /^[0-9]+$/.test(s.trim());
}
function isPosInt(s: string): boolean {
  return /^[0-9]+$/.test(s.trim()) && Number(s) >= 1;
}

// yamlString quotes a scalar when YAML would otherwise misread it (empty, or a
// value with leading/trailing space or YAML-significant chars). Keeps the emitted
// agent.yaml unambiguous without pulling in a YAML library for output.
function yamlString(v: string): string {
  if (v === "") return '""';
  if (/^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$/.test(v)) return v;
  return JSON.stringify(v); // JSON strings are valid YAML double-quoted scalars.
}

// toAgentYAML serializes a form into the simplified agent.yaml the BFF expands.
// It omits empty optional fields so the output mirrors what a hand-written
// agent.yaml would contain — the SAME schema the CLI `expand` consumes.
export function toAgentYAML(form: ConfigForm): string {
  const lines: string[] = [];
  lines.push(`name: ${yamlString(form.name.trim())}`);
  lines.push(`image: ${yamlString(form.image.trim())}`);
  if (form.executionModel && form.executionModel !== "serving") {
    lines.push(`executionModel: ${form.executionModel}`);
  }

  const cpu = form.resourcesCpu.trim();
  const mem = form.resourcesMemory.trim();
  if (cpu || mem) {
    lines.push("resources:");
    if (cpu) lines.push(`  cpu: ${yamlString(cpu)}`);
    if (mem) lines.push(`  memory: ${yamlString(mem)}`);
  }

  const min = form.scalingMin.trim();
  const max = form.scalingMax.trim();
  if (min || max) {
    lines.push("scaling:");
    if (min) lines.push(`  min: ${Number(min)}`);
    if (max) lines.push(`  max: ${Number(max)}`);
  }

  const route = form.modelRoute.trim();
  if (route) {
    lines.push("model:");
    lines.push(`  route: ${yamlString(route)}`);
  }

  if (form.budgetEnabled) {
    const conv = form.budgetPerConversationUSD.trim();
    const agent = form.budgetPerAgentUSD.trim();
    const pct = form.budgetSoftThresholdPct.trim();
    if (conv || agent || pct) {
      lines.push("budget:");
      if (conv) lines.push(`  perConversationUSD: ${Number(conv)}`);
      if (agent) lines.push(`  perAgentUSD: ${Number(agent)}`);
      if (pct) lines.push(`  softThresholdPct: ${Number(pct)}`);
    }
  }

  if (form.evalEnabled) {
    lines.push("eval:");
    lines.push(`  suite: ${yamlString(form.evalSuite.trim())}`);
    lines.push(`  dataset: ${yamlString(form.evalDataset.trim())}`);
    lines.push(`  threshold: ${yamlString(form.evalThreshold.trim())}`);
    if (form.evalGate && form.evalGate !== "block") {
      lines.push(`  gate: ${form.evalGate}`);
    }
    const scorers = form.scorers.filter((s) => s.name.trim() || s.type.trim());
    if (scorers.length > 0) {
      lines.push("  scorers:");
      for (const s of scorers) {
        lines.push(`    - name: ${yamlString(s.name.trim())}`);
        lines.push(`      type: ${yamlString(s.type.trim())}`);
        if (s.weight.trim()) lines.push(`      weight: ${Number(s.weight)}`);
      }
    }
  }

  if (form.promptEnabled) {
    lines.push("prompt:");
    lines.push(`  name: ${yamlString(form.promptName.trim())}`);
    lines.push("  git:");
    lines.push(`    repo: ${yamlString(form.promptRepo.trim())}`);
    lines.push(`    ref: ${yamlString(form.promptRef.trim())}`);
    lines.push(`    path: ${yamlString(form.promptPath.trim())}`);
  }

  return lines.join("\n") + "\n";
}
