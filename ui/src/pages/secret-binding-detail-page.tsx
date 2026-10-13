import * as React from "react";
import { useNavigate } from "react-router-dom";

import { ResourceDetailPage, type ResourceDetailKind } from "@/components/resource-detail";
import { UsedBySection } from "@/components/used-by-section";

import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ForbiddenInline,
  KeyValueList,
  PageHeader,
  QuietNote,
  SectionHeader,
  StatusBadge,
  Wizard,
  type KeyValueItem,
  type WizardStep,
  useToast,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import { api, ApiError, type SecretBindingDetail } from "@/lib/api";
import { useCapabilities, useCapabilitiesIn } from "@/lib/capabilities";
import { RES_SECRETS } from "@/lib/nav";

// SecretBindingDetailPage — the binding reader + its edit form, on the shared
// ResourceDetailPage (M151 §6.1 archetype A2, form-edit variant). Route: /secrets/:ns/:name
//
// ── THE STANDING INVARIANT: THIS PAGE NEVER RENDERS A SECRET VALUE ──────────
// Not in the body, not in a `title`, not in a placeholder, not after a
// successful save. Every field on this surface — and every field in the edit
// form and in the PUT body it sends — concerns the REFERENCE only: which
// Kubernetes Secret, which key inside it. The DTO itself carries no value
// field (`SecretBindingDetail` is name/namespace/backend/secretRef/phase/ready),
// the BFF never reads the referenced Secret, and the controller resolves the
// pointer inside the cluster. So the absence is not an oversight the page is
// papering over; it is the design, and the page says so out loud rather than
// leaving a reader to wonder where the value went.
//
// ── THE PAGE'S ONE IDEA: A BINDING IS A POINTER, NOT A STORE ────────────────
// The main column carries what it points AT (Secret + key) with the invariant
// stated beside it; the rail carries the bound facts, ending on the deliberate
// absence — because "Credential value — never read here" where a reader looks
// for values is a stronger promise than a footnote at the foot of the page.
//
// ── WHAT THIS PAGE MAY NOT CLAIM (§7.1) ────────────────────────────────────
// `GET /api/secretbindings/{ns}/{name}` returns identity, backend, the ref and
// the phase. It returns NO last-resolved time, NO rotation date and NO usage —
// so none is drawn. The one figure the page could invent (a value) is the one
// it must never invent.
//
// data-testid contract:
//   secret-detail-page     — root container (ready state)
//   secret-detail-loading  — the loading state
//   secret-detail-error    — the generic error state
//   secret-not-found       — the 404 state
//   secret-detail-panel    — the reference panel
//   secret-ref-name        — the K8s Secret NAME (never its contents)
//   secret-ref-key         — the KEY within that Secret (never its value)
//   no-value-note          — the standing invariant, stated on the surface
//   no-value-form-note     — the same invariant, stated in the edit form
//   edit-secret-button / delete-secret-button — the header actions

function SecretReference({ detail }: { detail: SecretBindingDetail }) {
  // Both values are machine strings, so both truncate on ONE line with the full value in
  // `title` (§4.5). Absence is a real state here (a malformed binding): "not set".
  const reference: KeyValueItem[] = [
    {
      key: "K8s Secret",
      value: detail.secretRef.name ? (
        <span className="block truncate" data-testid="secret-ref-name" title={detail.secretRef.name}>
          {detail.secretRef.name}
        </span>
      ) : undefined,
      absent: "not set",
      title: "This binding names no Secret, so nothing can resolve it.",
    },
    {
      key: "Key in that Secret",
      value: detail.secretRef.key ? (
        <span className="block truncate" data-testid="secret-ref-key" title={detail.secretRef.key}>
          {detail.secretRef.key}
        </span>
      ) : undefined,
      absent: "not set",
      title: "This binding names no key inside the Secret.",
    },
  ];

  return (
    <>
      {/* The reference — and only the reference. NO value field. */}
      <Card className="min-w-0" data-testid="secret-detail-panel">
        <PanelHeader title="What this points at" />
        <CardContent>
          <p className="mb-4 max-w-[64ch] text-sm text-secondary-foreground">
            A binding is a pointer, not a store. It names the Kubernetes Secret that holds the
            credential and the key inside it; the controller follows that pointer in the cluster
            when an agent needs it.
          </p>
          <KeyValueList items={reference} />
          {/* A QuietNote and not an alert because nothing is wrong: the value is absent by
              design, and dressing a deliberate absence as a failure teaches operators to
              ignore real failures. */}
          <div className="mt-4" data-testid="no-value-note">
            <QuietNote title="The credential value is never shown here.">
              Everything on this page is the reference — which Secret, which key. The console
              never displays or transmits secret values; the value itself is read inside the
              cluster, by the controller that resolves this binding. It is not absent because
              we could not read it. It is absent because reading it here would be the leak.
            </QuietNote>
          </div>
        </CardContent>
      </Card>

      {/* The ModelRoutes whose providers reference this binding. */}
      <UsedBySection
        kind="secretbinding"
        name={detail.name}
        namespace={detail.namespace}
        title="Used by model routes"
      />
    </>
  );
}

