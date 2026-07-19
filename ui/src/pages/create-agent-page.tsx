import * as React from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  Boxes,
  CheckCircle2,
  ChevronRight,
  PlugZap,
  Plus,
  Rocket,
  Search,
  Server,
  Sparkles,
  Terminal,
  Wrench,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  EmptyState,
  ForbiddenInline,
  Wizard,
  useToast,
  type WizardStep,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { RES_AGENTS } from "@/lib/nav";
import {
  api,
  ApiError,
  type CatalogTool,
  type ProviderSummary,
  type CreatedObject,
  type GenerateAgentResponse,
} from "@/lib/api";
import {
  emptyForm,
  toAgentYAML,
  validate,
  type ConfigForm,
  type FieldErrors,
} from "@/lib/config-form";
import { groupToolsByServer } from "@/lib/tool-groups";

// CreateAgentPage — the create-agent wizard, the HEART of the aha (spec §5, ADR
// 0013/0014). TWO entrances converge on ONE review:
//
//   • "Describe it" — a prompt hero → POST /api/agents/generate → a friendly
//     review of the generated config (raw CRD behind Advanced) + the tool picker
//     (the generated tools pre-selected). A 422 (keyed off the `regenerate` FLAG,
//     NOT the status) → the reason + a Regenerate button; the raw agentYAML is
//     preserved so nothing is lost. Generation NEVER auto-applies.
//   • "Configure it" — the M12 config-builder form SPLIT into Wizard steps
//     (basics → behavior → resources → optional), reusing lib/config-form's
//     validate + toAgentYAML (NOT rewritten), converging on the SAME review.
//
// The shared review renders the agent.yaml as a friendly summary (raw behind
// Advanced — no hand-editing as the primary path), the tool picker (GET
// /api/tools — curated + user-added, with schema/pending badges), a Preview
// (POST /api/expand → the CRD) and Create (POST /api/agents, the m12.6 path).
// On create → navigate to the agent landing page (m14.11) if built, else the
// agents list with a success toast.
//
// RBAC-aware (display-only, ADR 0011): Create gates on agentdeployments.create;
// a forced 403 renders ForbiddenInline. A caller with NO connected provider is
// steered to connect one first — the Describe-it path needs a provider to
// generate (and running any agent needs a model).

// The mode is the top-level flow selection: the entrance fork, then one of the
// two entrances. `configure` and `describe` both end at the shared review.
type Mode = "entrance" | "describe" | "configure";

// The Describe-it sub-state. `prompt` is the hero; `review` is the generation
// review (valid generation); `regenerate` is the 422 outcome (reason + raw YAML
// preserved). The shared review is reached by advancing `review` → `shared`.
type DescribeStage =
  | { kind: "prompt" }
  | { kind: "generating" }
  | { kind: "review"; gen: GenerateAgentResponse }
  | { kind: "regenerate"; reason: string; rawYAML: string };

// CreateState covers the shared review's Preview/Create lifecycle. It carries
// the final agentYAML (tools injected) so Preview and Create act on the SAME
// body — one apply path for both entrances (no divergence).
type CreateState =
  | { kind: "idle" }
  | { kind: "previewing" }
  | { kind: "preview"; manifest: string }
  | { kind: "creating" }
  | { kind: "created"; created: CreatedObject[] }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

export function CreateAgentPage() {
  const [params] = useSearchParams();
  // m21: arriving from Providers ("Use") carries ?provider= (provider-as-model-home):
  // jump straight to Configure with that provider's model pre-picked.
  const initialProvider = params.get("provider") ?? "";
  const [mode, setMode] = React.useState<Mode>(
    initialProvider ? "configure" : "entrance",
  );

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">New agent</h2>
        <p className="text-sm text-muted-foreground">
          Describe it in a sentence and we generate a validated config — or
          configure every knob yourself. Both converge on one review + tool
          picker before anything is created.
        </p>
      </div>
      <ProviderGate>
        {mode === "entrance" && <Entrance onPick={setMode} />}
        {mode === "describe" && (
          <DescribeFlow onBack={() => setMode("entrance")} />
        )}
        {mode === "configure" && (
          <ConfigureFlow
            onBack={() => setMode("entrance")}
            initialProvider={initialProvider}
          />
        )}
      </ProviderGate>
    </div>
  );
}

