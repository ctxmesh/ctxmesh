import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Check, KeyRound, PlugZap, ShieldAlert } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ForbiddenInline,
  Wizard,
  useToast,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_SECRETS } from "@/lib/nav";
import { api, ApiError, type ConnectProviderResponse } from "@/lib/api";

// ConnectProviderPage — the FIRST UI of the aha (spec §5, ADR 0015). A guided
// wizard that connects a provider by pasting a key ONCE:
//
//   1. Provider  — pick who serves the model (anthropic / openai / google / …).
//   2. API key   — paste the key (+ optional base URL for OpenAI-compatible).
//                  The step's forward action SUBMITS: the BFF validates the key
//                  and creates the Secret / SecretBinding / ModelRoute
//                  server-side (caller-scoped) and returns the LIVE model list.
//   3. Review    — render that live model list + the created `secretName`.
//
// THE KEY NEVER LIVES CLIENT-SIDE AFTER SUBMIT. It is held ONLY in the key
// field's local state until submit, snapshotted into the POST body, and CLEARED
// the instant the request is dispatched. It is never written to a store /
// localStorage / sessionStorage / URL. After connect the UI only ever shows the
// model list + the `secretName` reference — the key is server-side.
//
// Failure modes are honest (never swallowed):
//   • bad key (400/401) → inline error on the key step + retry.
//   • viewer (403)      → ForbiddenInline (the API is the real gate; ADR 0011).
//   • kill-switch (404) → the "provider-connect is disabled" fallback state
//     pointing at reference-an-existing-SecretBinding (hardened install).

// The providers the connect flow supports. `custom` needs a base URL (an
// OpenAI-compatible endpoint); the rest default to the provider's own API.
const PROVIDERS = [
  { id: "anthropic", name: "Anthropic", models: "Claude", needsBaseURL: false },
  { id: "openai", name: "OpenAI", models: "GPT", needsBaseURL: false },
  { id: "google", name: "Google", models: "Gemini", needsBaseURL: false },
  {
    id: "custom",
    name: "Custom / OpenAI-compatible",
    models: "base URL",
    needsBaseURL: true,
  },
] as const;

type ProviderId = (typeof PROVIDERS)[number]["id"];

// Submit is the connect request lifecycle. `error` is the key-step honest
// failure; `forbidden` and `killed` are the two terminal non-key states.
type Submit =
  | { kind: "idle" }
  | { kind: "connecting" }
  | { kind: "connected"; res: ConnectProviderResponse }
  | { kind: "error"; message: string; status?: number }
  | { kind: "forbidden"; message: string }
  | { kind: "killed" };

const STEP_PROVIDER = 0;
const STEP_KEY = 1;
const STEP_REVIEW = 2;

