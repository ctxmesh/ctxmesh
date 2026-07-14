import * as React from "react";
import {
  GitBranch,
  Loader2,
  Minus,
  Plus,
  RefreshCw,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  ForbiddenInline,
  SkeletonTable,
  useToast,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import {
  api,
  ApiError,
  type PromptDiffLine,
  type PromptDiffResponse,
  type PromptVersionSummary,
} from "@/lib/api";
import { RES_PROMPTVERSIONS } from "@/lib/nav";

// PromptsPage — the PromptVersion list + textual diff viewer (m17.12).
//
// Two surfaces:
//   1. List — all PromptVersions for the current namespace.
//   2. Diff viewer — pick a "from" + "to" version → GET .../diff?from=
//      → renders the textual line diff.
//
// Honest degrade contract (non-negotiable):
//   200 → render lines; resolveMode="textual" ALWAYS shown explicitly.
//   501 → "prompt resolution not configured" calm state — NOT an error toast.
//   404 → "version/ref not found" — distinct honest state.
//   502 → "resolve failed (retry)" — real transient error.
//   NEVER fabricate a diff.
//
// RBAC: list/read open; create/delete gated on promptversions/create+delete.

// ---- discriminated state types -----------------------------------------------

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; versions: PromptVersionSummary[] }
  | { kind: "empty" }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

// DiffState captures the full discriminated outcome for the diff viewer.
// Each status-code outcome is DISTINCT — no fabricated diffs at any branch.
type DiffState =
  | { kind: "idle" }
  | { kind: "loading" }
  // 200 — real diff. resolveMode="textual" shown explicitly.
  | { kind: "ready"; diff: PromptDiffResponse }
  // 501 — no resolver configured: calm, NOT an error.
  | { kind: "not-configured" }
  // 404 — the from/to ref doesn't exist.
  | { kind: "not-found"; message: string }
  // 502 — resolver found but failed: retryable error.
  | { kind: "resolve-failed"; message: string }
  // other errors (network, 403, etc.)
  | { kind: "error"; message: string; forbidden?: boolean };

type DeleteState =
  | { kind: "idle" }
  | { kind: "deleting" }
  | { kind: "error"; message: string };

// ---- main page ----------------------------------------------------------------

