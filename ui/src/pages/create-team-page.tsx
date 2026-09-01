import * as React from "react";
import { useNavigate } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  ClosingNote,
  ErrorState,
  KeyValueList,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  Wizard,
  useToast,
  type KeyValueItem,
  type WizardStep,
} from "@/components/kit";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type AgentRegistrySummary,
  type AgentTeamSummary,
  type GenerateTeamResponse,
} from "@/lib/api";

// CreateTeamPage — "describe a team, review the roster it proposes" (m71.7, ADR
// 0065 D4; redesigned onto the editorial system in M151, spec §6.1 archetype A4
// and the `create-team-page` row of §6.2).
//
// ── THE SHAPE: two steps, and the second one is the finish line ─────────────
//
// Describe → Review. It is the kit `Wizard` (§5.12), not a hand-rolled stage
// machine, so the step rail, the forward gating, the focus move on step change
// and — the one that matters — the Esc/Cancel discard guard all come from the
// same place every other creation flow gets them. The guard is armed with a
// real `dirty` (a typed description, or a roster already proposed), because a
// wizard that drops a half-written team on a stray Escape is worse than an ugly
// one.
//
// Generation runs on the FORWARD press (the Wizard's documented "async work on
// Next" contract): the button says what it will do — "Generate the roster" —
// and the step does not advance until a roster actually exists.
//
// ── THE HONESTY RULE THIS PAGE TURNS ON: A PROPOSAL IS NOT A FACT ───────────
//
// Everything on the review step was written by a model and confirmed by nobody.
// So the step says so, in three places that cannot drift apart: a `proposed`
// tag (the `open` variant — declared, never exercised), the composing model
// named in mono beside it, and a sentence stating the team does not exist yet.
// The word "create" appears on exactly one control, the one that does it.
//
// The same rule governs the eligible-member set. `eligibleMembers` arriving
// empty is NOT "no agents were eligible" — a generation that succeeded proves
// otherwise — it is "the generator did not report them". It therefore renders
// as a stated absence with a title, never as a zero and never as an empty list
// that implies emptiness (§7.1: unknown and zero never share a glyph).
//
// ── WHERE FAILURES LAND ────────────────────────────────────────────────────
//
// Inline, in the step that caused them (§7, A4) — never a toast, never a page
// swap. A 422 keyed on the `regenerate` FLAG (never the status, the ADR 0014
// landmine) keeps the description on screen so the user edits rather than
// retypes; the empty-registry variant of it is a different problem with a
// different next step, and says so. A create 403 renders the calm permission
// boundary (ErrorState forbidden, `permission: "create"`), because "you may
// read teams but not make one" is a routine fact about a role, not a failure.
//
// data-testid contract:
//   create-team-page      — root
//   registry-select       — registry picker
//   team-description      — description textarea
//   roster-review         — the review step's body
//   team-supervisor       — supervisor agentRef in the review
//   team-roster-entry-{n} — each roster entry in the review (0-indexed)
//   regenerate-hint       — the 422 reason display
//   empty-registry-hint   — the empty-registry 422 hint
//   team-yaml             — the raw team.yaml code well
//
// The forward control is the kit Wizard's own button ("Generate the roster" /
// "Create the team"); it carries no testid because the kit owns it. Tests reach
// it by its accessible name, which is also what a person reads.

const STEP_DESCRIBE = 0;
const STEP_REVIEW = 1;

/** How the registry picker's source answered. Drives which honest note shows. */
type RegistryLoad = "loading" | "ready" | "error";

/** What the generator handed back, and whether a person has acted on it yet. */
type Phase = "idle" | "generating" | "creating" | "created";

interface GenIssue {
  reason: string;
  /** The registry has nothing publishable — a different problem, different fix. */
  emptyRegistry: boolean;
}

interface CreateFailure {
  message: string;
  forbidden: boolean;
}

