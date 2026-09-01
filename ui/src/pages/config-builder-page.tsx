import { useState } from "react";
import { Plus, Rocket, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  ForbiddenInline,
  NextStepLink,
  PageHeader,
  QuietNote,
  SkeletonText,
  resourcePath,
} from "@/components/kit";
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

// ConfigBuilderPage — form on the left, the manifest it generates on the right
// (M151 §6.1 archetype A8, the workbench). Route: /config
//
// ── THE PAGE'S ONE IDEA: THE ARTIFACT SITS BESIDE THE BUTTON THAT CHANGES IT ─
// The user NEVER hand-edits raw YAML. They fill a form; the engine expands it
// server-side (the same expand core the CLI runs, so there is no second
// mapping); the expansion is a READ-ONLY artifact they review before it reaches
// the cluster. That is why the primary action lives at the foot of the FORM
// column rather than in the page header (§6.1 A8): the thing you press and the
// thing that changes have to be in one glance. Above `lg` the artifact is
// sticky, so it stays beside the fields while you scroll them.
//
// ── THE WELL IS A YAML WELL, NOT A TEXTAREA (§4.5, §4.6) ────────────────────
// The manifest used to render in a read-only <textarea>, which soft-wraps. In
// YAML, wrapping is data loss: a continuation line starts at column 0 and the
// indentation — which is the whole structure — reads as a different nesting
// than the one that would be applied. So the well is a `<pre>` on the sunk
// plane with `white-space: pre` (never `break-all`, never a wrap) that OWNS its
// own scrollbar. Wide output scrolls inside the well; the page never widens
// (§4.6, the acceptance being `document.body.scrollWidth === innerWidth`).
//
// ── WHAT THE PAGE MAY CLAIM (§7.1) ──────────────────────────────────────────
// `POST /api/agents` answers with the objects it CREATED. It says nothing about
// whether any of them is serving — the controller has not reconciled them yet.
// So the applied panel is `progressing`, not `ok`, its copy says the objects
// are coming up rather than up, and the kind chips are `muted`: a Kind is the
// object's identity, not its health, and a green Kind chip would be the page
// claiming a readiness nobody reported.
//
// ── STACKED ORDER BELOW `lg` (§4.7) ─────────────────────────────────────────
// The grid collapses to one column at `lg` and DOM order is then the reading
// order: the form, the Preview button at its foot, then the manifest. A jump
// link appears at that width so the button and its result stay one gesture
// apart even when they are a screenful apart.
//
// data-testid contract:
//   apply-readonly-note   — the viewer's read-only explanation (no Apply button)
//   crd-preview           — the YAML well (also aria-labelled "Expanded CRD preview")
//   crd-applied           — the post-apply panel

/**
 * `phase` records WHICH action failed, so the message lands where the press
 * happened: a preview/validation failure belongs at the foot of the form
 * column, an apply failure belongs in the artifact pane next to Apply (§7 A8).
 * Without it both errors render in one place and half of them are orphaned from
 * the button that produced them.
 */
type Submit =
  | { kind: "idle" }
  | { kind: "previewing" }
  | { kind: "preview"; yaml: string; manifest: string }
  | { kind: "applying"; yaml: string; manifest: string }
  | { kind: "applied"; created: CreatedObject[] }
  | {
      kind: "error";
      phase: "preview" | "apply";
      message: string;
      status?: number;
      forbidden?: boolean;
    };