export function PromptsPage() {
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [diffState, setDiffState] = React.useState<DiffState>({ kind: "idle" });
  const [fromName, setFromName] = React.useState("");
  const [toName, setToName] = React.useState("");
  const [deleteTarget, setDeleteTarget] =
    React.useState<PromptVersionSummary | null>(null);
  const [deleteState, setDeleteState] = React.useState<DeleteState>({
    kind: "idle",
  });
  const [showNewForm, setShowNewForm] = React.useState(false);

  const { can } = useCapabilities();
  const { namespace: shellNs } = useNamespace();
  const { toast } = useToast();

  const canCreate = can(RES_PROMPTVERSIONS, "create");
  const canDelete = can(RES_PROMPTVERSIONS, "delete");

  const load = React.useCallback(
    (signal?: AbortSignal) => {
      setPage({ kind: "loading" });
      api
        .listPromptVersions({ namespace: shellNs || undefined }, signal)
        .then((res) => {
          if (signal?.aborted) return;
          if (res.items.length === 0) {
            setPage({ kind: "empty" });
          } else {
            setPage({ kind: "ready", versions: res.items });
          }
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return;
          if (err instanceof ApiError) {
            if (err.isForbidden) {
              setPage({ kind: "forbidden", message: err.message });
              return;
            }
            setPage({ kind: "error", message: err.message });
            return;
          }
          setPage({
            kind: "error",
            message: err instanceof Error ? err.message : "request failed",
          });
        });
    },
    [shellNs],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // Find the "to" version object for namespace resolution
  const toVersion = React.useMemo(() => {
    if (page.kind !== "ready") return null;
    return page.versions.find((v) => v.name === toName) ?? null;
  }, [page, toName]);

  async function handleFetchDiff() {
    if (!toName || !fromName || !toVersion) return;
    setDiffState({ kind: "loading" });
    try {
      const result = await api.promptVersionDiff(
        toVersion.namespace,
        toVersion.name,
        fromName,
      );
      if (result === null) {
        // 501 — no resolver configured. Calm state, NOT an error.
        setDiffState({ kind: "not-configured" });
        return;
      }
      setDiffState({ kind: "ready", diff: result });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          setDiffState({ kind: "error", message: err.message, forbidden: true });
          return;
        }
        if (err.isNotFound) {
          // 404 — version/ref not found. Distinct from other errors.
          setDiffState({ kind: "not-found", message: err.message });
          return;
        }
        if (err.status === 502) {
          // 502 — resolver found but the resolve call failed. Retryable.
          setDiffState({ kind: "resolve-failed", message: err.message });
          return;
        }
        setDiffState({ kind: "error", message: err.message });
        return;
      }
      setDiffState({
        kind: "error",
        message: err instanceof Error ? err.message : "diff request failed",
      });
    }
  }

  async function handleDelete() {
    if (!deleteTarget) return;
    setDeleteState({ kind: "deleting" });
    try {
      await api.removePromptVersion(deleteTarget.namespace, deleteTarget.name);
      setDeleteState({ kind: "idle" });
      setDeleteTarget(null);
      toast({ variant: "success", title: `Deleted ${deleteTarget.name}` });
      load();
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? err.message
          : err instanceof Error
          ? err.message
          : "delete failed";
      setDeleteState({ kind: "error", message: msg });
    }
  }

  return (
    <div className="mx-auto max-w-4xl space-y-6" data-testid="prompts-page">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Prompts</h2>
          <p className="text-sm text-muted-foreground">
            Reusable, versioned prompts you can attach to an agent — pick one under{" "}
            <span className="font-medium">Prompt version</span> when you{" "}
            <span className="font-medium">Configure</span> an agent, then roll a new
            version here and the agent picks it up (safe, reviewable prompt changes).
            Compare any two versions with a line diff below.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="icon"
            onClick={() => load()}
            aria-label="Refresh prompts"
            data-testid="prompts-refresh"
          >
            <RefreshCw className="h-4 w-4" />
          </Button>
          {canCreate && (
            <Button
              size="sm"
              onClick={() => setShowNewForm(true)}
              data-testid="prompts-new-btn"
            >
              New version
            </Button>
          )}
        </div>
      </div>

      {page.kind === "loading" && <SkeletonTable rows={4} />}

      {page.kind === "forbidden" && (
        <ForbiddenInline
          title="Not allowed to view prompt versions"
          description="Reading prompt versions requires list permission."
          detail={page.message}
        />
      )}

      {page.kind === "error" && (
        <ErrorState
          title="Couldn't load prompt versions"
          description={page.message}
          onRetry={() => load()}
        />
      )}

      {page.kind === "empty" && (
        <EmptyState
          icon={GitBranch}
          title="No prompt versions yet"
          description="Create a prompt version to start tracking prompt changes over time."
          action={
            canCreate
              ? {
                  label: "New version",
                  onClick: () => setShowNewForm(true),
                  variant: "default",
                }
              : undefined
          }
        />
      )}

      {page.kind === "ready" && (
        <>
          {/* Version list */}
          <div
            className="rounded-lg border bg-card shadow-card divide-y"
            data-testid="prompt-version-list"
          >
            {page.versions.map((v) => (
              <VersionRow
                key={`${v.namespace}/${v.name}`}
                version={v}
                canDelete={canDelete}
                onDelete={() => setDeleteTarget(v)}
              />
            ))}
          </div>

          {/* Diff viewer */}
          <div className="rounded-lg border bg-card p-4 space-y-4">
            <h3 className="text-sm font-semibold">Diff viewer</h3>
            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-1.5">
                <Label htmlFor="diff-from">From (base version name)</Label>
                <Input
                  id="diff-from"
                  value={fromName}
                  onChange={(e) => setFromName(e.target.value)}
                  placeholder="v1"
                  list="prompt-version-names"
                  data-testid="diff-from-input"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="diff-to">To (target version name)</Label>
                <Input
                  id="diff-to"
                  value={toName}
                  onChange={(e) => setToName(e.target.value)}
                  placeholder="v2"
                  list="prompt-version-names"
                  data-testid="diff-to-input"
                />
              </div>
            </div>
            {/* Datalist for autocomplete */}
            <datalist id="prompt-version-names">
              {page.versions.map((v) => (
                <option key={v.name} value={v.name} />
              ))}
            </datalist>
            <Button
              size="sm"
              disabled={!fromName || !toName || !toVersion || diffState.kind === "loading"}
              onClick={handleFetchDiff}
              data-testid="diff-compare-btn"
            >
              {diffState.kind === "loading" ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  Comparing…
                </>
              ) : (
                "Compare"
              )}
            </Button>

            {/* Diff output — honest degrade per contract */}
            {diffState.kind !== "idle" && (
              <DiffOutput
                state={diffState}
                fromName={fromName}
                toName={toName}
                onRetry={handleFetchDiff}
              />
            )}
          </div>
        </>
      )}

      {/* New prompt version form */}
      {showNewForm && (
        <NewPromptVersionForm
          onClose={() => setShowNewForm(false)}
          onCreated={() => {
            setShowNewForm(false);
            load();
          }}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          open
          title={`Delete ${deleteTarget.name}?`}
          description={`This will delete the prompt version "${deleteTarget.name}" in namespace "${deleteTarget.namespace}". This cannot be undone.`}
          confirmLabel={deleteState.kind === "deleting" ? "Deleting…" : "Delete"}
          busy={deleteState.kind === "deleting"}
          onConfirm={handleDelete}
          onCancel={() => {
            setDeleteTarget(null);
            setDeleteState({ kind: "idle" });
          }}
          impact={
            deleteState.kind === "error" ? (
              <p className="text-sm text-destructive" role="alert">
                {deleteState.message}
              </p>
            ) : undefined
          }
        />
      )}
    </div>
  );
}

