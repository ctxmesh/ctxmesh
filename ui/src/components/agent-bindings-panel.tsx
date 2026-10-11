import { Link } from "react-router-dom";

import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { NextStepLink, QuietNote, humanizeStatusReason } from "@/components/kit";
import type {
  AgentDependency,
  AgentDependencyKind,
  AgentDetailResponse,
} from "@/lib/api";

// AgentBindingsPanel — everything one agent references, each resolved (m184.15).
//
// The rail names a few references and the reach panel covers the tools, but an
// agent depends on more than either shows: the secret binding behind its model
// route, its knowledge bases, its memory, who may approve its paused tool calls
// and which feedback it accepts (approval rules and feedback sources are agent
// spec fields since ADR 0152, so this panel is where a person now sees them).
//
// The BFF resolves each reference as the caller (ADR 0011). The panel renders
// what it said and nothing more:
//   ready     ok        found, and its own status says it works
//   notReady  crit, or progressing while the machine is still converging
//   missing   crit      nothing of that name exists; never hidden
//   unknown   open      the caller cannot read it — not a claim either way
//   declared  muted     a spec field with nothing behind it to resolve
//
// data-testid contract:
//   agent-bindings-panel · bindings-attention · bindings-group-<group>
//   dep-<kind>-<name> · dep-state-<kind>-<name> · dep-fix-<kind>-<name>
//   approvals-anyone-note · approvals-unused-note

type Group = {
  id: string;
  title: string;
  kinds: AgentDependencyKind[];
  empty: string;
};

const GROUPS: Group[] = [
  {
    id: "model",
    title: "Model",
    kinds: ["modelRoute", "secretBinding"],
    empty: "No model route, so this agent has no model to call.",
  },
  { id: "guardrails", title: "Guardrails", kinds: ["guardrailPolicy"], empty: "No guardrail policy." },
  { id: "tools", title: "Tools", kinds: ["toolBinding"], empty: "No tool is bound to this agent." },
  { id: "knowledge", title: "Knowledge", kinds: ["knowledgeBase"], empty: "No knowledge base." },
  {
    id: "memory",
    title: "Memory",
    kinds: ["sessionMemory", "longTermMemory"],
    empty: "No memory. Each message is answered on its own.",
  },
  {
    id: "approvals",
    title: "Approvals",
    kinds: ["approvalRule", "approver"],
    empty: "No tool call waits for a person's approval.",
  },
  { id: "feedback", title: "Feedback", kinds: ["feedbackSource"], empty: "No feedback source is declared." },
];

const KIND_LABEL: Record<AgentDependencyKind, string> = {
  modelRoute: "Model route",
  secretBinding: "Secret binding",
  guardrailPolicy: "Guardrail policy",
  toolBinding: "Tool binding",
  knowledgeBase: "Knowledge base",
  sessionMemory: "Session memory",
  longTermMemory: "Long-term memory",
  approvalRule: "Needs approval",
  approver: "Approver",
  feedbackSource: "Feedback source",
};

/** Not-ready reasons that mean the machine is still converging on its own. */
const CONVERGING = new Set([
  "NotYetServed",
  "GatewayRolling",
  "Ingesting",
  "Pending",
  "NotReconciled",
  "NotValidated",
]);

type Tag = { variant: NonNullable<BadgeProps["variant"]>; label: string };

function stateTag(d: AgentDependency): Tag {
  switch (d.state) {
    case "ready":
      return { variant: "ok", label: "ready" };
    case "missing":
      return { variant: "crit", label: "missing" };
    case "unknown":
      return { variant: "open", label: "unknown" };
    case "declared":
      return { variant: "muted", label: "declared" };
    default:
      if (d.reason === "NotYetServed") return { variant: "progressing", label: "model loading" };
      if (d.reason === "GatewayRolling") return { variant: "progressing", label: "rolling out" };
      if (d.reason && CONVERGING.has(d.reason)) return { variant: "progressing", label: "not ready yet" };
      return { variant: "crit", label: "not ready" };
  }
}

/** A row that will not work without a change. */
function blocking(d: AgentDependency): boolean {
  return d.state === "missing" || (d.state === "notReady" && stateTag(d).variant === "crit");
}

const linkClass = "border-b border-accent font-mono text-primary hover:border-primary";

/** The page a reference opens, when it has one and exists. */
function pageFor(d: AgentDependency): string | null {
  const ns = encodeURIComponent(d.namespace ?? "");
  const nm = encodeURIComponent(d.name);
  if (d.state === "missing" || !d.name) return null;
  switch (d.kind) {
    case "modelRoute":
      return `/routes/${ns}/${nm}`;
    case "secretBinding":
      return `/secrets/${ns}/${nm}`;
    case "guardrailPolicy":
      return "/guardrails";
    case "knowledgeBase":
      return `/knowledgebases/${ns}/${nm}`;
    default:
      return null;
  }
}

/** Where to go to make a missing reference exist. */
function fixFor(d: AgentDependency): { label: string; to: string } | null {
  if (d.state !== "missing") return null;
  switch (d.kind) {
    case "modelRoute":
      return { label: "Create the route", to: "/routes/new" };
    case "secretBinding":
      return { label: "Create the binding", to: "/secrets/new" };
    case "guardrailPolicy":
      return { label: "Open guardrails", to: "/guardrails" };
    case "knowledgeBase":
      return { label: "Open knowledge", to: "/knowledgebases" };
    default:
      return null;
  }
}

