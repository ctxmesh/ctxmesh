import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Sparkles, Waypoints } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { useToast } from "@/components/kit";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type AgentRegistrySummary,
  type AgentTeamSummary,
  type GenerateTeamResponse,
} from "@/lib/api";

// CreateTeamPage — the "describe a team" surface (m71.7, ADR 0065 D4).
//
// Flow: pick an AgentRegistry → describe the team → generateTeam → review the
// returned roster (supervisor + roster members + eligible agents) → create via
// createTeam. On 422 + regenerate the reason is shown inline; on an
// empty-registry 422 (no eligible members) a hint links to the single-agent
// builder. A viewer (403 on create) is told why.
//
// data-testid contract:
//   create-team-page      — root
//   registry-select       — registry picker
//   team-description      — description textarea
//   generate-btn          — generate action
//   roster-review         — the review panel after generation
//   team-supervisor       — supervisor agentRef in the review
//   team-roster-entry-{n} — each roster entry in the review (0-indexed)
//   create-btn            — the create action in the review
//   regenerate-hint       — the 422 reason display
//   empty-registry-hint   — the empty-registry 422 hint

type Stage =
  | { kind: "describe" }
  | { kind: "generating" }
  | { kind: "review"; gen: GenerateTeamResponse }
  | { kind: "creating" }
  | { kind: "created"; team: AgentTeamSummary }
  | { kind: "regenerate"; reason: string; emptyRegistry: boolean }
  | { kind: "error"; message: string };

export function CreateTeamPage() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const { namespace } = useNamespace();

  const [registries, setRegistries] = React.useState<AgentRegistrySummary[]>([]);
  const [registryRef, setRegistryRef] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [targetNs, setTargetNs] = React.useState(namespace || "default");
  const [stage, setStage] = React.useState<Stage>({ kind: "describe" });

  // Load available registries so the user can pick one.
  React.useEffect(() => {
    const controller = new AbortController();
    api
      .listAgentRegistries(undefined, controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setRegistries(res.items);
        if (res.items.length > 0 && !registryRef) {
          setRegistryRef(res.items[0].name);
        }
      })
      .catch(() => {
        // A probe failure just means no registry dropdown.
      });
    return () => controller.abort();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  React.useEffect(() => {
    if (namespace) setTargetNs(namespace);
  }, [namespace]);

  async function onGenerate() {
    if (!registryRef || !description.trim()) return;
    setStage({ kind: "generating" });
    try {
      const res = await api.generateTeam({
        description: description.trim(),
        registryRef,
        namespace: targetNs || "default",
      });
      // Branch on the FLAG (same pattern as generateAgent): a regenerate outcome
      // keeps the reason so the user can see what went wrong.
      if (res.regenerate) {
        const isEmpty = (res.error || res.reason || "").includes("no eligible agents");
        setStage({
          kind: "regenerate",
          reason: res.reason ?? res.error ?? "The generated team spec was not valid.",
          emptyRegistry: isEmpty,
        });
        return;
      }
      setStage({ kind: "review", gen: res });
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? `${err.message}${err.status ? ` (${err.status})` : ""}`
          : err instanceof Error
            ? err.message
            : "generation failed";
      const isEmpty =
        msg.includes("no eligible agents") ||
        msg.includes("no published members");
      setStage({ kind: "regenerate", reason: msg, emptyRegistry: isEmpty });
    }
  }

  async function onCreate(teamYAML: string) {
    setStage((prev) => {
      if (prev.kind === "review") return { ...prev, kind: "creating" } as Stage;
      return prev;
    });
    // If we're now creating, kick it off.
    try {
      const team = await api.createTeam({
        teamYAML,
        namespace: targetNs || "default",
      });
      setStage({ kind: "created", team });
      toast({
        variant: "success",
        title: "Team created",
        description: `${team.name} is ready — supervisor ${team.supervisor}, ${team.roster.length} member(s).`,
      });
      // Navigate to teams list after a short pause.
      setTimeout(() => navigate("/teams"), 1200);
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? `${err.message}${err.status ? ` (${err.status})` : ""}`
          : err instanceof Error
            ? err.message
            : "create failed";
      setStage({ kind: "error", message: msg });
    }
  }

  const busy = stage.kind === "generating" || stage.kind === "creating";
  const canGenerate = registryRef.trim().length > 0 && description.trim().length > 0 && !busy;

  return (
    <div className="mx-auto max-w-3xl space-y-6" data-testid="create-team-page">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">New team</h2>
        <p className="text-sm text-muted-foreground">
          Describe the team you want. We compose a supervisor + roster from the
          registry's published agents — review before creating.
        </p>
      </div>

      {/* Description form */}
      {(stage.kind === "describe" || stage.kind === "generating") && (
        <DescribeForm
          registries={registries}
          registryRef={registryRef}
          onRegistryRefChange={setRegistryRef}
          description={description}
          onDescriptionChange={setDescription}
          namespace={targetNs}
          onNamespaceChange={setTargetNs}
          onGenerate={onGenerate}
          busy={stage.kind === "generating"}
          canGenerate={canGenerate}
        />
      )}

      {/* Regenerate / invalid outcome */}
      {stage.kind === "regenerate" && (
        <RegeneratePanel
          reason={stage.reason}
          emptyRegistry={stage.emptyRegistry}
          onRetry={() => setStage({ kind: "describe" })}
        />
      )}

      {/* Roster review */}
      {(stage.kind === "review" || stage.kind === "creating") && (
        <RosterReview
          gen={(stage as { gen: GenerateTeamResponse }).gen}
          busy={stage.kind === "creating"}
          onBack={() => setStage({ kind: "describe" })}
          onCreate={onCreate}
        />
      )}

      {/* Create error */}
      {stage.kind === "error" && (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
          <strong>Create failed: </strong>
          {stage.message}
          <div className="mt-3">
            <Button variant="outline" size="sm" onClick={() => setStage({ kind: "describe" })}>
              Try again
            </Button>
          </div>
        </div>
      )}

      {/* Success */}
      {stage.kind === "created" && (
        <div className="rounded-lg border border-green-200 bg-green-50 p-4 text-sm text-green-800 dark:border-green-800 dark:bg-green-950 dark:text-green-200">
          Team <strong>{stage.team.name}</strong> created — redirecting to teams list…
        </div>
      )}
    </div>
  );
}