// ---- VersionRow ----------------------------------------------------------------

interface VersionRowProps {
  version: PromptVersionSummary;
  canDelete: boolean;
  onDelete: () => void;
}

function VersionRow({ version, canDelete, onDelete }: VersionRowProps) {
  return (
    <div
      className="flex items-center gap-4 px-4 py-3"
      data-testid={`prompt-version-${version.name}`}
    >
      <GitBranch className="h-4 w-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1 space-y-0.5">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-sm font-medium">{version.name}</span>
          <Badge variant="secondary" className="text-[10px]">
            {version.namespace}
          </Badge>
          <Badge variant="outline" className="text-[10px]">
            ref: {version.ref}
          </Badge>
        </div>
        {version.promptName && (
          <p className="text-xs text-muted-foreground">
            prompt: {version.promptName}
          </p>
        )}
        {version.createdAt && (
          <p className="text-xs text-muted-foreground">
            {new Date(version.createdAt).toLocaleString()}
          </p>
        )}
      </div>
      {canDelete && (
        <Button
          variant="ghost"
          size="sm"
          className="shrink-0 text-destructive hover:text-destructive"
          onClick={onDelete}
          data-testid={`prompt-delete-${version.name}`}
        >
          Delete
        </Button>
      )}
    </div>
  );
}

// ---- DiffOutput ---------------------------------------------------------------
// Renders the diff result HONESTLY per the contract.
// Each of the four degrade states is DISTINCT — no fabricated diffs.

interface DiffOutputProps {
  state: DiffState;
  fromName: string;
  toName: string;
  onRetry: () => void;
}

function DiffOutput({ state, fromName, toName, onRetry }: DiffOutputProps) {
  if (state.kind === "loading") {
    return (
      <div
        className="flex items-center gap-2 py-2 text-sm text-muted-foreground"
        data-testid="prompt-diff-loading"
      >
        <Loader2 className="h-4 w-4 animate-spin" />
        Comparing…
      </div>
    );
  }

  // 501 — no resolver configured. Calm state. NOT a toast or a fabricated diff.
  if (state.kind === "not-configured") {
    return (
      <p
        className="text-sm text-muted-foreground"
        data-testid="prompt-diff-not-configured"
      >
        Prompt resolution is not configured on this cluster. Contact your
        operator to enable the textual resolver.
      </p>
    );
  }

  // 404 — version/ref not found. Distinct from 502 and other errors.
  if (state.kind === "not-found") {
    return (
      <p
        className="text-sm text-muted-foreground"
        data-testid="prompt-diff-not-found"
        role="alert"
      >
        Version or ref not found: {state.message}
      </p>
    );
  }

  // 502 — resolver found but the resolve call failed. Retryable.
  if (state.kind === "resolve-failed") {
    return (
      <div
        className="flex items-center gap-3 rounded-md border border-warning/30 bg-warning/5 px-4 py-3"
        data-testid="prompt-diff-resolve-failed"
      >
        <div className="flex-1">
          <p className="text-sm font-medium text-warning-foreground">
            Resolve failed — retry
          </p>
          <p className="text-xs text-muted-foreground">{state.message}</p>
        </div>
        <Button size="sm" variant="outline" onClick={onRetry}>
          Retry
        </Button>
      </div>
    );
  }

  // Other errors (network, 403, etc.)
  if (state.kind === "error") {
    return (
      <p
        className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
        role="alert"
        data-testid="prompt-diff-error"
      >
        {state.message}
      </p>
    );
  }

  // 200 — real diff. resolveMode="textual" ALWAYS shown explicitly.
  if (state.kind === "ready") {
    const { diff } = state;
    return (
      <div className="space-y-2" data-testid="prompt-diff">
        {/* resolveMode ALWAYS shown explicitly — never implied semantic */}
        <div className="flex items-center gap-2">
          <Badge
            variant="secondary"
            className="text-[10px]"
            data-testid="prompt-diff-resolve-mode"
          >
            resolve mode: {diff.resolveMode}
          </Badge>
          <span className="text-xs text-muted-foreground">
            {fromName} → {toName}
          </span>
        </div>
        {diff.lines.length === 0 ? (
          <p className="text-sm text-muted-foreground">No differences found.</p>
        ) : (
          <div
            className="rounded-md border bg-muted/30 overflow-x-auto"
            data-testid="prompt-diff-lines"
          >
            <pre className="p-3 text-xs font-mono leading-relaxed">
              {diff.lines.map((line, i) => (
                <DiffLineRow key={i} line={line} />
              ))}
            </pre>
          </div>
        )}
      </div>
    );
  }

  return null;
}

