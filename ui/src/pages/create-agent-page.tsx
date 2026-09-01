import * as React from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ChevronRight, PlugZap, Plus, Search, Send, Server } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  ClosingNote,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  SkeletonText,
  Wizard,
  useToast,
  type KeyValueItem,
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
  type CheckRequirementsResponse,
  type ProviderSummary,
  type RecipeSummary,
  type CreatedObject,
  type GenerateAgentResponse,
  type RefineAgentResponse,
  type RefineTurn,
} from "@/lib/api";
import {
  emptyForm,
  toAgentYAML,
  validate,
  type ConfigForm,
  type FieldErrors,
} from "@/lib/config-form";
import { groupToolsByServer } from "@/lib/tool-groups";

// CreateAgentPage — the create-agent wizard, the HEART of the aha (ADR 0013 /
// 0014; redesigned onto the editorial system in M151, spec §6.1 archetype A4 and
// the `create-agent-page` row of §6.2). TWO entrances converge on ONE review:
//
//   • "Describe it" — a sentence → POST /api/agents/generate → the proposed
//     config, which the user can refine by chat, create as a draft and test,
//     or take straight to the shared review. A 422 (keyed off the `regenerate`
//     FLAG, NOT the status) renders INLINE in the step with the description
//     still on screen and the raw attempt preserved — nothing is lost, and the
//     user edits rather than retypes. Generation NEVER auto-applies.
//   • "Configure it" — the M12 config-builder form split into kit Wizard steps
//     (basics → behaviour → resources → optional → review), reusing
//     lib/config-form's validate + toAgentYAML (NOT rewritten), converging on
//     the SAME review.
//
// ── A GENERATED CONFIG IS A PROPOSAL, NOT A FACT ────────────────────────────
//
// Everything a model wrote and a person has not confirmed is marked as such,
// consistently, by `ProposalMark`: the `open` tag (declared, never exercised),
// the composing model named in mono, and one sentence saying the agent does not
// exist yet. A config the USER typed carries no proposal tag — they wrote it —
// but the same "nothing exists yet" sentence, because that is a fact about the
// cluster either way. Recipes sit in between and say so.
//
// ── A REQUIREMENT THAT WASN'T CHECKED IS NOT A SATISFIED ONE ────────────────
//
// `check-requirements` is advisory (m72.3, ADR 0066 D3) and it can be absent
// entirely — an older server 501s it, a probe can fail. The checklist therefore
// has FIVE states, not two: ready / needs approval / needs consent / not
// registered / **not checked**. The last one is what the surface falls back to
// when the pre-flight didn't answer: the requirements are still listed (we know
// them from the config), each honestly marked unchecked, with one QuietNote
// saying why. A green tick the platform never earned is the one thing this
// surface must not draw (§7.1).
//
// The checklist never blocks Create — the cluster is the real gate.
//
// ── RBAC-aware (display-only, ADR 0011) ─────────────────────────────────────
// Create gates on agentdeployments.create; a forced 403 renders the calm
// permission boundary. A caller with NO connected provider is steered to
// connect one first — generation needs a provider, and running any agent needs
// a model.

// The mode is the top-level flow selection: the entrance fork, then one of the
// entrances. `configure`, `describe` and `recipe` all end at the shared review.
type Mode = "entrance" | "describe" | "configure" | "recipe";

// The Describe-it sub-state. `prompt` is the form; `review` hands off to the
// refine/draft surface. `issue` is the 422 outcome, rendered INLINE in the
// prompt step with the raw attempt preserved.
type DescribeStage =
  | { kind: "prompt" }
  | { kind: "generating" }
  | { kind: "review"; gen: GenerateAgentResponse };

interface GenIssue {
  reason: string;
  rawYAML: string;
}

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

type DraftState =
  | { kind: "none" }
  | { kind: "creating" }
  | { kind: "created"; ns: string; name: string; resourceVersion?: string; isDraft: boolean }
  | { kind: "applying" }
  | { kind: "publishing" }
  | { kind: "published" }
  | { kind: "conflict" }
  | { kind: "error"; message: string };

/** Where a config on the review came from — it decides how strongly it is claimed. */
type Origin = "generated" | "recipe" | "form";