// DescribeForm — the registry picker + description textarea + generate button.
function DescribeForm({
  registries,
  registryRef,
  onRegistryRefChange,
  description,
  onDescriptionChange,
  namespace,
  onNamespaceChange,
  onGenerate,
  busy,
  canGenerate,
}: {
  registries: AgentRegistrySummary[];
  registryRef: string;
  onRegistryRefChange: (v: string) => void;
  description: string;
  onDescriptionChange: (v: string) => void;
  namespace: string;
  onNamespaceChange: (v: string) => void;
  onGenerate: () => void;
  busy: boolean;
  canGenerate: boolean;
}) {
  return (
    <div className="rounded-lg border bg-card p-6 shadow-card space-y-5">
      <div className="flex h-11 w-11 items-center justify-center rounded-xl bg-gradient-to-br from-primary to-brand-2 text-primary-foreground">
        <Waypoints className="h-6 w-6" />
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="registry-select">Agent registry</Label>
        {registries.length > 0 ? (
          <Select
            id="registry-select"
            data-testid="registry-select"
            value={registryRef}
            onChange={(e) => onRegistryRefChange(e.target.value)}
          >
            {registries.map((r) => (
              <option key={`${r.namespace}/${r.name}`} value={r.name}>
                {r.name} ({r.namespace})
              </option>
            ))}
          </Select>
        ) : (
          <input
            id="registry-select"
            data-testid="registry-select"
            className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
            placeholder="registry name (e.g. prod-registry)"
            value={registryRef}
            onChange={(e) => onRegistryRefChange(e.target.value)}
          />
        )}
        <p className="text-xs text-muted-foreground">
          The team's eligible agents come from this registry's published members.
        </p>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="team-description">Team description</Label>
        <Textarea
          id="team-description"
          data-testid="team-description"
          rows={4}
          className="text-sm"
          value={description}
          onChange={(e) => onDescriptionChange(e.target.value)}
          placeholder="An orchestrator that delegates research to a web searcher and summarization to a writer agent…"
        />
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="team-namespace">Namespace</Label>
        <input
          id="team-namespace"
          className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
          value={namespace}
          onChange={(e) => onNamespaceChange(e.target.value)}
        />
      </div>

      <div className="flex justify-end">
        <Button
          data-testid="generate-btn"
          onClick={onGenerate}
          disabled={!canGenerate}
        >
          <Sparkles className="h-4 w-4" />
          {busy ? "Generating…" : "Generate team"}
        </Button>
      </div>
    </div>
  );
}

// RegeneratePanel — the invalid / empty-registry 422 outcome.
function RegeneratePanel({
  reason,
  emptyRegistry,
  onRetry,
}: {
  reason: string;
  emptyRegistry: boolean;
  onRetry: () => void;
}) {
  return (
    <div
      className="rounded-lg border border-warning/30 bg-warning/5 p-5 space-y-3"
      data-testid="regenerate-hint"
    >
      {emptyRegistry ? (
        <>
          <p className="text-sm font-medium text-warning-foreground" data-testid="empty-registry-hint">
            No eligible agents in this registry
          </p>
          <p className="text-sm text-muted-foreground">
            A team needs at least one published agent in the registry. Create and
            publish agents first, then come back to build a team.
          </p>
          <Button variant="outline" size="sm" onClick={onRetry}>
            Pick a different registry
          </Button>
        </>
      ) : (
        <>
          <p className="text-sm font-medium text-warning-foreground">
            Generation produced an invalid team spec
          </p>
          <p className="text-xs text-muted-foreground font-mono whitespace-pre-wrap">
            {reason}
          </p>
          <Button variant="outline" size="sm" onClick={onRetry}>
            Try again
          </Button>
        </>
      )}
    </div>
  );
}

