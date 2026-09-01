import * as React from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { ExternalLink, Share2, Trash2, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  CellEntity,
  ClosingNote,
  ConfirmDialog,
  CredentialSourceBadge,
  DataTable,
  DetailDrawer,
  FilterChipRow,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuantityValue,
  StatusBadge,
  useFocusTrap,
  useToast,
  VisibilityBadge,
  nextStepRank,
  resolveStatus,
  type Column,
  type DataTableError,
  type NextStepTone,
  type StatusTone,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { cn } from "@/lib/utils";
import {
  api,
  ApiError,
  type McpServerReference,
  type McpServerSummary,
} from "@/lib/api";

// McpServersPage — archetype A1 (index/table), resource-list column budget
// (M151 spec §6.1/§4.4). The LIST of registered BYO-MCP servers, with the
// "Add MCP server" action IN THE HEADER (not a separate nav item, m25 S8).
// Read-open so a viewer sees the servers; the Add button is gated on create
// agentregistries.
//
// ── FROM CARDS TO A TABLE ───────────────────────────────────────────────────
// This page used to be a stack of cards, which is why it could never say which
// server was blocking: nine equal-weight cards in alphabetical order have no
// first row. It is now the console's one table, ordered by what needs a person
// (§6.1 A1) — refused first, then awaiting approval, then a server that yields
// no tools; everything healthy says "Nothing needed" and sinks to the bottom.
//
// ── WHY THERE IS A DRAWER ───────────────────────────────────────────────────
// An MCP server has no detail route, and the §4.4 budget drops its URL, auth
// and sharing columns below 1280/1024. "Dropped ≠ lost" is only true if the row
// opens somewhere, so a row-click opens a drawer carrying every field — the URL
// in a code well, because a URL never belongs in a bare table cell (§4.5).

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; servers: McpServerSummary[] }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

// LocationState carries the highlight anchor from a post-connect navigation (T11).
interface LocationState {
  highlight?: string; // "<ns>/<name>" of the newly-connected server
}

type View = "all" | "attention" | "shared";

/** Attention order inside a next-step group (§6.1 A1). */
const TONE_RANK: Record<StatusTone, number> = {
  failed: 0,
  waiting: 1,
  progressing: 2,
  ready: 3,
  draft: 4,
};

/**
 * The BFF's approval `status` → the status vocabulary (M144.1 / §2.2). A pending
 * server is NOT "converging": nothing happens until a person approves it, which
 * is the hold state — the one distinction ops tooling most often loses.
 */
function serverStatus(s: McpServerSummary): { ready: boolean; phase: string } {
  switch ((s.status ?? "").toLowerCase()) {
    case "approved":
      return { ready: true, phase: "Approved" };
    case "pending":
      return { ready: false, phase: "AwaitingApproval" };
    case "rejected":
      return { ready: false, phase: "Rejected" };
    default:
      return { ready: false, phase: s.status ?? "" };
  }
}

/** What the row needs from a person (§7.2). `none` renders the inert "Nothing needed". */
type StepKind = "refusal" | "request" | "connection" | "none";

function stepKind(s: McpServerSummary): StepKind {
  const st = (s.status ?? "").toLowerCase();
  if (st === "rejected") return "refusal";
  if (st === "pending") return "request";
  // Connected, approved, and yielding nothing: the tools are the entire point
  // of an MCP server, so zero of them is a real problem — and a KNOWN zero,
  // which is why the Tools cell prints `0` rather than a dash.
  if (s.toolCount === 0) return "connection";
  return "none";
}

const STEP_LABEL: Record<Exclude<StepKind, "none">, string> = {
  refusal: "Review the refusal", //   18 chars
  request: "Review the request", //   18
  connection: "Check the connection", // 20
};

const STEP_TONE: Record<Exclude<StepKind, "none">, NextStepTone> = {
  refusal: "crit",
  request: "default",
  connection: "default",
};

/**
 * The closing line's copy (§5.18) — a SIGHTED FLOURISH restating the table's
 * ratio, built only from counts the backend actually sent. `totalTools` is a sum
 * of real per-server counts, never an estimate.
 */