function subline(d: AgentDependency): string {
  const parts: string[] = [];
  if (d.detail) parts.push(d.detail);
  if (d.via) parts.push(d.kind === "secretBinding" ? `used by route ${d.via}` : `via ${d.via}`);
  if (d.kind === "approver" && !d.detail) parts.push("approver");
  return parts.join(" · ");
}

function DependencyRow({ d }: { d: AgentDependency }) {
  const id = `${d.kind}-${d.name || "unlisted"}`;
  const tag = stateTag(d);
  const page = pageFor(d);
  const fix = fixFor(d);
  const sub = subline(d);
  // The condition's own words matter whenever the row is not simply fine. A ready row's message
  // ("the gateway serves this route") restates its tag, so it shows only when it is a caveat.
  const explain =
    d.state === "notReady" ||
    d.state === "missing" ||
    d.state === "unknown" ||
    (!!d.message && (d.state === "declared" || d.reason === "PartiallyIngested"));
  return (
    <li
      className="flex min-w-0 flex-col gap-1 border-b border-border-soft py-2.5 last:border-b-0"
      data-testid={`dep-${id}`}
    >
      <div className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="font-mono text-2xs uppercase tracking-wide text-faint">{KIND_LABEL[d.kind]}</p>
          <p className="truncate text-sm" title={d.name}>
            {page ? (
              <Link to={page} className={linkClass}>
                {d.name}
              </Link>
            ) : (
              <span className="font-mono">{d.name || "—"}</span>
            )}
          </p>
          {sub && <p className="truncate text-xs text-faint">{sub}</p>}
        </div>
        <Badge
          variant={tag.variant}
          className="shrink-0"
          data-testid={`dep-state-${id}`}
          title={d.reason ? humanizeStatusReason(d.reason) : undefined}
        >
          {tag.label}
        </Badge>
      </div>
      {explain && (d.message || d.reason) && (
        <p className="break-words text-xs text-secondary-foreground">
          {d.reason && d.state !== "declared" && (
            <span className="font-mono text-faint">{d.reason}: </span>
          )}
          {d.message}
        </p>
      )}
      {fix && (
        <span>
          <NextStepLink label={fix.label} to={fix.to} tone="crit" testId={`dep-fix-${id}`} />
        </span>
      )}
    </li>
  );
}

export function AgentBindingsPanel({ detail }: { detail: AgentDetailResponse }) {
  const deps = detail.dependencies;
  const attention = deps ? deps.filter(blocking).length : 0;

  return (
    <Card className="min-w-0" data-testid="agent-bindings-panel">
      <PanelHeader
        title="Bindings"
        meta={deps && deps.length > 0 ? `${deps.length} reference${deps.length === 1 ? "" : "s"}` : undefined}
      />
      <CardContent>
        <p className="mb-4 max-w-[66ch] text-sm text-secondary-foreground">
          Every policy, store and credential this agent refers to, each looked up
          as you. A reference that does not resolve is listed as missing.
        </p>

        {!deps ? (
          <QuietNote title="References aren't resolved on this server.">
            The console's server did not send the agent's references, so nothing
            is listed here rather than a guess.
          </QuietNote>
        ) : (
          <>
            {attention > 0 && (
              <div
                className="mb-4 border border-border border-l-2 border-l-destructive bg-surface-2 px-4 py-3 text-sm text-secondary-foreground"
                data-testid="bindings-attention"
              >
                <span className="font-mono font-semibold tabular-nums text-destructive">{attention}</span>{" "}
                {attention === 1 ? "reference stops" : "references stop"} this agent working as
                declared. Each says why below.
              </div>
            )}
            <div className="space-y-5">
              {GROUPS.map((g) => {
                const rows = deps.filter((d) => g.kinds.includes(d.kind));
                return (
                  <section key={g.id} data-testid={`bindings-group-${g.id}`} aria-label={g.title}>
                    <h3 className="font-serif text-md font-medium">{g.title}</h3>
                    {rows.length === 0 ? (
                      <p className="mt-1 text-sm text-faint">{g.empty}</p>
                    ) : (
                      <ul className="mt-1">
                        {rows.map((d) => (
                          <DependencyRow key={`${d.kind}/${d.namespace ?? ""}/${d.name}`} d={d} />
                        ))}
                      </ul>
                    )}
                    {g.id === "approvals" && <ApprovalNotes rows={rows} />}
                  </section>
                );
              })}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

/** What the approvals rows mean together, which neither row says alone. */
function ApprovalNotes({ rows }: { rows: AgentDependency[] }) {
  const rules = rows.some((r) => r.kind === "approvalRule");
  const approvers = rows.some((r) => r.kind === "approver");
  if (rules && !approvers) {
    return (
      <p className="mt-2 text-xs text-faint" data-testid="approvals-anyone-note">
        No approvers are listed, so anyone who can act on the run and read this agent may approve.
      </p>
    );
  }
  if (approvers && !rules) {
    return (
      <p className="mt-2 text-xs text-faint" data-testid="approvals-unused-note">
        No tool requires approval, so these approvers are never asked.
      </p>
    );
  }
  return null;
}