export function ConnectProviderPage() {
  const navigate = useNavigate();
  const [current, setCurrent] = React.useState(STEP_PROVIDER);
  const [providerId, setProviderId] = React.useState<ProviderId>("anthropic");
  // ── The key lives ONLY here — a local field value, cleared on submit. ──────
  const [apiKey, setApiKey] = React.useState("");
  const [baseURL, setBaseURL] = React.useState("");
  // m22 (named connections, ADR 0026): an optional connection NAME lets a user
  // hold multiple keys per provider type (anthropic-prod, anthropic-team-x).
  // Empty → defaults to the provider type (the existing single-connection behavior).
  const [connection, setConnection] = React.useState("");
  const [submit, setSubmit] = React.useState<Submit>({ kind: "idle" });

  const { toast } = useToast();
  // RBAC-aware chrome (§3): connecting a provider creates a Secret + binding +
  // route — a write. A viewer (no create on secretbindings) sees the gated entry
  // in the shell; if they reach this page directly the API 403 is the real gate.
  // DISPLAY-ONLY (ADR 0011).
  const { can, reprobe } = useCapabilities();
  const canConnect = can(RES_SECRETS, "create");

  const provider = PROVIDERS.find((p) => p.id === providerId)!;

  async function onConnect() {
    setSubmit({ kind: "connecting" });
    // Snapshot the key into the request body, then WIPE it from component state
    // immediately — it never survives the submit in any client-side store.
    const req = {
      provider: providerId,
      ...(connection.trim() ? { connection: connection.trim() } : {}),
      displayName: connection.trim() || provider.name,
      apiKey,
      ...(provider.needsBaseURL && baseURL ? { baseURL } : {}),
    };
    setApiKey("");
    try {
      const res = await api.connectProvider(req);
      setSubmit({ kind: "connected", res });
      setCurrent(STEP_REVIEW);
      toast({
        variant: "success",
        title: `Connected ${provider.name}`,
        description: `${(res.provider.models ?? []).length} model${(res.provider.models ?? []).length === 1 ? "" : "s"} available`,
      });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isNotFound) {
          setSubmit({ kind: "killed" });
          return;
        }
        if (err.isForbidden) {
          // A 403 despite an optimistic "yes" means the cached map was stale —
          // correct it (reprobe) AND surface the real 403 (ForbiddenInline).
          reprobe();
          setSubmit({ kind: "forbidden", message: err.message });
          return;
        }
        setSubmit({ kind: "error", message: err.message, status: err.status });
        return;
      }
      setSubmit({
        kind: "error",
        message: err instanceof Error ? err.message : "request failed",
      });
    }
  }

  // The Wizard's forward button advances the index; we intercept so that leaving
  // the KEY step (1 → 2) SUBMITS instead — the review step is entered only on a
  // successful connect (onConnect sets current). Backward moves pass through.
  function onStepChange(next: number) {
    if (current === STEP_KEY && next > STEP_KEY) {
      void onConnect();
      return;
    }
    setCurrent(next);
  }

  const connecting = submit.kind === "connecting";
  const keyReady =
    apiKey.trim().length > 0 &&
    (!provider.needsBaseURL || baseURL.trim().length > 0);

  // The kill-switch is a whole-page fallback (the flow doesn't exist here).
  if (submit.kind === "killed") return <KillSwitchFallback />;

  // A hard 403 replaces the wizard body with the explain-and-suggest primitive.
  if (submit.kind === "forbidden") {
    return (
      <PageFrame>
        <ForbiddenInline
          title="Not allowed to connect a provider"
          description="Connecting a provider securely stores your API key, which needs a permission your account doesn't have on this cluster. An admin can connect the provider for you, or grant your account that permission."
          detail={submit.message}
        />
      </PageFrame>
    );
  }

  const steps: WizardStep[] = [
    {
      id: "provider",
      title: "Provider",
      description: "Pick who serves the model",
      content: (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Which provider are you connecting?
          </p>
          <div className="grid grid-cols-2 gap-3">
            {PROVIDERS.map((p) => {
              const active = p.id === providerId;
              return (
                <button
                  key={p.id}
                  type="button"
                  aria-pressed={active}
                  onClick={() => setProviderId(p.id)}
                  className={`flex items-center gap-3 rounded-lg border p-3 text-left transition-colors hover:bg-surface-2 ${
                    active ? "border-primary ring-1 ring-primary" : ""
                  }`}
                >
                  <div className="flex h-9 w-9 items-center justify-center rounded-md bg-surface-2">
                    <PlugZap className="h-4 w-4 text-primary" />
                  </div>
                  <div>
                    <p className="text-sm font-medium">{p.name}</p>
                    <p className="text-xs text-muted-foreground">{p.models}</p>
                  </div>
                </button>
              );
            })}
          </div>
        </div>
      ),
    },
    {
      id: "key",
      title: "API key",
      description: "Pasted once, stored server-side",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="provider-key">{provider.name} API key</Label>
            <div className="relative">
              <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                id="provider-key"
                type="password"
                autoComplete="off"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder="Paste your key…"
                className="pl-9 font-mono text-xs"
              />
            </div>
          </div>
          {provider.needsBaseURL && (
            <div className="space-y-1.5">
              <Label htmlFor="provider-base-url">Base URL</Label>
              <Input
                id="provider-base-url"
                value={baseURL}
                onChange={(e) => setBaseURL(e.target.value)}
                placeholder="https://api.example.com/v1"
                className="font-mono text-xs"
              />
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="provider-connection">Connection name (optional)</Label>
            <Input
              id="provider-connection"
              value={connection}
              onChange={(e) => setConnection(e.target.value)}
              placeholder={providerId}
              className="font-mono text-xs"
              data-testid="connect-connection-name"
            />
            <p className="text-xs text-muted-foreground">
              Name this connection to hold <em>multiple keys</em> for the same
              provider (e.g. <code>{providerId}-prod</code>,{" "}
              <code>{providerId}-team-x</code>). Leave blank to use{" "}
              <code>{providerId}</code>.
            </p>
          </div>
          <div className="rounded-md border border-info/30 bg-info/5 p-3 text-xs text-muted-foreground">
            The key goes straight to the BFF, which validates it against the
            provider and stores it as a Kubernetes Secret under <em>your</em>{" "}
            identity. It never returns to the browser and is never logged.
          </div>
          {submit.kind === "error" && (
            <p
              className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
              role="alert"
              data-testid="connect-error"
            >
              Couldn&apos;t connect: {submit.message}
              {submit.status ? ` (${submit.status})` : ""}. Check the key and
              try again.
            </p>
          )}
        </div>
      ),
    },
    {
      id: "review",
      title: "Done",
      review: true,
      content:
        submit.kind === "connected" ? (
          <ReviewStep res={submit.res} onDone={() => navigate("/providers")} />
        ) : (
          <p className="text-sm text-muted-foreground">
            Submit your key to validate it and list the provider&apos;s models.
          </p>
        ),
    },
  ];

  // Per-step forward gating. Provider → always; Key → a key (and base URL for
  // custom) present AND the caller may create (display-only); Review → active once
  // connected, where the finish action is "Create an agent with this" (no dead button).
  const canProceed =
    current === STEP_PROVIDER
      ? true
      : current === STEP_KEY
        ? keyReady && canConnect
        : submit.kind === "connected";

  return (
    <PageFrame>
      {!canConnect && (
        <p
          className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-center text-xs text-muted-foreground"
          data-testid="connect-readonly-note"
        >
          Your account doesn&apos;t have permission to connect a provider on
          this cluster (it needs to store a credential). An admin can connect
          one for you, or grant your account that permission.
        </p>
      )}
      <div className="rounded-lg border bg-card p-6 shadow-card">
        <Wizard
          steps={steps}
          current={current}
          onStepChange={onStepChange}
          canProceed={canProceed}
          busy={connecting}
          // The KEY step's forward action IS the submit (intercepted above).
          nextLabel={current === STEP_KEY ? "Connect provider" : "Continue"}
          // The Done step's finish keeps the momentum: straight into creating an
          // agent that uses the model you just connected (the models are already
          // in the create-agent picker). "Done" back to Providers is the secondary
          // action inside ReviewStep.
          finishLabel="Create an agent with this"
          onFinish={() => navigate("/agents/new")}
        />
      </div>
    </PageFrame>
  );
}