export function CreateAgentPage() {
  const [params] = useSearchParams();
  // m21: arriving from Providers ("Use") carries ?provider= (provider-as-model-home):
  // jump straight to Configure with that provider's model pre-picked.
  const initialProvider = params.get("provider") ?? "";
  // m74 P1-2: arriving from the template gallery "Install" CTA carries ?recipe=<name>.
  // We fetch the recipe list and pre-fill the recipe flow — the same path as the
  // in-page recipe picker (m72.5). Never read ?spec= (a full YAML in a query string
  // is fragile and may be truncated by URL-length limits).
  const initialRecipe = params.get("recipe") ?? "";
  const [mode, setMode] = React.useState<Mode>(
    initialProvider ? "configure" : "entrance",
  );
  // recipeSpec: the spec from a recipe the user clicked (m72.5). When set, mode="recipe"
  // and SharedReview renders with this spec pre-filled.
  const [recipeSpec, setRecipeSpec] = React.useState<string | null>(null);

  function onPickRecipe(spec: string) {
    setRecipeSpec(spec);
    setMode("recipe");
  }

  // Bootstrap the recipe flow when arriving via ?recipe=<name> from the gallery.
  React.useEffect(() => {
    if (!initialRecipe) return;
    api
      .listRecipes()
      .then((r) => {
        const found = (r.recipes ?? []).find((rec) => rec.name === initialRecipe);
        if (found) {
          setRecipeSpec(found.spec);
          setMode("recipe");
        }
        // If not found, fall through to the entrance (the recipe may have been removed).
      })
      .catch(() => {
        /* soft miss: fall through to the entrance */
      });
  }, [initialRecipe]);

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="New agent"
        lede="Describe it in a sentence and we compose a validated config — or set every knob yourself. Both paths end at the same review, where you see the whole thing and pick its tools before anything is created."
      />
      {/* The A4 content measure, on the header's own left margin (the band's
          px-6) so the h1 and every step heading share one edge. Cancel lives in
          the wizard footer, so the header carries no side actions. */}
      <div className="min-w-0 px-6">
        <div className="min-w-0 max-w-[46rem]">
        <ProviderGate>
          {mode === "entrance" && (
            <Entrance onPick={setMode} onPickRecipe={onPickRecipe} />
          )}
          {mode === "describe" && (
            <DescribeFlow onBack={() => setMode("entrance")} />
          )}
          {mode === "configure" && (
            <ConfigureFlow
              onBack={() => setMode("entrance")}
              initialProvider={initialProvider}
            />
          )}
          {mode === "recipe" && recipeSpec && (
            <RecipeReview
              spec={recipeSpec}
              onBack={() => setMode("entrance")}
            />
          )}
        </ProviderGate>
        </div>
      </div>
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
    // §7 A4: the panel shows shaped skeleton text, never a spinner or a
    // "Loading…" string that the layout then jumps away from.
    return (
      <div data-testid="provider-gate-loading">
        <SkeletonText lines={3} />
      </div>
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

// ── The entrance fork ───────────────────────────────────────────────────────
//
// Two ways in, said in the user's terms, plus the recipe shelf. The recommended
// card is the only pine-bordered element on the surface, because pine means
// "you can act here" and both cards are actions — the border and the tint carry
// the recommendation, not a second colour.
function Entrance({
  onPick,
  onPickRecipe,
}: {
  onPick: (m: Mode) => void;
  onPickRecipe: (spec: string) => void;
}) {
  const [showRecipes, setShowRecipes] = React.useState(false);
  const [recipes, setRecipes] = React.useState<RecipeSummary[]>([]);
  const [recipesLoading, setRecipesLoading] = React.useState(false);

  function handleShowRecipes() {
    setShowRecipes(true);
    if (recipes.length === 0) {
      setRecipesLoading(true);
      api
        .listRecipes()
        .then((r) => {
          setRecipes(r.recipes ?? []);
          setRecipesLoading(false);
        })
        .catch(() => {
          setRecipesLoading(false);
        });
    }
  }

  const cardBase =
    "rounded-lg border p-5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2";

  return (
    <div className="min-w-0 space-y-6" data-testid="create-entrance">
      <SectionHeader
        title="How do you want to start?"
        lede="Both paths end at the same review — the whole config, in plain words, with its tools — before anything is created."
      />
      <div className="grid gap-4 md:grid-cols-2">
        <button
          type="button"
          onClick={() => onPick("describe")}
          className={`${cardBase} border-primary bg-accent hover:bg-brand-2/10`}
          data-testid="entrance-describe"
        >
          <p className="font-serif text-lg font-medium">Describe it</p>
          <p className="mt-1.5 text-sm text-secondary-foreground">
            Say what it should do in a sentence. We compose a validated config
            you read before creating.
          </p>
          <p className="mt-2 text-xs font-semibold text-primary">Recommended</p>
        </button>
        <button
          type="button"
          onClick={() => onPick("configure")}
          className={`${cardBase} border-border bg-card hover:bg-surface-2`}
          data-testid="entrance-configure"
        >
          <p className="font-serif text-lg font-medium">Configure it</p>
          <p className="mt-1.5 text-sm text-secondary-foreground">
            A guided form — full control over runtime, model, prompt and tools.
          </p>
          <p className="mt-2 text-xs text-faint">Five steps</p>
        </button>
      </div>

      {/* The recipe shelf (m72.5) — a third way in, deliberately quieter. */}
      {!showRecipes ? (
        <button
          type="button"
          onClick={handleShowRecipes}
          className="flex w-full items-center gap-2 rounded-lg border border-dashed border-border-strong p-4 text-sm text-muted-foreground transition-colors hover:border-faint hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          data-testid="entrance-recipes-toggle"
        >
          Start from a recipe
        </button>
      ) : (
        <section aria-labelledby="recipes-head" data-testid="recipe-gallery">
          <SectionHeader
            as="h3"
            id="recipes-head"
            title="Recipes"
            lede="Pre-written configs. Picking one opens the same review, pre-filled — it is a starting point, not a decision."
          />
          {recipesLoading ? (
            <SkeletonText lines={2} />
          ) : recipes.length === 0 ? (
            <p className="text-sm text-faint">
              No recipes are published on this install.
            </p>
          ) : (
            <div className="grid gap-3 sm:grid-cols-2">
              {recipes.map((r) => (
                <button
                  key={r.name}
                  type="button"
                  onClick={() => onPickRecipe(r.spec)}
                  className="rounded-lg border border-border bg-card p-4 text-left transition-colors hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
                  data-testid={`recipe-card-${r.name}`}
                >
                  {r.icon && (
                    <span className="mb-2 block text-lg" role="img" aria-label={r.title}>
                      {r.icon}
                    </span>
                  )}
                  <p className="font-serif text-md font-medium">{r.title}</p>
                  <p className="mt-0.5 line-clamp-2 text-xs text-faint">
                    {r.description}
                  </p>
                </button>
              ))}
            </div>
          )}
        </section>
      )}
    </div>
  );
}

// ── Describe it ─────────────────────────────────────────────────────────────

const EXAMPLE_PROMPTS = [
  "A support agent that looks up orders, answers from our docs, and can open a ticket when it can't resolve the issue.",
  "A summarizer that condenses long documents into a few bullet points.",
  "A triage bot that classifies incoming issues and routes them to the right team.",
];

function DescribeFlow({ onBack }: { onBack: () => void }) {
  const [description, setDescription] = React.useState("");
  const [model, setModel] = React.useState("");
  const [stage, setStage] = React.useState<DescribeStage>({ kind: "prompt" });
  // The 422 outcome lives BESIDE the form, not instead of it: the description
  // that produced it stays on screen and editable, and the raw attempt is kept.
  const [issue, setIssue] = React.useState<GenIssue | null>(null);

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
    setIssue(null);
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
        setStage({ kind: "prompt" });
        setIssue({
          reason:
            res.reason ?? res.error ?? "The generated config didn't validate.",
          rawYAML: res.agentYAML ?? "",
        });
        return;
      }
      setStage({ kind: "review", gen: res });
    } catch (err) {
      setStage({ kind: "prompt" });
      setIssue({
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

  // The generation review advanced past → the refine + draft surface (m71.4/m71.5).
  if (stage.kind === "review") {
    return (
      <RefineAndDraftSurface
        gen={stage.gen}
        model={model}
        modelChoices={modelChoices}
        onBack={() => setStage({ kind: "prompt" })}
        onRegenerate={() => setStage({ kind: "prompt" })}
      />
    );
  }

  return (
    <div className="min-w-0 space-y-6" data-testid="describe-flow">
      <SectionHeader
        title="Describe your agent"
        lede="One sentence about what it should do. We compose a validated config from it — nothing is created until you have read the whole thing."
      />

      <div className="space-y-1.5">
        <Label htmlFor="agent-description">Agent description</Label>
        <Textarea
          id="agent-description"
          rows={4}
          value={description}
          onChange={(e) => {
            setDescription(e.target.value);
            setIssue(null);
          }}
          placeholder="A support agent that looks up orders, answers from our docs, and can open a ticket…"
        />
        <div className="flex flex-wrap gap-2 pt-1">
          {EXAMPLE_PROMPTS.map((t) => (
            <button
              key={t}
              type="button"
              onClick={() => {
                setDescription(t);
                setIssue(null);
              }}
              className="rounded-md border border-border-strong bg-card px-2.5 py-1 text-xs text-secondary-foreground transition-colors hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
            >
              {t.split(" ").slice(0, 3).join(" ")}…
            </button>
          ))}
        </div>
      </div>

      {modelChoices.length > 1 && (
        <div className="space-y-1.5">
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
          <p className="text-xs text-faint">
            The model that WRITES the config. It is not the model the agent runs
            on — you pick that at the review.
          </p>
        </div>
      )}

      {issue && <GenerationIssue issue={issue} />}

      <div className="flex items-center justify-between border-t border-border pt-5">
        <Button variant="ghost" onClick={onBack} disabled={generating}>
          Back
        </Button>
        <Button
          onClick={generate}
          disabled={!ready || generating}
          data-testid={issue ? "regenerate-button" : undefined}
        >
          {generating
            ? "Working…"
            : issue
              ? "Regenerate"
              : "Generate"}
        </Button>
      </div>
    </div>
  );
}

// GenerationIssue — the 422 outcome, inline in the step that caused it (§7, A4).
// Nothing is discarded: the reason is stated in words, the raw attempt is kept
// in a read-only code well, and the description that produced them is still in
// the field above. Keyed off the `regenerate` flag, never a status sniff.
function GenerationIssue({ issue }: { issue: GenIssue }) {
  return (
    <div
      className="border border-border border-l-2 border-l-destructive bg-card px-4 py-3"
      data-testid="regenerate-state"
    >
      <p className="font-serif text-md font-medium">
        That generation needs another pass.
      </p>
      <p
        className="mt-1 text-sm text-secondary-foreground"
        role="alert"
        data-testid="regenerate-reason"
      >
        {issue.reason}
      </p>
      <p className="mt-2 text-xs text-faint">
        Nothing was created. Sharpen the description above — naming the tools or
        the tone usually fixes it — then generate again.
      </p>
      {issue.rawYAML && (
        <div className="mt-3 min-w-0">
          <p className="mb-1.5 font-mono text-2xs uppercase tracking-wide text-faint">
            What was produced, kept so nothing is lost
          </p>
          <pre
            data-testid="regenerate-raw-yaml"
            className="max-h-64 min-w-0 overflow-auto rounded-md bg-surface-3 p-3 font-mono text-xs leading-relaxed text-secondary-foreground"
          >
            {issue.rawYAML}
          </pre>
        </div>
      )}
    </div>
  );
}

// ProposalMark — the one place the console says "a model wrote this and nobody
// has confirmed it". Every surface that shows generated config renders it, so
// the claim cannot drift between them (§7.1: never present a proposal as a fact).
function ProposalMark({
  origin,
  model,
  modelTestId,
}: {
  origin: Origin;
  model?: string;
  /** testid for the model attribution — the "which model wrote this" contract. */
  modelTestId?: string;
}) {
  const tag =
    origin === "generated" ? "proposed" : origin === "recipe" ? "from a recipe" : null;
  return (
    <div className="mb-4 flex flex-wrap items-center gap-2">
      {tag && <Badge variant="open">{tag}</Badge>}
      <span className="text-xs text-faint">
        {origin === "generated" && (
          <>
            composed by{" "}
            <span className="font-mono" data-testid={modelTestId}>
              {model || "an unnamed model"}
            </span>{" "}
            ·{" "}
          </>
        )}
        {origin === "recipe" && <>a published starting point · </>}
        nothing exists in the cluster until you create it
      </span>
    </div>
  );
}

// RefineAndDraftSurface — the builder surface (m71.4 + m71.5): after generation,
// the user can refine by chat (each message calls /api/agents/refine), create a
// draft (labelled stage:draft), test it inline, and publish. The one-shot path
// (generate → review → create without refining) is preserved beside it.
function RefineAndDraftSurface({
  gen,
  model,
  modelChoices,
  onBack,
  onRegenerate,
}: {
  gen: GenerateAgentResponse;
  model: string;
  modelChoices: { id: string; provider: string; connection: string }[];
  onBack: () => void;
  onRegenerate: () => void;
}) {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { namespace, list } = useNamespace();
  const nsOptions =
    list.kind === "ready" ? list.namespaces.map((n) => n.name) : [];
  const [targetNs, setTargetNs] = React.useState(namespace || "default");
  React.useEffect(() => {
    if (list.kind !== "ready") return;
    const names = list.namespaces.map((n) => n.name);
    if (names.length > 0 && !names.includes(targetNs)) setTargetNs(names[0]);
  }, [list, targetNs]);

  const { can, reprobe } = useCapabilities();
  const canCreate = can(RES_AGENTS, "create");

  // The current candidate spec — starts at the generated YAML, evolves with each refine turn.
  const [candidateYAML, setCandidateYAML] = React.useState(gen.agentYAML);

  // The refine chat state: transcript (capped to 8 turns), input, loading.
  const [refineTranscript, setRefineTranscript] = React.useState<RefineTurn[]>([]);
  const [refineDiff, setRefineDiff] = React.useState<string[] | null>(null);
  const [refineInput, setRefineInput] = React.useState("");
  const [refineLoading, setRefineLoading] = React.useState(false);
  const [refineError, setRefineError] = React.useState<string | null>(null);

  // The draft lifecycle state.
  const [draftState, setDraftState] = React.useState<DraftState>({ kind: "none" });

  // The inline test chat state for the created draft.
  const [testTurns, setTestTurns] = React.useState<{ id: number; role: "user" | "agent"; text: string; pending?: boolean; error?: string }[]>([]);
  const [testInput, setTestInput] = React.useState("");
  const [testBusy, setTestBusy] = React.useState(false);
  const [testConvId] = React.useState(() => `draft-test-${Date.now()}`);
  const testTurnId = React.useRef(0);

  // The advanced YAML view toggle.
  const [showYAML, setShowYAML] = React.useState(false);
  // The expanded form shown in the Advanced section — starts as the generated CRD
  // preview and updates whenever refineAgent returns an expanded field.
  const [advancedYAML, setAdvancedYAML] = React.useState(gen.expanded ?? gen.agentYAML);

  // The summary from the current candidate.
  const summary = React.useMemo(() => summarizeYAML(candidateYAML), [candidateYAML]);

  async function onRefine() {
    const instruction = refineInput.trim();
    if (!instruction || refineLoading) return;
    setRefineLoading(true);
    setRefineError(null);
    setRefineDiff(null);
    const userTurn: RefineTurn = { role: "user", text: instruction };
    // Cap transcript to last 8 turns client-side.
    const capped = [...refineTranscript, userTurn].slice(-8);
    try {
      const res: RefineAgentResponse = await api.refineAgent({
        currentSpec: candidateYAML,
        instruction,
        transcript: capped.slice(0, -1), // send history before this turn
      });
      if (res.regenerate) {
        setRefineError(res.reason ?? res.error ?? "Refinement needs another pass.");
        setRefineTranscript((t) => [...t, userTurn]);
      } else {
        setCandidateYAML(res.agentYAML);
        if (res.expanded) setAdvancedYAML(res.expanded);
        setRefineDiff(res.diff ?? null);
        const assistantTurn: RefineTurn = {
          role: "assistant",
          text: res.diff && res.diff.length > 0
            ? `Applied: changed ${res.diff.join(", ")}.`
            : "Applied the refinement.",
        };
        setRefineTranscript((t) => [...t, userTurn, assistantTurn].slice(-8));
      }
    } catch (err) {
      setRefineError(err instanceof Error ? err.message : "refinement failed");
      setRefineTranscript((t) => [...t, userTurn]);
    } finally {
      setRefineLoading(false);
      setRefineInput("");
    }
  }

  function onRefineKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void onRefine();
    }
  }

  async function onCreateDraft() {
    if (!canCreate) return;
    setDraftState({ kind: "creating" });
    try {
      const modelPick = modelChoices.find((c) => c.id === model);
      const res = await api.createAgent(
        candidateYAML,
        targetNs,
        modelPick ? { connection: modelPick.connection, provider: modelPick.provider, model: modelPick.id } : undefined,
        "draft",
      );
      const agent = res.created?.find((o) => o.kind === "AgentDeployment") ?? res.created?.[0];
      if (!agent) throw new Error("No agent returned from create");
      setDraftState({ kind: "created", ns: agent.namespace, name: agent.name, isDraft: true });
      toast({ variant: "success", title: "Draft created", description: `${agent.name} — test it below, then publish when ready.` });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setDraftState({ kind: "error", message: err instanceof Error ? err.message : "create failed" });
    }
  }

  async function onApplyRefinement() {
    if (draftState.kind !== "created") return;
    const { ns, name, resourceVersion } = draftState;
    setDraftState({ kind: "applying" });
    try {
      await api.updateAgentSpec(ns, name, candidateYAML, resourceVersion);
      // Re-fetch the resourceVersion after apply.
      const detail = await api.agentDetail(ns, name);
      setDraftState({
        kind: "created",
        ns,
        name,
        resourceVersion: detail.resourceVersion,
        isDraft: detail.isDraft ?? true,
      });
      toast({ variant: "success", title: "Refinement applied", description: "The draft agent has been updated." });
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setDraftState({ kind: "conflict" });
        return;
      }
      setDraftState({
        kind: "created",
        ns,
        name,
        resourceVersion,
        isDraft: true,
      });
    }
  }

  async function onPublish() {
    if (draftState.kind !== "created") return;
    const { ns, name } = draftState;
    setDraftState({ kind: "publishing" });
    try {
      await api.publishAgent(ns, name);
      setDraftState({ kind: "published" });
      toast({ variant: "success", title: "Agent published", description: `${name} is now live.` });
      setTimeout(() => {
        navigate(`/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`);
      }, 1200);
    } catch (err) {
      setDraftState({ kind: "error", message: err instanceof Error ? err.message : "publish failed" });
    }
  }

  async function onTestSend() {
    if (!testInput.trim() || testBusy || draftState.kind !== "created") return;
    const text = testInput.trim();
    const userId = ++testTurnId.current;
    const agentId = ++testTurnId.current;
    setTestTurns((t) => [
      ...t,
      { id: userId, role: "user", text },
      { id: agentId, role: "agent", text: "", pending: true },
    ]);
    setTestInput("");
    setTestBusy(true);
    try {
      const res = await api.invoke({
        agent: draftState.name,
        namespace: draftState.ns,
        input: { input: text },
        conversationId: testConvId,
      });
      setTestTurns((ts) =>
        ts.map((t) =>
          t.id === agentId ? { ...t, pending: false, text: res.response } : t,
        ),
      );
    } catch (err) {
      setTestTurns((ts) =>
        ts.map((t) =>
          t.id === agentId
            ? { ...t, pending: false, error: err instanceof Error ? err.message : "invoke failed" }
            : t,
        ),
      );
    } finally {
      setTestBusy(false);
    }
  }

  function onTestKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void onTestSend();
    }
  }

  const draftCreated = draftState.kind === "created";
  const draftApplying = draftState.kind === "applying";
  const draftPublishing = draftState.kind === "publishing";
  const hasRefinements = refineTranscript.length > 0;

  // The one-shot path: skip refining and go straight to the shared review, which
  // is where tools are attached and the agent is actually created.
  const [useOneShot, setUseOneShot] = React.useState(false);
  if (useOneShot) {
    const hit = modelChoices.find((c) => c.id === model);
    return (
      <SharedReview
        baseYAML={candidateYAML}
        initialTools={parseToolsFromYAML(candidateYAML)}
        summary={summarizeYAML(candidateYAML)}
        advancedYAML={advancedYAML}
        origin="generated"
        originModel={gen.model}
        onRegenerate={onRegenerate}
        modelPick={hit ? { connection: hit.connection, provider: hit.provider, model: hit.id } : undefined}
        onBack={onBack}
      />
    );
  }

  return (
    <div className="min-w-0 space-y-8" data-testid="refine-and-draft-surface">
      {/* What was proposed. */}
      <section aria-labelledby="proposal-head" className="min-w-0">
        <div data-testid="generation-review">
          <SectionHeader
            id="proposal-head"
            title="Here’s what we propose"
            lede="A model wrote this from your sentence. Read it, change it in words below, or take it to the review and create it."
            actions={
              <Button variant="outline" size="sm" onClick={onRegenerate}>
                Regenerate
              </Button>
            }
          />
          <ProposalMark
            origin="generated"
            model={gen.model}
            modelTestId="gen-model-tag"
          />
          {gen.warnings && gen.warnings.length > 0 && (
            <div className="mb-4 border border-border border-l-2 border-l-warning bg-card px-4 py-3">
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="warn">heads up</Badge>
                <p className="font-serif text-md font-medium">
                  Worth knowing before you create it
                </p>
              </div>
              <ul className="mt-2 space-y-1 text-sm text-secondary-foreground">
                {gen.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          )}
        </div>

        <div className="border border-border bg-card p-5">
          <ConfigFacts
            summary={{ ...summary, tools: parseToolsFromYAML(candidateYAML) }}
          />
        </div>

        <Disclosure
          open={showYAML}
          onToggle={() => setShowYAML((v) => !v)}
          testId="advanced-toggle"
          label="the generated agent.yaml"
        >
          <CodeWell testId="advanced-yaml">{advancedYAML}</CodeWell>
        </Disclosure>
      </section>

      {/* Refine by chat (m71.4). */}
      <section aria-labelledby="refine-head" className="min-w-0" data-testid="refine-chat-panel">
        <SectionHeader
          as="h3"
          id="refine-head"
          title="Change it in words"
          lede="“Add web search”, “be stricter about formatting”. Each message rewrites the proposal above — it still creates nothing."
        />
        <div className="border border-border bg-card p-5">
          {refineTranscript.length > 0 && (
            <div className="mb-3 space-y-2" data-testid="refine-transcript">
              {refineTranscript.map((turn, i) => (
                <div
                  key={i}
                  className={`flex ${turn.role === "user" ? "justify-end" : "justify-start"}`}
                >
                  <div
                    className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
                      turn.role === "user"
                        ? "bg-accent text-accent-foreground"
                        : "border border-border bg-surface-2 text-foreground"
                    }`}
                    data-testid={turn.role === "user" ? "refine-turn-user" : "refine-turn-assistant"}
                  >
                    {turn.text}
                  </div>
                </div>
              ))}
            </div>
          )}
          {refineDiff && refineDiff.length > 0 && (
            <div
              className="mb-2 flex flex-wrap items-center gap-2 text-xs text-secondary-foreground"
              data-testid="refine-diff-chip"
            >
              <Badge variant="ok">changed</Badge>
              <span className="min-w-0 break-words font-mono">
                {refineDiff.join(", ")}
              </span>
            </div>
          )}
          {refineError && (
            <p
              className="mb-2 border border-border border-l-2 border-l-destructive bg-card px-3 py-2 text-xs text-secondary-foreground"
              role="alert"
              data-testid="refine-error"
            >
              {refineError}
            </p>
          )}
          <div className="flex items-end gap-2">
            <Textarea
              aria-label="Refine instruction"
              rows={2}
              value={refineInput}
              onChange={(e) => setRefineInput(e.target.value)}
              onKeyDown={onRefineKeyDown}
              placeholder="Add web search, be stricter about formatting…"
              className="resize-none text-xs"
              data-testid="refine-input"
              disabled={refineLoading}
            />
            <Button
              size="icon"
              onClick={() => void onRefine()}
              disabled={refineLoading || !refineInput.trim()}
              data-testid="refine-send"
              aria-label="Send refinement"
              className="h-9 w-9 shrink-0"
            >
              <Send className="h-4 w-4" />
            </Button>
          </div>
          {refineLoading && (
            <p className="mt-1.5 text-xs text-faint" data-testid="refine-loading">
              Refining…
            </p>
          )}
          <p className="mt-1.5 text-2xs text-faint">
            Enter to send · Shift+Enter for a newline · the last 8 turns are kept
          </p>
        </div>
      </section>

      {/* Draft lifecycle + inline test (m71.5). */}
      <section aria-labelledby="draft-head" className="min-w-0" data-testid="draft-lifecycle">
        <SectionHeader
          as="h3"
          id="draft-head"
          title="Try it before anyone else can"
          lede="A draft is a real agent that is not published: it runs for you, and nothing routes to it until you publish."
        />

        {draftState.kind === "conflict" && (
          <p
            className="mb-3 border border-border border-l-2 border-l-destructive bg-card px-3 py-2 text-xs text-secondary-foreground"
            role="alert"
            data-testid="draft-conflict"
          >
            The agent changed since you loaded it — reload to get the latest version.
          </p>
        )}
        {draftState.kind === "error" && (
          <p
            className="mb-3 border border-border border-l-2 border-l-destructive bg-card px-3 py-2 text-xs text-secondary-foreground"
            role="alert"
            data-testid="draft-error"
          >
            {draftState.message}
          </p>
        )}
        {draftState.kind === "published" && (
          <div
            className="mb-3 flex flex-wrap items-center gap-2 text-sm"
            data-testid="draft-published"
          >
            <Badge variant="ok">published</Badge>
            <span className="text-secondary-foreground">
              It is live — opening its page…
            </span>
          </div>
        )}

        <div className="border border-border bg-card p-5">
          {canCreate && nsOptions.length > 0 && draftState.kind === "none" && (
            <div className="mb-4">
              <FormField id="draft-namespace" label="Workspace">
                <Select
                  id="draft-namespace"
                  value={nsOptions.includes(targetNs) ? targetNs : ""}
                  onChange={(e) => setTargetNs(e.target.value)}
                  data-testid="draft-namespace-select"
                >
                  {nsOptions.map((n) => (
                    <option key={n} value={n}>{n}</option>
                  ))}
                </Select>
              </FormField>
            </div>
          )}
          <div className="flex flex-wrap items-center gap-3">
            <Button variant="ghost" onClick={onBack} disabled={draftState.kind === "creating" || draftState.kind === "publishing"}>
              Back
            </Button>
            {draftState.kind === "none" && (
              <Button
                variant="outline"
                onClick={() => setUseOneShot(true)}
                data-testid="create-agent-direct"
              >
                Go to the review
              </Button>
            )}
            {canCreate && draftState.kind === "none" && (
              <Button
                onClick={() => void onCreateDraft()}
                data-testid="create-draft-button"
              >
                Create a draft and test it
              </Button>
            )}
            {draftCreated && hasRefinements && (
              <Button
                variant="outline"
                onClick={() => void onApplyRefinement()}
                disabled={draftApplying}
                data-testid="apply-refinement-button"
              >
                {draftApplying ? "Working…" : "Apply refinement"}
              </Button>
            )}
            {draftCreated && (
              <Button
                onClick={() => void onPublish()}
                disabled={draftPublishing}
                data-testid="publish-button"
              >
                {draftPublishing ? "Working…" : "Publish"}
              </Button>
            )}
            {draftState.kind === "creating" && (
              <p className="text-xs text-faint" data-testid="draft-creating">
                Creating draft…
              </p>
            )}
          </div>

          {draftCreated && (
            <div className="mt-5 rounded-lg border border-border" data-testid="draft-test-panel">
              <div className="border-b border-border px-4 py-3">
                <p className="font-serif text-md font-medium">
                  Testing draft{" "}
                  <span className="font-mono text-sm">{draftState.name}</span>
                </p>
                <p className="text-xs text-faint">
                  The first message may wait while the draft scales up from zero.
                </p>
              </div>
              <div className="flex min-h-40 flex-col gap-3 px-4 py-4" data-testid="draft-test-thread">
                {testTurns.length === 0 ? (
                  <p className="text-xs text-faint" data-testid="draft-test-empty">
                    Send it something to see how it answers.
                  </p>
                ) : (
                  testTurns.map((t) => (
                    <div key={t.id} className={`flex ${t.role === "user" ? "justify-end" : "justify-start"}`}>
                      <div
                        className={`max-w-[85%] rounded-lg px-3 py-2 text-xs ${
                          t.role === "user"
                            ? "bg-accent text-accent-foreground"
                            : "border border-border bg-surface-2"
                        }`}
                        data-testid={t.role === "user" ? "test-turn-user" : "test-turn-agent"}
                      >
                        {t.pending ? (
                          <span className="inline-flex items-center gap-1" data-testid="test-pending">
                            <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost [animation-delay:-0.25s]" />
                            <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost [animation-delay:-0.12s]" />
                            <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-ghost" />
                          </span>
                        ) : t.error ? (
                          <span className="text-destructive" role="alert">{t.error}</span>
                        ) : (
                          t.text
                        )}
                      </div>
                    </div>
                  ))
                )}
              </div>
              <div className="border-t border-border p-3">
                <div className="flex items-end gap-2">
                  <Textarea
                    aria-label="Test message"
                    rows={2}
                    value={testInput}
                    onChange={(e) => setTestInput(e.target.value)}
                    onKeyDown={onTestKeyDown}
                    placeholder="Ask the draft something…"
                    className="resize-none text-xs"
                    data-testid="draft-test-input"
                    disabled={testBusy}
                  />
                  <Button
                    size="icon"
                    onClick={() => void onTestSend()}
                    disabled={testBusy || !testInput.trim()}
                    data-testid="draft-test-send"
                    aria-label="Send test message"
                    className="h-9 w-9 shrink-0"
                  >
                    <Send className="h-4 w-4" />
                  </Button>
                </div>
              </div>
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

// ── Configure it ────────────────────────────────────────────────────────────

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
  const [seed] = React.useState<ConfigForm>(() => ({
    ...emptyForm(),
    runtime: "managed",
  }));
  const [form, setForm] = React.useState<ConfigForm>(seed);
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
        // Auto-default: pick the primary (flagship) model from the connected set.
        // "Primary" mirrors Go's primaryModel(): prefer flagship tiers
        // (opus/sonnet/gpt-5/gpt-4/large/ultra/pro/max) over small tiers
        // (haiku/fable/mini/nano/small/lite/flash/embed).
        const smallTierRe = /haiku|fable|mini|nano|\bsmall\b|lite|flash|embed/i;
        const flagship = flat.find((x) => !smallTierRe.test(x.model));
        const autoDefault = flagship ?? flat[0];
        if (autoDefault) {
          setPickedModel((prev) => prev || `${autoDefault.connection}|${autoDefault.model}`);
        }
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
  const modelPick = React.useMemo(() => {
    const hit = connectedModels.find(
      (c) => `${c.connection}|${c.model}` === pickedModel,
    );
    return hit
      ? { connection: hit.connection, provider: hit.provider, model: hit.model }
      : undefined;
  }, [connectedModels, pickedModel]);

  if (done) {
    return (
      <SharedReview
        baseYAML={toAgentYAML(form)}
        initialTools={form.tools}
        summary={summarizeYAML(toAgentYAML(form))}
        advancedYAML={toAgentYAML(form)}
        origin="form"
        // m21: the picked model → the BFF ensures its route. If the user instead used
        // the Advanced "existing route" field, modelPick is empty and that route wins.
        modelPick={modelPick}
        onBack={() => setDone(false)}
      />
    );
  }

  const managed = form.runtime === "managed";
  // Anything the user has typed is work an Escape must not silently drop. The
  // kit Wizard owns the confirm dialog; this is the flag that arms it.
  const dirty = JSON.stringify(form) !== JSON.stringify(seed);

  const runtimeCard =
    "rounded-lg border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2";

  const steps: WizardStep[] = [
    {
      id: "basics",
      title: "Basics",
      description: "Name + runtime",
      content: (
        <div className="min-w-0 space-y-5">
          <SectionHeader
            title="Basics"
            lede="What the agent is called, and whether the platform runs it for you or you bring a container."
          />
          <FormField
            id="cfg-name"
            label="Name"
            error={errors.name}
            hint="DNS label, ≤ 44 chars."
          >
            <Input
              id="cfg-name"
              className="font-mono"
              value={form.name}
              onChange={(e) => set("name", e.target.value)}
              placeholder="support-agent"
            />
          </FormField>
          <div className="space-y-1.5">
            <Label>Runtime</Label>
            <div className="grid gap-3 sm:grid-cols-2">
              <button
                type="button"
                aria-pressed={managed}
                onClick={() => set("runtime", "managed")}
                className={`${runtimeCard} ${
                  managed
                    ? "border-primary bg-accent"
                    : "border-border bg-card hover:bg-surface-2"
                }`}
              >
                <p className="text-sm font-medium">Managed</p>
                <p className="mt-0.5 text-xs text-faint">
                  The stock tool-calling loop — no Docker build.
                </p>
              </button>
              <button
                type="button"
                aria-pressed={!managed}
                onClick={() => set("runtime", "custom")}
                className={`${runtimeCard} ${
                  !managed
                    ? "border-primary bg-accent"
                    : "border-border bg-card hover:bg-surface-2"
                }`}
              >
                <p className="text-sm font-medium">Custom image</p>
                <p className="mt-0.5 text-xs text-faint">
                  Bring your own container.
                </p>
              </button>
            </div>
          </div>
          {!managed && (
            <FormField id="cfg-image" label="Image" error={errors.image}>
              <Input
                id="cfg-image"
                className="font-mono"
                value={form.image}
                onChange={(e) => set("image", e.target.value)}
                placeholder="ghcr.io/acme/agent:v1"
              />
            </FormField>
          )}
          {managed && form.image.trim() === "" && (
            <p className="text-xs text-faint">
              A managed agent runs the platform&apos;s stock image — nothing to
              build or push.
            </p>
          )}
        </div>
      ),
    },
    {
      id: "behavior",
      title: "Behaviour",
      description: "Prompt + model",
      content: (
        <div className="min-w-0 space-y-5">
          <SectionHeader
            title="Behaviour"
            lede="What it is told to do, which model it runs on, and who is allowed to call it."
          />
          {managed && (
            <FormField
              id="cfg-system-prompt"
              label="System prompt"
              hint="What the managed agent should do — its persona and instructions."
            >
              <Textarea
                id="cfg-system-prompt"
                data-testid="cfg-system-prompt"
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
          <details className="rounded-md border border-border bg-surface-2 px-3 py-2">
            <summary className="cursor-pointer select-none text-sm text-secondary-foreground">
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
                    className="font-mono"
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
        <div className="min-w-0 space-y-5">
          <SectionHeader
            title="Resources"
            lede="What each replica asks for, and whether one stays warm so requests never cold-start."
          />
          <div className="grid gap-4 sm:grid-cols-2">
            <FormField id="cfg-cpu" label="CPU request">
              <Input
                id="cfg-cpu"
                className="font-mono"
                value={form.resourcesCpu}
                onChange={(e) => set("resourcesCpu", e.target.value)}
                placeholder="500m"
              />
            </FormField>
            <FormField id="cfg-memory" label="Memory request">
              <Input
                id="cfg-memory"
                className="font-mono"
                value={form.resourcesMemory}
                onChange={(e) => set("resourcesMemory", e.target.value)}
                placeholder="256Mi"
              />
            </FormField>
          </div>
          <div className="flex items-start gap-3 rounded-lg border border-border bg-surface-2 p-4">
            <input
              id="cfg-keep-warm"
              type="checkbox"
              className="mt-0.5 h-4 w-4 cursor-pointer rounded-sm border-input accent-primary"
              checked={form.keepWarm}
              onChange={(e) => set("keepWarm", e.target.checked)}
              data-testid="cfg-keep-warm"
            />
            <label htmlFor="cfg-keep-warm" className="cursor-pointer space-y-0.5">
              <span className="text-sm font-medium">Keep warm</span>
              <p className="text-xs text-faint">
                Always keeps at least one replica running so requests never
                cold-start. Advanced min/max scaling lives in the raw-YAML editor.
              </p>
            </label>
          </div>
        </div>
      ),
    },
    {
      id: "optional",
      title: "Optional",
      description: "Budget · prompt · eval",
      content: <OptionalStep form={form} set={set} errors={errors} />,
    },
    {
      id: "review",
      title: "Review",
      review: true,
      content: (
        <div className="min-w-0 space-y-5" data-testid="configure-review-step">
          <SectionHeader
            title="Review what you set"
            lede="Nothing exists in the cluster yet. Finish to attach tools and create it — the same review the “Describe it” path lands on."
          />
          <div className="border border-border bg-card p-5">
            <ConfigFacts
              summary={summarizeYAML(toAgentYAML(form))}
              modelPick={modelPick}
            />
          </div>
        </div>
      ),
    },
  ];

  // Forward gating: only the basics step hard-gates (name + image); later steps
  // are optional-until-review (the review re-runs the full validate).
  const canProceed =
    current === STEP_BASICS ? Object.keys(basicsErrors).length === 0 : true;

  return (
    <div className="min-w-0" data-testid="configure-flow">
      <Wizard
        steps={steps}
        current={current}
        onStepChange={onStepChange}
        canProceed={canProceed}
        dirty={dirty}
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
    <div className="min-w-0 space-y-5">
      <SectionHeader
        title="Optional"
        lede="Bounds and gates. Everything here is off unless you turn it on — an untouched block claims nothing."
      />
      <ToggleSection
        id="cfg-budget"
        label="Cost budget"
        enabled={form.budgetEnabled}
        onToggle={(v) => set("budgetEnabled", v)}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField
            id="cfg-budget-conv"
            label="Per conversation (USD)"
            error={errors.budgetPerConversationUSD}
          >
            <Input
              id="cfg-budget-conv"
              className="font-mono"
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
              className="font-mono"
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
            className="font-mono"
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
            className="font-mono"
            value={form.promptRepo}
            onChange={(e) => set("promptRepo", e.target.value)}
            placeholder="https://github.com/acme/prompts"
          />
        </FormField>
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField id="cfg-prompt-ref" label="Ref" error={errors.promptRef}>
            <Input
              id="cfg-prompt-ref"
              className="font-mono"
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
              className="font-mono"
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
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField
            id="cfg-eval-suite"
            label="Suite name"
            error={errors.evalSuite}
          >
            <Input
              id="cfg-eval-suite"
              className="font-mono"
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
              className="font-mono"
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
            className="font-mono"
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
    <div className="rounded-lg border border-border bg-card p-4">
      <label
        htmlFor={id}
        className="flex items-center gap-2 text-sm font-medium"
      >
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

// ── Recipe review (m72.5) ───────────────────────────────────────────────────

// RecipeReview pre-fills the SharedReview surface with a recipe's simplified
// agent.yaml spec. The recipe spec is the same format as the generate/refine
// output — SharedReview handles tool injection, Preview, and Create identically.
function RecipeReview({ spec, onBack }: { spec: string; onBack: () => void }) {
  const summary = summarizeYAML(spec);
  return (
    <SharedReview
      baseYAML={spec}
      initialTools={summary.tools ?? []}
      summary={summary}
      advancedYAML={spec}
      origin="recipe"
      onBack={onBack}
    />
  );
}

// ── The shared review + tool picker + Create ────────────────────────────────

function SharedReview({
  baseYAML,
  initialTools,
  summary,
  advancedYAML,
  origin,
  originModel,
  onRegenerate,
  onBack,
  modelPick,
}: {
  baseYAML: string;
  initialTools: string[];
  summary: YAMLSummary;
  advancedYAML: string;
  origin: Origin;
  /** The model that COMPOSED the config, when one did. */
  originModel?: string;
  onRegenerate?: () => void;
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
  // the calm permission boundary (the API is the real gate, ADR 0011).
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

  // Check-requirements checklist (m72.3, ADR 0066 D3): advisory pre-flight against
  // the finalYAML. `null` means it has not answered — which the checklist renders
  // as "not checked", NEVER as satisfied.
  const [reqCheck, setReqCheck] = React.useState<CheckRequirementsResponse | null>(null);
  React.useEffect(() => {
    const c = new AbortController();
    setReqCheck(null);
    api
      .checkRequirements(finalYAML, targetNs, c.signal)
      .then((r) => {
        if (!c.signal.aborted) setReqCheck(r);
      })
      .catch(() => {
        /* soft miss — advisory only; the checklist says "not checked" instead */
      });
    return () => c.abort();
  }, [finalYAML, targetNs]);

  async function onPreview() {
    setState({ kind: "previewing" });
    try {
      const manifest = await api.expand(finalYAML);
      setState({ kind: "preview", manifest });
      // Open the disclosure — a preview the reader has to go hunting for is a
      // button that appears to do nothing.
      setAdvanced(true);
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
      <section
        aria-labelledby="created-head"
        className="min-w-0"
        data-testid="create-success"
      >
        <SectionHeader
          id="created-head"
          title={agent ? `${agent.name} exists now` : "The agent exists now"}
          lede="Opening its page, where you can watch it come up and send it something."
        />
        {/* The kv register, hand-laid rather than KeyValueList: a create can
            return several objects of the SAME kind (one MCPToolBinding per
            tool), and the kit keys its rows by the key string. */}
        <div className="border border-border bg-card px-5 py-2">
          <dl className="min-w-0">
            {state.created.map((obj) => (
              <div
                key={`${obj.kind}/${obj.namespace}/${obj.name}`}
                className="flex items-baseline justify-between gap-3 border-b border-border-soft py-2 last:border-0"
              >
                <dt className="shrink-0 font-mono text-2xs uppercase tracking-wide text-faint">
                  {obj.kind}
                </dt>
                <dd className="min-w-0 break-words text-right">
                  <span className="font-mono text-sm">{obj.name}</span>
                  <span className="mt-0.5 block text-xs text-faint">
                    in {obj.namespace}
                  </span>
                </dd>
              </div>
            ))}
          </dl>
        </div>
      </section>
    );
  }

  const busy = state.kind === "previewing" || state.kind === "creating";
  const isManaged = /(^|\n)runtime:\s*managed(\s|$)/.test(baseYAML);
  // `api.checkRequirements` answers a 501 with a synthetic all-clear rather
  // than throwing, so "it answered" has to be established, not assumed.
  const answered =
    reqCheck !== null && preflightAnswered(reqCheck, selected, summary.model);
  const requirements = readRequirements(
    answered ? reqCheck : null,
    selected,
    summary.model,
  );

  return (
    <div className="min-w-0 space-y-8" data-testid="shared-review">
      {/* The whole config, plainly. Nothing that decides what gets created is
          hidden behind a chevron — only the raw manifest is. */}
      <section aria-labelledby="review-head" className="min-w-0">
        <SectionHeader
          id="review-head"
          title="Review before creating"
          lede="This is everything that will be created, in the words it will be created with."
          actions={
            origin === "generated" && onRegenerate ? (
              <Button variant="outline" size="sm" onClick={onRegenerate}>
                Regenerate
              </Button>
            ) : undefined
          }
        />
        <ProposalMark origin={origin} model={originModel} />
        <div className="border border-border bg-card p-5">
          <ConfigFacts
            summary={{ ...summary, tools: selected }}
            namespace={targetNs}
            modelPick={modelPick}
          />
        </div>
        <Disclosure
          open={advanced}
          onToggle={() => setAdvanced((v) => !v)}
          testId="advanced-toggle"
          label={
            state.kind === "preview"
              ? "the expanded CRDs"
              : "the generated agent.yaml"
          }
        >
          <CodeWell testId="advanced-yaml">
            {state.kind === "preview" ? state.manifest : advancedYAML}
          </CodeWell>
        </Disclosure>
      </section>

      {/* What it needs to actually run. Advisory — it never blocks Create. */}
      <section aria-labelledby="requirements-head" className="min-w-0">
        <SectionHeader
          as="h3"
          id="requirements-head"
          title="What it needs before it can run"
          lede="Checked against this workspace. None of it blocks creating the agent — it tells you what to connect next."
        />
        {!answered && requirements.length > 0 && (
          <QuietNote
            className="mb-3"
            title="The pre-flight check hasn’t answered."
          >
            This install either doesn&apos;t offer{" "}
            <span className="font-mono text-xs">check-requirements</span> or it
            didn&apos;t reply, so nothing below has been verified against the
            cluster. Everything is listed as <em>not checked</em> rather than
            ready — an unchecked requirement is not a satisfied one, and nothing
            here is guessed.
          </QuietNote>
        )}
        {requirements.length > 0 ? (
          <div
            className="border border-border bg-card px-5 py-2"
            data-testid="requirements-checklist"
          >
            <ul className="min-w-0">
              {requirements.map((r) => (
                <RequirementRow key={`${r.kind}:${r.name}`} req={r} />
              ))}
            </ul>
          </div>
        ) : (
          <p className="text-sm text-faint">
            This config names no model route and no tools, so there is nothing to
            connect.
          </p>
        )}
      </section>

      {/* Tool picker — managed agents only (a custom image brings its own). */}
      {isManaged && (
        <section aria-labelledby="tools-head" className="min-w-0">
          <SectionHeader
            as="h3"
            id="tools-head"
            title="Attach tools"
            lede="The MCP tools this agent may call. Expand a server to pick individual tools."
          />
          <div className="border border-border bg-card p-5">
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
        </section>
      )}

      {/* Where it lands, and the two acts. */}
      <section aria-labelledby="create-head" className="min-w-0">
        <SectionHeader
          as="h3"
          id="create-head"
          title="Create it"
          lede="Preview expands the config into the CRDs the cluster will hold — it changes nothing."
        />
        <div className="border border-border bg-card p-5">
          {state.kind === "error" && state.forbidden ? (
            <div className="mb-4">
              <ForbiddenInline
                title="Not allowed to create this agent"
                description="Your account can preview the manifest but can't create AgentDeployments in this cluster."
                detail={state.message}
              />
            </div>
          ) : (
            state.kind === "error" && (
              <div className="mb-4" data-testid="create-error">
                <ErrorState
                  title="The agent wasn’t created."
                  description="Nothing was applied to the cluster. Everything above is still exactly as you left it."
                  detail={`${state.message}${state.status ? ` (${state.status})` : ""}`}
                  onRetry={() => setState({ kind: "idle" })}
                  retryLabel="Dismiss"
                />
              </div>
            )
          )}
          {canCreate && nsOptions.length > 0 && (
            <div className="mb-4">
              <FormField id="create-namespace" label="Workspace">
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
            </div>
          )}
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex items-center gap-2">
              <Button variant="ghost" onClick={onBack} disabled={busy}>
                Back
              </Button>
              <Button variant="outline" onClick={onPreview} disabled={busy}>
                {state.kind === "previewing" ? "Working…" : "Preview CRD"}
              </Button>
            </div>
            {canCreate ? (
              <Button
                onClick={onCreate}
                disabled={busy}
                data-testid="create-button"
              >
                {state.kind === "creating" ? "Working…" : "Create agent"}
              </Button>
            ) : (
              <p
                className="rounded-md border border-dashed border-border-strong px-3 py-2 text-center text-xs text-faint"
                data-testid="create-readonly-note"
              >
                You have read-only access — creating an agent needs create
                permission on AgentDeployments.
              </p>
            )}
          </div>
        </div>
        <ClosingNote>
          Creating this agent writes its CRDs. Nothing has been written yet.
        </ClosingNote>
      </section>
    </div>
  );
}

// ── The requirements checklist (m72.3) ──────────────────────────────────────
//
// FIVE states, not two. `not-checked` is the state that keeps the surface
// honest: when the pre-flight didn't answer, the requirements are still listed
// — read off the config we already hold — and every one of them says it was
// not checked. A tick the platform never earned would be the worst thing on
// this page (§7.1).

type ReqState =
  | "ready"
  | "needs-approval"
  | "needs-consent"
  | "not-found"
  | "not-connected"
  | "not-checked";

interface Requirement {
  kind: "model" | "tool";
  name: string;
  state: ReqState;
}

/** The tag word for each state. Uppercased by the Badge recipe; ≤16 chars (§4.5). */
const REQ_WORD: Record<ReqState, string> = {
  ready: "ready",
  "needs-approval": "needs approval",
  "needs-consent": "needs consent",
  "not-found": "not registered",
  "not-connected": "not connected",
  "not-checked": "not checked",
};

/**
 * The hue per state, and the reasoning behind the two that are not obvious:
 *   • needs-approval / needs-consent are HOLD, not warn — work is paused
 *     because a PERSON must decide (an operator approving a tool, an end user
 *     consenting at run time). That is the exact definition of hold (§2.4).
 *   • not-checked is `open` — declared but never exercised. Never `ok`.
 */
const REQ_VARIANT: Record<ReqState, "ok" | "hold" | "crit" | "open"> = {
  ready: "ok",
  "needs-approval": "hold",
  "needs-consent": "hold",
  "not-found": "crit",
  "not-connected": "crit",
  "not-checked": "open",
};

/** One line of plain language per state, so the tag is never the only telling. */
const REQ_SAYS: Record<ReqState, string> = {
  ready: "connected and bindable",
  "needs-approval": "an operator has to approve it before it binds",
  "needs-consent": "the person using the agent consents at run time",
  "not-found": "no MCP server here provides it",
  "not-connected": "no provider is connected for it",
  "not-checked": "unknown — the pre-flight didn’t answer",
};

/** The user's next act, when there is one. Verb-first, ≤22 chars (§7.2). */
function reqNextStep(r: Requirement): { label: string; to: string } | null {
  if (r.state === "not-connected") {
    return { label: "Connect a provider", to: "/providers/connect" };
  }
  if (r.state === "not-found") {
    return { label: "Register the server", to: "/tools/add-mcp" };
  }
  if (r.state === "needs-approval") {
    return { label: "Open the approvals", to: "/tools/approvals" };
  }
  return null;
}

/**
 * Did the pre-flight actually run?
 *
 * `api.checkRequirements` maps a 501 (the endpoint is absent on an older
 * server) to a synthetic `{ model: { required: false, connected: true },
 * tools: [] }` so callers "degrade silently". That object has the exact shape
 * of a SATISFIED answer, and this surface may not repeat it as one.
 *
 * The tell is arithmetic, not a guess: an answer that requires no model and
 * names no tools cannot be describing a config that names either. So when the
 * config carries a route or selected tools and the response mentions neither,
 * nothing was checked — and the checklist says so instead of showing ticks the
 * platform never earned (§7.1).
 */
function preflightAnswered(
  check: CheckRequirementsResponse,
  selectedTools: string[],
  configuredRoute: string | undefined,
): boolean {
  if (check.model.required || check.tools.length > 0) return true;
  return selectedTools.length === 0 && !configuredRoute;
}

/**
 * Read the checklist from the pre-flight when it answered, and from the CONFIG
 * when it did not. The second branch is the point: a requirement we cannot
 * verify still gets listed, marked `not-checked` — silence would read as "all
 * clear", which is the claim we are not entitled to make.
 */
function readRequirements(
  check: CheckRequirementsResponse | null,
  selectedTools: string[],
  configuredRoute: string | undefined,
): Requirement[] {
  if (check === null) {
    const rows: Requirement[] = [];
    if (configuredRoute) {
      rows.push({ kind: "model", name: configuredRoute, state: "not-checked" });
    }
    for (const name of selectedTools) {
      rows.push({ kind: "tool", name, state: "not-checked" });
    }
    return rows;
  }

  const rows: Requirement[] = [];
  if (check.model.required) {
    rows.push({
      kind: "model",
      name: check.model.route || configuredRoute || "the model route",
      state: check.model.connected ? "ready" : "not-connected",
    });
  }
  for (const t of check.tools) {
    rows.push({ kind: "tool", name: t.name, state: toReqState(t.status) });
  }
  return rows;
}

/** An unrecognised status is NOT a pass — it is a value we cannot interpret. */
function toReqState(status: string): ReqState {
  switch (status) {
    case "ready":
    case "needs-approval":
    case "needs-consent":
    case "not-found":
      return status;
    default:
      return "not-checked";
  }
}

function RequirementRow({ req }: { req: Requirement }) {
  const next = reqNextStep(req);
  return (
    <li className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-border-soft py-3 last:border-0">
      <div className="min-w-0">
        <span className="block font-mono text-2xs uppercase tracking-wide text-faint">
          {req.kind === "model" ? "model route" : "tool"}
        </span>
        <span className="block break-words font-mono text-sm">{req.name}</span>
      </div>
      <div className="flex min-w-0 flex-wrap items-center justify-end gap-2">
        <span className="text-xs text-faint">{REQ_SAYS[req.state]}</span>
        <Badge variant={REQ_VARIANT[req.state]}>{REQ_WORD[req.state]}</Badge>
        {next && <NextStepLink label={next.label} to={next.to} />}
      </div>
    </li>
  );
}

// ── Shared bits of furniture ────────────────────────────────────────────────

// Disclosure — the one chevron on these pages, and it only ever hides the RAW
// manifest. Everything a person needs in order to decide is above it (§6.1 A4).
function Disclosure({
  open,
  onToggle,
  testId,
  label,
  children,
}: {
  open: boolean;
  onToggle: () => void;
  testId: string;
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="mt-4 min-w-0">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        data-testid={testId}
        className="flex items-center gap-1.5 text-sm font-medium text-secondary-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
      >
        <ChevronRight
          aria-hidden="true"
          className={`h-4 w-4 transition-transform ${open ? "rotate-90" : ""}`}
        />
        {open ? "Hide" : "Show"} {label}
      </button>
      {open && children}
    </div>
  );
}

// CodeWell — machine text lives here and nowhere else: sunk plane, mono, and it
// scrolls INSIDE ITSELF so a 200-column manifest never widens the page (§4.6).
function CodeWell({
  children,
  testId,
}: {
  children: React.ReactNode;
  testId?: string;
}) {
  return (
    <pre
      data-testid={testId}
      className="mt-3 max-h-96 min-w-0 overflow-auto rounded-md bg-surface-3 p-4 font-mono text-xs leading-relaxed text-secondary-foreground"
    >
      {children}
    </pre>
  );
}

// ConfigFacts — the review's fact register (§5.25). Every value a person has to
// weigh is here, in the kv register, at full strength: nothing that decides what
// gets created hides behind a disclosure. Machine-owned values (name, image,
// route, tools) are mono; prose (the system prompt) is not.
function ConfigFacts({
  summary,
  namespace,
  modelPick,
}: {
  summary: YAMLSummary;
  namespace?: string;
  modelPick?: { connection?: string; provider: string; model: string };
}) {
  const items: KeyValueItem[] = [
    {
      key: "Name",
      value: summary.name,
      absent: "not named yet",
      title: summary.name || "You haven’t named it yet.",
    },
    {
      key: "Runtime",
      value: <Badge variant="muted">{summary.runtime}</Badge>,
      mono: false,
    },
    {
      key: "Image",
      value: summary.image,
      absent:
        summary.runtime === "managed"
          ? "the platform’s stock image"
          : "not set yet",
      title: summary.image
        ? summary.image
        : summary.runtime === "managed"
          ? "A managed agent runs the platform’s own image — there is nothing to build or push."
          : "A custom-runtime agent needs an image, and none is set yet.",
    },
  ];

  if (modelPick) {
    items.push({
      key: "Model",
      value: modelPick.model,
      title: modelPick.connection
        ? `via the “${modelPick.connection}” connection (${modelPick.provider}) — a route is created for it`
        : modelPick.provider,
    });
  } else {
    items.push({
      key: "Model route",
      value: summary.model,
      absent: "none — the registry default applies",
      title:
        summary.model ??
        "No route is named on this config, so the registry’s default is used.",
    });
  }

  if (namespace) items.push({ key: "Workspace", value: namespace });

  items.push({
    key: "System prompt",
    value: summary.systemPrompt ? (
      <span className="italic">“{truncate(summary.systemPrompt, 140)}”</span>
    ) : undefined,
    mono: false,
    absent: "none — the stock loop’s own instructions",
    title:
      summary.systemPrompt ??
      "No system prompt is set, so the managed loop uses its own instructions.",
  });

  items.push({
    key: `Tools${summary.tools.length > 0 ? ` · ${summary.tools.length}` : ""}`,
    mono: false,
    value:
      summary.tools.length > 0 ? (
        <span
          className="flex flex-wrap justify-end gap-1.5"
          data-testid="summary-tools"
        >
          {summary.tools.map((t) => (
            <span
              key={t}
              className="rounded-sm border border-border-soft bg-surface-2 px-1.5 py-0.5 font-mono text-2xs text-secondary-foreground"
            >
              {t}
            </span>
          ))}
        </span>
      ) : (
        // A known zero — the user chose no tools. `text-ghost` is the sanctioned
        // register for a real zero (quantity.tsx), and the words carry it.
        <span className="text-ghost" data-testid="summary-tools">
          none selected
        </span>
      ),
  });

  return (
    <div className="min-w-0" data-testid="friendly-summary">
      <KeyValueList items={items} />
    </div>
  );
}

// ToolPicker — the merged catalog (curated + user-added) with source + schema +
// pending badges. Pre-selected tools (from generation) start checked; the user
// adjusts. A pending tool is bindable-if-approved — it's still checkable (the
// selection flows into the agent's tools; the operator's approval gates binding
// server-side), it just carries the honest tag.
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
      <div data-testid="tools-loading">
        <SkeletonText lines={3} />
      </div>
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
    <div className="min-w-0 space-y-3">
      {catalog.kind === "error" && (
        <div data-testid="tools-error">
          <QuietNote title="The full tool catalogue didn’t load.">
            {catalog.message}. The tools already selected for this agent are
            listed below and you can still create it — but this is not the whole
            catalogue, so read the list as partial rather than complete.
          </QuietNote>
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-0 flex-1">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-faint"
          />
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

      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-faint">
        <span>
          <QuantityValue value={selected.length} className="text-foreground" />{" "}
          selected of <QuantityValue value={tools.length} /> across{" "}
          <QuantityValue value={serverGroups.length} /> server
          {serverGroups.length === 1 ? "" : "s"}
        </span>
        {selected.length > 0 && (
          <button
            type="button"
            className="border-b border-accent font-semibold text-primary hover:border-primary"
            onClick={() => onSelectMany(selected, false)}
            data-testid="tool-clear-selected"
          >
            Clear selection
          </button>
        )}
      </div>

      {/* MCP servers, collapsed by default; expand to pick individual tools. The server
          checkbox selects/clears the whole server (indeterminate when partial, m25 S12/S13). */}
      <div className="min-w-0 space-y-2" data-testid="tool-picker-list">
        {filtered.length === 0 && (
          <p className="rounded-lg border border-dashed border-border-strong py-8 text-center text-sm text-faint">
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
              className="min-w-0 overflow-hidden rounded-lg border border-border"
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
                  className="h-4 w-4 rounded-sm border-input accent-primary"
                  aria-label={`Select all tools from ${server}`}
                  data-testid={`tool-server-checkbox-${server}`}
                />
                <button
                  type="button"
                  className="flex min-w-0 flex-1 items-center gap-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
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
                    aria-hidden="true"
                    className={`h-4 w-4 shrink-0 text-faint transition-transform ${isOpen ? "rotate-90" : ""}`}
                  />
                  <Server aria-hidden="true" className="h-4 w-4 shrink-0 text-faint" />
                  <span className="min-w-0 truncate font-mono text-sm">
                    {server}
                  </span>
                </button>
                <div className="flex shrink-0 items-center gap-2 text-xs text-faint">
                  {selectedCount > 0 && (
                    <span>
                      <QuantityValue value={selectedCount} /> selected
                    </span>
                  )}
                  <span>
                    <QuantityValue value={groupTools.length} /> tool
                    {groupTools.length === 1 ? "" : "s"}
                  </span>
                </div>
              </div>
              {isOpen && (
                <div className="border-t border-border">
                  {groupTools.map((t) => {
                    const on = selected.includes(t.name);
                    const pending = t.approvalStatus === "pending";
                    const hasSchema = t.inputSchema != null;
                    const schemaShown = schemaOpen.has(t.name);
                    return (
                      <div
                        key={t.name}
                        className="min-w-0 border-b border-border-soft bg-surface-2 last:border-0"
                      >
                        <label className="flex cursor-pointer items-start gap-3 py-2.5 pl-10 pr-3 transition-colors hover:bg-accent">
                          <input
                            type="checkbox"
                            checked={on}
                            onChange={() => onToggle(t.name)}
                            className="mt-0.5 h-4 w-4 shrink-0 rounded-sm border-input accent-primary"
                            aria-label={`Bind ${t.name}`}
                          />
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-2">
                              <span className="truncate font-mono text-sm">
                                {t.name}
                              </span>
                              {/* A person must approve it — that is hold, not
                                  a warning about a bound (§2.4). */}
                              {pending && (
                                <Badge variant="hold" className="shrink-0">
                                  pending approval
                                </Badge>
                              )}
                            </div>
                            {t.description && (
                              <p className="mt-0.5 truncate text-xs text-faint">
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
                              // A link, not a chip: a tag never carries an
                              // affordance (§2.3), so the disclosure is written
                              // in the link register instead.
                              className="shrink-0 self-center border-b border-accent text-xs font-semibold text-primary hover:border-primary"
                            >
                              {schemaShown ? "hide schema" : "schema"}
                            </button>
                          )}
                        </label>
                        {schemaShown && hasSchema && (
                          <pre className="mb-3 ml-10 mr-3 max-h-64 min-w-0 overflow-auto rounded-md bg-surface-3 p-3 font-mono text-xs text-secondary-foreground">
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

// ── agent.yaml helpers (a tiny, tolerant reader of our OWN serializer) ───────
// These parse only the fields our serializer emits (a full YAML lib is overkill
// for output we control) — the review summary + the tool round-trip. The BFF
// `expand` is the real validator; these are display + a targeted tools rewrite.

interface YAMLSummary {
  name: string;
  runtime: string;
  image?: string;
  model?: string;
  systemPrompt?: string;
  tools: string[];
}

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

/**
 * The model route, in both shapes our own writers emit: the flat `model: <id>`
 * from generation, and the nested block `model:\n  route: <alias>` from
 * `toAgentYAML`.
 *
 * The nested case used to be read with a top-level `route:` lookup, which never
 * matches an INDENTED key — so a config that plainly names `anthropic-primary`
 * rendered as "none — the registry default applies". That is not a formatting
 * miss, it is the review stating the opposite of the config it is reviewing,
 * which is the one thing this surface exists to prevent.
 */
function readModelRoute(yaml: string): string | undefined {
  const flat = scalar(yaml, "model");
  if (flat) return flat;
  const lines = yaml.split("\n");
  const start = lines.findIndex((l) => /^model:\s*$/.test(l));
  if (start >= 0) {
    for (let i = start + 1; i < lines.length; i++) {
      // A non-indented line ends the block.
      if (!/^\s+\S/.test(lines[i])) break;
      const m = /^\s+route:\s*(.+?)\s*$/.exec(lines[i]);
      if (m) return m[1].replace(/^"(.*)"$/, "$1") || undefined;
    }
  }
  return scalar(yaml, "route");
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
  const model = readModelRoute(yaml);
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
// surface renders the calm permission boundary (explain-and-suggest).
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