const SECRET_BINDING: ResourceDetailKind<SecretBindingDetail> = {
  noun: "Secret binding",
  short: "binding",
  plural: "secret bindings",
  crdKind: "SecretBinding",
  list: { to: "/secrets", label: "Secret bindings", back: "Back to bindings" },
  resource: RES_SECRETS,
  testids: {
    page: "secret-detail-page",
    loading: "secret-detail-loading",
    error: "secret-detail-error",
    notFound: "secret-not-found",
    edit: "edit-secret-button",
    delete: "delete-secret-button",
  },
  read: (ns, name, signal) => api.secretBindingDetail(ns, name, signal),
  status: (d) => <StatusBadge ready={d.ready} phase={d.phase} />,
  meta: (d) => `${d.namespace} · ${d.backend}`,
  Main: SecretReference,
  // The rail ends on the deliberate absence: a reader scanning it for values finds, in the
  // place a value would be, the reason there will never be one.
  facts: (d) => [
    { key: "Binding", value: d.name, title: d.name },
    { key: "Workspace", value: d.namespace, absent: "not recorded" },
    { key: "Backend", value: d.backend, absent: "not recorded" },
    {
      key: "Credential value",
      absent: "never read here",
      title:
        "The console never reads or displays credential values — only the reference to them. This absence is deliberate, not a figure we failed to fetch.",
    },
  ],
  edit: {
    subtitle: () => "Updates the Secret reference — never the credential value",
    Wizard: SecretBindingEditWizard,
  },
  remove: {
    call: (ns, name) => api.removeSecretBinding(ns, name),
    description: (d) =>
      `This will permanently delete the SecretBinding "${d.name}". Any ModelRoute that references it will lose its credential link.`,
    confirmLabel: "Delete binding",
    toastTitle: "Secret binding deleted",
  },
};

export function SecretBindingDetailPage() {
  return <ResourceDetailPage kind={SECRET_BINDING} />;
}


// ── Edit Wizard ───────────────────────────────────────────────────────────────
// SECURITY: the form edits ONLY secretRef.name + secretRef.key (and backend).
// There is NO value/credential field here.

type EditForm = {
  backend: string;
  secretRefName: string;
  secretRefKey: string;
};

type EditState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "error"; message: string; forbidden: boolean };