/** The §7 A8 artifact empty state, said once. */
const PREVIEW_EMPTY_TITLE = "The preview appears when the form is valid.";

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
      setSubmit({
        kind: "error",
        phase: "preview",
        message: "Fix the highlighted fields before previewing.",
      });
      return;
    }
    const yaml = toAgentYAML(form);
    setSubmit({ kind: "previewing" });
    try {
      const manifest = await api.expand(yaml);
      setSubmit({ kind: "preview", yaml, manifest });
    } catch (err) {
      setSubmit(errorState(err, "preview"));
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
      setSubmit(errorState(err, "apply"));
    }
  }

  const busy = submit.kind === "previewing" || submit.kind === "applying";
  const manifest =
    submit.kind === "preview" || submit.kind === "applying" ? submit.manifest : "";
  const previewError = submit.kind === "error" && submit.phase === "preview" ? submit : null;
  const applyError = submit.kind === "error" && submit.phase === "apply" ? submit : null;

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Config builder"
        lede="Fill in the form; the manifest it generates appears beside it, expanded server-side by the same engine as the CLI. Nothing reaches the cluster until you apply it."
      />

      {/* §4.7: two equal columns above `lg`, one below it — with explicit grid
          placement, because DOM order is also the stacked reading order. Both
          children carry `min-w-0` so the YAML well can shrink instead of
          setting the page's width (§4.6). */}
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        {/* ── Define ─────────────────────────────────────────────────────── */}
        <div className="min-w-0 space-y-5 lg:col-start-1 lg:row-start-1">
          <Card className="min-w-0">
            <PanelHeader title="The agent" />
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

              <FormField
                id="modelRoute"
                label="Model route"
                hint="ModelRoute alias for the agent's LLM calls (optional)."
              >
                <Input
                  id="modelRoute"
                  value={form.modelRoute}
                  onChange={(e) => set("modelRoute", e.target.value)}
                  placeholder="default-model"
                />
              </FormField>
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="How it runs" />
            <CardContent className="space-y-5">
              <div className="grid gap-4 sm:grid-cols-2">
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

              <div className="flex items-start gap-3 rounded-md border border-border bg-surface-2 p-4">
                <input
                  id="keepWarm"
                  type="checkbox"
                  className="mt-0.5 h-4 w-4 cursor-pointer rounded-sm border-input accent-primary"
                  checked={form.keepWarm}
                  onChange={(e) => set("keepWarm", e.target.checked)}
                />
                <label htmlFor="keepWarm" className="cursor-pointer space-y-0.5">
                  <span className="text-sm font-medium">Keep warm</span>
                  <p className="text-xs text-faint">
                    Keeps at least one replica running so requests never cold-start.
                    Advanced min/max scaling is available in the raw-YAML editor.
                  </p>
                </label>
              </div>
            </CardContent>
          </Card>

          <Card className="min-w-0">
            <PanelHeader title="Bounds and quality" />
            <CardContent className="space-y-4">
              {/* ── Budget (opt-in) ──────────────────────────────────────── */}
              <ToggleSection
                id="budgetEnabled"
                label="Cost budget"
                enabled={form.budgetEnabled}
                onToggle={(v) => set("budgetEnabled", v)}
              >
                <div className="grid gap-4 sm:grid-cols-2">
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
                <div className="grid gap-4 sm:grid-cols-2">
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
                <div className="grid gap-4 sm:grid-cols-2">
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
                <div className="grid gap-4 sm:grid-cols-2">
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
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <span className="text-sm font-medium">Scorers</span>
                    <Button type="button" variant="outline" size="sm" onClick={addScorer}>
                      <Plus className="h-4 w-4" /> Add scorer
                    </Button>
                  </div>
                  {errors.scorers && (
                    <p className="font-mono text-xs text-destructive" role="alert">
                      {errors.scorers}
                    </p>
                  )}
                  {form.scorers.map((scorer, i) => (
                    <div
                      key={i}
                      className="flex flex-wrap items-end gap-2 rounded-md border border-border bg-card p-3"
                    >
                      <div className="min-w-[8rem] flex-1">
                        <FormField
                          id={`scorer-${i}-name`}
                          label="Name"
                          error={errors[`scorer.${i}.name`]}
                        >
                          <Input
                            id={`scorer-${i}-name`}
                            value={scorer.name}
                            onChange={(e) => setScorer(i, { name: e.target.value })}
                            placeholder="exact-match"
                          />
                        </FormField>
                      </div>
                      <div className="min-w-[8rem] flex-1">
                        <FormField
                          id={`scorer-${i}-type`}
                          label="Type"
                          error={errors[`scorer.${i}.type`]}
                        >
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
                        className="text-faint hover:text-destructive"
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
            </CardContent>
          </Card>

          {/* The primary action lives at the FOOT of the form column, not in the
              header (§6.1 A8) — the artifact has to be beside the button that
              changes it. Preview/validation failures land here too, next to the
              press that produced them (§7 A8). */}
          <div className="space-y-3">
            <div className="flex flex-wrap items-center gap-3">
              <Button onClick={onPreview} disabled={busy}>
                {submit.kind === "previewing" ? "Expanding…" : "Preview CRD"}
              </Button>
              {/* Below `lg` the manifest is a screenful further down; the jump
                  link keeps the press and its result one gesture apart (§6.1 A8).
                  An in-page anchor, so it carries the §2.3 inline-link register
                  rather than the router-bound NextStepLink. */}
              {manifest && (
                <a
                  href="#crd-preview"
                  className="whitespace-nowrap border-b border-accent text-sm font-semibold text-primary hover:border-primary lg:hidden"
                >
                  Jump to the manifest
                  <span aria-hidden="true"> ↓</span>
                </a>
              )}
            </div>
            {previewError && (
              <p className="font-mono text-xs text-destructive" role="alert">
                {previewError.message}
                {previewError.status ? ` (${previewError.status})` : ""}
              </p>
            )}
          </div>
        </div>

        {/* ── The artifact ───────────────────────────────────────────────── */}
        {/* `self-start` is load-bearing: a grid item stretches by default, and a
            stretched item cannot stick. With it the card is its own height and
            `lg:sticky` holds it beside the fields (§6.1 A8). `top-[4.5rem]`
            clears the shell's 48px sticky topbar. */}
        <div
          id="crd-preview"
          className="min-w-0 self-start lg:sticky lg:top-[4.5rem] lg:col-start-2 lg:row-start-1"
        >
          {submit.kind === "applied" ? (
            <AppliedPanel
              created={submit.created}
              onReset={() => {
                setForm(emptyForm());
                setErrors({});
                setSubmit({ kind: "idle" });
              }}
            />
          ) : (
            <Card className="min-w-0">
              <PanelHeader
                title="The manifest"
                meta={
                  manifest ? (
                    <span className="tabular-nums">{lineCount(manifest)} lines</span>
                  ) : undefined
                }
              >
                {manifest ? <Badge variant="muted">preview</Badge> : null}
              </PanelHeader>
              <CardContent className="space-y-4">
                {submit.kind === "previewing" ? (
                  <div className="rounded-md bg-surface-3 p-4">
                    <SkeletonText lines={8} />
                  </div>
                ) : manifest ? (
                  // The YAML well (§4.5/§4.6). `<pre>` keeps `white-space: pre`,
                  // so indentation — the structure itself — is never reflowed,
                  // and the element owns its own scrollbars so a long line
                  // scrolls HERE and not in the document. `tabIndex` because a
                  // scroll container a mouse can reach must be reachable by a
                  // keyboard too (WCAG 2.1.1).
                  <pre
                    role="region"
                    aria-label="Expanded CRD preview"
                    data-testid="crd-preview"
                    tabIndex={0}
                    className="max-h-[60vh] overflow-auto rounded-md bg-surface-3 p-4 font-mono text-xs leading-relaxed text-secondary-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    {manifest}
                  </pre>
                ) : (
                  <QuietNote title={PREVIEW_EMPTY_TITLE}>
                    Nothing has been sent anywhere yet. Press{" "}
                    <span className="font-medium">Preview CRD</span> and the
                    engine expands the form into the exact manifest that would be
                    applied — the same expansion the CLI performs, so what you
                    read here is what the cluster would get.
                  </QuietNote>
                )}

                {applyError?.forbidden ? (
                  // A surface-level 403 → the explain-and-suggest primitive, not a
                  // terse line (ADR 0012; the ForbiddenInline seam).
                  <ForbiddenInline
                    title="Not allowed to apply"
                    description="Your account can preview the manifest but can't create agents in this cluster."
                    detail={applyError.message}
                  />
                ) : applyError ? (
                  <p className="font-mono text-xs text-destructive" role="alert">
                    {applyError.message}
                    {applyError.status ? ` (${applyError.status})` : ""}
                  </p>
                ) : null}

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
                  // Not an error and not a hue: nothing is broken, the account
                  // simply cannot write (§5.27).
                  <div data-testid="apply-readonly-note">
                    <QuietNote title="You have read-only access.">
                      Previewing costs nothing and stays available. Applying to
                      the cluster needs create permission on AgentDeployments —
                      ask an admin for a role that has it.
                    </QuietNote>
                  </div>
                )}
              </CardContent>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

/** Line count of the manifest, for the panel's mono meta. */
function lineCount(manifest: string): number {
  return manifest.replace(/\n$/, "").split("\n").length;
}

// AppliedPanel — what the cluster actually took.
//
// The honest claim (§7.1): `POST /api/agents` reports what it CREATED, and
// nothing about readiness. So this is `progressing`, not `ok` — the controller
// is still bringing the objects up — and each object's Kind is a `muted` chip,
// because a Kind is identity, not health. The one green thing on this page
// would otherwise be a readiness claim the backend never made.
function AppliedPanel({
  created,
  onReset,
}: {
  created: CreatedObject[];
  onReset: () => void;
}) {
  const agent = created.find((o) => o.kind === "AgentDeployment") ?? created[0];
  const to = agent ? resourcePath("agent", agent.namespace, agent.name) : null;
  const n = created.length;
  return (
    <Card className="min-w-0" data-testid="crd-applied">
      <PanelHeader title="Applied to the cluster">
        <Badge variant="progressing">applied</Badge>
      </PanelHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-secondary-foreground">
          {n === 1 ? "One object was" : `${n} objects were`} created. They are not
          serving yet — the controller reconciles them next, and the agent's own
          page is where readiness appears.
        </p>
        <ul>
          {created.map((obj) => (
            <li
              key={`${obj.kind}/${obj.namespace}/${obj.name}`}
              className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-border-soft py-3 last:border-0"
            >
              <div className="min-w-0">
                <p className="truncate text-sm font-medium" title={obj.name}>
                  {obj.name}
                </p>
                <p className="truncate font-mono text-xs text-faint" title={obj.namespace}>
                  {obj.namespace}
                </p>
              </div>
              <Badge variant="muted">{obj.kind}</Badge>
            </li>
          ))}
        </ul>
        {to && (
          <NextStepLink
            label="Watch it come up"
            to={to}
            ariaLabel={`Watch ${agent.name} come up`}
          />
        )}
        <div>
          <Button variant="outline" onClick={onReset}>
            Build another
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

// ToggleSection — a labelled opt-in block (budget/prompt/eval). Its checkbox
// gates whether the block's fields are shown + serialized. Token-styled; the
// checkbox uses the `accent-primary` utility so the checked state is pine — the
// §2.3 selection colour, never a status hue.
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
    <div className="rounded-md border border-border bg-surface-2 p-4">
      <label htmlFor={id} className="flex items-center gap-2 text-sm font-medium">
        <input
          id={id}
          type="checkbox"
          checked={enabled}
          onChange={(e) => onToggle(e.target.checked)}
          className="h-4 w-4 rounded-sm border-input accent-primary"
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
// explain-and-suggest 403 primitive) rather than a terse red line. `phase` says
// which button produced it, so the message renders where the press happened.
function errorState(err: unknown, phase: "preview" | "apply"): Submit {
  if (err instanceof ApiError) {
    return {
      kind: "error",
      phase,
      message: err.message,
      status: err.status,
      forbidden: err.isForbidden,
    };
  }
  return {
    kind: "error",
    phase,
    message: err instanceof Error ? err.message : "request failed",
  };
}