// RosterReview — shows the generated team for review before creating.
function RosterReview({
  gen,
  busy,
  onBack,
  onCreate,
}: {
  gen: GenerateTeamResponse;
  busy: boolean;
  onBack: () => void;
  onCreate: (teamYAML: string) => void;
}) {
  return (
    <div className="space-y-4" data-testid="roster-review">
      <div className="rounded-lg border bg-card p-6 shadow-card space-y-4">
        <div className="flex items-center gap-2">
          <Waypoints className="h-5 w-5 text-primary" />
          <h3 className="font-semibold">Review the generated team</h3>
        </div>

        <div className="space-y-3 text-sm">
          <div>
            <span className="text-muted-foreground">Supervisor: </span>
            <span
              className="font-mono font-medium"
              data-testid="team-supervisor"
            >
              {extractSupervisor(gen.teamYAML)}
            </span>
          </div>

          <div>
            <p className="text-muted-foreground mb-1.5">Roster:</p>
            <ul className="space-y-1.5">
              {extractRoster(gen.teamYAML).map((entry, i) => (
                <li
                  key={i}
                  className="flex items-center gap-2"
                  data-testid={`team-roster-entry-${i}`}
                >
                  <Badge variant="secondary" className="font-mono text-xs">
                    {entry.agentRef}
                  </Badge>
                  {entry.description && (
                    <span className="text-xs text-muted-foreground">
                      — {entry.description}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </div>

          {gen.eligibleMembers.length > 0 && (
            <div>
              <p className="text-xs text-muted-foreground">
                Eligible agents in this registry:{" "}
                {gen.eligibleMembers.join(", ")}
              </p>
            </div>
          )}

          {gen.warnings && gen.warnings.length > 0 && (
            <div className="text-xs text-warning-foreground">
              {gen.warnings.map((w, i) => (
                <p key={i}>{w}</p>
              ))}
            </div>
          )}
        </div>

        <details className="text-xs">
          <summary className="cursor-pointer text-muted-foreground select-none">
            Show YAML
          </summary>
          <pre className="mt-2 overflow-auto rounded bg-surface-2 p-3 font-mono text-xs leading-relaxed">
            {gen.teamYAML}
          </pre>
        </details>

        <div className="flex items-center justify-between pt-2">
          <Button variant="ghost" onClick={onBack} disabled={busy}>
            Back
          </Button>
          <Button
            data-testid="create-btn"
            onClick={() => onCreate(gen.teamYAML)}
            disabled={busy}
          >
            {busy ? "Creating…" : "Create team"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// extractSupervisor parses the supervisor agentRef from the team YAML without
// importing a full YAML parser. Falls back to the raw YAML search.
function extractSupervisor(teamYAML: string): string {
  const m = /agentRef:\s*(\S+)/.exec(teamYAML);
  return m ? m[1] : "(unknown)";
}

// extractRoster parses the roster entries from the YAML — a simple line-scan
// approach sufficient for the review display.
function extractRoster(
  teamYAML: string,
): { agentRef: string; description: string }[] {
  const entries: { agentRef: string; description: string }[] = [];
  const lines = teamYAML.split("\n");
  let currentEntry: { agentRef?: string; description?: string } | null = null;
  let inRoster = false;

  for (const line of lines) {
    if (/^\s+roster:/.test(line)) {
      inRoster = true;
      continue;
    }
    if (!inRoster) continue;
    // A new roster item starts with "- name:" or "- agentRef:"
    if (/^\s+- /.test(line)) {
      if (currentEntry?.agentRef) {
        entries.push({
          agentRef: currentEntry.agentRef,
          description: currentEntry.description ?? "",
        });
      }
      currentEntry = {};
    }
    if (currentEntry !== null) {
      const refM = /agentRef:\s*(\S+)/.exec(line);
      if (refM) currentEntry.agentRef = refM[1];
      const descM = /description:\s*(.+)/.exec(line);
      if (descM) currentEntry.description = descM[1].trim();
    }
  }
  if (currentEntry?.agentRef) {
    entries.push({
      agentRef: currentEntry.agentRef,
      description: currentEntry.description ?? "",
    });
  }
  return entries;
}