function SecretBindingEditWizard({
  detail,
  onClose,
  onSaved,
}: {
  detail: SecretBindingDetail;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilitiesIn(detail.namespace);

  const [form, setForm] = React.useState<EditForm>({
    backend: detail.backend,
    secretRefName: detail.secretRef.name,
    secretRefKey: detail.secretRef.key,
  });
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });

  function set<K extends keyof EditForm>(k: K, v: EditForm[K]) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  // What the form would change, computed against the bound facts rather than
  // tracked as a boolean — a field edited and then edited back is NOT a change,
  // and a "dirty" flag that says otherwise makes the discard guard cry wolf.
  const bound = {
    backend: detail.backend || "kubernetes",
    secretRefName: detail.secretRef.name,
    secretRefKey: detail.secretRef.key,
  };
  const now = {
    backend: form.backend.trim() || "kubernetes",
    secretRefName: form.secretRefName.trim(),
    secretRefKey: form.secretRefKey.trim(),
  };
  const changed = {
    backend: now.backend !== bound.backend,
    secretRefName: now.secretRefName !== bound.secretRefName,
    secretRefKey: now.secretRefKey !== bound.secretRefKey,
  };
  const dirty =
    changed.backend || changed.secretRefName || changed.secretRefKey;

  /** The unsaved line. It rides at the top of EVERY step, not only the review,
   *  because the moment a field diverges from the record is the moment the
   *  reader needs to know nothing has been written yet. */
  const unsavedNote = dirty ? (
    <p className="text-sm text-secondary-foreground" data-testid="secret-edit-unsaved">
      <span className="font-mono text-2xs uppercase tracking-wide text-faint">
        Unsaved
      </span>{" "}
      This form differs from the binding as it stands. Nothing changes until you
      press Save changes.
    </p>
  ) : null;

  /** One review row: what it is now, and what it becomes. Unchanged rows say so
   *  rather than quietly looking identical to the changed ones. */
  function change(was: string, next: string, isChanged: boolean): React.ReactNode {
    if (!isChanged) {
      return (
        <span className="block truncate text-faint" title={was}>
          {was || "—"} <span className="text-ghost">(unchanged)</span>
        </span>
      );
    }
    return (
      <span className="block truncate" title={`${was || "—"} → ${next || "—"}`}>
        <span className="text-faint line-through">{was || "—"}</span>{" "}
        <span aria-hidden="true" className="text-ghost">
          →
        </span>{" "}
        {next || "—"}
      </span>
    );
  }

  async function doSave() {
    setSaveState({ kind: "saving" });
    try {
      await api.updateSecretBinding(detail.namespace, detail.name, {
        name: detail.name,
        backend: form.backend.trim() || "kubernetes",
        // SECURITY: submitting only the ref — no value field.
        secretRef: {
          name: form.secretRefName.trim(),
          key: form.secretRefKey.trim(),
        },
      });
      toast({ variant: "success", title: "Secret binding updated", description: `${detail.name} saved.` });
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

  const refStep: WizardStep = {
    id: "ref",
    title: "Secret reference",
    description: "Which K8s Secret holds the credential (never the value itself)",
    content: (
      <div className="space-y-4">
        {/* The invariant, restated where the typing happens. Calm register: the
            form is not dangerous, it simply has no value field to be dangerous
            with. */}
        <div data-testid="no-value-form-note">
          <QuietNote title="This form edits the reference, not the credential.">
            It changes which Kubernetes Secret is named and which key inside it.
            It does not accept, display, or store credential values — the
            credential lives only in the Secret, and only the controller reads it.
          </QuietNote>
        </div>
        {unsavedNote}
        <FormField
          id="sb-backend"
          label="Backend"
          hint={changed.backend ? "Changed — not saved yet" : undefined}
        >
          <Input
            id="sb-backend"
            value={form.backend}
            onChange={(e) => set("backend", e.target.value)}
            placeholder="kubernetes"
            data-testid="edit-sb-backend"
          />
        </FormField>
        <FormField
          id="sb-secret-name"
          label="K8s Secret name (not the value)"
          hint={changed.secretRefName ? "Changed — not saved yet" : undefined}
        >
          <Input
            id="sb-secret-name"
            value={form.secretRefName}
            onChange={(e) => set("secretRefName", e.target.value)}
            placeholder="my-api-key-secret"
            data-testid="edit-sb-secret-name"
          />
        </FormField>
        <FormField
          id="sb-secret-key"
          label="Key within the Secret (not the value)"
          hint={changed.secretRefKey ? "Changed — not saved yet" : undefined}
        >
          <Input
            id="sb-secret-key"
            value={form.secretRefKey}
            onChange={(e) => set("secretRefKey", e.target.value)}
            placeholder="apiKey"
            data-testid="edit-sb-secret-key"
          />
        </FormField>
      </div>
    ),
  };

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-4" data-testid="secret-edit-review">
        <SectionHeader
          title={dirty ? "What changes when you save" : "Nothing has changed"}
          lede={
            dirty
              ? "The binding still points where it did. Saving repoints it."
              : "This form matches the binding as it stands, so saving would write the same reference back."
          }
          as="h3"
        />
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="You don't have permission to edit this binding."
            resource="secret bindings"
            permission="update"
            detail={saveState.message}
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <p className="text-sm text-destructive" role="alert" data-testid="secret-edit-error">
            {saveState.message}
          </p>
        )}
        <KeyValueList
          items={[
            {
              key: "Backend",
              value: change(bound.backend, now.backend, changed.backend),
            },
            {
              key: "K8s Secret",
              value: change(
                bound.secretRefName,
                now.secretRefName,
                changed.secretRefName,
              ),
            },
            {
              key: "Key in that Secret",
              value: change(
                bound.secretRefKey,
                now.secretRefKey,
                changed.secretRefKey,
              ),
            },
          ]}
        />
        <QuietNote>
          No credential value is part of this change, or of the request it sends.
          The body carries the binding&rsquo;s name, its backend and its Secret
          reference — nothing else.
        </QuietNote>
      </div>
    ),
  };

  return (
    <Wizard
      steps={[refStep, reviewStep]}
      current={current}
      onStepChange={setCurrent}
      busy={saveState.kind === "saving"}
      onFinish={() => { void doSave(); }}
      finishLabel="Save changes"
      onCancel={onClose}
      // An edit that has diverged from the record routes Cancel/Esc through the
      // discard guard, so the unsaved change cannot vanish on a stray keystroke.
      dirty={dirty}
    />
  );
}