// PageFrame — the page header + a max-width column shared by the wizard, the
// forbidden state, and the kill-switch fallback.
function PageFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">
          Connect a provider
        </h2>
        <p className="text-sm text-muted-foreground">
          Paste a key once → validated server-side → pick a model. The first
          step of running your first agent (no YAML, no kubectl).
        </p>
      </div>
      {children}
    </div>
  );
}

// ReviewStep renders the LIVE model list from the connect response + the created
// route reference. No secret material — only the `secretName` reference.
function ReviewStep({
  res,
  onDone,
}: {
  res: ConnectProviderResponse;
  onDone: () => void;
}) {
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-success">
        <Check className="h-5 w-5" />
        <p className="text-sm font-medium text-foreground">
          {res.provider.displayName || res.provider.provider} connected —{" "}
          {(res.provider.models ?? []).length} model
          {(res.provider.models ?? []).length === 1 ? "" : "s"} available
        </p>
      </div>
      <div className="space-y-1.5">
        <p className="text-sm font-medium">Available models</p>
        <div className="space-y-2" data-testid="model-list">
          {(res.provider.models ?? []).map((m: string) => (
            <div
              key={m}
              className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-3 py-2 text-sm"
            >
              <span className="font-mono">{m}</span>
            </div>
          ))}
        </div>
      </div>
      <div className="rounded-md border bg-surface-2/40 px-3 py-2 text-xs text-muted-foreground">
        Your key is stored securely under your identity and stays server-side —
        the browser only ever sees the models above, never the key.
      </div>

      {/* Storage plumbing is Advanced detail — collapsed by default so the happy
          path reads as intent ("connected, models ready"), not Kubernetes nouns. */}
      <details className="rounded-md border bg-surface-2/20 px-3 py-2 text-xs text-muted-foreground">
        <summary className="cursor-pointer select-none font-medium text-foreground">
          Advanced: how it&apos;s stored
        </summary>
        <p className="mt-2 leading-relaxed">
          Stored as{" "}
          <span className="font-mono text-foreground">
            {res.provider.secretName}
          </span>{" "}
          — a Secret (the key), a SecretBinding (the reference), and a
          ModelRoute (which provider serves which models), all created under
          your identity. You never need to touch these for the common path.
        </p>
      </details>

      {/* Secondary action — the primary "Create an agent with this" lives in the
          wizard footer; "Done" just returns to Providers. */}
      <button
        type="button"
        onClick={onDone}
        data-testid="connect-done"
        className="text-xs font-medium text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
      >
        Done — back to Providers
      </button>
    </div>
  );
}

// KillSwitchFallback — the hardened-install state: provider-connect is disabled
// (the endpoint 404s), so we teach the operator-driven alternative rather than
// dead-ending (ADR 0015 kill-switch).
function KillSwitchFallback() {
  return (
    <PageFrame>
      <Card data-testid="kill-switch-fallback">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldAlert className="h-4 w-4 text-warning" />
            Provider-connect is disabled on this cluster
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm text-muted-foreground">
          <p>
            This is a hardened install — the guided connect flow is turned off.
            Provider keys are managed out-of-band by an operator.
          </p>
          <p>
            To run an agent, reference an existing{" "}
            <span className="font-mono text-foreground">SecretBinding</span> +{" "}
            <span className="font-mono text-foreground">ModelRoute</span>{" "}
            (created by your operator) when you configure the agent.
          </p>
        </CardContent>
      </Card>
    </PageFrame>
  );
}
