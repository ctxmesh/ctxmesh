import * as React from "react";
import { OctagonX } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { FieldError, Label } from "@/components/ui/label";
import { ConfirmDialog } from "@/components/kit/confirm-dialog";
import { KeyValueList, type KeyValueItem } from "@/components/kit/kv-list";
import { UnknownValue } from "@/components/kit/quiet-note";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";

// StopControl + StopNotice — the scoped kill switch and its banner
// (M151 §5.23; semantics from ADR 0126 "The scoped kill switch").
//
// StopControl sits in the frame's top bar on EVERY page and never collapses at
// any breakpoint. StopNotice is pinned under the PageHeader of every page whose
// scope intersects an active stop. Between them they are the most consequential
// pair of components in the console, and the copy is the feature.
//
// ── Why the words matter more than the pixels ─────────────────────────────────
//
// A kill switch nobody dares press is not a safety control. The reason people
// hesitate is always the same: they do not know what it destroys. So both
// components state the contract in plain words, every time, with counts:
//
//     new runs are REFUSED
//     queued work is HELD — it keeps its place and runs when the stop is lifted
//     work in flight stops at its NEXT MODEL OR TOOL CALL
//     nothing is discarded
//
// Every clause of that is what ADR 0126 actually built, not what would sound
// reassuring. Layer (c) refuses creation and layer (b) stops the queue drain —
// both fail-CLOSED against the control plane, so a stop survives a state-layer
// outage. Layer (a), the marker the agent reads back through the proxy, is the
// accelerator that interrupts work already running, and it lands at model-call
// and tool-call boundaries.
//
// That last fact produces the one limit we state rather than imply: an agent
// spinning in pure local computation is NOT stopped until it next calls out.
// ADR 0126 deliberately refuses to let the phrase "kill switch" claim otherwise,
// and so does this component. Pod-kill escalation is a different mechanism with
// a different blast radius, and it is not this button.
//
// Un-kill is symmetric: lifting is a distinct, authorized, audited act (never a
// silent expiry), and it releases the held backlog the instant it happens — so
// it gets its own confirmation stating what is about to start.
//
// ── Hue discipline ───────────────────────────────────────────────────────────
// Crit is the one hue that may fill a control, and only for destructive acts
// (§2.3). The notice's label block is one of exactly two full-bleed semantic
// surfaces allowed console-wide (§2.2). Nothing here is warn (no bound was
// crossed) and nothing here is hold (no one is being asked to decide).

/** The scope hierarchy of ADR 0126, narrowed to what a person picks in the UI. */
export type StopScopeKind = "agent" | "team" | "workspace" | "fleet";

/**
 * What a stop at some scope actually holds. Every field is a COUNT FROM THE
 * BACKEND. Omit any the backend did not answer — an omitted field renders as an
 * explicit absence, never as 0. Claiming "0 runs held" when we were not told is
 * the difference between a control an operator trusts and one they double-check
 * in kubectl.
 */
export interface StopImpact {
  /** Agents that will refuse / are refusing new runs. */
  agents?: number;
  /** Queued runs that will be / are held — kept, never discarded. */
  queued?: number;
  /** Runs in flight that stop at their next model or tool call. */
  running?: number;
}

export interface StopScopeOption {
  kind: StopScopeKind;
  /**
   * The exact thing being stopped, qualified: "ns/agent-a", "ns/team-b", the
   * namespace, or the cluster name. Shown in mono so it is unmistakable.
   */
  name?: string;
  /** Backend counts for THIS scope — the blast radius the dialog states. */
  impact?: StopImpact;
  /** The caller lacks the stop verb for this scope (ADR 0126 §5). */
  disabled?: boolean;
  /** Why it is disabled, e.g. "Stopping the fleet needs the platform-admin role." */
  disabledReason?: string;
}

export interface StopRequest {
  scope: StopScopeKind;
  name?: string;
  /** The operator's reason — becomes the audit line and the banner quote. */
  reason: string;
}

// ── The copy. Exported so the pages, the tests and the acceptance playbook all
// assert the SAME sentences, and a reassuring rewrite cannot land unnoticed. ──