function DiffLineRow({ line }: { line: PromptDiffLine }) {
  const cls =
    line.op === "+"
      ? "text-success bg-success/5"
      : line.op === "-"
      ? "text-destructive bg-destructive/5"
      : "text-muted-foreground";
  return (
    <div className={`flex items-start gap-2 ${cls}`} data-testid={`diff-line-${line.op}`}>
      <span className="w-4 shrink-0 select-none text-center">
        {line.op === "+" ? (
          <Plus className="h-3 w-3 inline" />
        ) : line.op === "-" ? (
          <Minus className="h-3 w-3 inline" />
        ) : (
          " "
        )}
      </span>
      <span>{line.content}</span>
    </div>
  );
}

// ---- NewPromptVersionForm ----------------------------------------------------

interface NewPromptVersionFormProps {
  onClose: () => void;
  onCreated: () => void;
}

type FormState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "error"; message: string };

function NewPromptVersionForm({ onClose, onCreated }: NewPromptVersionFormProps) {
  const [ref, setRef] = React.useState("");
  const [promptName, setPromptName] = React.useState("");
  const [name, setName] = React.useState("");
  const [namespace, setNamespace] = React.useState("");
  const [content, setContent] = React.useState("");
  const [formState, setFormState] = React.useState<FormState>({ kind: "idle" });

  const { namespace: shellNs } = useNamespace();
  const { reprobe } = useCapabilities();
  const { toast } = useToast();

  React.useEffect(() => {
    if (shellNs) setNamespace(shellNs);
  }, [shellNs]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!ref.trim() || !promptName.trim()) return;
    setFormState({ kind: "submitting" });
    try {
      const created = await api.createPromptVersion({
        name: name.trim() || undefined,
        namespace: namespace.trim() || undefined,
        ref: ref.trim(),
        promptName: promptName.trim(),
        content: content.trim() || undefined,
      });
      toast({ variant: "success", title: `Created ${created.name}` });
      onCreated();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isForbidden) {
          reprobe();
          setFormState({ kind: "error", message: `Not allowed: ${err.message}` });
          return;
        }
        setFormState({ kind: "error", message: err.message });
        return;
      }
      setFormState({
        kind: "error",
        message: err instanceof Error ? err.message : "create failed",
      });
    }
  }

  const busy = formState.kind === "submitting";

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      data-testid="new-prompt-version-form"
    >
      <div className="w-full max-w-lg rounded-xl bg-background shadow-xl p-6 space-y-4">
        <h3 className="text-lg font-semibold">New prompt version</h3>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="pv-prompt-name">
              Prompt name <span className="text-destructive">*</span>
            </Label>
            <Input
              id="pv-prompt-name"
              value={promptName}
              onChange={(e) => setPromptName(e.target.value)}
              placeholder="my-system-prompt"
              required
              data-testid="pv-prompt-name-input"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pv-ref">
              Ref <span className="text-destructive">*</span>
            </Label>
            <Input
              id="pv-ref"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              placeholder="v1 or abc1234"
              required
              data-testid="pv-ref-input"
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="pv-name">Name (optional)</Label>
              <Input
                id="pv-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="auto-generated"
                data-testid="pv-name-input"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pv-namespace">Namespace</Label>
              <Input
                id="pv-namespace"
                value={namespace}
                onChange={(e) => setNamespace(e.target.value)}
                placeholder="default"
                data-testid="pv-namespace-input"
              />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pv-content">Content (optional)</Label>
            <textarea
              id="pv-content"
              className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[80px]"
              value={content}
              onChange={(e) => setContent(e.target.value)}
              placeholder="You are a helpful assistant…"
              data-testid="pv-content-input"
            />
          </div>
          {formState.kind === "error" && (
            <p
              className="text-sm text-destructive rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2"
              role="alert"
              data-testid="pv-form-error"
            >
              {formState.message}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !ref.trim() || !promptName.trim()}>
              {busy ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  Creating…
                </>
              ) : (
                "Create"
              )}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