function closingNote(total: number, needing: number, totalTools: number): string {
  const tools = `${totalTools} ${totalTools === 1 ? "tool" : "tools"}`;
  if (total === 1)
    return needing > 0
      ? "The one server here needs a person."
      : `The one server here is connected, exposing ${tools}.`;
  if (needing === 0)
    return `All ${total} servers are connected, exposing ${tools} between them. Nothing here needs a person.`;
  return `${total} servers here, exposing ${tools} between them. ${needing} of them need a person.`;
}

/** The auth Tag: a declared capability, never a health hue (§5.6). */
function AuthBadge({ server }: { server: McpServerSummary }) {
  if (server.authType === "oauth") return <Badge variant="muted">OAuth</Badge>;
  if (server.secretName) return <Badge variant="muted">Key</Badge>;
  // Declared but nothing attached — the dashed `open` Tag (§2.5).
  return <Badge variant="open">no auth</Badge>;
}

export function McpServersPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const { can } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");
  const canDelete = can(RES_REGISTRIES, "delete");
  // Promoting a server to org scope UPDATES its ToolRegistry — the RBAC admin gate.
  const canPromote = can(RES_REGISTRIES, "update");
  const [page, setPage] = React.useState<PageState>({ kind: "loading" });
  const [query, setQuery] = React.useState("");
  const [view, setView] = React.useState<View>("all");
  // The server whose drawer is open (row-click) — where the dropped columns live.
  const [inspect, setInspect] = React.useState<McpServerSummary | null>(null);
  // The server currently queued for deletion (its ConfirmDialog is open).
  const [toDelete, setToDelete] = React.useState<McpServerSummary | null>(null);
  // The server queued for the unified Share flow (T5).
  const [toShare, setToShare] = React.useState<McpServerSummary | null>(null);

  // T11 — post-connect row highlight. Read once from location.state and clear so
  // a back-navigate doesn't re-trigger the flash.
  const highlightKey = (location.state as LocationState | null)?.highlight ?? null;
  const [highlighted, setHighlighted] = React.useState<string | null>(highlightKey);
  React.useEffect(() => {
    if (!highlightKey) return;
    // Clear location state so a reload doesn't re-highlight.
    window.history.replaceState({}, "");
    const t = setTimeout(() => setHighlighted(null), 2000);
    return () => clearTimeout(t);
  }, [highlightKey]);

  const load = React.useCallback((signal?: AbortSignal) => {
    setPage({ kind: "loading" });
    api
      .listMcpServers(signal)
      .then((res) => {
        if (signal?.aborted) return;
        setPage({ kind: "ready", servers: res.items ?? res.servers ?? [] });
      })
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        if (err instanceof ApiError && err.isForbidden) {
          setPage({ kind: "forbidden", message: err.message });
          return;
        }
        setPage({
          kind: "error",
          message: err instanceof Error ? err.message : "request failed",
        });
      });
  }, []);

  React.useEffect(() => {
    const c = new AbortController();
    load(c.signal);
    return () => c.abort();
  }, [load]);

  const servers = React.useMemo(
    () => (page.kind === "ready" ? page.servers : []),
    [page],
  );

  // Decorate once — status, next step and sort key from the same resolve.
  const decorated = React.useMemo(
    () =>
      servers
        .map((s) => {
          const { ready, phase } = serverStatus(s);
          return { row: s, tone: resolveStatus(ready, phase).tone, kind: stepKind(s) };
        })
        .sort(
          (a, b) =>
            nextStepRank(a.kind === "none" ? "none" : "default") -
              nextStepRank(b.kind === "none" ? "none" : "default") ||
            TONE_RANK[a.tone] - TONE_RANK[b.tone] ||
            a.row.name.localeCompare(b.row.name),
        ),
    [servers],
  );

  const q = query.trim().toLowerCase();
  const inView = decorated.filter((d) => {
    if (view === "attention") return d.kind !== "none";
    if (view === "shared") return d.row.credentialSource === "shared";
    return true;
  });
  const visible = q ? inView.filter((d) => d.row.name.toLowerCase().includes(q)) : inView;
  const rows = visible.map((d) => d.row);
  const kindFor = new Map(
    decorated.map((d) => [`${d.row.namespace}/${d.row.name}`, d.kind] as const),
  );

  // Ratio inputs — counts of what the backend actually sent, never a guess.
  const needing = decorated.filter((d) => d.kind !== "none").length;
  const totalTools = decorated.reduce((n, d) => n + d.row.toolCount, 0);

  const error: DataTableError | null =
    page.kind === "forbidden"
      ? { message: page.message, forbidden: true, resource: "MCP servers" }
      : page.kind === "error"
        ? { message: page.message, onRetry: () => load() }
        : null;

  // §4.4 resource-list budget. Server / State / Next step are priority 1 and
  // survive 768; URL + Sharing take the p4 slot, Auth + Tools p3.
  const columns: Column<McpServerSummary>[] = [
    {
      id: "name",
      header: "Server",
      className: "max-w-[10rem] lg:max-w-[13rem] xl:max-w-[18rem]",
      cell: (s) => (
        <div
          data-testid={`mcp-server-${s.name}`}
          className={cn(
            "min-w-0 rounded-sm transition-shadow duration-700",
            // Selection/attention is always pine-family, never a status hue
            // (§2.3). The ring is a box-shadow, so it never widens the row.
            highlighted === `${s.namespace}/${s.name}` && "bg-accent ring-2 ring-primary",
          )}
        >
          <CellEntity name={s.name} namespace={s.namespace} />
        </div>
      ),
    },
    {
      id: "url",
      header: "Endpoint",
      priority: 4,
      className: "max-w-[9rem] lg:max-w-[12rem] xl:max-w-[16rem]",
      cell: (s) => (
        <span className="block truncate font-mono text-xs text-faint" title={s.url}>
          {s.url}
        </span>
      ),
    },
    {
      id: "auth",
      header: "Auth",
      // 4, not 3: with Auth showing, this table needs 1009px and the content
      // column at a 1280 viewport is 974. How a server authenticates is a
      // property you check on one server, not a column you scan down — it
      // stays in the row's detail below xl.
      priority: 4,
      cell: (s) => (
        <div className="flex flex-wrap items-center gap-1.5">
          <AuthBadge server={s} />
          <CredentialSourceBadge credentialSource={s.credentialSource} name={s.name} />
        </div>
      ),
    },
    {
      id: "tools",
      header: "Tools",
      priority: 3,
      numeric: true,
      cell: (s) => <QuantityValue value={s.toolCount} />,
    },
    {
      id: "sharing",
      header: "Sharing",
      priority: 4,
      cell: (s) => (
        <div className="flex flex-wrap items-center gap-1.5">
          <VisibilityBadge visibility={s.visibility} name={s.name} />
          {s.scope && (
            // A declared scope, not a health state — muted, never a hue (§5.6).
            <Badge variant="muted" data-testid={`scope-${s.name}`}>
              {s.scope}
            </Badge>
          )}
        </div>
      ),
    },
    {
      id: "state",
      header: "State",
      className: "w-[9rem]",
      cell: (s) => {
        const { ready, phase } = serverStatus(s);
        return <StatusBadge ready={ready} phase={phase} />;
      },
    },
    {
      id: "nextStep",
      header: "Next step",
      className: "w-[11rem]",
      cell: (s) => {
        const kind = kindFor.get(`${s.namespace}/${s.name}`) ?? "none";
        if (kind === "none")
          return <NextStepLink tone="none" testId={`next-step-${s.name}`} />;
        return (
          <NextStepLink
            label={STEP_LABEL[kind]}
            tone={STEP_TONE[kind]}
            testId={`next-step-${s.name}`}
            // A refused or pending server is decided in the approvals queue; a
            // server yielding no tools is diagnosed from its own wiring, which
            // is what the drawer shows.
            to={kind === "connection" ? undefined : "/tools/approvals"}
            onClick={kind === "connection" ? () => setInspect(s) : undefined}
          />
        );
      },
    },
  ];

  const emptyState =
    view !== "all"
      ? {
          icon: Wrench,
          intent: "filtered" as const,
          title: view === "attention" ? "Nothing here needs you" : "No shared credentials",
          description:
            view === "attention"
              ? "Every server is approved and exposing tools."
              : "Every server here is connected per-user or needs no credential at all — nobody is sharing one token.",
          totalCount: decorated.length > 0 ? decorated.length : undefined,
          countNoun: "servers",
          action: {
            label: "Show everything",
            variant: "outline" as const,
            onClick: () => setView("all"),
          },
        }
      : {
          icon: Wrench,
          title: "No MCP servers yet",
          description: (
            <>
              Add your own MCP server, or{" "}
              <Link
                to="/gallery?tab=mcp"
                className="text-primary border-b border-accent hover:border-primary"
                data-testid="gallery-discover-link"
              >
                discover shared servers
              </Link>{" "}
              in the Gallery and connect one to your namespace.
            </>
          ),
          action: canAdd
            ? { label: "Add MCP server", onClick: () => navigate("/tools/add-mcp") }
            : undefined,
        };

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="MCP servers"
        lede="The MCP servers this namespace can reach. Each exposes tools you attach to agents — whatever is refused, awaiting a decision, or yielding nothing is at the top."
        actions={
          canAdd
            ? [
                {
                  id: "add",
                  label: "Add MCP server",
                  onClick: () => navigate("/tools/add-mcp"),
                  primary: true,
                },
              ]
            : undefined
        }
      />

      {/* Views, not filters (§5.28) — one question, one answer. No counts: the
          chip contract takes backend counts only. */}
      {decorated.length > 0 && (
        <FilterChipRow
          label="Filter servers"
          value={view}
          onChange={(id) => setView(id as View)}
          chips={[
            { id: "all", label: "Everything" },
            { id: "attention", label: "Needs attention" },
            { id: "shared", label: "Shared credential" },
          ]}
        />
      )}

      <DataTable<McpServerSummary>
        columns={columns}
        rows={rows}
        rowKey={(s) => `${s.namespace}/${s.name}`}
        loading={page.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter servers by name…"
        ariaLabel="MCP servers"
        onRowClick={(s) => setInspect(s)}
        rowActions={
          canPromote || canDelete
            ? (s) => (
                <div
                  className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
                  onClick={(e) => e.stopPropagation()}
                >
                  {canPromote && (
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7"
                      onClick={() => setToShare(s)}
                      data-testid={`share-mcp-${s.name}`}
                      aria-label={`Share ${s.name}`}
                      title="Share"
                    >
                      <Share2 className="h-3.5 w-3.5" />
                    </Button>
                  )}
                  {canDelete && (
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7 text-destructive hover:text-destructive"
                      onClick={() => setToDelete(s)}
                      data-testid={`delete-mcp-${s.name}`}
                      aria-label={`Delete ${s.name}`}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  )}
                </div>
              )
            : undefined
        }
        empty={emptyState}
      />

      {page.kind === "ready" && decorated.length > 0 && (
        <ClosingNote>
          {closingNote(decorated.length, needing, totalTools)}
        </ClosingNote>
      )}

      {/* Dropped ≠ lost (§4.4): the columns the budget hides render here, and
          the URL gets a code well rather than a bare cell (§4.5). */}
      <DetailDrawer
        open={inspect !== null}
        onClose={() => setInspect(null)}
        title={inspect?.name ?? ""}
        subtitle={inspect?.namespace}
        status={
          inspect ? (
            <StatusBadge {...serverStatus(inspect)} />
          ) : undefined
        }
        footer={
          <Button asChild variant="outline" className="w-full">
            <Link to="/tools/catalog">
              <ExternalLink className="h-4 w-4" />
              Browse its tools
            </Link>
          </Button>
        }
      >
        {inspect && (
          <div className="space-y-5" data-testid="mcp-server-drawer">
            <div>
              <p className="font-mono text-2xs uppercase tracking-wide text-faint">
                Endpoint
              </p>
              <pre className="mt-1.5 overflow-x-auto rounded-md bg-surface-3 px-3 py-2 font-mono text-xs text-secondary-foreground">
                <span className="break-all">{inspect.url}</span>
              </pre>
            </div>
            <KeyValueList
              items={[
                { key: "Tools", value: <QuantityValue value={inspect.toolCount} /> },
                {
                  key: "Auth",
                  value:
                    inspect.authType === "oauth"
                      ? "OAuth"
                      : inspect.secretName
                        ? "API key"
                        : "",
                  absent: "no credential attached",
                  mono: false,
                },
                { key: "Secret", value: inspect.secretName, absent: "not attached" },
                {
                  key: "Credential",
                  value:
                    inspect.credentialSource && inspect.credentialSource !== "none"
                      ? inspect.credentialSource
                      : "",
                  absent: "none needed",
                },
                { key: "Visibility", value: inspect.visibility, absent: "not published" },
                { key: "Scope", value: inspect.scope, absent: "not yet known" },
                { key: "Approval", value: inspect.status, absent: "not yet known" },
              ]}
            />
          </div>
        )}
      </DetailDrawer>

      {toDelete && (
        <DeleteMcpDialog
          server={toDelete}
          onClose={() => setToDelete(null)}
          onDeleted={() => {
            setToDelete(null);
            load();
          }}
        />
      )}

      {toShare && (
        <ShareDialog
          server={toShare}
          onClose={() => setToShare(null)}
          onDone={() => {
            setToShare(null);
            load();
          }}
        />
      )}
    </div>
  );
}