/** §5.23 verbatim — the contract, stated on the confirmation. */
export const STOP_CONTRACT =
  "Stopping refuses new runs and holds queued ones. Running work stops at its next model or tool call. Nothing is discarded.";

/** What "held" means, said out loud so nobody reads it as "lost". */
export const STOP_HELD_EXPLAINER =
  "Held runs keep their place in the queue and start when the stop is lifted. Nothing is cancelled, discarded, or restarted.";

/** The honest limit (ADR 0126 consequences) — stated, never implied. */
export const STOP_LIMIT =
  "A run doing pure local work is not interrupted until it next calls a model or a tool. Stopping does not delete, scale down, or kill anything.";

export const STOP_REASON_LABEL = "Why are you stopping this?";

export const STOP_REASON_HINT =
  "Required. This becomes the audit record, and everyone whose work is stopped reads it on the banner.";

export const STOP_REASON_REQUIRED =
  "A reason is required — it is what tells the next person why their work stopped.";

/** The banner's contract line — the same promise, in the past tense. */
export const STOP_NOTICE_CONTRACT =
  "New runs are refused and queued work is held — nothing is discarded. Work already running stops at its next model or tool call.";

/** Lifting is its own audited act, and it releases the backlog immediately. */
export const LIFT_CONTRACT =
  "Lifting lets new runs start again and releases everything that is held. It is recorded against you, exactly as the stop was.";

/** The reason is the audit line; long enough to be useful, short enough to read. */
export const STOP_REASON_MAX = 120;

const SCOPE_NOUN: Record<StopScopeKind, string> = {
  agent: "This agent",
  team: "This team",
  workspace: "Workspace",
  fleet: "Everything",
};

/** What the scope covers, in words — used where the name alone is ambiguous. */
const SCOPE_REACH: Record<StopScopeKind, string> = {
  agent: "every run of this one agent",
  team: "every agent on this team",
  workspace: "every agent in this workspace",
  fleet: "every agent in every workspace, cluster-wide",
};

const CONFIRM_LABEL: Record<StopScopeKind, string> = {
  agent: "Stop this agent",
  team: "Stop this team",
  workspace: "Stop this workspace",
  fleet: "Stop everything",
};

function plural(n: number, one: string, many = `${one}s`): string {
  return n === 1 ? one : many;
}

/** A count the backend did not send is stated as absent — never rendered as 0. */
const NOT_REPORTED_TITLE =
  "The backend did not report this count. It is unknown — not zero.";

function countRow(key: string, n: number | undefined): KeyValueItem {
  if (typeof n === "number") {
    return {
      key,
      value: <span className="font-semibold tabular-nums">{n}</span>,
    };
  }
  return { key, absent: "not reported", title: NOT_REPORTED_TITLE };
}

/**
 * The blast radius as a kv register: counts, each either a real number or a
 * stated absence. Adjectives ("a lot of work", "several agents") are banned
 * here — a person authorizing a stop is entitled to the numbers.
 *
 * `lift` states the mirror image, because lifting releases the backlog at once
 * and that is its own consequential act. A run that was in flight when the stop
 * landed has already halted, so it has no row there.
 */
function impactItems(
  impact: StopImpact | undefined,
  mode: "stop" | "lift" = "stop",
): KeyValueItem[] {
  if (mode === "lift") {
    return [
      countRow("Held runs that will start", impact?.queued),
      countRow("Agents accepting runs again", impact?.agents),
    ];
  }
  return [
    countRow("Agents refusing new runs", impact?.agents),
    countRow("Queued runs held", impact?.queued),
    countRow("Runs in flight", impact?.running),
  ];
}

// ─────────────────────────────────────────────────────────────────────────────
// StopControl — the frame kill switch
// ─────────────────────────────────────────────────────────────────────────────

export interface StopControlProps {
  /**
   * The scopes this caller may stop, most specific first. The console builds
   * this from page context: an agent detail page offers the agent, its team,
   * its workspace and the fleet; an index page offers the workspace and the
   * fleet.
   */
  scopes: StopScopeOption[];
  /** Which scope is pre-selected. Defaults to the first entry (most specific). */
  defaultScopeKind?: StopScopeKind;
  /**
   * The cluster's name — the typed-name gate for a fleet-wide stop. If a fleet
   * scope is offered without one, the gate falls back to the word "everything":
   * a cluster-wide stop is never one click.
   */
  clusterName?: string;
  onStop: (req: StopRequest) => void | Promise<void>;
  /** The caller holds no stop verb anywhere — the control renders disabled. */
  disabled?: boolean;
  /** Why it is disabled; shown as the button's title. */
  disabledReason?: string;
  className?: string;
}

