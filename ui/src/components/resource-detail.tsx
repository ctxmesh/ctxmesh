import * as React from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Pencil, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import {
  ConfirmDialog,
  DetailDrawer,
  ErrorState,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuietNote,
  Skeleton,
  SkeletonCard,
  type KeyValueItem,
  useToast,
} from "@/components/kit";
import { ApiError } from "@/lib/api";
import { useCapabilitiesIn } from "@/lib/capabilities";
import { useLoad } from "@/lib/use-load";

// ResourceDetailPage — the one detail page for the namespaced CRUD kinds (model routes,
// secret bindings, agent registries): M151 §6.1 archetype A2, form-edit variant.
//
// Everything the kinds share lives here: the read and its four states (loading, the
// permission boundary, the 404 as an honest absence, a retryable error), the header with
// Edit and Delete, the hub grid (the subject on the left, "The record" in a 300px rail
// that stacks below `lg`), the edit drawer at `?edit=1` and the typed-name delete at
// `?delete=1`. What a kind is for — its panels, its facts, its edit wizard — it supplies.
//
// Edit and Delete ask the item's own namespace, never the shell's: under "All workspaces"
// a row from another namespace opens here, and the shell's answer is about a different
// place. A 403 from the delete re-probes that namespace.

export interface ResourceIdentity {
  name: string;
  namespace: string;
}

export interface ResourceDetailKind<T extends ResourceIdentity> {
  /** Sentence case, "Model route": the title while loading, and the 404's heading. */
  noun: string;
  /** Lower case, "route": "Loading the route's record". */
  short: string;
  /** Lower-case plural, "model routes": the permission boundary's resource. */
  plural: string;
  /** The CRD kind the 404 names, "ModelRoute". */
  crdKind: string;
  /** The list page this kind lives under. `back` is the 404's way out. */
  list: { to: string; label: string; back: string };
  /** The capability resource: Edit asks `update`, Delete asks `delete`. */
  resource: string;
  testids: {
    page: string;
    loading: string;
    error: string;
    notFound: string;
    edit: string;
    delete: string;
  };
  read: (ns: string, name: string, signal: AbortSignal) => Promise<T>;
  status: (detail: T) => React.ReactNode;
  meta: (detail: T) => string;
  /** The main column: what the resource does. `canEdit` lets an empty section offer the fix. */
  Main: React.ComponentType<{ detail: T; canEdit: boolean }>;
  /** "The record": the bound facts, in the kv register. */
  facts: (detail: T) => KeyValueItem[];
  /** Rail cards after the record. */
  Rail?: React.ComponentType<{ detail: T }>;
  edit: {
    subtitle: (detail: T) => string;
    Wizard: React.ComponentType<{ detail: T; onClose: () => void; onSaved: () => void }>;
  };
  remove: {
    call: (ns: string, name: string) => Promise<unknown>;
    description: (detail: T) => string;
    confirmLabel: string;
    toastTitle: string;
  };
}

