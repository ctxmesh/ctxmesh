import * as React from "react";
import { Check, ExternalLink, KeyRound, ShieldAlert, Wrench } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Select } from "@/components/ui/select";
import {
  ForbiddenInline,
  Wizard,
  useToast,
  type WizardStep,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_REGISTRIES } from "@/lib/nav";
import { api, ApiError, type AddMcpResponse, type DiscoveredTool } from "@/lib/api";

// AddMcpPage — the BYO-MCP wizard (spec §5, ADR 0016). A guided flow to add
// your own MCP server so its tools land in the catalog:
//
//   1. Server  — a name + a remote URL (or an image) + auth mode (key or OAuth).
//                The step's forward action SUBMITS: the BFF probes the server
//                (key-auth) or starts the OAuth 2.1 flow (OAuth).
//                Key-auth: runs tools/list discovery, stores the key as a Secret.
//                OAuth: returns 202 + { authorizationURL, state } — the SPA
//                REDIRECTS the browser to that URL (window.location.href). The
//                OAuth provider's consent page runs, the BFF handles the callback
//                server-side, and the user lands back with the MCP registered.
//                The SPA NEVER sees, stores, or displays an OAuth token.
//   2. Review  — the discovered tools (names + descriptions) + that they're now
//                in the merged catalog. On a hardened install they render as
//                pending-approval (an operator approves before binding).
//                For an OAuth add, the SPA redirects before this step renders —
//                the user lands back in the console via the callback.
//
// THE BEARER KEY NEVER LIVES CLIENT-SIDE AFTER SUBMIT — same discipline as the
// provider connect: held only in the field until submit, snapshotted into the
// POST body, cleared on dispatch; never a store / localStorage / sessionStorage
// / URL. After add, the UI only shows the discovered tools + their approval
// state — the key is server-side.
//
// OAUTH TOKENS NEVER REACH THE SPA — only the authorization URL + the opaque
// state handle. The full token exchange (auth-code → access-token → refresh)
// happens server-side in the BFF callback (GET /api/mcp/oauth/callback).
//
// Honest failures (never swallowed):
//   • probe failure (422/502) → a TEACHING error on the server step + retry.
//   • viewer (403)            → ForbiddenInline (the API is the real gate).
//   • kill-switch (404)       → the "BYO-MCP is disabled" fallback state.
//   • OAuth init failure      → same error surface as probe failure.

type SourceKind = "url" | "image";
type AuthMode = "key" | "oauth";

type Submit =
  | { kind: "idle" }
  | { kind: "adding" }
  | { kind: "added"; res: AddMcpResponse }
  | { kind: "oauth-redirecting"; authorizationURL: string }
  | { kind: "error"; message: string; status?: number }
  | { kind: "forbidden"; message: string }
  | { kind: "killed" };

const STEP_SERVER = 0;
const STEP_REVIEW = 1;

