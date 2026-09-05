import * as React from "react";
import { useNavigate } from "react-router-dom";
import { KeyRound, Lock, PlugZap } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { FieldError } from "@/components/ui/label";
import { FormField } from "@/components/config/form-field";
import {
  CredentialSourceBadge,
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuietNote,
  SectionHeader,
  Wizard,
  useToast,
  type KeyValueItem,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { cn } from "@/lib/utils";
import { api, ApiError, type ConnectProviderResponse } from "@/lib/api";

// ConnectProviderPage — the FIRST UI of the aha (spec §5, ADR 0015; M151 §6.1
// archetype A4). A guided wizard that connects a provider by pasting a key ONCE:
//
//   1. Provider  — pick who serves the model (anthropic / openai / google / …).
//   2. API key   — paste the key (+ optional base URL for OpenAI-compatible).
//                  The step's forward action SUBMITS: the BFF validates the key
//                  and creates the Secret / SecretBinding / ModelRoute
//                  server-side (caller-scoped) and returns the LIVE model list.
//   3. Review    — render that live model list + the created `secretName`.
//
// ── THE KEY NEVER LIVES CLIENT-SIDE AFTER SUBMIT ─────────────────────────────
// It is held ONLY in the key field's local state until submit, snapshotted into
// the POST body, and CLEARED the instant the request is dispatched. It is never
// written to a store / localStorage / sessionStorage / URL, never echoed back
// into the field, and never placed in a `title` attribute. After connect the UI
// shows the model list, the `secretName` reference, and a CredentialSourceBadge
// — WHERE the credential came from, never what it is.
//
// ── VALIDATION HAPPENS ON STEP-FORWARD, AND CAN END THREE WAYS ───────────────
// Continuing off the key step submits, and that is the moment this page is most
// likely to be wrong about something. "Couldn't connect" would fuse three
// different truths into one unusable sentence, so they stay apart:
//
//   • REJECTED  — the API reached the provider and the provider refused the
//                 credential. The key (or the base URL beside it) has to change.
//                 Crit: the key field is marked invalid with a FieldError, and
//                 the provider's own words are quoted verbatim.
//   • NEVER RAN — the request never came back at all (offline, DNS, the BFF is
//                 down). NOTHING was sent to the provider and nothing was
//                 stored; the key may be perfectly good. Warn, no field blamed,
//                 and the same submit is worth retrying unchanged.
//   • NOT WIRED — the API answered 501: this install cannot validate credentials
//                 here. Nothing tried, nothing broken, retrying will not help —
//                 a QuietNote (§7.1), never an error.
//
// A rejected key is a fact about the key. A request that never left the browser
// is a fact about the network. Telling a user to "check the key" for the second
// one sends them to rotate a credential that was fine.
//
// The other two terminal states:
//   • viewer (403)      → ForbiddenInline (the API is the real gate; ADR 0011).
//   • kill-switch (404) → the "provider-connect is disabled" fallback, pointing
//     at reference-an-existing-SecretBinding (hardened install).

// The providers the connect flow supports. `custom` needs a base URL (an
// OpenAI-compatible endpoint); the rest default to the provider's own API.
const PROVIDERS = [
  { id: "anthropic", name: "Anthropic", models: "Claude", needsBaseURL: false },
  { id: "openai", name: "OpenAI", models: "GPT", needsBaseURL: false },
  // Google/Gemini is deliberately NOT a tile. It was one, and the BFF rejected
  // `google` as an unsupported provider — so picking it meant typing an API key
  // into a form that could never work (found by the M153 journey test; the gate
  // hack/provider-parity.sh now fails the build on any such divergence).
  //
  // The capability is not lost: Gemini serves an OpenAI-compatible API, so it
  // connects today through "Custom / OpenAI-compatible" with the base URL
  // https://generativelanguage.googleapis.com/v1beta/openai. A native tile means
  // a native probe path, which is real work with no way to prove it offline —
  // carded rather than guessed at (m52, Theme R).
  {
    id: "custom",
    name: "Custom / OpenAI-compatible",
    models: "base URL",
    needsBaseURL: true,
  },
] as const;

type ProviderId = (typeof PROVIDERS)[number]["id"];

/** How the credential check ended. See the header note — three different truths. */
type Failure =
  | { reach: "rejected"; message: string; status: number }
  | { reach: "never-ran"; message: string }
  | { reach: "not-wired" };

// Submit is the connect request lifecycle. `failed` is the key-step honest
// failure; `forbidden` and `killed` are the two terminal non-key states.
type Submit =
  | { kind: "idle" }
  | { kind: "connecting" }
  | { kind: "connected"; res: ConnectProviderResponse }
  | { kind: "failed"; failure: Failure }
  | { kind: "forbidden"; message: string }
  | { kind: "killed" };

/**
 * Sort a thrown error into the three outcomes. 403 (RBAC) and 404 (kill-switch)
 * are terminal page states and are peeled off by the caller first.
 *
 * The last line carries the doctrine: anything that is NOT an ApiError never
 * completed a request, so the provider never saw the key and nothing about it
 * was learned.
 */
function classify(err: unknown): Failure {
  if (err instanceof ApiError) {
    if (err.isNotImplemented) return { reach: "not-wired" };
    return { reach: "rejected", message: err.message, status: err.status };
  }
  return {
    reach: "never-ran",
    message: err instanceof Error ? err.message : "the request never completed",
  };
}

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
  const { canFlow, reprobe } = useCapabilities();
  const { workingNamespace } = useNamespace();
  // The SERVER answers whether the whole connect flow is completable — see flows.go.
  const canConnect = canFlow("connectProvider");

  const provider = PROVIDERS.find((p) => p.id === providerId)!;

  // A verdict describes the credential that was SUBMITTED. The moment any of
  // the inputs change it stops describing anything, so it goes.
  function clearVerdict() {
    setSubmit((s) => (s.kind === "failed" ? { kind: "idle" } : s));
  }

  async function onConnect() {
    setSubmit({ kind: "connecting" });
    // Snapshot the key into the request body, then WIPE it from component state
    // immediately — it never survives the submit in any client-side store.
    const req = {
      provider: providerId,
      // Name the namespace explicitly. Omitting it let the BFF fall back to a hardcoded
      // "default", so the console wrote somewhere the user had not selected while the
      // capability probe asked about a third namespace entirely.
      ...(workingNamespace ? { namespace: workingNamespace } : {}),
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
        // The CONNECTION's name, not the vendor's: a user who named this one
        // "anthropic-prod" should not be told "Connected Anthropic" and left
        // wondering which of their three keys just landed.
        title: `Connected ${connection.trim() || provider.name}`,
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
      }
      setSubmit({ kind: "failed", failure: classify(err) });
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
  const failure = submit.kind === "failed" ? submit.failure : null;
  // Only a REJECTION is a fact about the key. The field is emptied at submit, so
  // the error line has to say "paste it again" — otherwise a field that blanked
  // itself and turned red reads as a bug in the console rather than a verdict on
  // the credential.
  const keyRejected = failure?.reach === "rejected";
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
          resource="provider credentials"
          permission="create"
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
        <div className="space-y-5">
          <SectionHeader
            title="Who serves the model"
            lede="One provider per connection. You can connect more later — an agent picks the route it runs on, not the vendor."
          />
          <div className="grid gap-3 sm:grid-cols-2">
            {PROVIDERS.map((p) => {
              const active = p.id === providerId;
              return (
                <button
                  key={p.id}
                  type="button"
                  aria-pressed={active}
                  onClick={() => setProviderId(p.id)}
                  className={cn(
                    "flex items-center gap-3 rounded-lg border p-3 text-left transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                    // Selection is always pine-family, never a status hue (§2.3).
                    active
                      ? "border-primary bg-accent text-accent-foreground"
                      : "border-border bg-card hover:bg-surface-2",
                  )}
                >
                  <span
                    className={cn(
                      "flex h-9 w-9 shrink-0 items-center justify-center rounded-md",
                      active ? "bg-primary text-primary-foreground" : "bg-surface-2 text-faint",
                    )}
                  >
                    <PlugZap className="h-4 w-4" />
                  </span>
                  <span className="min-w-0">
                    <span className="block text-sm font-medium">{p.name}</span>
                    <span className="block text-xs text-faint">{p.models}</span>
                  </span>
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
        <div className="space-y-5">
          <SectionHeader
            title="The key"
            lede={`Continuing sends it to the console’s API, which checks it against ${provider.name} before anything is stored. Nothing is created if the check fails.`}
          />
          <div className="space-y-4">
            <FormField id="provider-key" label={`${provider.name} API key`}>
              <div className="relative">
                <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-faint" />
                <Input
                  id="provider-key"
                  type="password"
                  autoComplete="off"
                  value={apiKey}
                  onChange={(e) => { clearVerdict(); setApiKey(e.target.value); }}
                  placeholder="Paste your key…"
                  className="pl-9 font-mono text-xs"
                  aria-invalid={keyRejected || undefined}
                  aria-describedby={keyRejected ? "provider-key-error" : undefined}
                />
              </div>
              <FieldError id="provider-key-error">
                {keyRejected
                  ? `${provider.name} didn’t accept this key. It isn’t kept after a submit — paste it again.`
                  : undefined}
              </FieldError>
            </FormField>
            {provider.needsBaseURL && (
              <FormField id="provider-base-url" label="Base URL">
                <Input
                  id="provider-base-url"
                  value={baseURL}
                  onChange={(e) => { clearVerdict(); setBaseURL(e.target.value); }}
                  placeholder="https://api.example.com/v1"
                  className="font-mono text-xs"
                />
                <p className="text-xs text-faint">
                  The OpenAI-compatible endpoint the key belongs to. A rejected
                  key on a custom endpoint can mean either one is wrong.
                </p>
              </FormField>
            )}
            <FormField id="provider-connection" label="Connection name (optional)">
              <Input
                id="provider-connection"
                value={connection}
                onChange={(e) => { clearVerdict(); setConnection(e.target.value); }}
                placeholder={providerId}
                className="font-mono text-xs"
                data-testid="connect-connection-name"
              />
              <p className="text-xs text-faint">
                Name this connection to hold <em>multiple keys</em> for the same
                provider (e.g. <code>{providerId}-prod</code>,{" "}
                <code>{providerId}-team-x</code>). Leave blank to use{" "}
                <code>{providerId}</code>.
              </p>
            </FormField>
          </div>

          <QuietNote title="Where the key goes.">
            Straight to the console’s API, which validates it against the
            provider and stores it as a Kubernetes Secret under <em>your</em>{" "}
            identity. It never comes back to the browser and is never logged.
          </QuietNote>

          {failure?.reach === "rejected" && (
            <div
              className="rounded-lg border border-destructive/40 bg-destructive-surface/40 px-4 py-3"
              role="alert"
              data-testid="connect-error"
            >
              <p className="font-serif text-md font-medium text-destructive">
                {provider.name} refused that credential.
              </p>
              {/* The provider's own words, verbatim — the one clue a reader can
                  act on. Never paraphrased into "something went wrong". */}
              <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
                {failure.message} ({failure.status})
              </pre>
              <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
                Nothing was stored. Check the key hasn’t expired or been revoked
                {provider.needsBaseURL ? ", and that the base URL is the endpoint it belongs to" : ""}
                , then try again.
              </p>
            </div>
          )}

          {failure?.reach === "never-ran" && (
            <div
              className="rounded-lg border border-warning/40 bg-warning-surface/40 px-4 py-3"
              role="alert"
              data-testid="connect-unreachable"
            >
              <p className="font-serif text-md font-medium text-warning">
                The check never ran.
              </p>
              <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
                {failure.message}
              </pre>
              <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
                The request didn’t reach the console’s API, so the key never
                reached {provider.name} and nothing was stored. That says nothing
                about the key itself. Check your connection, then paste it again.
              </p>
            </div>
          )}

          {failure?.reach === "not-wired" && (
            <div data-testid="connect-unsupported">
              <QuietNote title="Connecting a provider isn’t wired up on this install.">
                The console asked its API to validate the key, and the API
                answered that it does not do that here. Nothing was sent to{" "}
                {provider.name} and nothing was stored — trying again will not
                change it. An operator either turns the connect flow on, or
                creates the SecretBinding for you.
              </QuietNote>
            </div>
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
          <SectionHeader
            title="Not connected yet"
            lede="The model list comes from the provider itself, so it only exists once your key has been checked. Go back a step and submit it."
          />
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
      {!canConnect && <ReadOnlyNote />}
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
        // Cancel belongs in the footer (§6.1 A4) while there is still something
        // to cancel. Once the provider IS connected there is nothing to abandon,
        // and the review step offers "Done — back to Providers" instead.
        onCancel={
          submit.kind === "connected" ? undefined : () => navigate("/providers")
        }
        dirty={apiKey !== "" || connection.trim() !== "" || baseURL.trim() !== ""}
      />
    </PageFrame>
  );
}

// PageFrame — the §6.1 A4 shell: the page band, then a column wide enough for
// the Wizard's 15rem rail + its 2rem gap + the 46rem content column the
// archetype fixes. Capping the OUTER column is what sizes the inner one without
// reaching into the kit. Shared by the wizard, the forbidden state, and the
// kill-switch fallback.
function PageFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Connect a provider"
        lede="Paste a key once. The console checks it with the provider, stores it server-side, and the models it unlocks become available to your agents — no YAML, no kubectl."
      />
      <div className="max-w-[63rem] space-y-5">{children}</div>
    </div>
  );
}

// ReadOnlyNote — the display-only RBAC gate above the wizard (ADR 0011). A
// permission boundary is routine, not a failure, so it reads in the calm lock
// register (M99 C1); the API is still the real gate.
function ReadOnlyNote() {
  return (
    <div
      className="flex items-start gap-3 rounded-lg border border-border bg-surface-2/40 px-4 py-3"
      data-testid="connect-readonly-note"
    >
      <Lock className="mt-0.5 h-4 w-4 shrink-0 text-faint" />
      <p className="max-w-[64ch] text-sm text-secondary-foreground">
        You have read-only access here. Connecting a provider stores a credential
        in this cluster, which your account can’t do — an admin can connect one
        for you, or grant your account that role.
      </p>
    </div>
  );
}

// ReviewStep renders the LIVE model list from the connect response + the created
// route reference. No secret material — only the `secretName` reference and a
// CredentialSourceBadge saying where the credential came from.
function ReviewStep({
  res,
  onDone,
}: {
  res: ConnectProviderResponse;
  onDone: () => void;
}) {
  const models = res.provider.models ?? [];
  const label = res.provider.displayName || res.provider.provider;
  const facts: KeyValueItem[] = [
    { key: "Connection", value: label },
    { key: "Provider", value: res.provider.provider },
    { key: "Models", value: models.length },
    {
      key: "Credential",
      // What it IS is never rendered; where it came from is (kit
      // CredentialSourceBadge). Every agent on this route uses this one key.
      value: <CredentialSourceBadge credentialSource="shared" name={res.provider.provider} />,
      mono: false,
    },
    {
      key: "State",
      value: res.provider.ready ? (
        <Badge variant="ok">ready</Badge>
      ) : (
        <Badge variant="progressing">settling</Badge>
      ),
      mono: false,
    },
  ];

  return (
    <div className="space-y-5">
      <SectionHeader
        title={`${label} is connected`}
        lede={
          models.length === 0
            ? "The key was accepted, but the provider listed no models on it. Check the key's scope with the provider."
            : `The provider listed ${models.length} model${models.length === 1 ? "" : "s"} on this key. They are ready to route to.`
        }
      />
      <KeyValueList items={facts} />

      <div className="space-y-2">
        <p className="font-mono text-2xs uppercase tracking-wide text-faint">
          Available models
        </p>
        <div className="space-y-2" data-testid="model-list">
          {models.map((m: string) => (
            <div
              key={m}
              className="flex items-center gap-3 rounded-md border border-border bg-surface-2/40 px-3 py-2 text-sm"
            >
              <span className="font-mono">{m}</span>
            </div>
          ))}
        </div>
      </div>

      <QuietNote title="The key stayed server-side.">
        It is stored under your identity and never returned to the browser — the
        console only ever sees the model list above and the reference below.
      </QuietNote>

      {/* Storage plumbing is Advanced detail — collapsed by default so the happy
          path reads as intent ("connected, models ready"), not Kubernetes nouns. */}
      <details className="rounded-lg border border-border bg-surface-2/40 px-4 py-3 text-sm text-secondary-foreground">
        <summary className="cursor-pointer select-none font-medium text-foreground">
          Advanced: how it’s stored
        </summary>
        <p className="mt-2 max-w-[64ch] leading-relaxed">
          Stored as{" "}
          <span className="font-mono text-xs text-foreground">
            {res.provider.secretName}
          </span>{" "}
          — a Secret (the key), a SecretBinding (the reference), and a ModelRoute
          (which provider serves which models), all created under your identity.
          You never need to touch these for the common path.
        </p>
      </details>

      {/* Secondary action — the primary "Create an agent with this" lives in the
          wizard footer; "Done" just returns to Providers. */}
      <button
        type="button"
        onClick={onDone}
        data-testid="connect-done"
        className="text-sm font-medium text-primary underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
      >
        Done — back to Providers
      </button>
    </div>
  );
}

// KillSwitchFallback — the hardened-install state: provider-connect is disabled
// (the endpoint 404s). Nothing is broken, so it reads as a calm note (§7.1)
// rather than an error, and it teaches the operator-driven alternative instead
// of dead-ending (ADR 0015 kill-switch).
function KillSwitchFallback() {
  return (
    <PageFrame>
      <div data-testid="kill-switch-fallback">
        <QuietNote title="Provider-connect is turned off on this cluster.">
          <p>
            This is a hardened install — the guided connect flow is disabled and
            provider keys are managed out-of-band by an operator.
          </p>
          <p className="mt-2">
            To run an agent, reference an existing{" "}
            <span className="font-mono text-xs text-foreground">SecretBinding</span>{" "}
            +{" "}
            <span className="font-mono text-xs text-foreground">ModelRoute</span>{" "}
            (created by your operator) when you configure the agent.
          </p>
        </QuietNote>
      </div>
    </PageFrame>
  );
}