// ShareDialog (m76.2 T5) — ONE unified "Share" entry point that replaces the two
// side-by-side share controls. The user picks a mode first:
//   - "byo" (BYO-safe, recommended): publish the server definition; teammates connect
//     their OWN accounts — no credential sharing. Reveals a visibility picker.
//   - "shared-cred": share ONE org credential so every user's runs inject it.
//     Reveals a credential input. Warns clearly that this is the non-BYO path.
//
// Both backend paths are preserved; the entry point is unified (T5 fix).
//
// T7: dialogs stay OPEN on failure, showing an inline error. Only close on success.

type ShareMode = "byo" | "shared-cred";
type PublishVisibility = "team" | "org" | "public";

function ShareDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [mode, setMode] = React.useState<ShareMode>("byo");
  const [inlineError, setInlineError] = React.useState<string | null>(null);

  // BYO publish sub-state
  const defaultVisibility: PublishVisibility =
    server.visibility === "org" || server.visibility === "public" || server.visibility === "team"
      ? (server.visibility as PublishVisibility)
      : "team";
  const [selected, setSelected] = React.useState<PublishVisibility>(defaultVisibility);
  const [publicConfirmed, setPublicConfirmed] = React.useState(false);

  // Shared-cred sub-state
  const [credential, setCredential] = React.useState("");

  const [busy, setBusy] = React.useState(false);
  const panelRef = useFocusTrap<HTMLDivElement>({ active: true, onEscape: onClose });

  function handleModeChange(m: ShareMode) {
    setMode(m);
    setInlineError(null);
    if (m !== "byo") setPublicConfirmed(false);
  }

  function handleVisibilitySelect(v: PublishVisibility) {
    setSelected(v);
    if (v !== "public") setPublicConfirmed(false);
    setInlineError(null);
  }

  const isSubmitDisabled =
    busy ||
    (mode === "byo" && selected === "public" && !publicConfirmed) ||
    (mode === "shared-cred" && !credential.trim());

  async function onSubmit() {
    if (isSubmitDisabled) return;
    setBusy(true);
    setInlineError(null);

    if (mode === "byo") {
      try {
        await api.publishMcpServer(server.namespace, server.name, selected);
        toast({
          variant: "success",
          title: "Visibility updated",
          description: `${server.name} is now ${selected}-visible.`,
        });
        onDone();
      } catch (err) {
        const isForbidden = err instanceof ApiError && err.isForbidden;
        const serverMsg = err instanceof ApiError ? err.message : null;
        const fallback = isForbidden
          ? `You need Tenant-admin rights to publish ${selected}-wide.`
          : err instanceof Error
          ? err.message
          : "publish failed";
        // T7: stay open with inline error; use server message when available (T12)
        setInlineError(serverMsg || fallback);
        setBusy(false);
      }
    } else {
      // shared-cred mode
      const cred = credential;
      setCredential(""); // wipe before the round-trip — the secret never lingers in state
      try {
        await api.setOrgCredential({
          server: server.name,
          namespace: server.namespace,
          credential: cred,
        });
        toast({
          variant: "success",
          title: "Org credential set",
          description: `${server.name} is now shared org-wide.`,
        });
        onDone();
      } catch (err) {
        const serverMsg = err instanceof ApiError ? err.message : null;
        const fallback = err instanceof Error ? err.message : "failed";
        // T7: stay open with inline error; use server message when available (T12)
        setInlineError(serverMsg || fallback);
        // Restore credential so user can fix without re-pasting
        setCredential(cred);
        setBusy(false);
      }
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${server.name}`}
      data-testid="share-dialog"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        className="relative w-full max-w-lg rounded-lg border bg-card p-6 shadow-overlay outline-none"
      >
        <h2 className="text-lg font-semibold tracking-snug">Share {server.name}</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Choose how teammates can access this server.
        </p>
        {server.visibility && (
          <p className="mt-1 text-xs text-muted-foreground" data-testid="share-current-visibility">
            Currently: <span className="font-medium">{server.visibility}</span>
          </p>
        )}

        {/* Mode picker */}
        <div className="mt-4 space-y-2">
          <label
            className="flex cursor-pointer items-start gap-3 rounded-md border p-3 hover:bg-accent/40"
            data-testid="share-mode-byo"
          >
            <input
              type="radio"
              name="share-mode"
              value="byo"
              checked={mode === "byo"}
              onChange={() => handleModeChange("byo")}
              className="mt-0.5 accent-primary"
              data-testid="share-mode-byo-radio"
            />
            <div>
              <p className="font-medium">Teammates connect their own accounts <span className="text-xs text-muted-foreground font-normal">(recommended)</span></p>
              <p className="text-xs text-muted-foreground">
                Publish the server definition — teammates discover it and connect their OWN accounts. Your credentials are never shared.
              </p>
            </div>
          </label>

          <label
            className="flex cursor-pointer items-start gap-3 rounded-md border p-3 hover:bg-accent/40"
            data-testid="share-mode-shared-cred"
          >
            <input
              type="radio"
              name="share-mode"
              value="shared-cred"
              checked={mode === "shared-cred"}
              onChange={() => handleModeChange("shared-cred")}
              className="mt-0.5 accent-primary"
              data-testid="share-mode-shared-cred-radio"
            />
            <div>
              <p className="font-medium">Share one credential everyone uses</p>
              <p className="text-xs text-muted-foreground">
                Set one shared org credential — every user&apos;s runs inject it automatically, no per-user connect.
              </p>
            </div>
          </label>
        </div>

        {/* BYO sub-form: visibility picker */}
        {mode === "byo" && (
          <div className="mt-4 space-y-2" data-testid="share-byo-section">
            <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Visibility</p>
            {(["team", "org", "public"] as PublishVisibility[]).map((v) => (
              <label
                key={v}
                className="flex cursor-pointer items-center gap-3 rounded-md border p-3 hover:bg-accent/40"
                data-testid={`publish-option-${v}`}
              >
                <input
                  type="radio"
                  name="visibility"
                  value={v}
                  checked={selected === v}
                  onChange={() => handleVisibilitySelect(v)}
                  className="accent-primary"
                />
                <div>
                  <p className="font-medium capitalize">{v}</p>
                  <p className="text-xs text-muted-foreground">
                    {v === "team"
                      ? "Visible to your team's namespace"
                      : v === "org"
                      ? "Visible org-wide (Tenant-admin required)"
                      : "Visible to everyone (Platform-admin required)"}
                  </p>
                </div>
              </label>
            ))}
            {selected === "public" && (
              <div className="mt-2 space-y-2">
                <p className="text-sm text-warning" data-testid="publish-public-warning">
                  Public means every tenant on this cluster can discover it.
                </p>
                <label className="flex cursor-pointer items-center gap-2 text-sm" data-testid="publish-public-confirm-label">
                  <input
                    type="checkbox"
                    checked={publicConfirmed}
                    onChange={(e) => setPublicConfirmed(e.target.checked)}
                    className="accent-primary"
                    data-testid="publish-public-confirm"
                  />
                  I understand this is discoverable by all tenants
                </label>
              </div>
            )}
            {server.credentialSource === "shared" && (
              <p className="mt-2 text-sm text-warning" data-testid="publish-shared-cred-warning">
                Caution: this server uses a shared credential — widening visibility also widens
                access to that credential.
              </p>
            )}
          </div>
        )}

        {/* Shared-cred sub-form: credential input */}
        {mode === "shared-cred" && (
          <div className="mt-4 space-y-3" data-testid="share-shared-cred-section">
            <p className="text-sm text-warning" data-testid="org-cred-caution">
              This shares ONE credential with everyone — every user&apos;s runs use it. If teammates
              should connect their own accounts instead, choose the option above.
            </p>
            <div className="space-y-1.5">
              <Label htmlFor="org-cred">Shared credential (bearer token)</Label>
              <Input
                id="org-cred"
                type="password"
                autoComplete="off"
                value={credential}
                onChange={(e) => setCredential(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    void onSubmit();
                  }
                }}
                placeholder="paste the org's token"
                data-testid="org-cred-input"
              />
            </div>
          </div>
        )}

        {/* T7: inline error — stays visible without closing the dialog */}
        {inlineError && (
          <p
            className="mt-3 text-sm text-destructive"
            role="alert"
            data-testid="share-inline-error"
          >
            {inlineError}
          </p>
        )}

        <div className="mt-6 flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={isSubmitDisabled}
            data-testid="share-submit"
          >
            {busy
              ? mode === "byo"
                ? "Sharing…"
                : "Setting…"
              : mode === "byo"
              ? "Share"
              : "Set org credential"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// DeleteMcpDialog (m26.4) — loads the delete-impact (dependent bindings) then shows a
// typed-name ConfirmDialog. On confirm → DELETE the server bundle → reload the list.
// The dialog's impact list scrolls (the m26.1 ConfirmDialog fix), so a server with many
// dependent bindings never hides the Delete button.
type RefsLoad =
  | { kind: "loading" }
  | { kind: "ready"; references: McpServerReference[] }
  | { kind: "error"; message: string };

function DeleteMcpDialog({
  server,
  onClose,
  onDeleted,
}: {
  server: McpServerSummary;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const { toast } = useToast();
  const [refs, setRefs] = React.useState<RefsLoad>({ kind: "loading" });
  const [deleting, setDeleting] = React.useState(false);

  React.useEffect(() => {
    const c = new AbortController();
    api
      .mcpServerReferences(server.namespace, server.name, c.signal)
      .then((res) => {
        if (c.signal.aborted) return;
        setRefs({ kind: "ready", references: res.references });
      })
      .catch((err: unknown) => {
        if (c.signal.aborted) return;
        setRefs({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load delete impact",
        });
      });
    return () => c.abort();
  }, [server.namespace, server.name]);

  async function onConfirm() {
    setDeleting(true);
    try {
      await api.deleteMcpServer(server.namespace, server.name);
      toast({
        variant: "success",
        title: "MCP server deleted",
        description: `${server.name} and its tools were removed.`,
      });
      onDeleted();
    } catch (err) {
      toast({
        variant: "error",
        title: "Delete failed",
        description: err instanceof Error ? err.message : "delete failed",
      });
      setDeleting(false);
      onClose();
    }
  }

  const impact =
    refs.kind === "loading" ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-loading">
        Loading delete impact…
      </p>
    ) : refs.kind === "error" ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-error">
        Couldn't load delete impact ({refs.message}) — proceeding will still delete the
        server.
      </p>
    ) : refs.references.length === 0 ? (
      <p className="text-sm text-muted-foreground" data-testid="mcp-refs-empty">
        No agents depend on this server's tools.
      </p>
    ) : (
      <div data-testid="mcp-refs-list">
        <p className="mb-2 text-xs font-medium text-muted-foreground">
          Deleting this server breaks {refs.references.length} bound{" "}
          {refs.references.length === 1 ? "tool" : "tools"} (they become
          RegistryNotFound):
        </p>
        <ul className="space-y-1.5">
          {refs.references.map((r) => (
            <li
              key={r.name}
              className="flex items-center justify-between gap-2 text-sm"
              data-testid={`mcp-ref-${r.name}`}
            >
              <span className="truncate font-mono text-xs">{r.name}</span>
              <Badge variant="secondary" className="text-[10px]">
                agent {r.agentRef}
              </Badge>
            </li>
          ))}
        </ul>
      </div>
    );

  return (
    <ConfirmDialog
      open={true}
      onCancel={onClose}
      onConfirm={onConfirm}
      title={`Delete ${server.name}?`}
      description={`This permanently deletes the MCP server "${server.name}" — its catalog, credential, and egress. Bound tools break until re-pointed.`}
      confirmText={server.name}
      confirmLabel="Delete server"
      busy={deleting}
      impact={impact}
    />
  );
}