export function AddMcpPage() {
  const [current, setCurrent] = React.useState(STEP_SERVER);
  const [name, setName] = React.useState("");
  const [sourceKind, setSourceKind] = React.useState<SourceKind>("url");
  const [authMode, setAuthMode] = React.useState<AuthMode>("key");
  const [url, setUrl] = React.useState("");
  const [image, setImage] = React.useState("");
  // ── The bearer key lives ONLY here — a local field value, cleared on submit.
  const [apiKey, setApiKey] = React.useState("");
  const [submit, setSubmit] = React.useState<Submit>({ kind: "idle" });

  const { toast } = useToast();
  // RBAC-aware chrome (§3): registering an MCP server creates a ToolRegistry
  // entry (+ a Secret when a key is given) — a write. A viewer sees the gated
  // entry in the shell; if they reach this page directly the API 403 is the
  // real gate. DISPLAY-ONLY (ADR 0011).
  const { can, reprobe } = useCapabilities();
  const canAdd = can(RES_REGISTRIES, "create");

  async function onAdd() {
    setSubmit({ kind: "adding" });

    if (authMode === "oauth") {
      // OAuth 2.1 flow: POST to /api/mcpservers with authType "oauth". The BFF
      // returns 202 + { authorizationURL, state }. The SPA redirects the browser
      // to the authorization URL — the ENTIRE token exchange is server-side.
      // We NEVER receive, store, or display an OAuth token.
      const req = {
        name: name.trim(),
        ...(sourceKind === "url" ? { url: url.trim() } : { image: image.trim() }),
        authType: "oauth" as const,
      };
      try {
        const oauthRes = await api.addMcpServerOAuth(req);
        // Transition to the "redirecting" state so the UI renders a brief
        // "redirecting to consent…" banner before the navigation fires.
        setSubmit({ kind: "oauth-redirecting", authorizationURL: oauthRes.authorizationURL });
        // Full-page redirect to the OAuth provider's authorization endpoint.
        // The consent happens there; the BFF callback handles the code exchange.
        // We use window.location.href because this is a correct OAuth redirect —
        // NOT a client-side navigation (the consent page is a different origin).
        window.location.href = oauthRes.authorizationURL;
      } catch (err) {
        if (err instanceof ApiError) {
          if (err.isNotFound) { setSubmit({ kind: "killed" }); return; }
          if (err.isForbidden) {
            reprobe();
            setSubmit({ kind: "forbidden", message: err.message });
            return;
          }
          setSubmit({ kind: "error", message: err.message, status: err.status });
          return;
        }
        setSubmit({
          kind: "error",
          message: err instanceof Error ? err.message : "OAuth initiation failed",
        });
      }
      return;
    }

    // Key-auth (or no-auth) flow: probe immediately, discover tools.
    // Snapshot the key into the request body, then WIPE it from state — it never
    // survives the submit in any client-side store.
    const req = {
      name: name.trim(),
      ...(sourceKind === "url" ? { url: url.trim() } : { image: image.trim() }),
      ...(apiKey ? { apiKey } : {}),
    };
    setApiKey("");
    try {
      const res = await api.addMcpServer(req);
      setSubmit({ kind: "added", res });
      setCurrent(STEP_REVIEW);
      toast({
        variant: "success",
        title: `Added ${res.name}`,
        description: `${res.tools.length} tool${res.tools.length === 1 ? "" : "s"} discovered`,
      });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.isNotFound) {
          setSubmit({ kind: "killed" });
          return;
        }
        if (err.isForbidden) {
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

  // Leaving the SERVER step (0 → 1) SUBMITS; the review step is entered only on a
  // successful add (onAdd sets current). Backward moves pass through.
  function onStepChange(next: number) {
    if (current === STEP_SERVER && next > STEP_SERVER) {
      void onAdd();
      return;
    }
    setCurrent(next);
  }

  const adding = submit.kind === "adding";
  const source = sourceKind === "url" ? url : image;
  const serverReady = name.trim().length > 0 && source.trim().length > 0;

  if (submit.kind === "killed") return <KillSwitchFallback />;

  if (submit.kind === "forbidden") {
    return (
      <PageFrame>
        <ForbiddenInline
          title="Not allowed to add an MCP server"
          description="Adding an MCP server creates a ToolRegistry entry (and a Secret for its key) — your account can't create those in this cluster. Ask an operator to add it for you."
          detail={submit.message}
        />
      </PageFrame>
    );
  }

  // OAuth redirecting: the SPA has the authorization URL and is about to redirect.
  // Render a brief "redirecting to consent…" banner while window.location is set.
  // The SPA NEVER holds a token here — only the URL we received and are navigating to.
  if (submit.kind === "oauth-redirecting") {
    return (
      <PageFrame>
        <div
          className="rounded-lg border bg-card p-6 shadow-card"
          data-testid="oauth-redirecting"
        >
          <div className="flex items-center gap-3">
            <ExternalLink className="h-5 w-5 text-primary" />
            <div>
              <p className="font-medium">Redirecting to consent page…</p>
              <p className="text-sm text-muted-foreground">
                You&apos;re being redirected to the OAuth provider to authorize
                this MCP server. After you consent, you&apos;ll return here with
                the server registered.
              </p>
            </div>
          </div>
          <div className="mt-4">
            <Button
              variant="outline"
              size="sm"
              onClick={() => { window.location.href = submit.authorizationURL; }}
              data-testid="oauth-redirect-button"
            >
              <ExternalLink className="mr-2 h-3.5 w-3.5" />
              Open consent page
            </Button>
          </div>
        </div>
      </PageFrame>
    );
  }

  const steps: WizardStep[] = [
    {
      id: "server",
      title: "Server",
      description: "URL or image + auth",
      content: (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="mcp-name">Name</Label>
            <Input
              id="mcp-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="acme-support"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="mcp-source-kind">Source</Label>
            <Select
              id="mcp-source-kind"
              value={sourceKind}
              onChange={(e) => setSourceKind(e.target.value as SourceKind)}
            >
              <option value="url">Remote URL</option>
              <option value="image">Sidecar image</option>
            </Select>
          </div>
          {sourceKind === "url" ? (
            <div className="space-y-1.5">
              <Label htmlFor="mcp-url">MCP server URL</Label>
              <Input
                id="mcp-url"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://mcp.acme.dev/sse"
                className="font-mono text-xs"
              />
            </div>
          ) : (
            <div className="space-y-1.5">
              <Label htmlFor="mcp-image">Image</Label>
              <Input
                id="mcp-image"
                value={image}
                onChange={(e) => setImage(e.target.value)}
                placeholder="ghcr.io/acme/mcp-support:v1"
                className="font-mono text-xs"
              />
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="mcp-auth-mode">Authentication</Label>
            <Select
              id="mcp-auth-mode"
              value={authMode}
              onChange={(e) => setAuthMode(e.target.value as AuthMode)}
              data-testid="mcp-auth-mode"
            >
              <option value="key">Bearer key</option>
              <option value="oauth">OAuth 2.1 (redirect to consent)</option>
            </Select>
          </div>
          {authMode === "key" && (
            <div className="space-y-1.5">
              <Label htmlFor="mcp-key">Bearer key (optional)</Label>
              <div className="relative">
                <KeyRound className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  id="mcp-key"
                  type="password"
                  autoComplete="off"
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  placeholder="If the server requires auth…"
                  className="pl-9 font-mono text-xs"
                />
              </div>
            </div>
          )}
          {authMode === "oauth" ? (
            <div className="rounded-md border border-info/30 bg-info/5 p-3 text-xs text-muted-foreground">
              OAuth 2.1 flow: clicking{" "}
              <span className="font-medium">Connect via OAuth</span> sends the
              server details to the BFF, which returns an authorization URL. Your
              browser will redirect to the provider&apos;s consent page — the token
              exchange happens entirely server-side. No tokens are stored in the
              browser.
            </div>
          ) : (
            <div className="rounded-md border border-info/30 bg-info/5 p-3 text-xs text-muted-foreground">
              The BFF probes the server and runs <span className="font-medium">tools/list</span>{" "}
              discovery. Any key is stored as a Secret and attached at the egress
              hop — never by the browser, never inside the agent container. Egress
              opens per approved server only.
            </div>
          )}
          {submit.kind === "error" && (
            <p
              className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning-foreground"
              role="alert"
              data-testid="probe-error"
            >
              Couldn&apos;t reach that server: {submit.message}
              {submit.status ? ` (${submit.status})` : ""}. Check the URL is a
              reachable MCP endpoint (and the bearer key if it needs auth), then
              try again.
            </p>
          )}
        </div>
      ),
    },
    {
      id: "tools",
      title: "Tools",
      review: true,
      content:
        submit.kind === "added" ? (
          <ReviewStep res={submit.res} />
        ) : (
          <p className="text-sm text-muted-foreground">
            Submit the server to probe it and discover its tools.
          </p>
        ),
    },
  ];

  const canProceed =
    current === STEP_SERVER ? serverReady && canAdd : false;

  return (
    <PageFrame>
      {!canAdd && (
        <p
          className="rounded-md border border-dashed bg-card/40 px-3 py-2 text-center text-xs text-muted-foreground"
          data-testid="add-mcp-readonly-note"
        >
          You have read-only access — adding an MCP server requires create
          permission on AgentRegistries. Ask an operator to add one for you.
        </p>
      )}
      <div className="rounded-lg border bg-card p-6 shadow-card">
        <Wizard
          steps={steps}
          current={current}
          onStepChange={onStepChange}
          canProceed={canProceed}
          busy={adding}
          nextLabel={
            current === STEP_SERVER
              ? authMode === "oauth"
                ? "Connect via OAuth"
                : "Probe + discover"
              : "Continue"
          }
        />
      </div>
    </PageFrame>
  );
}

function PageFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">
          Add an MCP server
        </h2>
        <p className="text-sm text-muted-foreground">
          Bring your own MCP server — URL (or image) → probe → discovered tools
          land in the catalog for your agents to use.
        </p>
      </div>
      {children}
    </div>
  );
}

// ReviewStep renders the discovered tools + their approval state. On a hardened
// install (approvalStatus === "pending") the tools queue for operator approval
// before any agent can bind them; otherwise they're immediately in the catalog.
function ReviewStep({ res }: { res: AddMcpResponse }) {
  const pending = res.approvalStatus === "pending";
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-success">
        <Check className="h-5 w-5" />
        <p className="text-sm font-medium text-foreground">
          {res.name} added — discovered {res.tools.length} tool
          {res.tools.length === 1 ? "" : "s"}
        </p>
      </div>
      <div className="space-y-2" data-testid="tool-list">
        {res.tools.map((t: DiscoveredTool) => {
          const toolPending = (t.approvalStatus ?? res.approvalStatus) === "pending";
          return (
            <div
              key={t.name}
              className="flex items-center gap-3 rounded-md border bg-surface-2/40 px-3 py-2"
            >
              <Wrench className="h-4 w-4 text-primary" />
              <div className="min-w-0 flex-1">
                <p className="font-mono text-sm">{t.name}</p>
                {t.description && (
                  <p className="text-xs text-muted-foreground">
                    {t.description}
                  </p>
                )}
              </div>
              {toolPending && (
                <Badge variant="warning" className="text-[10px]">
                  pending approval
                </Badge>
              )}
            </div>
          );
        })}
      </div>
      {pending ? (
        <div
          className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-xs text-muted-foreground"
          data-testid="pending-approval-note"
        >
          This is a hardened install — the discovered tools are queued for
          operator approval into the ToolRegistry before any agent can bind them.
        </div>
      ) : (
        <div className="rounded-md border bg-surface-2/40 px-3 py-2 text-xs text-muted-foreground">
          These tools are now in the merged catalog and can be bound to an agent.
          Egress opens to this server only for the agents that use them.
        </div>
      )}
    </div>
  );
}

function KillSwitchFallback() {
  return (
    <PageFrame>
      <Card data-testid="kill-switch-fallback">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldAlert className="h-4 w-4 text-warning" />
            Adding MCP servers is disabled on this cluster
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm text-muted-foreground">
          <p>
            This is a hardened install — self-serve MCP registration is turned
            off. Tools are curated into the{" "}
            <span className="font-mono text-foreground">ToolRegistry</span> by an
            operator.
          </p>
          <p>
            Ask an operator to add the MCP server you need; its tools will then
            appear in the catalog for your agents.
          </p>
        </CardContent>
      </Card>
    </PageFrame>
  );
}