export function ResourceDetailPage<T extends ResourceIdentity>({
  kind,
}: {
  kind: ResourceDetailKind<T>;
}) {
  const { ns = "", name = "" } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { read } = kind;
  const fetcher = React.useCallback(
    (signal: AbortSignal) => read(ns, name, signal),
    [read, ns, name],
  );
  const { state, reload } = useLoad(fetcher);

  function setFlag(flag: "edit" | "delete", on: boolean) {
    setSearchParams((p) => {
      if (on) p.set(flag, "1");
      else p.delete(flag);
      return p;
    });
  }

  // The same trail in every state, so a failed load still offers the way back.
  const crumbs = (title: string) => [{ label: kind.list.label, to: kind.list.to }, { label: title }];

  // The header band instantly, a SkeletonCard where the panel will be and kv-line bars in
  // the rail, so nothing re-lays-out under the reader when the answer arrives.
  if (state.kind === "loading") {
    return (
      <div className="min-w-0 space-y-6" data-testid={kind.testids.loading}>
        <PageHeader breadcrumb={crumbs(name)} title={name || kind.noun} titleMono loading />
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
          <SkeletonCard className="min-w-0" />
          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <div role="status" aria-busy="true" aria-label={`Loading the ${kind.short}'s record`}>
                {[0, 1, 2, 3, 4, 5].map((i) => (
                  <Skeleton decorative key={i} className="mb-3 h-3.5 w-full" />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    );
  }

  if (state.kind === "error") {
    // A permission boundary replaces the page under the header: calm, resource-named,
    // never the raw RBAC string.
    if (state.forbidden) {
      return (
        <div className="min-w-0 space-y-6">
          <PageHeader breadcrumb={crumbs(name)} title={name} titleMono />
          <ForbiddenInline
            title={`You don't have permission to view ${name}.`}
            resource={kind.plural}
            detail={state.message}
          />
        </div>
      );
    }
    if (state.status === 404) {
      return (
        <div className="min-w-0 space-y-6" data-testid={kind.testids.notFound}>
          <PageHeader
            breadcrumb={[{ label: kind.list.label, to: kind.list.to }]}
            title={`${kind.noun} not found`}
            lede="Nothing in this workspace answers to that name."
          />
          <QuietNote title={`No ${kind.noun.toLowerCase()} with this name was found.`}>
            There is no {kind.crdKind} <span className="font-mono text-xs">{name}</span> in{" "}
            <span className="font-mono text-xs">{ns || "this namespace"}</span>. It may have been
            deleted, or the link may name a {kind.short} from another cluster. Nothing is missing
            from this page — there is simply no {kind.short} to show.
            <span className="mt-3 block">
              <NextStepLink label={kind.list.back} to={kind.list.to} />
            </span>
          </QuietNote>
        </div>
      );
    }
    return (
      <div className="min-w-0 space-y-6" data-testid={kind.testids.error}>
        <PageHeader breadcrumb={crumbs(name)} title={name} titleMono />
        <ErrorState
          title={`The ${kind.noun.toLowerCase()} didn't load.`}
          description={`Nothing has changed about the ${kind.short} itself — only this page failed to read it.`}
          detail={state.message}
          onRetry={() => reload()}
        />
      </div>
    );
  }

  return (
    <ResourceDetailContent
      kind={kind}
      detail={state.data}
      crumbs={crumbs}
      editOpen={searchParams.get("edit") === "1"}
      deleteOpen={searchParams.get("delete") === "1"}
      setFlag={setFlag}
      onDeleted={() => navigate(kind.list.to)}
      onSaved={() => {
        setFlag("edit", false);
        reload();
      }}
    />
  );
}

// A separate component so its hooks run only once there is a record to gate on.
function ResourceDetailContent<T extends ResourceIdentity>({
  kind,
  detail,
  crumbs,
  editOpen,
  deleteOpen,
  setFlag,
  onDeleted,
  onSaved,
}: {
  kind: ResourceDetailKind<T>;
  detail: T;
  crumbs: (title: string) => { label: string; to?: string }[];
  editOpen: boolean;
  deleteOpen: boolean;
  setFlag: (flag: "edit" | "delete", on: boolean) => void;
  onDeleted: () => void;
  onSaved: () => void;
}) {
  const { can } = useCapabilitiesIn(detail.namespace);
  const canEdit = can(kind.resource, "update");
  const canDelete = can(kind.resource, "delete");
  const { Main, Rail, edit } = kind;

  return (
    <div className="min-w-0 space-y-6" data-testid={kind.testids.page}>
      <PageHeader
        breadcrumb={crumbs(detail.name)}
        title={detail.name}
        titleMono
        status={kind.status(detail)}
        meta={kind.meta(detail)}
        actionsSlot={
          canEdit || canDelete ? (
            <>
              {canEdit && (
                <Button
                  variant="outline"
                  size="sm"
                  className="text-sm"
                  onClick={() => setFlag("edit", true)}
                  data-testid={kind.testids.edit}
                >
                  <Pencil className="h-4 w-4" />
                  Edit
                </Button>
              )}
              {canDelete && (
                // Crit as an ACTION colour is §2.3's one sanctioned exception: a
                // destructive control, told apart from a crit status by form.
                <Button
                  variant="destructive"
                  size="sm"
                  className="text-sm"
                  onClick={() => setFlag("delete", true)}
                  data-testid={kind.testids.delete}
                >
                  <Trash2 className="h-4 w-4" />
                  Delete
                </Button>
              )}
            </>
          ) : undefined
        }
      />

      {/* §4.7 hub grid: the subject on the left, the bound facts in the 300px rail,
          which stacks UNDER the main column below `lg`. */}
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0 space-y-5">
          <Main detail={detail} canEdit={canEdit} />
        </div>
        <div className="min-w-0 space-y-5">
          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent>
              <KeyValueList items={kind.facts(detail)} />
            </CardContent>
          </Card>
          {Rail && <Rail detail={detail} />}
        </div>
      </div>

      <DetailDrawer
        open={editOpen}
        onClose={() => setFlag("edit", false)}
        title={`Edit ${detail.name}`}
        subtitle={edit.subtitle(detail)}
        size="lg"
      >
        {editOpen && (
          <edit.Wizard detail={detail} onClose={() => setFlag("edit", false)} onSaved={onSaved} />
        )}
      </DetailDrawer>

      {deleteOpen && (
        <DeleteDialog
          kind={kind}
          detail={detail}
          onClose={() => setFlag("delete", false)}
          onDeleted={onDeleted}
        />
      )}
    </div>
  );
}

function DeleteDialog<T extends ResourceIdentity>({
  kind,
  detail,
  onClose,
  onDeleted,
}: {
  kind: ResourceDetailKind<T>;
  detail: T;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const { reprobe } = useCapabilitiesIn(detail.namespace);
  const [deleting, setDeleting] = React.useState(false);

  async function onConfirm() {
    setDeleting(true);
    try {
      await kind.remove.call(detail.namespace, detail.name);
      toast({ variant: "success", title: kind.remove.toastTitle, description: `${detail.name} removed.` });
      onDeleted();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      toast({
        variant: "error",
        title: "Delete failed",
        description: err instanceof Error ? err.message : "delete failed",
      });
      setDeleting(false);
      onClose();
    }
  }

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${detail.name}?`}
      description={kind.remove.description(detail)}
      confirmText={detail.name}
      confirmLabel={kind.remove.confirmLabel}
      busy={deleting}
    />
  );
}