// ProviderGate steers a caller with NO connected provider to connect one first:
// the Describe-it path needs a provider to generate, and running any agent needs
// a model. A load FAILURE (incl. the kill-switch 404) is NOT treated as "no
// provider" — we let the flow through (an honest "no false gate"), so a hardened
// install that 404s /api/providers still reaches the form.
function ProviderGate({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate();
  const [state, setState] = React.useState<
    { kind: "loading" } | { kind: "gate" } | { kind: "ok" }
  >({ kind: "loading" });

  React.useEffect(() => {
    const controller = new AbortController();
    api
      .listProviders(controller.signal)
      .then((res: { providers: ProviderSummary[] }) => {
        if (controller.signal.aborted) return;
        setState(
          res.providers.length === 0 ? { kind: "gate" } : { kind: "ok" },
        );
      })
      .catch(() => {
        // A probe failure is not "no provider" — don't block on it.
        if (!controller.signal.aborted) setState({ kind: "ok" });
      });
    return () => controller.abort();
  }, []);

  if (state.kind === "loading") {
    return (
      <p
        className="text-sm text-muted-foreground"
        data-testid="provider-gate-loading"
      >
        Checking connected providers…
      </p>
    );
  }
  if (state.kind === "gate") {
    return (
      <div data-testid="no-provider-gate">
        <EmptyState
          icon={PlugZap}
          title="Connect a provider first"
          description="No model provider is connected yet. Creating an agent — and generating one from a description — needs a model to run against. Paste a key once (stored server-side), then come back."
          action={{
            label: "Connect a provider",
            icon: PlugZap,
            onClick: () => navigate("/providers/connect"),
          }}
        />
      </div>
    );
  }
  return <>{children}</>;
}

// Entrance — the fork. "Describe it" is visually the primary (recommended) path;
// "Configure it" is the full-control form. Matches the approved wireframe.
function Entrance({ onPick }: { onPick: (m: Mode) => void }) {
  return (
    <div className="grid gap-4 md:grid-cols-2" data-testid="create-entrance">
      <button
        type="button"
        onClick={() => onPick("describe")}
        className="rounded-xl border-2 border-primary bg-accent/40 p-6 text-left shadow-card transition-shadow hover:shadow-elevated"
        data-testid="entrance-describe"
      >
        <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground">
          <Boxes className="h-6 w-6" />
        </div>
        <p className="text-base font-semibold">Describe it</p>
        <p className="mt-1 text-sm text-muted-foreground">
          Say what it should do in a sentence. We generate a validated config
          you review before creating.{" "}
          <span className="font-medium text-primary">Recommended</span>.
        </p>
      </button>
      <button
        type="button"
        onClick={() => onPick("configure")}
        className="rounded-xl border p-6 text-left shadow-card transition-shadow hover:shadow-elevated"
        data-testid="entrance-configure"
      >
        <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-xl bg-surface-2">
          <Terminal className="h-6 w-6 text-muted-foreground" />
        </div>
        <p className="text-base font-semibold">Configure it</p>
        <p className="mt-1 text-sm text-muted-foreground">
          A guided multi-step form — full control over runtime, model, prompt,
          and tools.
        </p>
      </button>
    </div>
  );
}

// ─── Describe it ────────────────────────────────────────────────────────────

const EXAMPLE_PROMPTS = [
  "A support agent that looks up orders, answers from our docs, and can open a ticket when it can't resolve the issue.",
  "A summarizer that condenses long documents into a few bullet points.",
  "A triage bot that classifies incoming issues and routes them to the right team.",
];

function DescribeFlow({ onBack }: { onBack: () => void }) {
  const [description, setDescription] = React.useState("");
  const [model, setModel] = React.useState("");
  const [stage, setStage] = React.useState<DescribeStage>({ kind: "prompt" });

  // The connected providers drive the model dropdown: default the connected
  // provider's model; if the operator pinned platform models or multiple
  // providers exist, the user can pick. A single provider ⇒ no dropdown noise.
  const [providers, setProviders] = React.useState<ProviderSummary[]>([]);
  React.useEffect(() => {
    const controller = new AbortController();
    api
      .listProviders(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setProviders(res.providers);
      })
      .catch(() => {
        /* the gate already handled a hard failure; a soft miss just hides the dropdown */
      });
    return () => controller.abort();
  }, []);

  // Every model across the connected providers (id → provider). The dropdown
  // only renders when there's a real choice (>1 model), matching the wireframe.
  const modelChoices = React.useMemo(
    () =>
      providers.flatMap((p) =>
        p.models.map((m) => ({
          id: m,
          provider: p.provider,
          connection: p.name,
        })),
      ),
    [providers],
  );

  async function generate() {
    setStage({ kind: "generating" });
    try {
      const picked = modelChoices.find((c) => c.id === model);
      const res = await api.generateAgent({
        description: description.trim(),
        ...(model ? { model } : {}),
        ...(picked ? { provider: picked.provider } : {}),
      });
      // Branch on the FLAG, never the status code (ADR 0014 landmine): a
      // regenerate outcome keeps the raw agentYAML so nothing is lost.
      if (res.regenerate) {
        setStage({
          kind: "regenerate",
          reason:
            res.reason ?? res.error ?? "The generated config didn't validate.",
          rawYAML: res.agentYAML ?? "",
        });
        return;
      }
      setStage({ kind: "review", gen: res });
    } catch (err) {
      setStage({
        kind: "regenerate",
        reason:
          err instanceof ApiError
            ? `${err.message}${err.status ? ` (${err.status})` : ""}`
            : err instanceof Error
              ? err.message
              : "generation failed",
        rawYAML: "",
      });
    }
  }

  const generating = stage.kind === "generating";
  const ready = description.trim().length > 0;

  // The generation review advanced past → the shared review with the generated
  // tools pre-selected + the generated agentYAML as the base.
  if (stage.kind === "review") {
    return (
      <SharedReview
        baseYAML={stage.gen.agentYAML}
        initialTools={parseToolsFromYAML(stage.gen.agentYAML)}
        summary={summarizeYAML(stage.gen.agentYAML)}
        advancedYAML={stage.gen.expanded ?? stage.gen.agentYAML}
        // m21: run the agent on the SAME model the user picked to generate — the BFF
        // ensures a route for it (so any connected model works, not just the primary).
        modelPick={(() => {
          const hit = modelChoices.find((c) => c.id === model);
          return hit
            ? { connection: hit.connection, provider: hit.provider, model }
            : undefined;
        })()}
        onBack={() => setStage({ kind: "review", gen: stage.gen })}
        header={
          <GenerationReviewHeader
            gen={stage.gen}
            onRegenerate={() => setStage({ kind: "prompt" })}
          />
        }
      />
    );
  }

  return (
    <div className="space-y-4" data-testid="describe-flow">
      {stage.kind === "regenerate" ? (
        <RegenerateState
          reason={stage.reason}
          rawYAML={stage.rawYAML}
          busy={generating}
          onRegenerate={generate}
          onEdit={() => setStage({ kind: "prompt" })}
        />
      ) : (
        <div className="rounded-lg border bg-card p-6 shadow-card">
          <div className="mx-auto max-w-2xl py-4 text-center">
            <div className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground shadow-elevated">
              <Sparkles className="h-7 w-7" />
            </div>
            <h1 className="text-2xl font-semibold tracking-tight">
              Describe your agent
            </h1>
            <p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground">
              Say what it should do in a sentence. We generate a validated
              config you review before anything is created.
            </p>
            <div className="mx-auto mt-6 max-w-xl text-left">
              <Label htmlFor="agent-description" className="sr-only">
                Agent description
              </Label>
              <Textarea
                id="agent-description"
                rows={4}
                className="text-sm"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="A support agent that looks up orders, answers from our docs, and can open a ticket…"
              />
              {modelChoices.length > 1 && (
                <div className="mt-3 space-y-1.5">
                  <Label htmlFor="gen-model">Generation model</Label>
                  <Select
                    id="gen-model"
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                  >
                    <option value="">Default (connected provider)</option>
                    {modelChoices.map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.id} ({c.provider})
                      </option>
                    ))}
                  </Select>
                </div>
              )}
              <div className="mt-3 flex flex-wrap gap-2">
                {EXAMPLE_PROMPTS.map((t) => (
                  <button
                    key={t}
                    type="button"
                    onClick={() => setDescription(t)}
                    className="rounded-full border bg-surface-2/60 px-3 py-1 text-xs text-muted-foreground hover:bg-surface-2"
                  >
                    {t.split(" ").slice(0, 3).join(" ")}…
                  </button>
                ))}
              </div>
              <div className="mt-4 flex items-center justify-between">
                <Button variant="ghost" onClick={onBack} disabled={generating}>
                  Back
                </Button>
                <Button onClick={generate} disabled={!ready || generating}>
                  <Sparkles className="h-4 w-4" />
                  {generating ? "Generating…" : "Generate"}
                </Button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// GenerationReviewHeader shows the friendly "here's what we'll build" summary +
// the cost tag (generation burns the caller's key, shown not hidden — ADR 0014)
// + a Regenerate affordance that returns to the prompt.
function GenerationReviewHeader({
  gen,
  onRegenerate,
}: {
  gen: GenerateAgentResponse;
  onRegenerate: () => void;
}) {
  return (
    <div className="space-y-3" data-testid="generation-review">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2 text-success">
          <Sparkles className="h-5 w-5" />
          <p className="text-sm font-medium text-foreground">
            Here&apos;s what we&apos;ll build
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={onRegenerate}>
          <Sparkles className="h-4 w-4" /> Regenerate
        </Button>
      </div>
      {gen.model && (
        <Badge
          variant="secondary"
          className="text-[10px]"
          data-testid="gen-model-tag"
        >
          generated with {gen.model}
        </Badge>
      )}
      {gen.warnings && gen.warnings.length > 0 && (
        <div className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-xs text-muted-foreground">
          <p className="font-medium text-warning-foreground">Heads up</p>
          <ul className="mt-1 list-inside list-disc">
            {gen.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

// RegenerateState renders the 422 outcome: an honest reason + a Regenerate
// button, WITH the raw agentYAML preserved (nothing is lost). Keyed off the
// `regenerate` flag, never a status sniff.
function RegenerateState({
  reason,
  rawYAML,
  busy,
  onRegenerate,
  onEdit,
}: {
  reason: string;
  rawYAML: string;
  busy: boolean;
  onRegenerate: () => void;
  onEdit: () => void;
}) {
  return (
    <div
      className="rounded-lg border bg-card p-6 shadow-card"
      data-testid="regenerate-state"
    >
      <div className="mb-3 flex items-center gap-2 text-warning">
        <Sparkles className="h-5 w-5" />
        <p className="text-sm font-medium text-foreground">
          That generation needs another pass
        </p>
      </div>
      <p
        className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
        role="alert"
        data-testid="regenerate-reason"
      >
        {reason}
      </p>
      {rawYAML && (
        <div className="mt-4 space-y-1.5">
          <p className="text-xs font-medium text-muted-foreground">
            What was generated (kept so nothing is lost):
          </p>
          <Textarea
            aria-label="Raw generated agent.yaml"
            data-testid="regenerate-raw-yaml"
            readOnly
            className="min-h-[10rem] font-mono text-xs"
            value={rawYAML}
          />
        </div>
      )}
      <div className="mt-4 flex items-center justify-between">
        <Button variant="ghost" onClick={onEdit} disabled={busy}>
          Edit the description
        </Button>
        <Button
          onClick={onRegenerate}
          disabled={busy}
          data-testid="regenerate-button"
        >
          <Sparkles className="h-4 w-4" />
          {busy ? "Regenerating…" : "Regenerate"}
        </Button>
      </div>
    </div>
  );
}

// ─── Configure it ───────────────────────────────────────────────────────────

const STEP_BASICS = 0;
const STEP_OPTIONAL = 3;

function ConfigureFlow({
  onBack,
  initialProvider = "",
}: {
  onBack: () => void;
  initialProvider?: string;
}) {
  // Seed a MANAGED agent (the aha default — no Docker build); the user can flip
  // to a custom image in step 1. Reuses the shared ConfigForm model + validate +
  // toAgentYAML (NOT rewritten) — the tools are picked at the shared review.
  const [form, setForm] = React.useState<ConfigForm>(() => ({
    ...emptyForm(),
    runtime: "managed",
  }));
  const [errors, setErrors] = React.useState<FieldErrors>({});
  const [current, setCurrent] = React.useState(STEP_BASICS);
  const [done, setDone] = React.useState(false);

  // Compose from existing resources (m18.6): the model-route + prompt-version
  // pickers pull the real lists so the user SELECTS instead of typing a ref.
  const [routes, setRoutes] = React.useState<string[]>([]);
  const [prompts, setPrompts] = React.useState<string[]>([]);
  // m21: the intent-first model picker — a flat list of the connected providers'
  // models. Picking one sends it as `model` on create; the BFF ensures the route.
  // m22 (named connections, ADR 0026): a pick is a (connection, provider, model).
  // `connection` is the connect object's name — multiple connections may share a
  // provider type, so the pick must carry the connection, not just the type.
  const [connectedModels, setConnectedModels] = React.useState<
    { connection: string; provider: string; model: string }[]
  >([]);
  // pickedModel is the composite key `connection|model` (unique per pick).
  const [pickedModel, setPickedModel] = React.useState("");
  React.useEffect(() => {
    const c = new AbortController();
    api
      .listModelRoutes({ limit: 100 }, c.signal)
      .then((r) => {
        if (!c.signal.aborted) setRoutes(r.items.map((x) => x.name));
      })
      .catch(() => {
        /* soft miss: fall back to a free-text route */
      });
    api
      .listProviders(c.signal)
      .then((r) => {
        if (c.signal.aborted) return;
        const flat = (r.items ?? []).flatMap((p) =>
          p.models.map((m) => ({
            connection: p.name,
            provider: p.provider,
            model: m,
          })),
        );
        setConnectedModels(flat);
        // m21/m22 (provider-as-model-home): arriving from Providers "Use" pre-picks
        // the first model of that connection so the user lands ready to create.
        if (initialProvider) {
          const first = flat.find(
            (x) => x.connection === initialProvider || x.provider === initialProvider,
          );
          if (first)
            setPickedModel((prev) => prev || `${first.connection}|${first.model}`);
        }
      })
      .catch(() => {
        /* soft miss: no model picker, the Advanced route picker still works */
      });
    api
      .listPromptVersions({ limit: 100 }, c.signal)
      .then((r) => {
        if (!c.signal.aborted) setPrompts(r.items.map((x) => x.name));
      })
      .catch(() => {
        /* soft miss: the prompt picker just stays hidden */
      });
    return () => c.abort();
  }, [initialProvider]);

  function set<K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // Per-step validation mirrors config-form's rules (the review re-runs the full
  // validate before converging). Advancing past basics gates on a valid name +
  // (for custom) an image; the rest are optional-until-review.
  const basicsErrors = React.useMemo(() => {
    const all = validate(form);
    const keep: FieldErrors = {};
    if (all.name) keep.name = all.name;
    if (all.image) keep.image = all.image;
    return keep;
  }, [form]);

  function onStepChange(next: number) {
    if (current === STEP_BASICS && next > STEP_BASICS) {
      setErrors(basicsErrors);
      if (Object.keys(basicsErrors).length > 0) return;
    }
    setCurrent(next);
  }

  // The Configure-it review IS the shared review — once the user reaches the
  // last Wizard step and finishes, we hand the serialized agent.yaml to the same
  // SharedReview the Describe-it path uses (one convergence point).
  if (done) {
    return (
      <SharedReview
        baseYAML={toAgentYAML(form)}
        initialTools={form.tools}
        summary={summarizeYAML(toAgentYAML(form))}
        advancedYAML={toAgentYAML(form)}
        // m21: the picked model → the BFF ensures its route. If the user instead used
        // the Advanced "existing route" field, modelPick is empty and that route wins.
        modelPick={(() => {
          const hit = connectedModels.find(
            (c) => `${c.connection}|${c.model}` === pickedModel,
          );
          return hit
            ? {
                connection: hit.connection,
                provider: hit.provider,
                model: hit.model,
              }
            : undefined;
        })()}
        onBack={() => setDone(false)}
      />
    );
  }

  const managed = form.runtime === "managed";

  const steps: WizardStep[] = [
    {
      id: "basics",
      title: "Basics",
      description: "Name + runtime",
      content: (
        <div className="space-y-4">
          <FormField
            id="cfg-name"
            label="Name"
            error={errors.name}
            hint="DNS label, ≤ 44 chars."
          >
            <Input
              id="cfg-name"
              value={form.name}
              onChange={(e) => set("name", e.target.value)}
              placeholder="support-agent"
            />
          </FormField>
          <div className="space-y-1.5">
            <Label>Runtime</Label>
            <div className="grid grid-cols-2 gap-3">
              <button
                type="button"
                aria-pressed={managed}
                onClick={() => set("runtime", "managed")}
                className={`rounded-lg border p-3 text-left transition-colors hover:bg-surface-2 ${
                  managed ? "border-primary ring-1 ring-primary" : ""
                }`}
              >
                <p className="text-sm font-medium">Managed</p>
                <p className="text-xs text-muted-foreground">
                  Stock tool-calling loop — no Docker build
                </p>
              </button>
              <button
                type="button"
                aria-pressed={!managed}
                onClick={() => set("runtime", "custom")}
                className={`rounded-lg border p-3 text-left transition-colors hover:bg-surface-2 ${
                  !managed ? "border-primary ring-1 ring-primary" : ""
                }`}
              >
                <p className="text-sm font-medium">Custom image</p>
                <p className="text-xs text-muted-foreground">
                  Bring your own container
                </p>
              </button>
            </div>
          </div>
          {!managed && (
            <FormField id="cfg-image" label="Image" error={errors.image}>
              <Input
                id="cfg-image"
                value={form.image}
                onChange={(e) => set("image", e.target.value)}
                placeholder="ghcr.io/acme/agent:v1"
              />
            </FormField>
          )}
          {managed && form.image.trim() === "" && (
            <p className="text-xs text-muted-foreground">
              A managed agent runs the platform&apos;s stock image — no image to
              build or push.
            </p>
          )}
        </div>
      ),
    },
    {
      id: "behavior",
      title: "Behavior",
      description: "Prompt + model route",
      content: (
        <div className="space-y-4">
          {managed && (
            <FormField
              id="cfg-system-prompt"
              label="System prompt"
              hint="What the managed agent should do (its persona + instructions)."
            >
              <Textarea
                id="cfg-system-prompt"
                rows={4}
                value={form.systemPrompt}
                onChange={(e) => set("systemPrompt", e.target.value)}
                placeholder="You are a support agent for Acme…"
              />
            </FormField>
          )}
          {prompts.length > 0 && (
            <FormField
              id="cfg-prompt-ref"
              label="Prompt version"
              hint="Reference an existing PromptVersion, or leave blank to use the system prompt above."
            >
              <Select
                id="cfg-prompt-ref"
                value={form.promptVersionRef}
                onChange={(e) => set("promptVersionRef", e.target.value)}
              >
                <option value="">— none (use the system prompt) —</option>
                {prompts.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </Select>
            </FormField>
          )}
          {/* m21: the intent-first picker — choose a MODEL; the platform manages the
              route. The raw ModelRoute picker below is the Advanced fallback. */}
          {connectedModels.length > 0 && (
            <FormField
              id="cfg-model"
              label="Model"
              hint="The model this agent runs on. We create the route for you."
            >
              <Select
                id="cfg-model"
                value={pickedModel}
                onChange={(e) => setPickedModel(e.target.value)}
                data-testid="cfg-model-select"
              >
                <option value="">— pick a model —</option>
                {connectedModels.map((c) => (
                  <option
                    key={`${c.connection}|${c.model}`}
                    value={`${c.connection}|${c.model}`}
                  >
                    {c.connection} / {c.model}
                  </option>
                ))}
              </Select>
            </FormField>
          )}
          {/* m23.7b (audit B6): the raw ModelRoute picker is collapsed behind a
              disclosure. It competes with the Model picker above and is silently
              overridden by it, so it should not sit inline as a co-equal field —
              it is an advanced escape hatch for pre-built routes. */}
          <details className="rounded-md border border-border/60 bg-muted/30 px-3 py-2">
            <summary className="cursor-pointer select-none text-sm text-muted-foreground">
              Advanced: use an existing model route
            </summary>
            <div className="mt-3">
              <FormField
                id="cfg-model-route"
                label="Existing model route"
                hint="Pick a pre-built ModelRoute instead of the model above. If you also picked a model above, that model takes precedence."
              >
                {routes.length > 0 ? (
                  <Select
                    id="cfg-model-route"
                    value={form.modelRoute}
                    onChange={(e) => set("modelRoute", e.target.value)}
                  >
                    <option value="">— none (registry default) —</option>
                    {routes.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </Select>
                ) : (
                  <Input
                    id="cfg-model-route"
                    value={form.modelRoute}
                    onChange={(e) => set("modelRoute", e.target.value)}
                    placeholder="connect a provider to create a route"
                  />
                )}
              </FormField>
            </div>
          </details>
          <FormField id="cfg-execution-model" label="Execution model">
            <Select
              id="cfg-execution-model"
              value={form.executionModel}
              onChange={(e) =>
                set(
                  "executionModel",
                  e.target.value as ConfigForm["executionModel"],
                )
              }
            >
              <option value="serving">serving (request-driven)</option>
              <option value="eventing">eventing (broker-triggered)</option>
              <option value="job">job (one-shot)</option>
            </Select>
          </FormField>
          <FormField
            id="cfg-role"
            label="Role"
            hint="Within-registry A2A role (orchestrator / worker / reviewer). Optional."
          >
            <Input
              id="cfg-role"
              value={form.role}
              onChange={(e) => set("role", e.target.value)}
              placeholder="worker"
            />
          </FormField>
          <FormField
            id="cfg-allowed-callers"
            label="Allowed callers"
            hint="Comma-separated agents allowed to call this one. Optional."
          >
            <Input
              id="cfg-allowed-callers"
              value={form.allowedCallers}
              onChange={(e) => set("allowedCallers", e.target.value)}
              placeholder="orchestrator, auditor"
            />
          </FormField>
        </div>
      ),
    },
    {
      id: "resources",
      title: "Resources",
      description: "CPU/memory + scaling",
      content: (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-4">
            <FormField id="cfg-cpu" label="CPU request">
              <Input
                id="cfg-cpu"
                value={form.resourcesCpu}
                onChange={(e) => set("resourcesCpu", e.target.value)}
                placeholder="500m"
              />
            </FormField>
            <FormField id="cfg-memory" label="Memory request">
              <Input
                id="cfg-memory"
                value={form.resourcesMemory}
                onChange={(e) => set("resourcesMemory", e.target.value)}
                placeholder="256Mi"
              />
            </FormField>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <FormField
              id="cfg-min"
              label="Min replicas"
              error={errors.scalingMin}
            >
              <Input
                id="cfg-min"
                inputMode="numeric"
                value={form.scalingMin}
                onChange={(e) => set("scalingMin", e.target.value)}
                placeholder="0"
              />
            </FormField>
            <FormField
              id="cfg-max"
              label="Max replicas"
              error={errors.scalingMax}
            >
              <Input
                id="cfg-max"
                inputMode="numeric"
                value={form.scalingMax}
                onChange={(e) => set("scalingMax", e.target.value)}
                placeholder="3"
              />
            </FormField>
          </div>
        </div>
      ),
    },
    {
      id: "optional",
      title: "Optional",
      description: "Budget · eval · prompt",
      content: <OptionalStep form={form} set={set} errors={errors} />,
    },
    {
      id: "review",
      title: "Review",
      review: true,
      content: (
        <div className="space-y-4">
          <p className="text-sm font-medium">Ready to review + attach tools</p>
          <p className="text-sm text-muted-foreground">
            Finish to open the shared review — the same review + tool picker the
            &ldquo;Describe it&rdquo; path lands on. You&apos;ll pick tools,
            preview the CRD, and create there.
          </p>
          <FriendlySummary summary={summarizeYAML(toAgentYAML(form))} />
        </div>
      ),
    },
  ];

  // Forward gating: only the basics step hard-gates (name + image); later steps
  // are optional-until-review (the review re-runs the full validate).
  const canProceed =
    current === STEP_BASICS ? Object.keys(basicsErrors).length === 0 : true;

  return (
    <div
      className="rounded-lg border bg-card p-6 shadow-card"
      data-testid="configure-flow"
    >
      <Wizard
        steps={steps}
        current={current}
        onStepChange={onStepChange}
        canProceed={canProceed}
        onCancel={onBack}
        onFinish={() => {
          const found = validate(form);
          setErrors(found);
          if (Object.keys(found).length > 0) {
            // Route the user back to the first step that carries an error.
            if (found.name || found.image) setCurrent(STEP_BASICS);
            else setCurrent(STEP_OPTIONAL);
            return;
          }
          setDone(true);
        }}
        finishLabel="Review + tools"
      />
    </div>
  );
}

// OptionalStep — the budget/eval/prompt opt-in blocks, factored out of the
// original config-builder form (same field logic, reusing config-form errors).
function OptionalStep({
  form,
  set,
  errors,
}: {
  form: ConfigForm;
  set: <K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) => void;
  errors: FieldErrors;
}) {
  return (
    <div className="space-y-4">
      <ToggleSection
        id="cfg-budget"
        label="Cost budget"
        enabled={form.budgetEnabled}
        onToggle={(v) => set("budgetEnabled", v)}
      >
        <div className="grid grid-cols-2 gap-4">
          <FormField
            id="cfg-budget-conv"
            label="Per conversation (USD)"
            error={errors.budgetPerConversationUSD}
          >
            <Input
              id="cfg-budget-conv"
              value={form.budgetPerConversationUSD}
              onChange={(e) => set("budgetPerConversationUSD", e.target.value)}
              placeholder="0.50"
            />
          </FormField>
          <FormField
            id="cfg-budget-agent"
            label="Per agent (USD)"
            error={errors.budgetPerAgentUSD}
          >
            <Input
              id="cfg-budget-agent"
              value={form.budgetPerAgentUSD}
              onChange={(e) => set("budgetPerAgentUSD", e.target.value)}
              placeholder="10.00"
            />
          </FormField>
        </div>
      </ToggleSection>
      <ToggleSection
        id="cfg-prompt"
        label="Git-backed prompt"
        enabled={form.promptEnabled}
        onToggle={(v) => set("promptEnabled", v)}
      >
        <FormField
          id="cfg-prompt-name"
          label="Prompt name"
          error={errors.promptName}
        >
          <Input
            id="cfg-prompt-name"
            value={form.promptName}
            onChange={(e) => set("promptName", e.target.value)}
            placeholder="system-prompt"
          />
        </FormField>
        <FormField
          id="cfg-prompt-repo"
          label="Git repo"
          error={errors.promptRepo}
        >
          <Input
            id="cfg-prompt-repo"
            value={form.promptRepo}
            onChange={(e) => set("promptRepo", e.target.value)}
            placeholder="https://github.com/acme/prompts"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="cfg-prompt-ref" label="Ref" error={errors.promptRef}>
            <Input
              id="cfg-prompt-ref"
              value={form.promptRef}
              onChange={(e) => set("promptRef", e.target.value)}
              placeholder="main"
            />
          </FormField>
          <FormField
            id="cfg-prompt-path"
            label="Path"
            error={errors.promptPath}
          >
            <Input
              id="cfg-prompt-path"
              value={form.promptPath}
              onChange={(e) => set("promptPath", e.target.value)}
              placeholder="prompts/system.txt"
            />
          </FormField>
        </div>
      </ToggleSection>
      <ToggleSection
        id="cfg-eval"
        label="Eval gate"
        enabled={form.evalEnabled}
        onToggle={(v) => set("evalEnabled", v)}
      >
        <div className="grid grid-cols-2 gap-4">
          <FormField
            id="cfg-eval-suite"
            label="Suite name"
            error={errors.evalSuite}
          >
            <Input
              id="cfg-eval-suite"
              value={form.evalSuite}
              onChange={(e) => set("evalSuite", e.target.value)}
              placeholder="quality"
            />
          </FormField>
          <FormField
            id="cfg-eval-dataset"
            label="Dataset ref"
            error={errors.evalDataset}
          >
            <Input
              id="cfg-eval-dataset"
              value={form.evalDataset}
              onChange={(e) => set("evalDataset", e.target.value)}
              placeholder="golden-set"
            />
          </FormField>
        </div>
        <FormField
          id="cfg-eval-threshold"
          label="Threshold"
          error={errors.evalThreshold}
        >
          <Input
            id="cfg-eval-threshold"
            value={form.evalThreshold}
            onChange={(e) => set("evalThreshold", e.target.value)}
            placeholder="0.8"
          />
        </FormField>
      </ToggleSection>
    </div>
  );
}

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
      <label
        htmlFor={id}
        className="flex items-center gap-2 text-sm font-medium"
      >
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

// ─── The shared review + tool picker + Create ───────────────────────────────

function SharedReview({
  baseYAML,
  initialTools,
  summary,
  advancedYAML,
  header,
  onBack,
  modelPick,
}: {
  baseYAML: string;
  initialTools: string[];
  summary: YAMLSummary;
  advancedYAML: string;
  header?: React.ReactNode;
  onBack: () => void;
  // modelPick (m21): the (provider, model) the user picked. When set, the create
  // sends it so the BFF ensures a ModelRoute serving it and points the agent at it —
  // the user picked a MODEL, the platform manages the ROUTE. Absent → the YAML's
  // own model.route is used (the Advanced path).
  modelPick?: { connection?: string; provider: string; model: string };
}) {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { namespace, list } = useNamespace();
  // Target namespace = a populated dropdown of the caller's ACCESSIBLE namespaces
  // (M22/Theme 2), defaulting to the shell scope (or "default" when scope is
  // "all"). No more silently creating in "default" with no picker (U-namespace).
  const nsOptions =
    list.kind === "ready" ? list.namespaces.map((n) => n.name) : [];
  const [targetNs, setTargetNs] = React.useState(namespace || "default");
  // When the accessible list loads and doesn't include the current target, snap
  // to the first namespace the caller CAN use (access-scoped, not a dead default).
  React.useEffect(() => {
    if (list.kind !== "ready") return;
    const names = list.namespaces.map((n) => n.name);
    if (names.length > 0 && !names.includes(targetNs)) setTargetNs(names[0]);
  }, [list, targetNs]);
  // RBAC-aware chrome (§3, display-only): Create gates on agentdeployments.create.
  // A viewer sees no Create affordance; a forced 403 (stale "yes") → reprobe +
  // ForbiddenInline (the API is the real gate, ADR 0011).
  const { can, reprobe } = useCapabilities();
  const canCreate = can(RES_AGENTS, "create");

  const [selected, setSelected] = React.useState<string[]>(initialTools);
  const [advanced, setAdvanced] = React.useState(false);
  const [state, setState] = React.useState<CreateState>({ kind: "idle" });

  // The tool catalog (GET /api/tools) — the picker's source. A load failure
  // degrades to an empty catalog + note (the user can still create with the
  // pre-selected tools); it is never a hard dead-end.
  const [catalog, setCatalog] = React.useState<
    | { kind: "loading" }
    | { kind: "ready"; tools: CatalogTool[] }
    | { kind: "error"; message: string }
  >({ kind: "loading" });
  React.useEffect(() => {
    const controller = new AbortController();
    api
      .listTools(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setCatalog({ kind: "ready", tools: res.tools });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setCatalog({
          kind: "error",
          message:
            err instanceof Error
              ? err.message
              : "couldn't load the tool catalog",
        });
      });
    return () => controller.abort();
  }, []);

  // The final agent.yaml = the base config with the selected tools injected as
  // the `tools` field — the SAME field expand + generation use. Preview + Create
  // act on THIS (one body, one apply path). Only managed agents carry tools;
  // for a custom agent the picker isn't shown and selected stays empty.
  const finalYAML = React.useMemo(
    () => withTools(baseYAML, selected),
    [baseYAML, selected],
  );

  async function onPreview() {
    setState({ kind: "previewing" });
    try {
      const manifest = await api.expand(finalYAML);
      setState({ kind: "preview", manifest });
    } catch (err) {
      setState(errorState(err));
    }
  }

  async function onCreate() {
    setState({ kind: "creating" });
    try {
      const res = await api.createAgent(
        finalYAML,
        targetNs,
        modelPick && modelPick.provider && modelPick.model
          ? modelPick
          : undefined,
      );
      // Transactional (m18.7): the POST succeeded, so the agent EXISTS. Default a
      // missing/odd `created` to [] so the success screen + navigation can never
      // crash into an error that hides a real create (the orphaned-objects bug).
      const created = res.created ?? [];
      setState({ kind: "created", created });
      const agent =
        created.find((o) => o.kind === "AgentDeployment") ?? created[0];
      toast({
        variant: "success",
        title: "Agent created",
        description: agent ? `${agent.name} in ${agent.namespace}` : undefined,
      });
    } catch (err) {
      // A 403 despite a "yes" cap means the cached map was stale — correct it.
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setState(errorState(err));
    }
  }

  // On create → the agent LANDING page (m14.11): the detail/status/logs/run
  // surface where the user watches it come alive and runs it (the aha loop). We
  // navigate to /agents/{ns}/{name} using the created AgentDeployment's identity;
  // if — defensively — no AgentDeployment came back, fall back to the list.
  React.useEffect(() => {
    if (state.kind !== "created") return;
    const agent =
      state.created.find((o) => o.kind === "AgentDeployment") ??
      state.created[0];
    const t = setTimeout(() => {
      if (agent) {
        navigate(
          `/agents/${encodeURIComponent(agent.namespace)}/${encodeURIComponent(agent.name)}`,
        );
      } else {
        navigate("/agents");
      }
    }, 1200);
    return () => clearTimeout(t);
  }, [state, navigate]);

  if (state.kind === "created") {
    const agent =
      state.created.find((o) => o.kind === "AgentDeployment") ??
      state.created[0];
    return (
      <div
        className="rounded-lg border bg-card p-6 shadow-card"
        data-testid="create-success"
      >
        <div className="flex items-center gap-2 text-success">
          <CheckCircle2 className="h-5 w-5" />
          <p className="text-sm font-medium text-foreground">
            {agent ? `${agent.name} created` : "Agent created"} — opening its
            page…
          </p>
        </div>
        <div className="mt-3 grid gap-2">
          {state.created.map((obj) => (
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
      </div>
    );
  }

  const busy = state.kind === "previewing" || state.kind === "creating";
  const isManaged = /(^|\n)runtime:\s*managed(\s|$)/.test(baseYAML);

  return (
    <div className="space-y-4" data-testid="shared-review">
      {header && (
        <div className="rounded-lg border bg-card p-5 shadow-card">
          {header}
        </div>
      )}

      {/* Friendly summary — the primary review surface (raw behind Advanced). */}
      <div className="rounded-lg border bg-card p-5 shadow-card">
        <p className="mb-3 text-sm font-medium">Review before creating</p>
        <FriendlySummary summary={{ ...summary, tools: selected }} />
        <button
          type="button"
          onClick={() => setAdvanced((v) => !v)}
          className="mt-4 flex w-full items-center gap-2 text-sm font-medium text-muted-foreground hover:text-foreground"
          aria-expanded={advanced}
          data-testid="advanced-toggle"
        >
          <ChevronRight
            className={`h-4 w-4 transition-transform ${advanced ? "rotate-90" : ""}`}
          />
          Advanced — view the generated agent.yaml / CRDs
        </button>
        {advanced && (
          <Textarea
            aria-label="Advanced — generated manifest"
            data-testid="advanced-yaml"
            readOnly
            className="mt-3 min-h-[14rem] font-mono text-xs"
            value={state.kind === "preview" ? state.manifest : advancedYAML}
          />
        )}
      </div>

      {/* Tool picker — managed agents only (a custom image brings its own). */}
      {isManaged && (
        <div className="rounded-lg border bg-card p-5 shadow-card">
          <div className="mb-3">
            <p className="text-sm font-semibold">Attach tools</p>
            <p className="text-xs text-muted-foreground">
              Choose the MCP tools this agent can call. Expand a server to pick
              individual tools.
            </p>
          </div>
          <ToolPicker
            catalog={catalog}
            selected={selected}
            onToggle={(name) =>
              setSelected((s) =>
                s.includes(name) ? s.filter((t) => t !== name) : [...s, name],
              )
            }
            onSelectMany={(names, on) =>
              setSelected((s) => {
                if (on) return [...new Set([...s, ...names])];
                const drop = new Set(names);
                return s.filter((t) => !drop.has(t));
              })
            }
          />
        </div>
      )}

      {/* Preview / Create. */}
      <div className="rounded-lg border bg-surface-2/40 p-4">
        {state.kind === "error" && state.forbidden ? (
          <ForbiddenInline
            title="Not allowed to create this agent"
            description="Your account can preview the manifest but can't create AgentDeployments in this cluster."
            detail={state.message}
          />
        ) : (
          state.kind === "error" && (
            <p
              className="mb-3 text-sm text-destructive"
              role="alert"
              data-testid="create-error"
            >
              {state.message}
              {state.status ? ` (${state.status})` : ""}
            </p>
          )
        )}
        {canCreate && nsOptions.length > 0 && (
          <FormField id="create-namespace" label="Namespace">
            <Select
              id="create-namespace"
              value={nsOptions.includes(targetNs) ? targetNs : ""}
              onChange={(e) => setTargetNs(e.target.value)}
              data-testid="create-namespace-select"
            >
              {nsOptions.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
          </FormField>
        )}
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <Button variant="ghost" onClick={onBack} disabled={busy}>
              Back
            </Button>
            <Button variant="outline" onClick={onPreview} disabled={busy}>
              {state.kind === "previewing" ? "Expanding…" : "Preview CRD"}
            </Button>
          </div>
          {canCreate ? (
            <Button
              onClick={onCreate}
              disabled={busy}
              data-testid="create-button"
            >
              {state.kind === "creating" ? (
                <>Creating…</>
              ) : (
                <>
                  <Rocket className="h-4 w-4" /> Create agent
                </>
              )}
            </Button>
          ) : (
            <p
              className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-center text-xs text-muted-foreground"
              data-testid="create-readonly-note"
            >
              You have read-only access — creating an agent requires create
              permission on AgentDeployments.
            </p>
          )}
        </div>
      </div>
    </div>
  );
}

// ToolPicker — the merged catalog (curated + user-added) with source + schema +
// pending badges. Pre-selected tools (from generation) start checked; the user
// adjusts. A pending tool is bindable-if-approved — it's still checkable (the
// selection flows into the agent's tools; the operator's approval gates binding
// server-side), it just carries the honest badge.
function ToolPicker({
  catalog,
  selected,
  onToggle,
  onSelectMany,
}: {
  catalog:
    | { kind: "loading" }
    | { kind: "ready"; tools: CatalogTool[] }
    | { kind: "error"; message: string };
  selected: string[];
  onToggle: (name: string) => void;
  // onSelectMany selects/clears a whole server's tools at once (m25 S12 "add all").
  onSelectMany: (names: string[], on: boolean) => void;
}) {
  const [query, setQuery] = React.useState("");
  // Which MCP servers are expanded to reveal their tools (m25 S13). Collapsed by
  // default so the list starts as just the servers.
  const [expanded, setExpanded] = React.useState<Set<string>>(() => new Set());
  // Which tools have their inputSchema revealed (m25 S17 — clicking "schema").
  const [schemaOpen, setSchemaOpen] = React.useState<Set<string>>(() => new Set());

  if (catalog.kind === "loading") {
    return (
      <p className="text-sm text-muted-foreground" data-testid="tools-loading">
        Loading the tool catalog…
      </p>
    );
  }

  // On a catalog load error we still show the pre-selected tools (so a create
  // isn't blocked) plus an honest note — never a silent empty picker.
  const tools =
    catalog.kind === "ready"
      ? catalog.tools
      : selected.map((name) => ({ name }) as CatalogTool);

  const filtered = tools.filter((t) =>
    t.name.toLowerCase().includes(query.trim().toLowerCase()),
  );
  const serverGroups = groupToolsByServer(filtered);

  return (
    <div className="space-y-3">
      {catalog.kind === "error" && (
        <p
          className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-xs text-muted-foreground"
          data-testid="tools-error"
        >
          Couldn&apos;t load the full catalog ({catalog.message}). Your
          pre-selected tools are shown; you can still create the agent.
        </p>
      )}
      <div className="flex items-center gap-2">
        <div className="relative flex-1">
          <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            aria-label="Search tools"
            placeholder="Search tools…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="pl-9"
          />
        </div>
        {/* Don't see your server? Add one without leaving the flow — opens the add-MCP
            wizard in a new tab; re-open the picker to see the new server's tools. */}
        <a
          href="/tools/add-mcp"
          target="_blank"
          rel="noreferrer"
          data-testid="add-mcp-from-picker"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          <Plus className="h-4 w-4" />
          Add MCP server
        </a>
      </div>
      {/* At-a-glance selection summary — count + a clear affordance. */}
      <div className="flex items-center justify-between px-0.5 text-xs text-muted-foreground">
        <span>
          <span className="font-semibold text-foreground">{selected.length}</span>{" "}
          selected{" · "}
          {tools.length} tool{tools.length === 1 ? "" : "s"} across{" "}
          {serverGroups.length} server{serverGroups.length === 1 ? "" : "s"}
        </span>
        {selected.length > 0 && (
          <button
            type="button"
            className="font-medium underline-offset-2 hover:text-foreground hover:underline"
            onClick={() => onSelectMany(selected, false)}
            data-testid="tool-clear-selected"
          >
            Clear selection
          </button>
        )}
      </div>

      {/* MCP servers, collapsed by default; expand to pick individual tools. The server
          checkbox selects/clears the whole server (indeterminate when partial, m25 S12/S13). */}
      <div className="space-y-2" data-testid="tool-picker-list">
        {filtered.length === 0 && (
          <p className="rounded-lg border border-dashed py-8 text-center text-sm text-muted-foreground">
            No tools match{query.trim() ? ` “${query.trim()}”` : ""}.
          </p>
        )}
        {serverGroups.map(([server, groupTools]) => {
          const names = groupTools.map((t) => t.name);
          const selectedCount = names.filter((n) => selected.includes(n)).length;
          const allOn = selectedCount === names.length;
          const someOn = selectedCount > 0 && !allOn;
          const isOpen = expanded.has(server);
          return (
            <div
              key={server}
              className="overflow-hidden rounded-lg border bg-card"
            >
              {/* Server header: [select-all checkbox] [expand toggle] [selected + count]. */}
              <div className="flex items-center gap-3 px-3 py-2.5">
                <input
                  type="checkbox"
                  checked={allOn}
                  ref={(el) => {
                    if (el) el.indeterminate = someOn;
                  }}
                  onChange={() => onSelectMany(names, !allOn)}
                  className="h-4 w-4 rounded border-input accent-primary"
                  aria-label={`Select all tools from ${server}`}
                  data-testid={`tool-server-checkbox-${server}`}
                />
                <button
                  type="button"
                  className="flex min-w-0 flex-1 items-center gap-2 text-left"
                  onClick={() =>
                    setExpanded((s) => {
                      const next = new Set(s);
                      if (next.has(server)) next.delete(server);
                      else next.add(server);
                      return next;
                    })
                  }
                  aria-expanded={isOpen}
                  data-testid={`tool-server-toggle-${server}`}
                >
                  <ChevronRight
                    className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${isOpen ? "rotate-90" : ""}`}
                  />
                  <Server className="h-4 w-4 shrink-0 text-primary" />
                  <span className="min-w-0 truncate text-sm font-medium">
                    {server}
                  </span>
                </button>
                <div className="flex shrink-0 items-center gap-2">
                  {selectedCount > 0 && (
                    <Badge variant="success" className="text-[10px]">
                      {selectedCount} selected
                    </Badge>
                  )}
                  <span className="text-xs tabular-nums text-muted-foreground">
                    {groupTools.length} tool{groupTools.length === 1 ? "" : "s"}
                  </span>
                </div>
              </div>
              {isOpen && (
                <div className="divide-y border-t">
                  {groupTools.map((t) => {
                    const on = selected.includes(t.name);
                    const pending = t.approvalStatus === "pending";
                    const hasSchema = t.inputSchema != null;
                    const schemaShown = schemaOpen.has(t.name);
                    return (
                      <div key={t.name} className="bg-surface-2/30">
                        <label className="flex cursor-pointer items-start gap-3 py-2.5 pl-10 pr-3 transition-colors hover:bg-accent/40">
                          <input
                            type="checkbox"
                            checked={on}
                            onChange={() => onToggle(t.name)}
                            className="mt-0.5 h-4 w-4 shrink-0 rounded border-input accent-primary"
                            aria-label={`Bind ${t.name}`}
                          />
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <Wrench className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                              <span className="truncate font-mono text-sm font-medium">
                                {t.name}
                              </span>
                              {pending && (
                                <Badge variant="warning" className="shrink-0 text-[10px]">
                                  pending approval
                                </Badge>
                              )}
                            </div>
                            {t.description && (
                              <p className="mt-0.5 truncate text-xs text-muted-foreground">
                                {t.description}
                              </p>
                            )}
                          </div>
                          {hasSchema && (
                            <button
                              type="button"
                              // Inside the checkbox label — stop the click from toggling
                              // the checkbox; it only reveals the schema (m25 S17).
                              onClick={(e) => {
                                e.preventDefault();
                                e.stopPropagation();
                                setSchemaOpen((s) => {
                                  const next = new Set(s);
                                  if (next.has(t.name)) next.delete(t.name);
                                  else next.add(t.name);
                                  return next;
                                });
                              }}
                              data-testid={`tool-schema-toggle-${t.name}`}
                              className="shrink-0 self-center"
                            >
                              <Badge
                                variant="outline"
                                className="cursor-pointer text-[10px] hover:bg-muted"
                              >
                                {schemaShown ? "hide schema" : "schema"}
                              </Badge>
                            </button>
                          )}
                        </label>
                        {schemaShown && hasSchema && (
                          <pre className="mb-3 ml-10 mr-3 max-h-64 overflow-auto rounded-md border bg-muted/40 p-3 text-xs">
                            {JSON.stringify(t.inputSchema, null, 2)}
                          </pre>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

// FriendlySummary renders the config as a readable key/value list — the primary
// review (raw YAML stays behind Advanced, no hand-editing as the main path).
interface YAMLSummary {
  name: string;
  runtime: string;
  image?: string;
  model?: string;
  systemPrompt?: string;
  tools: string[];
}

function FriendlySummary({ summary }: { summary: YAMLSummary }) {
  const rows: { k: string; v: React.ReactNode }[] = [
    {
      k: "Name",
      v: summary.name || <span className="text-muted-foreground">—</span>,
    },
    {
      k: "Runtime",
      v: (
        <Badge variant="secondary" className="text-[10px]">
          {summary.runtime}
        </Badge>
      ),
    },
  ];
  if (summary.image)
    rows.push({
      k: "Image",
      v: <span className="font-mono text-xs">{summary.image}</span>,
    });
  if (summary.model)
    rows.push({
      k: "Model route",
      v: <span className="font-mono text-xs">{summary.model}</span>,
    });
  if (summary.systemPrompt)
    rows.push({
      k: "System prompt",
      v: (
        <span className="text-muted-foreground">
          &ldquo;{truncate(summary.systemPrompt, 80)}&rdquo;
        </span>
      ),
    });
  rows.push({
    k: "Tools",
    v:
      summary.tools.length > 0 ? (
        <span data-testid="summary-tools">{summary.tools.join(", ")}</span>
      ) : (
        <span className="text-muted-foreground">none</span>
      ),
  });

  return (
    <dl className="divide-y rounded-md border" data-testid="friendly-summary">
      {rows.map((r) => (
        <div
          key={r.k}
          className="flex items-start justify-between gap-4 px-3 py-2 text-sm"
        >
          <dt className="text-muted-foreground">{r.k}</dt>
          <dd className="text-right">{r.v}</dd>
        </div>
      ))}
    </dl>
  );
}

// ─── agent.yaml helpers (a tiny, tolerant reader of our OWN serializer) ──────
// These parse only the fields our serializer emits (a full YAML lib is overkill
// for output we control) — the friendly summary + the tool round-trip. The BFF
// `expand` is the real validator; these are display + a targeted tools rewrite.

function scalar(yaml: string, key: string): string | undefined {
  const m = new RegExp(`(?:^|\\n)${key}:[ \\t]*([^\\n]*)`).exec(yaml);
  if (!m) return undefined;
  const v = m[1].trim().replace(/^"(.*)"$/, "$1");
  return v || undefined;
}

// parseToolsFromYAML reads a top-level `tools:` list (the generation's proposed
// tools) so the picker pre-selects them. Tolerant of `- name` and `- "name"`.
function parseToolsFromYAML(yaml: string): string[] {
  const lines = yaml.split("\n");
  const start = lines.findIndex((l) => /^tools:\s*$/.test(l));
  if (start === -1) return [];
  const out: string[] = [];
  for (let i = start + 1; i < lines.length; i++) {
    const m = /^\s*-\s*(.+?)\s*$/.exec(lines[i]);
    if (!m) break;
    out.push(m[1].replace(/^"(.*)"$/, "$1").trim());
  }
  return out.filter(Boolean);
}

// withTools rewrites the agent.yaml's `tools:` block to exactly `selected`
// (dropping the block when empty). It strips any existing top-level tools block
// first, then appends the new one — so the picker's selection is authoritative
// and Preview/Create act on the SAME `tools` field expand consumes.
function withTools(yaml: string, selected: string[]): string {
  const lines = yaml.replace(/\n+$/, "").split("\n");
  const kept: string[] = [];
  let i = 0;
  while (i < lines.length) {
    if (/^tools:\s*$/.test(lines[i])) {
      i++;
      while (i < lines.length && /^\s*-\s+/.test(lines[i])) i++;
      continue;
    }
    kept.push(lines[i]);
    i++;
  }
  if (selected.length > 0) {
    kept.push("tools:");
    for (const t of selected) kept.push(`  - ${yamlName(t)}`);
  }
  return kept.join("\n") + "\n";
}

// yamlName quotes a tool name only when it would otherwise be ambiguous — the
// same discipline config-form uses for scalars.
function yamlName(v: string): string {
  if (/^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$/.test(v)) return v;
  return JSON.stringify(v);
}

function summarizeYAML(yaml: string): YAMLSummary {
  const runtime = scalar(yaml, "runtime") ?? "custom";
  // systemPrompt may be a block scalar (`systemPrompt: |`) — grab its first line
  // as a preview.
  let systemPrompt = scalar(yaml, "systemPrompt");
  if (systemPrompt === "|" || systemPrompt === undefined) {
    const m = /systemPrompt:\s*\|\s*\n\s+([^\n]*)/.exec(yaml);
    systemPrompt = m ? m[1].trim() : systemPrompt === "|" ? "" : undefined;
  }
  // model may be `model: <id>` (generation) or `model:\n  route: <alias>` (form).
  const model = scalar(yaml, "model") ?? scalar(yaml, "route");
  return {
    name: scalar(yaml, "name") ?? "",
    runtime,
    image: scalar(yaml, "image"),
    model,
    systemPrompt,
    tools: parseToolsFromYAML(yaml),
  };
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

// errorState maps a thrown error to the CreateState error, flagging a 403 so the
// surface renders ForbiddenInline (the explain-and-suggest primitive).
function errorState(err: unknown): CreateState {
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