// ── New SecretBinding page (create) ──────────────────────────────────────────
// SECURITY: the form accepts ONLY a ref (Secret name + key). No value field.
//
// The create surface is M151 §6.1 archetype A4 and composes the same editorial
// kit as the reader above it — deliberately, so a binding reads as the same
// object whether you are making one or looking at one.

export function NewSecretBindingPage() {
  const navigate = useNavigate();
  const [bindingName, setBindingName] = React.useState("");
  const [bindingNs, setBindingNs] = React.useState("");
  const [backend, setBackend] = React.useState("kubernetes");
  const [secretRefName, setSecretRefName] = React.useState("");
  const [secretRefKey, setSecretRefKey] = React.useState("");
  const [current, setCurrent] = React.useState(0);
  const [saveState, setSaveState] = React.useState<EditState>({ kind: "idle" });
  const { toast } = useToast();
  const { reprobe } = useCapabilities();

  async function doCreate() {
    setSaveState({ kind: "saving" });
    try {
      const created = await api.createSecretBinding({
        name: bindingName.trim(),
        namespace: bindingNs.trim() || undefined,
        backend: backend.trim() || "kubernetes",
        // SECURITY: submitting only the ref — no value field.
        secretRef: {
          name: secretRefName.trim(),
          key: secretRefKey.trim(),
        },
      });
      toast({ variant: "success", title: "Secret binding created", description: `${created.name} created.` });
      navigate(`/secrets/${encodeURIComponent(created.namespace)}/${encodeURIComponent(created.name)}`);
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
    description: "Name and namespace",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="Identity"
          lede="What the binding is called, and where it lives. A ModelRoute references it by this name."
        />
        <div className="space-y-4">
          <FormField id="new-sb-name" label="Name">
            <Input
              id="new-sb-name"
              value={bindingName}
              onChange={(e) => setBindingName(e.target.value)}
              placeholder="my-openai-binding"
              data-testid="new-sb-name"
            />
          </FormField>
          <FormField id="new-sb-ns" label="Namespace (blank = default)">
            <Input
              id="new-sb-ns"
              value={bindingNs}
              onChange={(e) => setBindingNs(e.target.value)}
              placeholder="default"
              data-testid="new-sb-namespace"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  const refStep: WizardStep = {
    id: "ref",
    title: "Secret reference",
    description: "Which K8s Secret holds the credential (not the value itself)",
    content: (
      <div className="space-y-5">
        <SectionHeader
          title="The reference"
          lede="Which Kubernetes Secret already holds the credential, and which key inside it. Not the credential."
        />
        {/* Explicit security notice — no value field. It reads as a calm aside
            rather than a warning: nothing is wrong, this is simply how the
            console is built. */}
        <div data-testid="no-value-create-note">
          <QuietNote title="There is no field for the value, and there never will be.">
            You give the <strong>reference</strong> — which Secret, which key
            inside it. The console never accepts, displays, or stores a
            credential value; the controller resolves the pointer inside the
            cluster.
          </QuietNote>
        </div>
        <div className="space-y-4">
          <FormField id="new-sb-backend" label="Backend">
            <Input
              id="new-sb-backend"
              value={backend}
              onChange={(e) => setBackend(e.target.value)}
              placeholder="kubernetes"
              data-testid="new-sb-backend"
            />
          </FormField>
          <FormField id="new-sb-secret-name" label="K8s Secret name">
            <Input
              id="new-sb-secret-name"
              value={secretRefName}
              onChange={(e) => setSecretRefName(e.target.value)}
              placeholder="my-api-key-secret"
              data-testid="new-sb-secret-name"
              className="font-mono text-xs"
            />
          </FormField>
          <FormField id="new-sb-secret-key" label="Key within the Secret">
            <Input
              id="new-sb-secret-key"
              value={secretRefKey}
              onChange={(e) => setSecretRefKey(e.target.value)}
              placeholder="apiKey"
              data-testid="new-sb-secret-key"
              className="font-mono text-xs"
            />
          </FormField>
        </div>
      </div>
    ),
  };

  // The choices, as facts. A blank field states what blank MEANS rather than
  // leaving an empty row (kit KeyValueList) — and every row here is a pointer,
  // never a value.
  const bindingFacts: KeyValueItem[] = [
    { key: "Name", value: bindingName.trim() || undefined, absent: "not named yet" },
    {
      key: "Namespace",
      value: bindingNs.trim() || undefined,
      absent: "default",
      title: "Left blank — the binding is created in the default namespace.",
    },
    { key: "Backend", value: backend.trim() || undefined, absent: "kubernetes" },
    {
      key: "Secret name",
      value: secretRefName.trim() || undefined,
      absent: "not chosen yet",
    },
    {
      key: "Key",
      value: secretRefKey.trim() || undefined,
      absent: "not chosen yet",
    },
  ];

  const reviewStep: WizardStep = {
    id: "review",
    title: "Review",
    review: true,
    content: (
      <div className="space-y-5" data-testid="new-secret-review">
        <SectionHeader
          title="Review the binding"
          lede="Every line below is a pointer. Creating it writes a SecretBinding that names this Secret — the value stays where it already is."
        />
        {saveState.kind === "error" && saveState.forbidden && (
          <ForbiddenInline
            title="Not allowed to create bindings"
            description="Your account can't create SecretBindings in this cluster."
            resource="secret bindings"
            permission="create"
          />
        )}
        {saveState.kind === "error" && !saveState.forbidden && (
          <div
            className="rounded-lg border border-destructive/40 bg-destructive-surface/40 px-4 py-3"
            role="alert"
            data-testid="new-secret-error"
          >
            <p className="font-serif text-md font-medium text-destructive">
              The binding wasn’t created.
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
        <KeyValueList items={bindingFacts} />
      </div>
    ),
  };

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="New secret binding"
        lede="Point at a credential that already exists in the cluster. The console handles the pointer; the value never passes through it."
      />
      {/* §6.1 A4: the Wizard's 15rem rail + its 2rem gap + the archetype's
          46rem content column. Capping the outer column sizes the inner one. */}
      <div className="max-w-[63rem]">
        <Wizard
          steps={[identityStep, refStep, reviewStep]}
          current={current}
          onStepChange={setCurrent}
          busy={saveState.kind === "saving"}
          onFinish={() => { void doCreate(); }}
          finishLabel="Create binding"
          onCancel={() => navigate("/secrets")}
          dirty={bindingName.trim() !== ""}
        />
      </div>
    </div>
  );
}