export function StopControl({
  scopes,
  defaultScopeKind,
  clusterName,
  onStop,
  disabled = false,
  disabledReason,
  className,
}: StopControlProps) {
  const [open, setOpen] = React.useState(false);
  const [scopeKind, setScopeKind] = React.useState<StopScopeKind | null>(null);
  const [reason, setReason] = React.useState("");
  const [reasonError, setReasonError] = React.useState<string | null>(null);
  const [submitError, setSubmitError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);
  const reasonId = React.useId();
  const groupName = React.useId();
  const reasonRef = React.useRef<HTMLInputElement>(null);

  const selectable = scopes.filter((s) => !s.disabled);
  const initialKind =
    (defaultScopeKind &&
      selectable.some((s) => s.kind === defaultScopeKind) &&
      defaultScopeKind) ||
    selectable[0]?.kind ||
    null;
  const selected =
    scopes.find((s) => s.kind === (scopeKind ?? initialKind)) ?? null;

  function reset() {
    setScopeKind(null);
    setReason("");
    setReasonError(null);
    setSubmitError(null);
  }

  function close() {
    setOpen(false);
    reset();
  }

  async function submit() {
    if (!selected) return;
    const trimmed = reason.trim();
    // The reason is not decoration: it is the audit line and the sentence the
    // people whose work just stopped will read. ConfirmDialog cannot express a
    // second gate, so it is enforced here — on submit, with focus moved to the
    // field and an announced error, rather than by a silently inert button.
    if (!trimmed) {
      setReasonError(STOP_REASON_REQUIRED);
      reasonRef.current?.focus();
      return;
    }
    setReasonError(null);
    setSubmitError(null);
    setSubmitting(true);
    try {
      await onStop({
        scope: selected.kind,
        name: selected.name,
        reason: trimmed,
      });
      close();
    } catch (err) {
      // Never swallow it: a stop that did not take effect must say so, loudly,
      // in the dialog the operator is still looking at.
      setSubmitError(
        err instanceof Error
          ? `The stop was not applied: ${err.message}`
          : "The stop was not applied. Nothing has been stopped.",
      );
    } finally {
      setSubmitting(false);
    }
  }

  const nothingToStop = selectable.length === 0;
  const buttonDisabled = disabled || nothingToStop;
  const gate =
    selected?.kind === "fleet" ? (clusterName || "everything") : undefined;

  return (
    <>
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={buttonDisabled}
        title={
          buttonDisabled
            ? (disabledReason ?? "There is nothing here that you can stop.")
            : "Stop new and queued work at a scope you choose"
        }
        onClick={() => setOpen(true)}
        className={cn(
          // Outlined crit at rest, filled crit on hover: the shape says
          // "destructive control", the hue says "it will not proceed" (§2.3).
          "h-8 border-destructive text-destructive",
          "hover:bg-destructive hover:text-destructive-foreground",
          className,
        )}
      >
        <OctagonX aria-hidden="true" />
        Stop
      </Button>

      <ConfirmDialog
        open={open}
        onCancel={close}
        onConfirm={submit}
        busy={submitting}
        title="Stop work"
        description={STOP_CONTRACT}
        confirmText={gate}
        confirmLabel={selected ? CONFIRM_LABEL[selected.kind] : "Stop"}
        impact={
          <div className="space-y-4">
            <fieldset>
              <legend className="font-mono text-2xs uppercase tracking-wide text-faint">
                What to stop
              </legend>
              <div className="mt-2 space-y-1">
                {scopes.map((scope) => {
                  const isSelected = selected?.kind === scope.kind;
                  return (
                    <label
                      key={scope.kind}
                      title={scope.disabled ? scope.disabledReason : undefined}
                      className={cn(
                        "flex cursor-pointer items-baseline gap-2 rounded-md border px-2.5 py-2",
                        isSelected
                          ? "border-destructive bg-destructive-surface"
                          : "border-border bg-card",
                        scope.disabled &&
                          "cursor-not-allowed border-border text-ghost",
                      )}
                    >
                      <input
                        type="radio"
                        name={groupName}
                        className="accent-primary"
                        value={scope.kind}
                        checked={isSelected}
                        disabled={scope.disabled}
                        onChange={() => setScopeKind(scope.kind)}
                      />
                      <span className="text-sm font-medium">
                        {SCOPE_NOUN[scope.kind]}
                      </span>
                      {scope.name ? (
                        <span className="min-w-0 truncate font-mono text-xs">
                          {scope.name}
                        </span>
                      ) : null}
                      <span className="ml-auto shrink-0 text-xs text-faint">
                        {SCOPE_REACH[scope.kind]}
                      </span>
                    </label>
                  );
                })}
              </div>
            </fieldset>

            <div>
              <p className="font-mono text-2xs uppercase tracking-wide text-faint">
                What this stops, right now
              </p>
              <KeyValueList className="mt-1" items={impactItems(selected?.impact)} />
              <p className="mt-2 text-sm text-secondary-foreground">
                {STOP_HELD_EXPLAINER}
              </p>
              <p className="mt-1 text-xs text-faint">{STOP_LIMIT}</p>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor={reasonId}>{STOP_REASON_LABEL}</Label>
              <Input
                id={reasonId}
                ref={reasonRef}
                value={reason}
                maxLength={STOP_REASON_MAX}
                aria-invalid={reasonError ? true : undefined}
                aria-describedby={`${reasonId}-hint`}
                autoComplete="off"
                placeholder="runaway delegation loop"
                onChange={(e) => {
                  setReason(e.target.value);
                  if (reasonError) setReasonError(null);
                }}
                // Enter must NOT submit from here: ConfirmDialog owns the
                // typed-name gate, and a keyboard shortcut that reached past it
                // would be a one-key cluster-wide stop.
                onKeyDown={(e) => {
                  if (e.key === "Enter") e.preventDefault();
                }}
              />
              <div className="flex items-baseline justify-between gap-3">
                <p id={`${reasonId}-hint`} className="text-xs text-faint">
                  {STOP_REASON_HINT}
                </p>
                <span className="shrink-0 font-mono text-2xs tabular-nums text-faint">
                  {reason.length}/{STOP_REASON_MAX}
                </span>
              </div>
              <FieldError>{reasonError}</FieldError>
            </div>

            <FieldError>{submitError}</FieldError>
          </div>
        }
      />
    </>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// StopNotice — the active-stop banner
// ─────────────────────────────────────────────────────────────────────────────

export interface StopNoticeProps {
  scope: StopScopeKind;
  /** The stopped thing, qualified: "ns/team-b". Omitted for a fleet stop. */
  scopeName?: string;
  /** The operator's reason, quoted verbatim on the banner. */
  reason: string;
  /** Who stopped it, e.g. "oncall@acme". */
  by?: string;
  /** When, as an ISO timestamp (or an already-formatted string). */
  at?: string;
  /** Live counts from the backend. Omitted fields render as an absence. */
  impact?: StopImpact;
  /**
   * Lift handler. ABSENT ⇒ NO BUTTON — a viewer sees the notice and reads why
   * their work is stopped, but never gets an affordance they cannot use.
   */
  onLift?: (scope: StopScopeKind, name?: string) => void | Promise<void>;
  /** Cluster name — the typed gate on lifting a fleet-wide stop. */
  clusterName?: string;
  className?: string;
}

/** The banner's impact terms — only counts the backend actually sent. */
function noticeTerms(impact: StopImpact | undefined) {
  const terms: Array<{ n: number; words: string }> = [];
  if (typeof impact?.queued === "number") {
    terms.push({
      n: impact.queued,
      words: `queued ${plural(impact.queued, "run")} held`,
    });
  }
  if (typeof impact?.running === "number") {
    terms.push({
      n: impact.running,
      words: `${plural(impact.running, "run")} in flight stopping`,
    });
  }
  if (typeof impact?.agents === "number") {
    terms.push({
      n: impact.agents,
      words: `${plural(impact.agents, "agent")} refusing new runs`,
    });
  }
  return terms;
}

export function StopNotice({
  scope,
  scopeName,
  reason,
  by,
  at,
  impact,
  onLift,
  clusterName,
  className,
}: StopNoticeProps) {
  const [confirming, setConfirming] = React.useState(false);
  const [lifting, setLifting] = React.useState(false);
  const [liftError, setLiftError] = React.useState<string | null>(null);

  const terms = noticeTerms(impact);
  const when = at ? formatDateTime(at) || at : "";
  const attribution = [by, when].filter(Boolean).join(", ");
  const label = scopeName || (scope === "fleet" ? "everything" : scope);

  async function lift() {
    if (!onLift) return;
    setLiftError(null);
    setLifting(true);
    try {
      await onLift(scope, scopeName);
      setConfirming(false);
    } catch (err) {
      setLiftError(
        err instanceof Error
          ? `The stop was not lifted: ${err.message}`
          : "The stop was not lifted. It is still in force.",
      );
    } finally {
      setLifting(false);
    }
  }

  return (
    <div
      role="status"
      className={cn(
        "flex w-full flex-wrap items-stretch rounded-lg border border-destructive bg-card",
        className,
      )}
    >
      {/* The label block — one of exactly two full-bleed semantic surfaces the
          system allows (§2.2), because a stop in force is the one thing that
          may shout. */}
      <div className="flex flex-col justify-center gap-0.5 border-r border-destructive bg-destructive-surface px-4 py-3">
        <span className="font-mono text-2xs uppercase tracking-wide text-destructive">
          Stopped
        </span>
        <span className="font-mono text-sm font-semibold">{label}</span>
        <span className="text-xs text-faint">{SCOPE_REACH[scope]}</span>
      </div>

      <div className="min-w-0 flex-1 px-4 py-3">
        <p className="text-sm">
          {terms.length > 0 ? (
            terms.map((t, i) => (
              <React.Fragment key={t.words}>
                {i > 0 ? <span className="px-2 text-ghost">·</span> : null}
                <span className="font-mono font-semibold tabular-nums">
                  {t.n}
                </span>{" "}
                <span className="text-secondary-foreground">{t.words}</span>
              </React.Fragment>
            ))
          ) : (
            // Nothing was reported. That is not "nothing is affected" — say so
            // with the unknown glyph rather than an invented zero.
            <span className="text-secondary-foreground">
              Impact{" "}
              <UnknownValue title="The backend has not reported what this stop is holding. It is unknown — not zero." />
            </span>
          )}
        </p>
        <p className="mt-1 text-sm text-secondary-foreground">
          {STOP_NOTICE_CONTRACT}
        </p>
        <p className="mt-2 font-serif text-md italic">
          “{reason}”
          {attribution ? (
            <span className="ml-2 font-sans text-xs not-italic text-faint">
              — {attribution}
            </span>
          ) : null}
        </p>
        {liftError ? (
          <div className="mt-2">
            <FieldError>{liftError}</FieldError>
          </div>
        ) : null}
      </div>

      {onLift ? (
        <div className="flex items-center px-4 py-3">
          <Button
            type="button"
            variant="destructive"
            size="sm"
            onClick={() => setConfirming(true)}
          >
            Lift the stop
          </Button>
        </div>
      ) : null}

      {onLift ? (
        <ConfirmDialog
          open={confirming}
          onCancel={() => {
            setConfirming(false);
            setLiftError(null);
          }}
          onConfirm={lift}
          busy={lifting}
          title={`Lift the stop on ${label}?`}
          description={LIFT_CONTRACT}
          confirmLabel="Lift the stop"
          confirmText={
            scope === "fleet" ? (clusterName || "everything") : undefined
          }
          impact={
            <div>
              <p className="font-mono text-2xs uppercase tracking-wide text-faint">
                What starts again
              </p>
              <KeyValueList className="mt-1" items={impactItems(impact, "lift")} />
              <p className="mt-2 text-sm text-secondary-foreground">
                Held runs start as soon as the stop is lifted — there is no
                staggered release, so expect the backlog to run at once.
              </p>
            </div>
          }
        />
      ) : null}
    </div>
  );
}
