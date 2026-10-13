import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Plus, X } from "lucide-react";

import { ResourceDetailPage, type ResourceDetailKind } from "@/components/resource-detail";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuantityValue,
  ResourceLink,
  SectionHeader,
  StatusBadge,
  Wizard,
  type KeyValueItem,
  type WizardStep,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import {
  api,
  ApiError,
  type AgentRegistryDetail,
  type LabelSelectorDTO,
  type RegistryGuardsDTO,
} from "@/lib/api";
import { useCapabilities, useCapabilitiesIn } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";

// AgentRegistryDetailPage — the registry reader + its edit form, on the shared
// ResourceDetailPage (M151 §6.1 archetype A2). Route: /registries/:ns/:name
//
// KEY CONSTRAINTS:
//   1. NO egress/allowlist field anywhere — the egress NetworkPolicy is
//      controller-managed; the console cannot alter the egress posture.
//   2. registryId is immutable after creation — shown READ-ONLY on edit.
//
// data-testid contract:
//   registry-detail-page / -loading / -error, registry-not-found — the page states
//   registry-detail-panel, registry-id-display — the configuration panel
//   registry-members-panel, member-link-<name> — the resolved members
//   edit-registry-button / delete-registry-button — the header actions

function RegistryPanels({ detail }: { detail: AgentRegistryDetail }) {
  const labels = Object.entries(detail.memberSelector.matchLabels ?? {});
  const config: KeyValueItem[] = [
    {
      key: "Registry ID",
      // Immutable, so it is always shown read-only.
      value: (
        <span className="block truncate" data-testid="registry-id-display" title={detail.registryId}>
          {detail.registryId}
        </span>
      ),
    },
    {
      key: "Roles",
      value:
        detail.roles.length > 0 ? (
          <span className="flex flex-wrap justify-end gap-1">
            {detail.roles.map((r) => (
              <Badge key={r} variant="muted">
                {r}
              </Badge>
            ))}
          </span>
        ) : undefined,
      absent: "none declared",
      mono: false,
    },
    ...(detail.guards
      ? [
          { key: "Max depth", value: detail.guards.maxDepth, absent: "not set" },
          { key: "Hop budget", value: detail.guards.hopBudget, absent: "not set" },
        ]
      : []),
  ];

  return (
    <>
      {/* Configuration — registryId read-only; NO egress field. */}
      <Card className="min-w-0" data-testid="registry-detail-panel">
        <PanelHeader title="Configuration" />
        <CardContent>
          <KeyValueList items={config} />
          {labels.length > 0 && (
            <div className="mt-4">
              <p className="mb-2 font-mono text-2xs uppercase tracking-wide text-faint">
                Member selector
              </p>
              <div className="flex flex-wrap gap-1">
                {labels.map(([k, v]) => (
                  <Badge key={`${k}=${v}`} variant="muted" className="font-mono">
                    {k}={v}
                  </Badge>
                ))}
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      {/* The resolved members. */}
      <Card className="min-w-0" data-testid="registry-members-panel">
        <PanelHeader
          title="Members"
          meta={detail.status.members.length > 0 ? `${detail.status.members.length}` : undefined}
        />
        <CardContent>
          {detail.status.members.length === 0 ? (
            <p className="text-sm text-faint">No members resolved yet.</p>
          ) : (
            <ul className="space-y-1.5">
              {detail.status.members.map((m) => (
                <li key={m} className="flex items-center gap-2 text-sm">
                  {/* A member is an AgentDeployment in this registry's namespace. */}
                  <ResourceLink
                    kind="agent"
                    namespace={detail.namespace}
                    name={m}
                    className="font-mono text-xs"
                    testId={`member-link-${m}`}
                  />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </>
  );
}

const AGENT_REGISTRY: ResourceDetailKind<AgentRegistryDetail> = {
  noun: "Agent registry",
  short: "registry",
  plural: "agent registries",
  crdKind: "AgentRegistry",
  list: { to: "/registries", label: "Registries", back: "Back to registries" },
  resource: RES_REGISTRIES,
  testids: {
    page: "registry-detail-page",
    loading: "registry-detail-loading",
    error: "registry-detail-error",
    notFound: "registry-not-found",
    edit: "edit-registry-button",
    delete: "delete-registry-button",
  },
  read: (ns, name, signal) => api.agentRegistryDetail(ns, name, signal),
  status: (d) => (
    <StatusBadge ready={d.status.ready} phase={d.status.phase || (d.status.ready ? "Ready" : "Pending")} />
  ),
  meta: (d) =>
    `${d.namespace} · ${d.status.members.length} member${d.status.members.length === 1 ? "" : "s"}`,
  Main: RegistryPanels,
  facts: (d) => [
    { key: "Registry", value: d.name, title: d.name },
    { key: "Workspace", value: d.namespace, absent: "not recorded" },
    { key: "Members", value: <QuantityValue value={d.status.members.length} />, mono: false },
  ],
  edit: {
    subtitle: (d) => `Registry ID: ${d.registryId} (read-only — immutable)`,
    Wizard: RegistryEditWizard,
  },
  remove: {
    call: (ns, name) => api.removeAgentRegistry(ns, name),
    description: (d) =>
      `This will permanently delete the AgentRegistry "${d.name}". Member agents are not deleted but lose their registry membership.`,
    confirmLabel: "Delete registry",
    toastTitle: "Registry deleted",
  },
};

export function AgentRegistryDetailPage() {
  return <ResourceDetailPage kind={AGENT_REGISTRY} />;
}


// ── Edit Wizard ───────────────────────────────────────────────────────────────
// registryId shown as read-only (immutable). NO egress/allowlist field.

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

// MatchLabels editor: key=value pairs
function MatchLabelsEditor({
  labels,
  onChange,
}: {
  labels: Record<string, string>;
  onChange: (l: Record<string, string>) => void;
}) {
  const entries = Object.entries(labels);

  function addEntry() {
    onChange({ ...labels, "": "" });
  }

  function removeEntry(key: string) {
    const next = { ...labels };
    delete next[key];
    onChange(next);
  }

  function updateKey(oldKey: string, newKey: string) {
    const val = labels[oldKey] ?? "";
    const next: Record<string, string> = {};
    for (const [k, v] of Object.entries(labels)) {
      next[k === oldKey ? newKey : k] = v;
    }
    onChange({ ...next, [newKey]: val });
  }

  function updateVal(key: string, val: string) {
    onChange({ ...labels, [key]: val });
  }

  return (
    <div className="space-y-2">
      {entries.map(([k, v], i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            value={k}
            onChange={(e) => updateKey(k, e.target.value)}
            placeholder="key"
            className="font-mono text-xs"
            data-testid={`label-key-${i}`}
          />
          <span className="text-muted-foreground">=</span>
          <Input
            value={v}
            onChange={(e) => updateVal(k, e.target.value)}
            placeholder="value"
            className="font-mono text-xs"
            data-testid={`label-val-${i}`}
          />
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7 shrink-0"
            aria-label={`Remove label ${k}`}
            data-testid={`remove-label-${i}`}
            onClick={() => removeEntry(k)}
          >
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={addEntry}
        data-testid="add-label-button"
      >
        <Plus className="h-4 w-4" />
        Add label
      </Button>
    </div>
  );
}

function RegistryEditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: AgentRegistryDetail;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilitiesIn(detail.namespace);

  const [matchLabels, setMatchLabels] = React.useState<Record<string, string>>(
    detail.memberSelector.matchLabels ?? {},
  );
  const [rolesText, setRolesText] = React.useState(detail.roles.join(", "));
  const [maxDepth, setMaxDepth] = React.useState(
    detail.guards?.maxDepth !== undefined ? String(detail.guards.maxDepth) : "",
  );
  const [hopBudget, setHopBudget] = React.useState(
    detail.guards?.hopBudget !== undefined ? String(detail.guards.hopBudget) : "",
  );
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      const memberSelector: LabelSelectorDTO = {};
      if (Object.keys(matchLabels).length > 0) {
        memberSelector.matchLabels = matchLabels;
      }
      const roles = rolesText
        .split(",")
        .map((r) => r.trim())
        .filter(Boolean);
      const guards: RegistryGuardsDTO | undefined =
        maxDepth.trim() || hopBudget.trim()
          ? {
              ...(maxDepth.trim() ? { maxDepth: parseInt(maxDepth, 10) } : {}),
              ...(hopBudget.trim() ? { hopBudget: parseInt(hopBudget, 10) } : {}),
            }
          : undefined;

      await api.updateAgentRegistry(detail.namespace, detail.name, {
        name: detail.name,
        memberSelector,
        guards,
        roles: roles.length > 0 ? roles : undefined,
      });
      toast({ variant: "success", title: "Registry updated", description: `${detail.name} saved.` });
      onSaved();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSaveState({
        kind: "error",
        message: err instanceof Error ? err.message : "update failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  const memberStep: WizardStep = {
    id: "members",
    title: "Member selector",
    description: "Labels that select AgentDeployments into this registry",
    content: (
      <div className="space-y-4">
        {/* registryId is read-only on edit — immutable after creation */}
        <div
          className="rounded-md border border-border bg-surface-2/40 px-3 py-2 text-sm"
          data-testid="registry-id-readonly"
        >
          <span className="text-muted-foreground">Registry ID (immutable): </span>
          <span className="font-mono text-xs">{detail.registryId}</span>
        </div>
        <FormField id="member-selector" label="Match labels">
          <MatchLabelsEditor labels={matchLabels} onChange={setMatchLabels} />
        </FormField>
      </div>
    ),
  };

  const rolesStep: WizardStep = {
    id: "roles",
    title: "Roles & guards",
    description: "Custom role names and conversation guards",
    content: (
      <div className="space-y-4">
        <FormField id="roles-input" label="Roles (comma-separated)">
          <Input
            id="roles-input"
            value={rolesText}
            onChange={(e) => setRolesText(e.target.value)}
            placeholder="planner, executor, reviewer"
            data-testid="edit-roles"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-4">
          <FormField id="max-depth" label="Max depth">
            <Input
              id="max-depth"
              inputMode="numeric"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
              placeholder="10"
              data-testid="edit-max-depth"
            />
          </FormField>
          <FormField id="hop-budget" label="Hop budget">
            <Input
              id="hop-budget"
              inputMode="numeric"
              value={hopBudget}
              onChange={(e) => setHopBudget(e.target.value)}
              placeholder="50"
              data-testid="edit-hop-budget"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="registry-edit-review">
        <p className="text-sm font-medium">Review changes</p>
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to edit this registry"
            description="Your account can't update AgentRegistries in this cluster."
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="registry-edit-error">
            {saveState.message}
          </p>
        )}
        <dl className="divide-y rounded-md border text-sm">
          <div className="flex items-start justify-between gap-4 px-3 py-2">
            <dt className="text-muted-foreground">Registry ID</dt>
            {/* read-only — always shows the live value */}
            <dd className="font-mono text-xs" data-testid="review-registry-id">{detail.registryId}</dd>
          </div>
          {Object.keys(matchLabels).length > 0 && (
            <div className="px-3 py-2">
              <dt className="text-muted-foreground mb-1">Match labels</dt>
              <dd className="flex flex-wrap gap-1">
                {Object.entries(matchLabels).map(([k, v]) => (
                  <Badge key={`${k}=${v}`} variant="outline" className="font-mono text-[10px]">
                    {k}={v}
                  </Badge>
                ))}
              </dd>
            </div>
          )}
          {rolesText.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Roles</dt>
              <dd>{rolesText}</dd>
            </div>
          )}
          {maxDepth.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Max depth</dt>
              <dd>{maxDepth}</dd>
            </div>
          )}
          {hopBudget.trim() && (
            <div className="flex items-start justify-between gap-4 px-3 py-2">
              <dt className="text-muted-foreground">Hop budget</dt>
              <dd>{hopBudget}</dd>
            </div>
          )}
        </dl>
      </div>
    ),
  };

  return (
    <Wizard
      steps={[memberStep, rolesStep, reviewStep]}
      current={current}
      onStepChange={setCurrent}
      busy={saveState.kind === "saving"}
      onFinish={() => { void doSave(); }}
      finishLabel="Save changes"
      onCancel={onClose}
    />
  );
}

// ── New AgentRegistry page (create) ──────────────────────────────────────────
// registryId is set at creation (immutable thereafter).
// NO egress/allowlist field.

export function NewAgentRegistryPage() {
  const navigate = useNavigate();
  const [regName, setRegName] = React.useState("");
  const [regNs, setRegNs] = React.useState("");
  const [registryId, setRegistryId] = React.useState("");
  const [matchLabels, setMatchLabels] = React.useState<Record<string, string>>({});
  const [rolesText, setRolesText] = React.useState("");
  const [maxDepth, setMaxDepth] = React.useState("");
  const [hopBudget, setHopBudget] = React.useState("");
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  async function doCreate() {
    setSaveState({ kind: "saving" });
    try {
      const memberSelector: LabelSelectorDTO = {};
      if (Object.keys(matchLabels).length > 0) {
        memberSelector.matchLabels = matchLabels;
      }
      const roles = rolesText
        .split(",")
        .map((r) => r.trim())
        .filter(Boolean);
      const guards: RegistryGuardsDTO | undefined =
        maxDepth.trim() || hopBudget.trim()
          ? {
              ...(maxDepth.trim() ? { maxDepth: parseInt(maxDepth, 10) } : {}),
              ...(hopBudget.trim() ? { hopBudget: parseInt(hopBudget, 10) } : {}),
            }
          : undefined;

      const created = await api.createAgentRegistry({
        name: regName.trim(),
        namespace: regNs.trim() || undefined,
        registryId: registryId.trim(),
        memberSelector,
        guards,
        roles: roles.length > 0 ? roles : undefined,
      });
      toast({ variant: "success", title: "Registry created", description: `${created.name} created.` });
      navigate(`/registries/${encodeURIComponent(created.namespace)}/${encodeURIComponent(created.name)}`);
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setSaveState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
        forbidden: err instanceof ApiError && err.isForbidden,
      });
    }
  }

  const identityStep: WizardStep = {
    id: "identity",
    title: "Identity",
    description: "Name, namespace, and registry ID",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="Identity"
          lede="What the registry is called, where it lives, and the ID agents address it by. The ID is fixed at creation — everything else can change later."
        />
        <div className="space-y-4">
          <FormField id="new-reg-name" label="Name">
            <Input
              id="new-reg-name"
              value={regName}
              onChange={(e) => setRegName(e.target.value)}
              placeholder="my-registry"
              data-testid="new-registry-name"
            />
          </FormField>
          <FormField id="new-reg-ns" label="Namespace (blank = default)">
            <Input
              id="new-reg-ns"
              value={regNs}
              onChange={(e) => setRegNs(e.target.value)}
              placeholder="default"
              data-testid="new-registry-namespace"
            />
          </FormField>
          <FormField id="new-registry-id" label="Registry ID (immutable after create)">
            <Input
              id="new-registry-id"
              value={registryId}
              onChange={(e) => setRegistryId(e.target.value)}
              placeholder="prod-registry"
              data-testid="new-registry-id"
              className="font-mono text-xs"
            />
            <p className="text-xs text-faint">
              Choose it carefully: it cannot be edited afterwards, only replaced
              by making a new registry.
            </p>
          </FormField>
        </div>
      </div>
    ),
  };

  const memberStep: WizardStep = {
    id: "members",
    title: "Member selector",
    description: "Labels selecting AgentDeployments into this registry",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="Who is in it"
          lede="Membership is by label, not by list: any AgentDeployment carrying these labels joins the registry, including ones created later."
        />
        <FormField id="new-member-selector" label="Match labels">
          <MatchLabelsEditor labels={matchLabels} onChange={setMatchLabels} />
        </FormField>
      </div>
    ),
  };

  const rolesStep: WizardStep = {
    id: "roles",
    title: "Roles & guards",
    description: "Custom role names and conversation guards",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="Roles and guards"
          lede="Role names members can take, and the bounds a conversation may not cross. Leave a guard blank to inherit the platform's."
        />
        <FormField id="new-roles-input" label="Roles (comma-separated)">
          <Input
            id="new-roles-input"
            value={rolesText}
            onChange={(e) => setRolesText(e.target.value)}
            placeholder="planner, executor"
            data-testid="new-registry-roles"
          />
        </FormField>
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField id="new-max-depth" label="Max depth">
            <Input
              id="new-max-depth"
              inputMode="numeric"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
              placeholder="10"
              data-testid="new-registry-max-depth"
            />
          </FormField>
          <FormField id="new-hop-budget" label="Hop budget">
            <Input
              id="new-hop-budget"
              inputMode="numeric"
              value={hopBudget}
              onChange={(e) => setHopBudget(e.target.value)}
              placeholder="50"
              data-testid="new-registry-hop-budget"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  // The choices, as facts. A blank field states what blank MEANS rather than
  // leaving an empty row (kit KeyValueList) — and no row claims a default the
  // console cannot see.
  const labelCount = Object.keys(matchLabels).length;
  const registryFacts: KeyValueItem[] = [
    { key: "Name", value: regName.trim() || undefined, absent: "not named yet" },
    {
      key: "Namespace",
      value: regNs.trim() || undefined,
      absent: "default",
      title: "Left blank — the registry is created in the default namespace.",
    },
    {
      key: "Registry ID",
      // The node carries the testid either way, so the id is findable whether
      // it was filled in or left for the reader to notice.
      value: (
        <span
          data-testid="review-new-registry-id"
          className={registryId.trim() ? undefined : "text-ghost"}
        >
          {registryId.trim() || "not set yet"}
        </span>
      ),
      title: registryId.trim()
        ? "Fixed at creation — it cannot be edited afterwards."
        : "Set it on the Identity step; it cannot be added later.",
    },
    {
      key: "Member labels",
      value: labelCount || undefined,
      absent: "none set",
      title: "Membership is by label — none were added on the selector step.",
    },
    {
      key: "Roles",
      value: rolesText.trim() || undefined,
      absent: "none set",
      mono: false,
    },
    { key: "Max depth", value: maxDepth.trim() || undefined, absent: "not set" },
    { key: "Hop budget", value: hopBudget.trim() || undefined, absent: "not set" },
  ];

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-5" data-testid="new-registry-review">
        <SectionHeader
          title="Review the registry"
          lede="Nothing exists in the cluster yet. Creating it writes one AgentRegistry — the agents its labels match are not modified."
        />
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create registries"
            description="Your account can't create AgentRegistries in this cluster."
            resource="agent registries"
            permission="create"
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <div
            className="rounded-lg border border-destructive/40 bg-destructive-surface/40 px-4 py-3"
            role="alert"
            data-testid="new-registry-error"
          >
            <p className="font-serif text-md font-medium text-destructive">
              The registry wasn’t created.
            </p>
            <pre className="mt-2 min-w-0 whitespace-pre-wrap break-words rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
              {saveState.message}
            </pre>
            <p className="mt-2 max-w-[64ch] text-sm text-secondary-foreground">
              Nothing was written to the cluster. Go back, fix what it named, and
              create it again.
            </p>
          </div>
        )}
        <KeyValueList items={registryFacts} />
      </div>
    ),
  };

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="New agent registry"
        lede="A registry is a named group of agents that may talk to each other, with the roles they can take and the bounds a conversation may not cross."
      />
      {/* §6.1 A4: the Wizard's 15rem rail + its 2rem gap + the archetype's
          46rem content column. Capping the outer column sizes the inner one. */}
      <div className="max-w-[63rem]">
        <Wizard
          steps={[identityStep, memberStep, rolesStep, reviewStep]}
          current={current}
          onStepChange={setCurrent}
          busy={saveState.kind === "saving"}
          onFinish={() => { void doCreate(); }}
          finishLabel="Create registry"
          onCancel={() => navigate("/registries")}
          dirty={regName.trim() !== ""}
        />
      </div>
    </div>
  );
}