export function CreateTeamPage() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { namespace } = useNamespace();

  const [registries, setRegistries] = React.useState<AgentRegistrySummary[]>([]);
  const [registryLoad, setRegistryLoad] = React.useState<RegistryLoad>("loading");
  const [registryRef, setRegistryRef] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [targetNs, setTargetNs] = React.useState(namespace || "default");

  const [current, setCurrent] = React.useState(STEP_DESCRIBE);
  const [phase, setPhase] = React.useState<Phase>("idle");
  const [gen, setGen] = React.useState<GenerateTeamResponse | null>(null);
  const [genIssue, setGenIssue] = React.useState<GenIssue | null>(null);
  const [createFailure, setCreateFailure] = React.useState<CreateFailure | null>(
    null,
  );
  const [team, setTeam] = React.useState<AgentTeamSummary | null>(null);

  // Load available registries so the user picks one instead of typing a ref.
  React.useEffect(() => {
    const controller = new AbortController();
    api
      .listAgentRegistries(undefined, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRegistries(res.items);
        setRegistryLoad("ready");
        if (res.items.length > 0) {
          setRegistryRef((prev) => prev || res.items[0].name);
        }
      })
      .catch(() => {
        // A probe failure is not "no registries" — say which it was, and still
        // let the user name one by hand rather than dead-ending the flow.
        if (!controller.signal.aborted) setRegistryLoad("error");
      });
    return () => controller.abort();
  }, []);

  React.useEffect(() => {
    if (namespace) setTargetNs(namespace);
  }, [namespace]);

  // Editing the inputs invalidates the roster composed from them — otherwise
  // pressing forward again would walk into a review of the OLD team while the
  // form on screen describes a different one.
  function onDescriptionChange(v: string) {
    setDescription(v);
    setGen(null);
    setGenIssue(null);
  }
  function onRegistryChange(v: string) {
    setRegistryRef(v);
    setGen(null);
    setGenIssue(null);
  }

  async function generate() {
    if (!registryRef.trim() || !description.trim()) return;
    setPhase("generating");
    setGenIssue(null);
    try {
      const res = await api.generateTeam({
        description: description.trim(),
        registryRef: registryRef.trim(),
        namespace: targetNs || "default",
      });
      // Branch on the FLAG, never the status (the generateAgent landmine): a
      // regenerate outcome is a legitimate answer, not an exception.
      if (res.regenerate) {
        setPhase("idle");
        setGenIssue({
          reason:
            res.reason ?? res.error ?? "The generated team spec was not valid.",
          emptyRegistry: isEmptyRegistry(`${res.error ?? ""} ${res.reason ?? ""}`),
        });
        return;
      }
      setGen(res);
      setPhase("idle");
      setCurrent(STEP_REVIEW);
    } catch (err) {
      const msg = describeError(err, "generation failed");
      setPhase("idle");
      setGenIssue({ reason: msg, emptyRegistry: isEmptyRegistry(msg) });
    }
  }

  async function create() {
    if (!gen) return;
    setPhase("creating");
    setCreateFailure(null);
    try {
      const created = await api.createTeam({
        teamYAML: gen.teamYAML,
        namespace: targetNs || "default",
      });
      setTeam(created);
      setPhase("created");
      toast({
        variant: "success",
        title: "Team created",
        description: `${created.name} — supervisor ${created.supervisor}, ${created.roster.length} member(s).`,
      });
      setTimeout(() => navigate("/teams"), 1200);
    } catch (err) {
      setPhase("idle");
      setCreateFailure({
        message: describeError(err, "create failed"),
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  const busy = phase === "generating" || phase === "creating";
  const described = registryRef.trim().length > 0 && description.trim().length > 0;
  // A typed description or a roster in hand is work that Escape must not drop.
  const dirty = description.trim().length > 0 || gen !== null;

  function onStepChange(next: number) {
    // Forward out of Describe IS the generation. The step only advances once a
    // roster exists, so the rail can never claim a review that isn't there.
    if (next === STEP_REVIEW && current === STEP_DESCRIBE && !gen) {
      void generate();
      return;
    }
    setCurrent(next);
  }

  const steps: WizardStep[] = [
    {
      id: "describe",
      title: "Describe it",
      description: "Registry + what the team does",
      content: (
        <DescribeStep
          registries={registries}
          registryLoad={registryLoad}
          registryRef={registryRef}
          onRegistryChange={onRegistryChange}
          description={description}
          onDescriptionChange={onDescriptionChange}
          namespace={targetNs}
          onNamespaceChange={setTargetNs}
          issue={genIssue}
          generating={phase === "generating"}
        />
      ),
    },
    {
      id: "review",
      title: "Review the roster",
      description: "Confirm before creating",
      review: true,
      content: gen ? (
        <ReviewStep
          gen={gen}
          namespace={targetNs || "default"}
          registryRef={registryRef}
          failure={createFailure}
          onDismiss={() => setCreateFailure(null)}
        />
      ) : null,
    },
  ];

  if (phase === "created" && team) {
    return (
      <Shell>
        <section aria-labelledby="team-created-head" className="min-w-0">
          <SectionHeader
            id="team-created-head"
            title="The team exists now"
            lede="Its supervisor and roster are live."
          />
          <div className="border border-border bg-card p-5">
            <KeyValueList
              items={[
                { key: "Team", value: team.name },
                { key: "Workspace", value: team.namespace },
                { key: "Registry", value: team.registry },
                { key: "Supervisor", value: team.supervisor },
                {
                  key: "Roster",
                  value: <QuantityValue value={team.roster.length} />,
                },
              ]}
            />
          </div>
          <ClosingNote>
            {team.name} created — redirecting to teams list…
          </ClosingNote>
        </section>
      </Shell>
    );
  }

  return (
    <Shell>
      <Wizard
        steps={steps}
        current={current}
        onStepChange={onStepChange}
        canProceed={current === STEP_DESCRIBE ? described : gen !== null}
        busy={busy}
        dirty={dirty}
        onCancel={() => navigate("/teams")}
        onFinish={() => void create()}
        nextLabel="Generate the roster"
        finishLabel="Create the team"
      />
    </Shell>
  );
}

// Shell — the A4 page band + the 46rem content column the archetype fixes. The
// header carries no actions on purpose: Cancel lives in the wizard footer, so
// there is exactly one way out and it sits with the other buttons.
function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-w-0 space-y-6" data-testid="create-team-page">
      <PageHeader
        title="New team"
        lede="Say what the team should get done. We compose a supervisor and a roster from the agents this registry has already published — and show you the whole thing before anything is created."
      />
      {/* The A4 content measure, on the header's own left margin (the band's
          px-6) so the h1 and every step heading share one edge. */}
      <div className="min-w-0 px-6">
        <div className="min-w-0 max-w-[46rem]">{children}</div>
      </div>
    </div>
  );
}

// ── Step 1 — describe it ────────────────────────────────────────────────────

function DescribeStep({
  registries,
  registryLoad,
  registryRef,
  onRegistryChange,
  description,
  onDescriptionChange,
  namespace,
  onNamespaceChange,
  issue,
  generating,
}: {
  registries: AgentRegistrySummary[];
  registryLoad: RegistryLoad;
  registryRef: string;
  onRegistryChange: (v: string) => void;
  description: string;
  onDescriptionChange: (v: string) => void;
  namespace: string;
  onNamespaceChange: (v: string) => void;
  issue: GenIssue | null;
  generating: boolean;
}) {
  const hasList = registryLoad === "ready" && registries.length > 0;
  return (
    <div className="min-w-0 space-y-6">
      <SectionHeader
        title="Describe the team"
        lede="A team is a supervisor that delegates to agents which already exist. Name the registry it may draw from, then say what the work is."
      />

      <div className="space-y-1.5">
        <Label htmlFor="registry-select">Agent registry</Label>
        {hasList ? (
          <Select
            id="registry-select"
            data-testid="registry-select"
            value={registryRef}
            onChange={(e) => onRegistryChange(e.target.value)}
          >
            {registries.map((r) => (
              <option key={`${r.namespace}/${r.name}`} value={r.name}>
                {r.name} ({r.namespace})
              </option>
            ))}
          </Select>
        ) : (
          <Input
            id="registry-select"
            data-testid="registry-select"
            className="font-mono"
            placeholder="registry name (e.g. prod-registry)"
            value={registryRef}
            onChange={(e) => onRegistryChange(e.target.value)}
          />
        )}
        <p className="text-xs text-faint">
          The team&apos;s eligible members are this registry&apos;s published
          agents.
        </p>
      </div>

      {/* The picker's source could not answer. Neither case is an error, and
          neither is a dead end — the field stays typeable (§7, A4). */}
      {registryLoad === "error" && (
        <QuietNote title="The registry list didn’t load.">
          Nothing is wrong with the team you are describing — the console just
          couldn&apos;t read the registries in this workspace, so it can&apos;t
          offer them as a list. Name the registry above and generation will use
          it. Nothing here is guessed; the list is simply absent.
        </QuietNote>
      )}
      {registryLoad === "ready" && registries.length === 0 && (
        <QuietNote title="No agent registry is visible here.">
          A team draws its members from a registry&apos;s published agents, and
          none is readable in this workspace — either none exists yet, or your
          role cannot list them. You can still name one above if you know it.
        </QuietNote>
      )}

      <div className="space-y-1.5">
        <Label htmlFor="team-description">What should the team do?</Label>
        <Textarea
          id="team-description"
          data-testid="team-description"
          rows={4}
          value={description}
          onChange={(e) => onDescriptionChange(e.target.value)}
          placeholder="An orchestrator that delegates research to a web searcher and summarisation to a writer agent…"
        />
        <p className="text-xs text-faint">
          One or two sentences. The supervisor and the split of work are composed
          from this.
        </p>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="team-namespace">Workspace</Label>
        <Input
          id="team-namespace"
          className="font-mono"
          value={namespace}
          onChange={(e) => onNamespaceChange(e.target.value)}
        />
      </div>

      {issue && <GenerationIssue issue={issue} />}

      {generating && (
        <p className="text-sm text-faint" data-testid="team-generating">
          Composing a roster from the registry&apos;s published agents…
        </p>
      )}
    </div>
  );
}

// GenerationIssue — the 422 outcome, inline in the step that caused it. Two
// different problems with two different next steps, so they are drawn as two
// different things rather than one message with a branch inside it.
function GenerationIssue({ issue }: { issue: GenIssue }) {
  return (
    <div
      className="border border-border border-l-2 border-l-destructive bg-card px-4 py-3"
      role="alert"
      data-testid="regenerate-hint"
    >
      {issue.emptyRegistry ? (
        <>
          <p
            className="font-serif text-md font-medium"
            data-testid="empty-registry-hint"
          >
            That registry has no published agents.
          </p>
          <p className="mt-1 text-sm text-secondary-foreground">
            A team is composed from agents that already exist, so there is
            nothing here to compose from. Publish at least one agent into the
            registry — or pick a different registry above — and generate again.
          </p>
        </>
      ) : (
        <>
          <p className="font-serif text-md font-medium">
            That roster didn&apos;t come back valid.
          </p>
          <p className="mt-1 text-sm text-secondary-foreground">
            Nothing was created. Sharpen the description above — naming the roles
            you expect usually fixes it — and generate again.
          </p>
          <pre className="mt-3 min-w-0 overflow-x-auto rounded-md bg-surface-3 p-3 font-mono text-xs leading-relaxed text-secondary-foreground">
            {issue.reason}
          </pre>
        </>
      )}
    </div>
  );
}

// ── Step 2 — review the roster ──────────────────────────────────────────────

function ReviewStep({
  gen,
  namespace,
  registryRef,
  failure,
  onDismiss,
}: {
  gen: GenerateTeamResponse;
  namespace: string;
  registryRef: string;
  failure: CreateFailure | null;
  onDismiss: () => void;
}) {
  const [showYAML, setShowYAML] = React.useState(false);
  const supervisor = extractSupervisor(gen.teamYAML);
  const roster = extractRoster(gen.teamYAML);
  const eligible = gen.eligibleMembers ?? [];

  const facts: KeyValueItem[] = [
    { key: "Team", value: extractTeamName(gen.teamYAML), absent: "not named" },
    { key: "Workspace", value: namespace },
    {
      key: "Registry",
      value: extractRegistryRef(gen.teamYAML) ?? registryRef,
      absent: "not named",
    },
    {
      key: "Supervisor",
      value: supervisor ? (
        <span data-testid="team-supervisor">{supervisor}</span>
      ) : undefined,
      absent: "not proposed",
    },
    { key: "Members", value: <QuantityValue value={roster.length} /> },
    {
      key: "Eligible",
      // An empty set is NOT "none were eligible" — a roster was composed, so
      // some were. It is "the generator didn't say", and it reads that way.
      value: eligible.length > 0 ? eligible.join(", ") : undefined,
      absent: "not reported",
      title:
        eligible.length > 0
          ? eligible.join(", ")
          : "The generator did not report the eligible set — unknown, not none.",
    },
    { key: "Composed by", value: gen.model, absent: "not reported" },
  ];

  const rosterItems: KeyValueItem[] = roster.map((entry, i) => ({
    key: entry.name || `member ${i + 1}`,
    value: (
      <span className="block" data-testid={`team-roster-entry-${i}`}>
        <span className="font-mono">{entry.agentRef}</span>
        {entry.description && (
          <span className="mt-0.5 block font-sans text-xs text-faint">
            {entry.description}
          </span>
        )}
      </span>
    ),
    absent: "not proposed",
  }));

  return (
    <div className="min-w-0 space-y-6" data-testid="roster-review">
      <section aria-labelledby="roster-head" className="min-w-0">
        <SectionHeader
          id="roster-head"
          title="Review the roster"
          lede="This team does not exist yet. Everything below was written by a model from your description and confirmed by nobody — read it, then create it."
        />
        <div className="mb-4 flex flex-wrap items-center gap-2">
          <Badge variant="open">proposed</Badge>
          <span className="text-xs text-faint">
            composed by{" "}
            <span className="font-mono">{gen.model || "an unnamed model"}</span>{" "}
            · nothing exists until you create it
          </span>
        </div>

        <div className="border border-border bg-card p-5">
          <KeyValueList items={facts} />
        </div>
      </section>

      <section aria-labelledby="roster-members-head" className="min-w-0">
        <SectionHeader
          as="h3"
          id="roster-members-head"
          title="Who does what"
          lede="Each role, and the published agent proposed to fill it."
        />
        <div className="border border-border bg-card p-5">
          {rosterItems.length > 0 ? (
            <KeyValueList items={rosterItems} />
          ) : (
            <p className="text-sm text-faint">
              The proposed spec names no roster members — only a supervisor.
            </p>
          )}
        </div>
      </section>

      {gen.warnings && gen.warnings.length > 0 && (
        <div className="border border-border border-l-2 border-l-warning bg-card px-4 py-3">
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

      {/* The raw spec, behind a disclosure, in a code well that scrolls inside
          itself (§4.5/§4.6) — the review above is the primary reading. */}
      <div className="min-w-0">
        <button
          type="button"
          onClick={() => setShowYAML((v) => !v)}
          aria-expanded={showYAML}
          data-testid="team-yaml-toggle"
          className="border-b border-accent text-sm font-semibold text-primary hover:border-primary"
        >
          {showYAML ? "Hide the team.yaml" : "Show the team.yaml"}
        </button>
        {showYAML && (
          <pre
            data-testid="team-yaml"
            className="mt-3 max-h-80 min-w-0 overflow-auto rounded-md bg-surface-3 p-4 font-mono text-xs leading-relaxed text-secondary-foreground"
          >
            {gen.teamYAML}
          </pre>
        )}
      </div>

      {failure &&
        (failure.forbidden ? (
          <ErrorState variant="forbidden" resource="teams" permission="create" />
        ) : (
          <ErrorState
            title="The team wasn’t created."
            description="Nothing was applied to the cluster. The roster above is still exactly as proposed — deal with the reason below, then create it again."
            detail={failure.message}
            onRetry={onDismiss}
            retryLabel="Dismiss"
          />
        ))}

      <ClosingNote>
        Creating this team adds one AgentTeam. The agents it names already exist
        — the team only records how they work together.
      </ClosingNote>
    </div>
  );
}

// ── team.yaml readers (a tolerant scan of a spec we control) ────────────────
// A full YAML parser is overkill for output the BFF generates from our own
// schema; these read the fields the review displays. The server is the real
// validator.

/** `no eligible agents` / `no published members` — the empty-registry outcome. */
function isEmptyRegistry(text: string): boolean {
  return (
    text.includes("no eligible agents") || text.includes("no published members")
  );
}

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return `${err.message}${err.status ? ` (${err.status})` : ""}`;
  }
  return err instanceof Error ? err.message : fallback;
}

// extractTeamName reads `metadata.name` — the first `name:` in the document,
// which precedes any roster entry's own name.
function extractTeamName(teamYAML: string): string | undefined {
  const m = /^\s*name:\s*(\S+)/m.exec(teamYAML);
  return m ? m[1].replace(/^"(.*)"$/, "$1") : undefined;
}

function extractRegistryRef(teamYAML: string): string | undefined {
  const m = /registryRef:\s*(\S+)/.exec(teamYAML);
  return m ? m[1].replace(/^"(.*)"$/, "$1") : undefined;
}

// extractSupervisor parses the supervisor agentRef from the team YAML without
// importing a full YAML parser. The supervisor block precedes the roster, so
// the first `agentRef:` is it.
function extractSupervisor(teamYAML: string): string | undefined {
  const m = /agentRef:\s*(\S+)/.exec(teamYAML);
  return m ? m[1] : undefined;
}

// extractRoster parses the roster entries from the YAML — a simple line-scan
// approach sufficient for the review display.
function extractRoster(
  teamYAML: string,
): { name: string; agentRef: string; description: string }[] {
  const entries: { name: string; agentRef: string; description: string }[] = [];
  const lines = teamYAML.split("\n");
  let currentEntry: {
    name?: string;
    agentRef?: string;
    description?: string;
  } | null = null;
  let inRoster = false;

  const flush = () => {
    if (currentEntry?.agentRef) {
      entries.push({
        name: currentEntry.name ?? "",
        agentRef: currentEntry.agentRef,
        description: currentEntry.description ?? "",
      });
    }
  };

  for (const line of lines) {
    if (/^\s+roster:/.test(line)) {
      inRoster = true;
      continue;
    }
    if (!inRoster) continue;
    // A new roster item starts with "- name:" or "- agentRef:".
    if (/^\s+- /.test(line)) {
      flush();
      currentEntry = {};
    }
    if (currentEntry !== null) {
      const nameM = /(?:^|\s)name:\s*(\S+)/.exec(line);
      if (nameM && !currentEntry.name) currentEntry.name = nameM[1];
      const refM = /agentRef:\s*(\S+)/.exec(line);
      if (refM) currentEntry.agentRef = refM[1];
      const descM = /description:\s*(.+)/.exec(line);
      if (descM) currentEntry.description = descM[1].trim();
    }
  }
  flush();
  return entries;
}
