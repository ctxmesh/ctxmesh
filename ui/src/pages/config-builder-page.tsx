import { useState } from "react";
import { CheckCircle2, Plus, Rocket, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { ForbiddenInline } from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { api, ApiError, type CreatedObject } from "@/lib/api";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";
import {
  emptyForm,
  toAgentYAML,
  validate,
  type ConfigForm,
  type FieldErrors,
} from "@/lib/config-form";

// ConfigBuilderPage — the m12.6 surface: forms → agent.yaml → expand → CRD,
// applied via the BFF. The user NEVER hand-edits raw YAML: the form is the
// primary path; the expanded CRD is a read-only reviewable preview. Flow:
//   fill the form → Preview (POST /api/expand → the CRD) → Apply (POST /api/agents
//   → client-go create, RBAC-scoped). Every control composes the design-token
//   primitives; no hardcoded colors.
type Submit =
  | { kind: "idle" }
  | { kind: "previewing" }
  | { kind: "preview"; yaml: string; manifest: string }
  | { kind: "applying"; yaml: string; manifest: string }
  | { kind: "applied"; created: CreatedObject[] }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

export function ConfigBuilderPage() {
  const [form, setForm] = useState<ConfigForm>(emptyForm);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [submit, setSubmit] = useState<Submit>({ kind: "idle" });
  // RBAC-aware chrome (§3): whether the caller may create AgentDeployments. The
  // Apply affordance is gated on this (hidden for a viewer). DISPLAY-ONLY — the
  // API server still enforces, so the 403 path below must also work when this is
  // wrong/optimistic (probe unavailable). reprobe() corrects a stale "yes".
  const { can, reprobe } = useCapabilities();
  const canApply = can(RES_AGENTS, "create");

  function set<K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  function setScorer(i: number, patch: Partial<ConfigForm["scorers"][number]>) {
    setForm((f) => ({
      ...f,
      scorers: f.scorers.map((s, idx) => (idx === i ? { ...s, ...patch } : s)),
    }));
  }
  function addScorer() {
    setForm((f) => ({
      ...f,
      scorers: [...f.scorers, { name: "", type: "heuristic", weight: "" }],
    }));
  }
  function removeScorer(i: number) {
    setForm((f) => ({
      ...f,
      scorers: f.scorers.filter((_, idx) => idx !== i),
    }));
  }

  async function onPreview() {
    const found = validate(form);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      setSubmit({ kind: "error", message: "Fix the highlighted fields before previewing." });
      return;
    }
    const yaml = toAgentYAML(form);
    setSubmit({ kind: "previewing" });
    try {
      const manifest = await api.expand(yaml);
      setSubmit({ kind: "preview", yaml, manifest });
    } catch (err) {
      setSubmit(errorState(err));
    }
  }

  async function onApply() {
    if (submit.kind !== "preview") return;
    const { yaml, manifest } = submit;
    setSubmit({ kind: "applying", yaml, manifest });
    try {
      const res = await api.createAgent(yaml, "");
      setSubmit({ kind: "applied", created: res.created });
    } catch (err) {
      // A 403 DESPITE a "yes" capability means the cached map was stale — correct
      // it (reprobe → honest banner) AND surface the real 403 (ForbiddenInline).
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSubmit(errorState(err));
    }
  }

  const busy = submit.kind === "previewing" || submit.kind === "applying";

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Config builder</h2>
        <p className="text-sm text-muted-foreground">
          Build an agent from a form — no raw YAML. Preview the generated CRD,
          then apply it to the cluster (client-go, RBAC-scoped).
        </p>
      </div>

      <div className="grid gap-6 lg:grid-cols-[1fr_1fr]">
        {/* ── The form ────────────────────────────────────────────────── */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Agent</CardTitle>
            <CardDescription>
              The simplified agent.yaml schema the engine expands into a CRD.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-5">
            <FormField id="name" label="Name" error={errors.name} hint="DNS label, ≤ 44 chars.">
              <Input
                id="name"
                value={form.name}
                onChange={(e) => set("name", e.target.value)}
                placeholder="echo-agent"
              />
            </FormField>

            <FormField id="image" label="Image" error={errors.image}>
              <Input
                id="image"
                value={form.image}
                onChange={(e) => set("image", e.target.value)}
                placeholder="ghcr.io/ctxmesh/echo:v1"
              />
            </FormField>

            <FormField id="executionModel" label="Execution model">
              <Select
                id="executionModel"
                value={form.executionModel}
                onChange={(e) =>
                  set("executionModel", e.target.value as ConfigForm["executionModel"])
                }
              >
                <option value="serving">serving (request-driven)</option>
                <option value="eventing">eventing (broker-triggered)</option>
                <option value="job">job (one-shot)</option>
              </Select>
            </FormField>

            <FormField id="modelRoute" label="Model route" hint="ModelRoute alias for the agent's LLM calls (optional).">
              <Input
                id="modelRoute"
                value={form.modelRoute}
                onChange={(e) => set("modelRoute", e.target.value)}
                placeholder="default-model"
              />
            </FormField>

            <div className="grid grid-cols-2 gap-4">
              <FormField id="resourcesCpu" label="CPU request">
                <Input
                  id="resourcesCpu"
                  value={form.resourcesCpu}
                  onChange={(e) => set("resourcesCpu", e.target.value)}
                  placeholder="500m"
                />
              </FormField>
              <FormField id="resourcesMemory" label="Memory request">
                <Input
                  id="resourcesMemory"
                  value={form.resourcesMemory}
                  onChange={(e) => set("resourcesMemory", e.target.value)}
                  placeholder="256Mi"
                />
              </FormField>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <FormField id="scalingMin" label="Min replicas" error={errors.scalingMin}>
                <Input
                  id="scalingMin"
                  inputMode="numeric"
                  value={form.scalingMin}
                  onChange={(e) => set("scalingMin", e.target.value)}
                  placeholder="0"
                />
              </FormField>
              <FormField id="scalingMax" label="Max replicas" error={errors.scalingMax}>
                <Input
                  id="scalingMax"
                  inputMode="numeric"
                  value={form.scalingMax}
                  onChange={(e) => set("scalingMax", e.target.value)}
                  placeholder="3"
                />
              </FormField>
            </div>

            {/* ── Budget (opt-in) ──────────────────────────────────────── */}
            <ToggleSection
              id="budgetEnabled"
              label="Cost budget"
              enabled={form.budgetEnabled}
              onToggle={(v) => set("budgetEnabled", v)}
            >
              <div className="grid grid-cols-2 gap-4">
                <FormField
                  id="budgetPerConversationUSD"
                  label="Per conversation (USD)"
                  error={errors.budgetPerConversationUSD}
                >
                  <Input
                    id="budgetPerConversationUSD"
                    value={form.budgetPerConversationUSD}
                    onChange={(e) => set("budgetPerConversationUSD", e.target.value)}
                    placeholder="0.50"
                  />
                </FormField>
                <FormField
                  id="budgetPerAgentUSD"
                  label="Per agent (USD)"
                  error={errors.budgetPerAgentUSD}
                >
                  <Input
                    id="budgetPerAgentUSD"
                    value={form.budgetPerAgentUSD}
                    onChange={(e) => set("budgetPerAgentUSD", e.target.value)}
                    placeholder="10.00"
                  />
                </FormField>
              </div>
              <FormField
                id="budgetSoftThresholdPct"
                label="Soft alert threshold (%)"
                error={errors.budgetSoftThresholdPct}
                hint="1–99; defaults to 80."
              >
                <Input
                  id="budgetSoftThresholdPct"
                  inputMode="numeric"
                  value={form.budgetSoftThresholdPct}
                  onChange={(e) => set("budgetSoftThresholdPct", e.target.value)}
                  placeholder="80"
                />
              </FormField>
            </ToggleSection>

            {/* ── Prompt (opt-in) ──────────────────────────────────────── */}
            <ToggleSection
              id="promptEnabled"
              label="Git-backed prompt"
              enabled={form.promptEnabled}
              onToggle={(v) => set("promptEnabled", v)}
            >
              <FormField id="promptName" label="Prompt name" error={errors.promptName}>
                <Input
                  id="promptName"
                  value={form.promptName}
                  onChange={(e) => set("promptName", e.target.value)}
                  placeholder="system-prompt"
                />
              </FormField>
              <FormField id="promptRepo" label="Git repo" error={errors.promptRepo}>
                <Input
                  id="promptRepo"
                  value={form.promptRepo}
                  onChange={(e) => set("promptRepo", e.target.value)}
                  placeholder="https://github.com/acme/prompts"
                />
              </FormField>
              <div className="grid grid-cols-2 gap-4">
                <FormField id="promptRef" label="Ref" error={errors.promptRef}>
                  <Input
                    id="promptRef"
                    value={form.promptRef}
                    onChange={(e) => set("promptRef", e.target.value)}
                    placeholder="main"
                  />
                </FormField>
                <FormField id="promptPath" label="Path" error={errors.promptPath}>
                  <Input
                    id="promptPath"
                    value={form.promptPath}
                    onChange={(e) => set("promptPath", e.target.value)}
                    placeholder="prompts/system.txt"
                  />
                </FormField>
              </div>
            </ToggleSection>

            {/* ── Eval gate (opt-in) ───────────────────────────────────── */}
            <ToggleSection
              id="evalEnabled"
              label="Eval gate"
              enabled={form.evalEnabled}
              onToggle={(v) => set("evalEnabled", v)}
            >
              <div className="grid grid-cols-2 gap-4">
                <FormField id="evalSuite" label="Suite name" error={errors.evalSuite}>
                  <Input
                    id="evalSuite"
                    value={form.evalSuite}
                    onChange={(e) => set("evalSuite", e.target.value)}
                    placeholder="quality"
                  />
                </FormField>
                <FormField id="evalDataset" label="Dataset ref" error={errors.evalDataset}>
                  <Input
                    id="evalDataset"
                    value={form.evalDataset}
                    onChange={(e) => set("evalDataset", e.target.value)}
                    placeholder="golden-set"
                  />
                </FormField>
              </div>
              <div className="grid grid-cols-2 gap-4">
                <FormField id="evalThreshold" label="Threshold" error={errors.evalThreshold}>
                  <Input
                    id="evalThreshold"
                    value={form.evalThreshold}
                    onChange={(e) => set("evalThreshold", e.target.value)}
                    placeholder="0.8"
                  />
                </FormField>
                <FormField id="evalGate" label="Gate">
                  <Select
                    id="evalGate"
                    value={form.evalGate}
                    onChange={(e) => set("evalGate", e.target.value as ConfigForm["evalGate"])}
                  >
                    <option value="block">block (fail closed)</option>
                    <option value="warn">warn (promote anyway)</option>
                  </Select>
                </FormField>
              </div>

              <div className="space-y-2">
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium">Scorers</span>
                  <Button type="button" variant="outline" size="sm" onClick={addScorer}>
                    <Plus className="h-4 w-4" /> Add scorer
                  </Button>
                </div>
                {errors.scorers && (
                  <p className="text-xs text-destructive" role="alert">
                    {errors.scorers}
                  </p>
                )}
                {form.scorers.map((scorer, i) => (
                  <div key={i} className="flex items-end gap-2 rounded-md border p-3">
                    <div className="flex-1">
                      <FormField id={`scorer-${i}-name`} label="Name" error={errors[`scorer.${i}.name`]}>
                        <Input
                          id={`scorer-${i}-name`}
                          value={scorer.name}
                          onChange={(e) => setScorer(i, { name: e.target.value })}
                          placeholder="exact-match"
                        />
                      </FormField>
                    </div>
                    <div className="flex-1">
                      <FormField id={`scorer-${i}-type`} label="Type" error={errors[`scorer.${i}.type`]}>
                        <Input
                          id={`scorer-${i}-type`}
                          value={scorer.type}
                          onChange={(e) => setScorer(i, { type: e.target.value })}
                          placeholder="heuristic"
                        />
                      </FormField>
                    </div>
                    <div className="w-20">
                      <FormField id={`scorer-${i}-weight`} label="Weight">
                        <Input
                          id={`scorer-${i}-weight`}
                          inputMode="numeric"
                          value={scorer.weight}
                          onChange={(e) => setScorer(i, { weight: e.target.value })}
                          placeholder="1"
                        />
                      </FormField>
                    </div>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      aria-label={`Remove scorer ${i + 1}`}
                      onClick={() => removeScorer(i)}
                      disabled={form.scorers.length === 1}
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </div>
                ))}
              </div>
            </ToggleSection>

            <div className="flex items-center gap-3 pt-2">
              <Button onClick={onPreview} disabled={busy}>
                {submit.kind === "previewing" ? "Expanding…" : "Preview CRD"}
              </Button>
            </div>
          </CardContent>
        </Card>

        {/* ── The preview + apply ─────────────────────────────────────── */}
        <Card>
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle className="text-base">Expanded CRD</CardTitle>
              <CardDescription>
                Generated server-side by the same engine as the CLI. Review, then
                apply.
              </CardDescription>
            </div>
            {(submit.kind === "preview" || submit.kind === "applying") && (
              <Badge variant="secondary">preview</Badge>
            )}
          </CardHeader>
          <CardContent className="space-y-4">
            {submit.kind === "applied" ? (
              <div className="space-y-3">
                <div className="flex items-center gap-2 text-success">
                  <CheckCircle2 className="h-5 w-5" />
                  <span className="text-sm font-medium">Applied to the cluster</span>
                </div>
                <div className="grid gap-2">
                  {submit.created.map((obj) => (
                    <div
                      key={`${obj.kind}/${obj.namespace}/${obj.name}`}
                      className="flex items-center justify-between rounded-md border p-3"
                    >
                      <div>
                        <p className="text-sm font-medium">{obj.name}</p>
                        <p className="text-xs text-muted-foreground">{obj.namespace}</p>
                      </div>
                      <Badge variant="success">{obj.kind}</Badge>
                    </div>
                  ))}
                </div>
                <Button
                  variant="outline"
                  onClick={() => {
                    setForm(emptyForm());
                    setErrors({});
                    setSubmit({ kind: "idle" });
                  }}
                >
                  Build another
                </Button>
              </div>
            ) : (
              <>
                <Textarea
                  aria-label="Expanded CRD preview"
                  readOnly
                  className="min-h-[24rem] font-mono text-xs"
                  value={
                    submit.kind === "preview" || submit.kind === "applying"
                      ? submit.manifest
                      : ""
                  }
                  placeholder="Fill the form and press “Preview CRD” to see the generated manifest."
                />
                {submit.kind === "error" && submit.forbidden ? (
                  // A surface-level 403 → the explain-and-suggest primitive, not a
                  // terse line (ADR 0012; the ForbiddenInline seam).
                  <ForbiddenInline
                    title="Not allowed to apply"
                    description="Your account can preview the manifest but can't create agents in this cluster."
                    detail={submit.message}
                  />
                ) : (
                  submit.kind === "error" && (
                    <p className="text-sm text-destructive" role="alert">
                      {submit.message}
                      {submit.status ? ` (${submit.status})` : ""}
                    </p>
                  )
                )}
                {canApply ? (
                  <Button
                    onClick={onApply}
                    disabled={submit.kind !== "preview"}
                    className="w-full"
                  >
                    <Rocket className="h-4 w-4" />
                    {submit.kind === "applying" ? "Applying…" : "Apply to cluster"}
                  </Button>
                ) : (
                  // RBAC-aware chrome: a viewer sees no Apply affordance — the
                  // preview stays available (read-only console by construction).
                  <p
                    className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-center text-xs text-muted-foreground"
                    data-testid="apply-readonly-note"
                  >
                    You have read-only access — applying to the cluster requires
                    create permission on AgentDeployments.
                  </p>
                )}
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

// ToggleSection — a labelled opt-in block (budget/prompt/eval). Its checkbox
// gates whether the block's fields are shown + serialized. Token-styled; the
// checkbox uses the accent-color utility so it follows the theme.
function ToggleSection({
  id,
  label,
  enabled,
  onToggle,
  children,
}: {
  id: string;
  label: string;
  enabled: boolean;
  onToggle: (v: boolean) => void;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-md border p-4">
      <label htmlFor={id} className="flex items-center gap-2 text-sm font-medium">
        <input
          id={id}
          type="checkbox"
          checked={enabled}
          onChange={(e) => onToggle(e.target.checked)}
          className="h-4 w-4 rounded border-input accent-primary"
        />
        {label}
      </label>
      {enabled && <div className="mt-4 space-y-4">{children}</div>}
    </div>
  );
}

// errorState maps a thrown error to the Submit error state, preserving the HTTP
// status when it's an ApiError so the user sees, e.g., a 403 RBAC denial reason.
// A 403 is flagged `forbidden` so the surface renders ForbiddenInline (the
// explain-and-suggest 403 primitive) rather than a terse red line.
function errorState(err: unknown): Submit {
  if (err instanceof ApiError) {
    return {
      kind: "error",
      message: err.message,
      status: err.status,
      forbidden: err.isForbidden,
    };
  }
  return {
    kind: "error",
    message: err instanceof Error ? err.message : "request failed",
  };
}
