import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Play, Rocket } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";

import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, PanelHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { ChatPanel } from "@/components/agent-chat";
import { RunInspector } from "@/components/dashboard/run-inspector";
import {
  ClosingNote,
  ComboSelect,
  DetailDrawer,
  ForbiddenInline,
  KeyValueList,
  NextStepLink,
  PageHeader,
  QuietNote,
  SkeletonText,
  Timeline,
  truncateId,
  type KeyValueItem,
  type TimelineStep,
} from "@/components/kit";
import { FormField } from "@/components/config/form-field";
import {
  api,
  ApiError,
  type AgentDetailResponse,
  type CreatedObject,
  type RunDetail,
  type WorkflowNodeStatus,
} from "@/lib/api";
import { formatLatency } from "@/lib/format";
import { markRunCompleted } from "@/lib/first-run";
import { useDurableRun, type RunPause } from "@/lib/use-durable-run";
import {
  isValidHttpUrl,
  MCP_OAUTH_MESSAGE,
  type McpOAuthPopupMessage,
  readMcpOAuthReturn,
} from "@/lib/oauth-popup";
import { useCapabilities } from "@/lib/capabilities";
import { RES_AGENTS } from "@/lib/nav";
import {
  emptyForm,
  toAgentYAML,
  validate,
  type ConfigForm,
  type FieldErrors,
} from "@/lib/config-form";

// PlaygroundPage — define on the left, run and its story on the right (M151
// §6.1 archetype A8, the workbench). Route: /playground
//
// ── PLAIN TEXT FIRST (m184.16) ──────────────────────────────────────────────
// A first run is a sentence, not a JSON document. By default the right column
// is the console ChatPanel for the picked agent: type, press Enter, read the
// answer. It sends {"input": "<text>"}, exactly as the agent page's chat does,
// and holds Send while the agent's model route is still loading (ADR 0151) —
// which is why the page reads the agent's detail and re-reads it until the
// agent has settled. "JSON request" is the advanced mode for an agent whose
// input is a structured object: the original define/run/story workbench below,
// unchanged. Both keep the record (run id, trace, how long it took) and the
// link to the full trace. ?mode=json opens the advanced mode directly.
//
// ── THE PAGE'S ONE IDEA: THE RUN IS A STORY, NOT A STATUS FIELD ─────────────
// A model reply, a consent gate, an approval, a workflow suspending between
// nodes — to the person watching, those are not four subsystems, they are what
// happened, in order. So the right column is the kit Timeline (§5.26), the same
// spine the run reader uses, and governance renders IN it rather than in a
// second panel with its own clock. The decision affordance sits directly under
// the spine that says a decision is needed.
//
// ── THE HUE RULING ON A SUSPENDED WORKFLOW (§2.2, §2.4, §2.5) ───────────────
// `waiting` used to render amber, which is now reserved for "a bound is near or
// crossed". It is NOT amber. It is also NOT hold-violet, and that is the part
// worth writing down, because hold is the tempting answer:
//
//   internal/run/run.go — "StatusWaiting — paused parked on one or more CHILD
//   RUNS, MACHINE-woken (vs requires_action = human-woken), per ADR 0060 §3 …
//   when the wait condition is met the store flips it back to `queued`".
//
// A suspended workflow instance is waiting on a CHILD RUN, not on a person. It
// holds no lease and no worker and it wakes itself. Painting it hold-violet
// would tell an operator to go and decide something about a run that needs
// nobody — the exact confusion §2.4 exists to prevent, and the same defect the
// run reader removed from its orchestration dots. It is the machine converging
// on its own, so it is `progressing` (§2.5). Hold is worn here by exactly two
// states, both genuinely human-woken: `consent_required` and `approval`.
//
// ── WHAT THE PAGE MAY NOT CLAIM (§7.1) ──────────────────────────────────────
// The run endpoints return identity, status, the message list and the pending
// action. No token counts, no spend, no per-step timing. The run panel states
// those absences in words rather than printing a zero, and a run with no trace
// id says "not linked" rather than offering a link to nowhere.
//
// ── STACKED ORDER BELOW `lg` (§4.7) ─────────────────────────────────────────
// The grid collapses to one column at `lg`, and DOM order is the reading order:
// what to run → the Run button at the form's foot → what happened → what it
// produced → the decision → the run's facts. Export-to-CRD is a separate errand
// and sits below both columns, after the result, so it never lands between a
// run and its outcome.
//
// data-testid contract:
//   playground-namespace / playground-agent — the identity pickers
//   playground-mode-chat / playground-mode-json — plain text (default) or a JSON request
//   playground-chat-mode / playground-json-mode — each mode's column; the other is hidden, not unmounted
//   playground-chat-empty / -loading / -missing / -invalid / -error — the chat column before a chat
//   chat-panel and its own chat-* ids — the chat itself (agent-chat.tsx)
//   playground-advanced      — the define-and-export fields disclosure
//   run-readonly-note        — the viewer's read-only explanation (no Run button)
//   chat-transcript          — the run's story panel
//   run-streaming / run-step — the live stream indicator and its step label
//   cancel-run               — stop a streaming run
//   trace-building           — the run panel while no trace id exists yet
//   trace-id                 — the trace id, whole
//   run-took                 — how long the run took, from the run store's own timestamps
//   view-full-trace          — the link to the native trace explorer
//   approval-request         — the approve/deny decision panel
//   connect-{server}         — one inline consent button per server
//   workflow-graph-section   — the per-node panel of a workflow instance run
//   crd-export-preview       — the exported CRD well

// Run is the lifecycle of a Playground invoke. On success we hold the returned
// traceId — the hand-off we link to the native /traces/:id (m16.7, m17.13).
// Approval is a human-in-the-loop pause (m32.4): the run awaits an approve/deny on `key`.
type Approval = { runId: string; key: string; summary: string };

type Run =
  | { kind: "idle" }
  // running carries the run id (for cancel) + the response accumulated live from token events +
  // the latest live step label (M78: "what step is my agent on right now"), empty until the first
  // `step` event.
  | { kind: "running"; runId: string; response: string; step?: string }
  | {
      kind: "done";
      runId: string;
      traceId: string;
      response: string;
      runStatus: string;
      consentRequired?: string[];
      approval?: Approval;
      /** Milliseconds from created to finished, as the run store recorded them; absent until finished. */
      took?: number;
      // Workflow instance fields (m67.9): present when the run is a workflow instance.
      workflowRef?: string;
      currentNode?: string;
      nodes?: WorkflowNodeStatus[];
    }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

// Export is the export-to-CRD lifecycle: expand (preview) → apply. It reuses the
// config-builder's expand/apply path verbatim (same server-side mapping as the
// CLI); no raw YAML is hand-edited.
type Export =
  | { kind: "idle" }
  | { kind: "previewing" }
  | { kind: "preview"; yaml: string; manifest: string }
  | { kind: "applying"; yaml: string; manifest: string }
  | { kind: "applied"; created: CreatedObject[] }
  | { kind: "error"; message: string; status?: number; forbidden?: boolean };

// ── The run's vocabulary: one status → one tag, one closing sentence ─────────

type TagVariant = NonNullable<BadgeProps["variant"]>;

/**
 * The tag for the run panel. Tag labels are budgeted to ≤16 characters (§4.5)
 * and are `whitespace-nowrap`, so the full sentence lives in the ClosingNote
 * under the spine instead — a 30-character tag is a width bug waiting for a
 * narrow viewport.
 */
function runTag(run: Run): { label: string; variant: TagVariant } | null {
  if (run.kind === "idle") return null;
  if (run.kind === "running") return { label: "Running", variant: "progressing" };
  if (run.kind === "error") return { label: "Failed", variant: "crit" };
  // A person is genuinely blocking this one — the only two states that are.
  if (run.approval || (run.consentRequired && run.consentRequired.length > 0)) {
    return { label: "Waiting on you", variant: "hold" };
  }
  switch (run.runStatus) {
    case "succeeded":
    case "completed":
      return { label: "Succeeded", variant: "ok" };
    case "cancelled":
      return { label: "Cancelled", variant: "crit" };
    case "requires_action":
      return { label: "Waiting on you", variant: "hold" };
    // Machine-woken, parked on a child run — converging, not held. See the hue
    // ruling in the file header.
    case "waiting":
      return { label: "Suspended", variant: "progressing" };
    default:
      return { label: "Running", variant: "progressing" };
  }
}

/**
 * The §5.18 closing line: restates in words what the spine already showed. It
 * is also where the two long-form phrases live, because they say something a
 * ≤16-character tag cannot.
 */
function closingLine(run: Run): string | null {
  if (run.kind === "running") return "The story fills in as the run makes it.";
  if (run.kind !== "done") return null;
  if (run.approval || (run.consentRequired && run.consentRequired.length > 0)) {
    return "It is held, not failed — the tool has not been called and the run is holding its place.";
  }
  if (run.runStatus === "waiting") {
    return "Suspended — awaiting next node. The run parked on the child it launched and holds no worker; the platform wakes it when that child finishes, so nothing is asked of you.";
  }
  if (run.runStatus === "cancelled") {
    return "The run was stopped. Whatever it had already done stands.";
  }
  return "Traced run complete — every step above was recorded, and the full waterfall is one click away.";
}

/**
 * The run's story, in order, from what this page actually knows. Titles are
 * SENTENCES (§5.26); the machine words — the JSON you sent, the step label the
 * stream reported — render as inline mono evidence in the detail line.
 */
function buildStory(input: string, run: Run): TimelineStep[] {
  const steps: TimelineStep[] = [
    {
      id: "sent",
      title: "You sent the request",
      detail: (
        <span className="whitespace-pre-wrap break-words font-mono text-xs">
          {input.trim() || "{}"}
        </span>
      ),
    },
  ];

  if (run.kind === "running") {
    steps.push({
      id: "working",
      title: "The agent is working",
      detail: run.step ? (
        <>
          It is on <span className="font-mono text-xs">{run.step}</span> right now.
        </>
      ) : (
        "Tokens arrive below as the model produces them."
      ),
    });
    return steps;
  }

  if (run.kind !== "done") return steps;

  if (run.consentRequired && run.consentRequired.length > 0) {
    steps.push({
      id: "consent",
      tone: "hold",
      title: "The run needs your own account connected before it goes on",
      detail:
        "Nothing has been lost. The tool has not been called — connect below and the run picks up exactly where it stopped.",
    });
    return steps;
  }

  if (run.approval) {
    steps.push({
      id: "approval",
      tone: "hold",
      title: "A person has to approve this before it goes on",
      detail: run.approval.summary || undefined,
    });
    return steps;
  }

  if (run.runStatus === "waiting") {
    // A plain moment, not a governance gate: nobody is being asked for
    // anything. See the hue ruling in the file header.
    steps.push({
      id: "suspended",
      title: "The workflow parked between nodes",
      detail:
        "It launched a child run and is holding no worker while that child works. The platform wakes it again on its own.",
    });
    return steps;
  }

  if (run.runStatus === "cancelled") {
    steps.push({
      id: "cancelled",
      tone: "failed",
      title: "The run was stopped before it finished",
      detail: "It will not go on. Nothing it had not already done was done.",
    });
    return steps;
  }

  steps.push({
    id: "end",
    tone: "done",
    title: "The run finished and answered",
  });
  return steps;
}

/** How the Playground talks to the agent: a chat (plain text) or one JSON request. */
type Mode = "chat" | "json";

/** An agent the picker offers, with the namespace it lives in. */
type AgentOption = { name: string; namespace: string };

/** The agent the chat talks to, read from its detail endpoint. */
type ChatAgent =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; detail: AgentDetailResponse }
  | { kind: "missing" }
  | { kind: "error"; message: string; forbidden: boolean };

/** The namespace the BFF runs an agent in when none is named (defaultCreateNamespace). */
const BFF_DEFAULT_NAMESPACE = "default";

/** Typing a custom name should not fetch on every keystroke. */
const PICK_DEBOUNCE_MS = 250;

/** How often an agent that has not settled is re-read, as on the agent page. */
const SETTLE_POLL_MS = 4_000;

/** A failed background re-read doubles the wait before the next one, up to this. */
const SETTLE_RETRY_MAX_MS = 30_000;

/** How long the chat keeps re-reading an agent that has not settled before it stops and offers
 *  "Check again", as the trace page does for a trace that has not landed. */
const SETTLE_MAX_MS = 10 * 60_000;

/** Ready, and its model route (if any) is served — the state the chat needs to stop polling. */
function agentSettled(d: AgentDetailResponse): boolean {
  return (
    d.ready &&
    !(d.conditions ?? []).some(
      (c) => c.type === "ModelRouteReady" && c.status === "False" && c.reason === "NotYetServed",
    )
  );
}

/**
 * Reads the agent the chat will talk to, and re-reads it while it settles: a freshly created
 * agent is not Ready yet and its new model route takes the gateway a minute to serve, and the
 * ChatPanel holds Send until it is (ADR 0151). Without the re-read the hold would never lift.
 * The re-reads stop after SETTLE_MAX_MS (`stopped`); `checkAgain` starts them over.
 */
function useChatAgent(
  ns: string,
  name: string,
  enabled: boolean,
): { agent: ChatAgent; stopped: boolean; checkAgain: () => void } {
  // Every read is stored with the agent it is about. A read of the agent picked before is not an
  // answer about this one, so between a pick and its first read the chat is loading: it never
  // offers Send on the strength of the previous agent's detail.
  const key = enabled && name ? `${ns}/${name}` : "";
  const [state, setReadState] = useState<{ key: string; agent: ChatAgent }>({
    key: "",
    agent: { kind: "idle" },
  });
  const [stoppedKey, setStoppedKey] = useState("");
  const restartRef = useRef<(() => void) | null>(null);
  useEffect(() => {
    if (!key) return;
    const setState = (agent: ChatAgent) => setReadState({ key, agent });
    const setStopped = (stopped: boolean) => setStoppedKey(stopped ? key : "");
    setStopped(false);
    const ctrl = new AbortController();
    let timer = 0;
    // Consecutive background re-reads that failed. Each one doubles the wait before the next.
    let failures = 0;
    // Time spent waiting between re-reads since the watch (re)started.
    let waited = 0;
    const reread = () => {
      const wait = Math.min(SETTLE_POLL_MS * 2 ** failures, SETTLE_RETRY_MAX_MS);
      if (waited + wait > SETTLE_MAX_MS) {
        setStopped(true);
        return;
      }
      waited += wait;
      timer = window.setTimeout(() => read(true), wait);
    };
    const read = (silent: boolean) => {
      if (silent && document.visibilityState === "hidden") {
        reread();
        return;
      }
      if (!silent) setState({ kind: "loading" });
      // The chat shows no references, so it does not ask the BFF to resolve them on every read.
      api
        .agentDetail(ns, name, ctrl.signal, { dependencies: false })
        .then((detail) => {
          if (ctrl.signal.aborted) return;
          failures = 0;
          setState({ kind: "ready", detail });
          if (!agentSettled(detail)) reread();
        })
        .catch((err: unknown) => {
          if (ctrl.signal.aborted) return;
          // A background re-read that fails keeps the detail already on screen, so the chat and
          // its transcript stay, and tries again later. Only the first read decides what renders.
          if (silent) {
            failures += 1;
            reread();
            return;
          }
          if (err instanceof ApiError && err.status === 404) {
            setState({ kind: "missing" });
            return;
          }
          setState({
            kind: "error",
            message: err instanceof Error ? err.message : "couldn't read the agent",
            forbidden: err instanceof ApiError && err.isForbidden,
          });
        });
    };
    restartRef.current = () => {
      window.clearTimeout(timer);
      failures = 0;
      waited = 0;
      setStopped(false);
      read(true);
    };
    timer = window.setTimeout(() => read(false), PICK_DEBOUNCE_MS);
    return () => {
      ctrl.abort();
      window.clearTimeout(timer);
      restartRef.current = null;
    };
  }, [key, ns, name]);
  const checkAgain = useCallback(() => restartRef.current?.(), []);
  const agent: ChatAgent = !key
    ? { kind: "idle" }
    : state.key === key
      ? state.agent
      : { kind: "loading" };
  return { agent, stopped: key !== "" && stoppedKey === key, checkAgain };
}

/** Why a name the picker lists in several namespaces cannot run until one is picked, or undefined. */
function ambiguousMessage(name: string, homes: string[]): string | undefined {
  if (homes.length === 0) return undefined;
  return `${name} exists in ${homes.join(", ")}. Pick which one from the list.`;
}

/** Milliseconds from a finished run's creation to its last state change, or undefined. */
function runDuration(detail: RunDetail): number | undefined {
  if (!["succeeded", "completed", "failed", "cancelled"].includes(detail.status)) return undefined;
  if (!detail.createdAt || !detail.updatedAt) return undefined;
  const ms = Date.parse(detail.updatedAt) - Date.parse(detail.createdAt);
  return Number.isFinite(ms) && ms >= 0 ? ms : undefined;
}

export function PlaygroundPage() {
  const [form, setForm] = useState<ConfigForm>(emptyForm);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [namespace, setNamespace] = useState("");
  const [input, setInput] = useState('{\n  "input": "Hello, agent"\n}');
  const [run, setRun] = useState<Run>({ kind: "idle" });
  const [exp, setExp] = useState<Export>({ kind: "idle" });
  // Inline consent (ADR 0031, m26.2): the server currently being connected (its inline
  // "Connect" button shows a spinner) and any begin-consent error.
  const [connecting, setConnecting] = useState<string | null>(null);
  const [connectError, setConnectError] = useState<string | null>(null);
  // RBAC-aware chrome (§3): running an agent and applying a CRD are both
  // create-shaped ops; a viewer's affordances are hidden. DISPLAY-ONLY — the API
  // still enforces, so the 403 paths below stay live even if this is optimistic.
  const { can, reprobe } = useCapabilities();
  const canRun = can(RES_AGENTS, "create");
  // The active inline-consent wait's teardown (OTH-2): onConnect registers a window `message`
  // listener + a popup-close poll interval; this holds their cleanup so the unmount effect can
  // clear them even if the user navigates away with the popup still open (they'd leak otherwise).
  const connectCleanupRef = useRef<(() => void) | null>(null);

  // Identity pickers + deep link (DX-5): the checklist "Run" step and create→run land here
  // with ?agent=<name>&ns=<namespace>; pre-fill so the user never re-types. The namespace +
  // agent fields are PICKERS over the caller's known set (listNamespaces / listAgents — the
  // console's "finite set → Select" principle) while still allowing a new name (define+export).
  const [searchParams] = useSearchParams();
  const [mode, setMode] = useState<Mode>(() =>
    searchParams.get("mode") === "json" ? "json" : "chat",
  );
  const [namespaceOptions, setNamespaceOptions] = useState<string[]>([]);
  const [agentOptions, setAgentOptions] = useState<AgentOption[]>([]);
  // The namespace of an agent picked as namespace/name, which the picker offers under All
  // workspaces when one name exists in more than one namespace.
  const [pickedNs, setPickedNs] = useState("");
  const didInitRef = useRef(false);
  // A same-tab (popup-blocked) MCP OAuth return the boot handler stashed (DX-6) — surfaced as
  // a notice so consent-on-a-blocked-popup no longer ends in silence.
  const [oauthReturn, setOauthReturn] = useState<McpOAuthPopupMessage | null>(null);
  // The chat's latest finished turn, for the record; and the trace a turn asked to open.
  const [lastChatRun, setLastChatRun] = useState<RunDetail | null>(null);
  const [inspectTrace, setInspectTrace] = useState<string | null>(null);

  // With no namespace picked, the agent list spans every namespace the caller can read, so a
  // picked agent runs in ITS namespace — not the BFF's default, where it does not exist. A name
  // that exists in more than one namespace is offered once per namespace, as namespace/name, and
  // runs in the one picked; until one is picked the page does not guess.
  const agentName = form.name.trim();
  const scoped = namespace.trim() !== "";
  const homesByName = useMemo(() => {
    const homes = new Map<string, string[]>();
    for (const o of agentOptions) {
      const list = homes.get(o.name) ?? [];
      if (!list.includes(o.namespace)) list.push(o.namespace);
      homes.set(o.name, list);
    }
    return homes;
  }, [agentOptions]);
  const agentPickerOptions = useMemo(() => {
    const values = agentOptions.map((o) =>
      !scoped && (homesByName.get(o.name)?.length ?? 0) > 1 ? `${o.namespace}/${o.name}` : o.name,
    );
    return [...new Set(values)];
  }, [agentOptions, homesByName, scoped]);
  const homes = scoped ? [] : (homesByName.get(agentName) ?? []);
  const pickedNamespace =
    homes.length === 1 ? homes[0] : homes.includes(pickedNs) ? pickedNs : "";
  const ambiguousHomes = homes.length > 1 && !pickedNamespace ? homes : [];
  const agentPickerValue =
    homes.length > 1 && pickedNamespace ? `${pickedNamespace}/${agentName}` : form.name;
  const runNamespace = namespace.trim() || pickedNamespace;
  const chatNamespace = runNamespace || BFF_DEFAULT_NAMESPACE;
  const nameError = agentName ? validate(form).name : undefined;
  const {
    agent: chatAgent,
    stopped: settleStopped,
    checkAgain: checkAgentAgain,
  } = useChatAgent(
    chatNamespace,
    agentName,
    canRun && agentName !== "" && !nameError && ambiguousHomes.length === 0,
  );

  // A namespace/name option names both; anything else is a name, picked or typed.
  function pickAgent(value: string) {
    const slash = value.indexOf("/");
    if (slash > 0 && agentPickerOptions.includes(value)) {
      setPickedNs(value.slice(0, slash));
      set("name", value.slice(slash + 1));
      return;
    }
    setPickedNs("");
    set("name", value);
  }

  // The durable run engine (ADR 0093) — the SAME create-or-resume → stream → finalize engine the
  // console ChatPanel uses (this page previously duplicated it in onRun + onApprove). It owns the
  // SSE stream lifecycle (aborted on New-run / cancel / unmount); the callbacks project its
  // live/finalized state into this page's richer `Run` union (traceId, consent/approval,
  // workflow nodes) so the define/run/export chrome is unchanged.
  const runEngine = useDurableRun({
    onForbidden: () => reprobe(),
    onFinalized: (detail, pause) => {
      // The Home checklist's "Run your agent" step reads a Langfuse-backed feed that is ready and
      // EMPTY on a default install, so it never checked off for a user who had just watched a run
      // finish. Record what the browser saw, under the namespace the run went to (the BFF's default
      // when none is named). Any terminal result counts: the step asks the user to send a message
      // and watch the trace, which a failed run also satisfies.
      markRunCompleted(chatNamespace);
      setRun(finalizedRun(detail, pause));
    },
    onFinalizeError: (err) => setRun(errorRun(err)),
  });

  // Mirror the durable engine's LIVE stream state (running + accumulated tokens + step label)
  // into the `Run` union so the streaming panel + Cancel keep working exactly as before.
  useEffect(() => {
    if (runEngine.status === "streaming" || runEngine.status === "creating") {
      setRun({
        kind: "running",
        runId: runEngine.runId ?? "",
        response: runEngine.responseText,
        step: runEngine.step || undefined,
      });
    } else if (runEngine.status === "error") {
      setRun({ kind: "error", message: runEngine.error ?? "The run failed." });
    } else if (runEngine.status === "forbidden") {
      setRun({ kind: "error", message: runEngine.error ?? "Forbidden.", forbidden: true });
    }
    // Only react to the durable-run transitions; `done` is handled by onFinalized.
  }, [runEngine.status, runEngine.responseText, runEngine.step, runEngine.error, runEngine.runId]);

  function set<K extends keyof ConfigForm>(key: K, value: ConfigForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // A new agent or workspace is a new conversation, so the record of the old one goes too.
  useEffect(() => {
    setLastChatRun(null);
  }, [chatNamespace, agentName]);

  // Deep-link pre-fill + namespace load, ONCE on mount (a ref guards against re-applying the
  // pre-fill over the user's later edits). A 403/failure just leaves the picker empty — the
  // field still accepts a custom value.
  useEffect(() => {
    if (didInitRef.current) return;
    didInitRef.current = true;
    const agentParam = searchParams.get("agent") ?? "";
    const nsParam = searchParams.get("ns") ?? searchParams.get("namespace") ?? "";
    if (agentParam) setForm((f) => ({ ...f, name: agentParam }));
    if (nsParam) setNamespace(nsParam);
    // Surface a same-tab (popup-blocked) MCP OAuth outcome the boot handler stashed (DX-6).
    setOauthReturn(readMcpOAuthReturn());
    const ctrl = new AbortController();
    api
      .namespaces(ctrl.signal)
      .then((res) => setNamespaceOptions(res.namespaces.map((n) => n.name)))
      .catch(() => {});
    return () => ctrl.abort();
  }, [searchParams]);

  // Load the agents in the selected namespace so the agent field is a picker of what's
  // actually deployed there; re-scopes when the namespace changes (prior request aborted).
  useEffect(() => {
    const ctrl = new AbortController();
    api
      .listAgents({ namespace: namespace.trim() || undefined, limit: 100 }, ctrl.signal)
      .then((res) =>
        setAgentOptions(res.agents.map((a) => ({ name: a.name, namespace: a.namespace }))),
      )
      .catch(() => setAgentOptions([]));
    return () => ctrl.abort();
  }, [namespace]);

  // Unmount cleanup (OTH-2): tear down any active inline-consent wait (the `message` listener +
  // popup-close poll). The durable run engine owns aborting its own in-flight SSE stream on
  // unmount, so we only clear the consent wait here.
  useEffect(() => {
    return () => {
      connectCleanupRef.current?.();
    };
  }, []);

  // Run invokes the DEFINED agent by name via the shared durable run engine (ADR 0093 — the
  // SAME create → stream → finalize the console chat uses). The agent must already be deployed
  // (the run resolves its endpoint server-side); the Playground's define form is the same
  // identity used for export. Input is parsed as JSON so the browser sends a real object (a
  // malformed input is caught before the round-trip). The engine streams tokens + finalizes;
  // this page projects the finalized detail via onFinalized → finalizedRun().
  async function onRun() {
    // Run invokes an EXISTING, already-deployed agent BY NAME — so it needs only a valid
    // agent name (+ valid JSON input below). Image / scaling / budget are define/export-time
    // fields validated by "Export to CRD"; requiring them here wrongly BLOCKED invoking a
    // live agent (the run silently never fired → no response, no consent CTA). Validate the
    // name only for a run.
    const runNameError = validate(form).name ?? ambiguousMessage(agentName, ambiguousHomes);
    if (runNameError) {
      setErrors({ name: runNameError });
      setRun({ kind: "error", message: runNameError });
      return;
    }
    setErrors({});
    let parsedInput: unknown;
    try {
      parsedInput = input.trim() ? JSON.parse(input) : {};
    } catch {
      setRun({ kind: "error", message: "Input must be valid JSON." });
      return;
    }
    await runEngine.start({
      agent: agentName,
      namespace: runNamespace,
      input: parsedInput,
    });
  }

  // onApprove / onDeny resolve a human-in-the-loop pause (m32.4). Approve RESUMES the paused run
  // (the engine re-streams to completion). Deny resolves it to a terminal "denied" state
  // directly (no re-stream). Both go through POST /api/runs/{id}/resume.
  async function onApprove(_runId: string) {
    // The engine resumes the run that asked (the paused run == _runId).
    await runEngine.resume("approve");
  }

  async function onDeny(runId: string) {
    // A sub-agent's approval: denying it ends only the sub-run, and the engine follows the run on.
    if (runEngine.pause?.descendant) {
      await runEngine.resume("deny");
      return;
    }
    try {
      await api.resumeRun(runId, "deny");
      setRun({
        kind: "done",
        runId,
        traceId: "",
        response: "Approval denied — run cancelled.",
        runStatus: "cancelled",
      });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setRun(errorRun(err));
    }
  }

  // onCancel stops a streaming run: the engine cancels its SSE stream + the run server-side,
  // then resets to idle.
  async function onCancel(_runId: string) {
    await runEngine.cancel();
    setRun({ kind: "idle" });
  }

  // onConnect runs the INLINE per-user consent (ADR 0031, m26.2): begin the OAuth grant
  // for the named server (the BFF recovers the OAuth config, so we send only the server),
  // open the provider consent in a POPUP so this run stays on screen, then — when the
  // popup messages back (the auto-close bridge) or is closed — RE-RUN the same invoke so
  // the freshly-connected credential is injected. The token never touches the SPA; the
  // exchange is entirely server-side. Popup blocked → full-page redirect fallback.
  async function onConnect(server: string) {
    setConnectError(null);
    setConnecting(server);
    let authorizationURL: string;
    try {
      const res = await api.beginMcpGrant({
        server,
        namespace: runNamespace,
        // The console's OAuth callback — needed for a legacy server the BFF must re-discover.
        redirectUri: `${window.location.origin}/api/mcp/oauth/callback`,
      });
      authorizationURL = res.authorizationURL;
    } catch (err) {
      setConnecting(null);
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setConnectError(err instanceof Error ? err.message : "Couldn't start the connect flow.");
      return;
    }

    const popup = window.open(
      authorizationURL,
      "ctxmesh-oauth-connect",
      "width=520,height=680,menubar=no,toolbar=no",
    );
    if (!popup) {
      // Popup blocked → fall back to a full-page SAME-TAB redirect. Validate the URL first
      // (DX-6) so a relative/`javascript:` value can't hijack the tab. The run state is lost,
      // but the connect completes and the boot handler surfaces the outcome on return so the
      // user knows to re-run. An invalid URL surfaces inline instead of a silent no-op.
      if (isValidHttpUrl(authorizationURL)) {
        window.location.href = authorizationURL;
      } else {
        setConnecting(null);
        setConnectError("the authorization URL returned by the server was invalid");
      }
      return;
    }

    // Resume when the popup reports back (message) or is closed; re-invoke once.
    let done = false;
    let poll = 0;
    function teardown() {
      window.removeEventListener("message", onMessage);
      window.clearInterval(poll);
      connectCleanupRef.current = null;
    }
    function finish() {
      if (done) return;
      done = true;
      teardown();
      setConnecting(null);
      void onRun(); // re-invoke in place — the resume
    }
    function onMessage(e: MessageEvent) {
      if (e.origin !== window.location.origin) return;
      const data = e.data as { type?: string } | null;
      if (data?.type === MCP_OAUTH_MESSAGE) finish();
    }
    window.addEventListener("message", onMessage);
    poll = window.setInterval(() => {
      if (popup.closed) finish();
    }, 700);
    // Expose the teardown so the unmount effect can clear the listener + poll (OTH-2) if the
    // user leaves while the consent popup is still open — otherwise both leak past this page.
    connectCleanupRef.current = teardown;
  }

  // Export-to-CRD: preview the expanded CRD (POST /api/expand), then apply it
  // (POST /api/agents) — the SAME caller-scoped path the config-builder uses.
  async function onPreview() {
    const found = validate(form);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      setExp({ kind: "error", message: "Fix the highlighted fields before exporting." });
      return;
    }
    const yaml = toAgentYAML(form);
    setExp({ kind: "previewing" });
    try {
      const manifest = await api.expand(yaml);
      setExp({ kind: "preview", yaml, manifest });
    } catch (err) {
      setExp(errorExport(err));
    }
  }

  async function onApply() {
    if (exp.kind !== "preview") return;
    const { yaml, manifest } = exp;
    setExp({ kind: "applying", yaml, manifest });
    try {
      const res = await api.createAgent(yaml, namespace.trim());
      setExp({ kind: "applied", created: res.created });
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) reprobe();
      setExp(errorExport(err));
    }
  }

  const running = run.kind === "running";
  const exporting = exp.kind === "previewing" || exp.kind === "applying";
  const started = run.kind === "running" || run.kind === "done";
  const tag = runTag(run);
  const closing = closingLine(run);
  const story = started ? buildStory(input, run) : [];
  const runId =
    run.kind === "running" || run.kind === "done" ? run.runId : "";
  const runTraceId = run.kind === "done" ? run.traceId : "";
  const consent = run.kind === "done" ? (run.consentRequired ?? []) : [];
  const approval = run.kind === "done" ? run.approval : undefined;
  const response = run.kind === "running" || run.kind === "done" ? run.response : "";
  const manifest =
    exp.kind === "preview" || exp.kind === "applying" ? exp.manifest : "";

  // In chat mode the record is the latest finished turn; in JSON mode, the run on screen.
  const record =
    mode === "chat"
      ? {
          runId: lastChatRun?.id ?? "",
          traceId: lastChatRun?.traceId ?? "",
          took: lastChatRun ? runDuration(lastChatRun) : undefined,
          started: lastChatRun !== null,
        }
      : {
          runId,
          traceId: runTraceId,
          took: run.kind === "done" ? run.took : undefined,
          started,
        };
  const traceId = record.traceId;

  // The record, as the run endpoints actually answer it (§7.1). Absences are
  // stated in words; nothing here is inferred.
  const facts: KeyValueItem[] = [
    { key: "Agent", value: agentName || undefined, absent: "not chosen yet" },
    {
      key: "Workspace",
      value:
        ambiguousHomes.length > 0
          ? undefined
          : (mode === "chat" && agentName ? chatNamespace : runNamespace) || undefined,
      absent: ambiguousHomes.length > 0 ? "not chosen yet" : "default",
    },
    {
      key: "Run id",
      value: record.runId ? <span title={record.runId}>{truncateId(record.runId)}</span> : undefined,
      absent: "not started",
      title: record.runId ? undefined : "No run has been created from this page yet.",
    },
    {
      key: "Trace",
      value: traceId ? (
        <span data-testid="trace-id" title={traceId}>
          {truncateId(traceId)}
        </span>
      ) : undefined,
      absent: "not linked",
      title: traceId ? undefined : "No trace was recorded for this run.",
    },
    {
      key: "Took",
      value:
        record.took !== undefined ? (
          <span data-testid="run-took">{formatLatency(record.took)}</span>
        ) : undefined,
      absent: record.started ? "not finished" : "not started",
      title: "From when the run was created to when it finished, as the run store recorded both.",
    },
  ];

  // The chat column: the ChatPanel once the picked agent has been read, and an
  // honest note for every state before that.
  function chatPlaceholder(testId: string, title: string, body: React.ReactNode) {
    return (
      <Card className="min-w-0" data-testid={testId}>
        <PanelHeader title="Chat" />
        <CardContent>
          <QuietNote title={title}>{body}</QuietNote>
        </CardContent>
      </Card>
    );
  }

  // RBAC-aware chrome: a viewer has no composer and no Run button (running is a write-shaped op),
  // so the place each would be says why. It renders in one place at a time: under the form for a
  // JSON request, as the chat column in chat mode.
  const readOnlyNote = (
    <div data-testid="run-readonly-note">
      <QuietNote title="You have read-only access.">
        Running an agent needs create permission on AgentDeployments — ask an admin for a role
        that has it. Previewing and exporting the definition stay available.
      </QuietNote>
    </div>
  );

  let chatColumn: React.ReactNode = null;
  if (!canRun) {
    chatColumn =
      mode === "chat" ? (
        <Card className="min-w-0">
          <PanelHeader title="Chat" />
          <CardContent>{readOnlyNote}</CardContent>
        </Card>
      ) : null;
  } else if (!agentName) {
    chatColumn = chatPlaceholder(
      "playground-chat-empty",
      "Pick an agent to talk to it.",
      "Type a sentence and press Enter. The answer appears here, and every message is a traced run.",
    );
  } else if (nameError) {
    chatColumn = chatPlaceholder("playground-chat-invalid", "That isn't an agent name.", nameError);
  } else if (ambiguousHomes.length > 0) {
    chatColumn = chatPlaceholder(
      "playground-chat-ambiguous",
      `There is an agent named ${agentName} in more than one workspace.`,
      `Pick the one to talk to from the list: ${ambiguousHomes.map((ns) => `${ns}/${agentName}`).join(", ")}.`,
    );
  } else if (chatAgent.kind === "missing") {
    chatColumn = chatPlaceholder(
      "playground-chat-missing",
      `No agent named ${agentName} in ${chatNamespace}.`,
      "Pick one from the list, or define it under Advanced and export it to a CRD below.",
    );
  } else if (chatAgent.kind === "error") {
    chatColumn = chatAgent.forbidden ? (
      <div data-testid="playground-chat-error">
        <ForbiddenInline
          title={`Not allowed to read ${agentName}`}
          description="Your account can't read this agent, so the console can't open a chat with it."
          detail={chatAgent.message}
        />
      </div>
    ) : (
      chatPlaceholder(
        "playground-chat-error",
        `${agentName} could not be read.`,
        <>
          The chat opens once the agent has been read. A JSON request does not
          need it.{" "}
          <span className="font-mono text-xs">{chatAgent.message}</span>
        </>,
      )
    );
  } else if (chatAgent.kind === "ready") {
    const d = chatAgent.detail;
    chatColumn = (
      <div className="min-w-0 space-y-3">
        {settleStopped && (
          <div className="space-y-3" data-testid="playground-settle-stopped">
            <QuietNote title="This page has stopped checking on the agent.">
              It has not settled in ten minutes, so the chat no longer re-reads it on its own.
              What it shows is the last read.
            </QuietNote>
            <Button variant="outline" size="sm" onClick={checkAgentAgain}>
              Check again
            </Button>
          </div>
        )}
        <ChatPanel
          key={`${chatNamespace}/${agentName}`}
          ns={chatNamespace}
          name={agentName}
          ready={d.ready}
          memoryBound={d.bindings.some((b) => b.kind === "memory")}
          modelRoute={(d.conditions ?? []).find((c) => c.type === "ModelRouteReady")}
          onTraced={(id) => setInspectTrace(id)}
          onRunFinalized={setLastChatRun}
        />
      </div>
    );
  } else {
    chatColumn = (
      <Card className="min-w-0" data-testid="playground-chat-loading">
        <PanelHeader title="Chat" />
        <CardContent>
          <SkeletonText lines={4} />
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="min-w-0 space-y-6">
      <PageHeader
        title="Playground"
        lede="Pick a deployed agent and talk to it in plain text. Every message is a traced run — open its trace, switch to a JSON request for an agent that takes structured input, or export the definition to a CRD."
      />

      {/* §4.7: two equal columns above `lg`, one below it — with explicit grid
          placement, because DOM order is the stacked reading order. Both
          children carry `min-w-0` so neither pane can push the page sideways
          (§4.6). */}
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        {/* ── Define ─────────────────────────────────────────────────────── */}
        <div className="min-w-0 space-y-5 lg:col-start-1 lg:row-start-1">
          {oauthReturn && (
            // Not an alert. The connect either worked or it did not, and the
            // one hue it wears is a 2px left rule — §2.2 allows no full-bleed
            // semantic surface here.
            <div
              role="status"
              data-testid="mcp-oauth-return"
              className={
                oauthReturn.error
                  ? "border border-border border-l-2 border-l-destructive bg-surface-2 px-4 py-3 text-sm"
                  : "border border-border border-l-2 border-l-success bg-surface-2 px-4 py-3 text-sm"
              }
            >
              {oauthReturn.error
                ? `Couldn't connect ${oauthReturn.server || "the server"}: ${oauthReturn.error}`
                : `Connected ${oauthReturn.server || "your account"} — run again to continue.`}
              <button
                type="button"
                className="ml-2 border-b border-accent font-semibold text-primary hover:border-primary"
                onClick={() => setOauthReturn(null)}
              >
                dismiss
              </button>
            </div>
          )}

          <Card className="min-w-0">
            <PanelHeader title="What to run" />
            <CardContent className="space-y-5">
              <FormField
                id="namespace"
                label="Namespace"
                hint="Pick a namespace you can access — empty → the default namespace."
              >
                <ComboSelect
                  id="namespace"
                  value={namespace}
                  options={namespaceOptions}
                  onChange={setNamespace}
                  placeholder="default namespace"
                  customPlaceholder="namespace"
                  testId="playground-namespace"
                />
              </FormField>

              <FormField
                id="name"
                label="Agent"
                error={errors.name}
                hint="Pick a deployed agent to run, or name a new one to define + export (≤ 44 chars)."
              >
                <ComboSelect
                  id="name"
                  value={agentPickerValue}
                  options={agentPickerOptions}
                  onChange={pickAgent}
                  placeholder="— pick an agent —"
                  customPlaceholder="new agent name"
                  testId="playground-agent"
                />
              </FormField>

              {/* A viewer can run neither, so there is no choice to offer. */}
              {canRun && (
                <div className="space-y-2">
                  <p className="text-sm font-medium" id="playground-mode-label">
                    How to talk to it
                  </p>
                  <div
                    role="group"
                    aria-labelledby="playground-mode-label"
                    className="inline-flex rounded-md border border-border bg-surface-2 p-0.5"
                  >
                    {(
                      [
                        ["chat", "Plain text"],
                        ["json", "JSON request"],
                      ] as const
                    ).map(([m, label]) => (
                      <button
                        key={m}
                        type="button"
                        aria-pressed={mode === m}
                        data-testid={`playground-mode-${m}`}
                        onClick={() => setMode(m)}
                        className={
                          mode === m
                            ? "rounded-sm bg-card px-3 py-1.5 text-sm font-medium text-foreground shadow-sm"
                            : "rounded-sm px-3 py-1.5 text-sm text-secondary-foreground hover:text-foreground"
                        }
                      >
                        {label}
                      </button>
                    ))}
                  </div>
                  <p className="text-xs text-faint">
                    {mode === "chat"
                      ? 'Each message is sent as {"input": "…"}, the shape a chat agent reads. Use a JSON request for an agent that takes a structured object.'
                      : "The object below is posted verbatim to the agent. Switch back to plain text to chat; a conversation started there is kept."}
                  </p>
                </div>
              )}

              {mode === "json" && (
                <FormField
                  id="input"
                  label="Message (JSON)"
                  hint="Posted verbatim to the agent's /invoke."
                >
                  <Textarea
                    id="input"
                    className="min-h-[6rem] font-mono text-xs"
                    value={input}
                    onChange={(e) => setInput(e.target.value)}
                  />
                </FormField>
              )}

              {/* Advanced — the CRD-authoring fields, needed only to define + export a
                  new agent (a deployed agent runs from just its name). Collapsed by
                  default so the primary flow is: pick agent → message → run. */}
              <details
                className="rounded-md border border-border bg-surface-2 px-4 py-3"
                data-testid="playground-advanced"
              >
                <summary className="cursor-pointer text-sm font-medium text-secondary-foreground">
                  Advanced — image, execution model, route (define &amp; export)
                </summary>
                <div className="space-y-4 pt-4">
                  <FormField
                    id="image"
                    label="Image"
                    error={errors.image}
                    hint="Used when exporting to a CRD."
                  >
                    <Input
                      id="image"
                      value={form.image}
                      onChange={(e) => set("image", e.target.value)}
                      placeholder="ghcr.io/ctxmesh/echo:v1"
                    />
                  </FormField>

                  <FormField id="executionModel" label="Execution model">
                    <Select
                      id="executionModel"
                      value={form.executionModel}
                      onChange={(e) =>
                        set("executionModel", e.target.value as ConfigForm["executionModel"])
                      }
                    >
                      <option value="serving">serving (request-driven)</option>
                      <option value="eventing">eventing (broker-triggered)</option>
                      <option value="job">job (one-shot)</option>
                    </Select>
                  </FormField>

                  <FormField
                    id="modelRoute"
                    label="Model route"
                    hint="ModelRoute alias for the agent's LLM calls (optional)."
                  >
                    <Input
                      id="modelRoute"
                      value={form.modelRoute}
                      onChange={(e) => set("modelRoute", e.target.value)}
                      placeholder="default-model"
                    />
                  </FormField>
                </div>
              </details>
            </CardContent>
          </Card>

          {/* The primary action at the foot of the form column (§6.1 A8), with
              run errors beside the press that produced them (§7 A8). */}
          <div className="space-y-3">
            <div className="flex flex-wrap items-center gap-3">
              {canRun ? (
                // In chat mode the composer is the action; the Run button is the JSON request's.
                mode === "json" && (
                  <>
                    <Button onClick={onRun} disabled={running}>
                      <Play className="h-4 w-4" />
                      {running ? "Running…" : "Run agent"}
                    </Button>
                    {run.kind === "running" && (
                      <Button
                        variant="outline"
                        onClick={() => void onCancel(run.runId)}
                        data-testid="cancel-run"
                      >
                        Cancel
                      </Button>
                    )}
                    {started && (
                      <a
                        href="#run-story"
                        className="whitespace-nowrap border-b border-accent text-sm font-semibold text-primary hover:border-primary lg:hidden"
                      >
                        Jump to the run
                        <span aria-hidden="true"> ↓</span>
                      </a>
                    )}
                  </>
                )
              ) : (
                mode === "json" && readOnlyNote
              )}
            </div>
            {mode === "json" && run.kind === "error" && !run.forbidden && (
              <p className="font-mono text-xs text-destructive" role="alert">
                {run.message}
                {run.status ? ` (${run.status})` : ""}
              </p>
            )}
            {mode === "json" && run.kind === "error" && run.forbidden && (
              // A surface-level 403 → the explain-and-suggest 403 primitive.
              <ForbiddenInline
                title="Not allowed to run this agent"
                description="Your account can't invoke agents in this cluster."
                detail={run.message}
              />
            )}
          </div>
        </div>

        {/* ── Run ────────────────────────────────────────────────────────── */}
        <div
          id="run-story"
          className="min-w-0 space-y-5 lg:col-start-2 lg:row-start-1"
        >
          {/* Both modes stay mounted and the other one is hidden, so switching to a JSON
              request and back does not throw the conversation away. */}
          <div hidden={mode !== "chat"} className="min-w-0" data-testid="playground-chat-mode">
            {chatColumn}
          </div>
          <div hidden={mode !== "json"} className="min-w-0 space-y-5" data-testid="playground-json-mode">
            <Card className="min-w-0" data-testid="chat-transcript">
              <PanelHeader
                title="What happened"
                meta={
                  story.length > 0 ? (
                    <span className="tabular-nums">
                      {story.length} step{story.length === 1 ? "" : "s"}
                    </span>
                  ) : undefined
                }
              >
                {tag ? <Badge variant={tag.variant}>{tag.label}</Badge> : null}
              </PanelHeader>
              <CardContent>
                {story.length === 0 ? (
                  <QuietNote title="Nothing has run yet.">
                    Pick an agent, write the message it should receive, and press{" "}
                    <span className="font-medium">Run agent</span>. Every step the
                    run takes appears here in order, and the full trace is one link
                    from it.
                  </QuietNote>
                ) : (
                  <>
                    <Timeline steps={story} label="Steps in this run" />
                    {closing && <ClosingNote>{closing}</ClosingNote>}
                  </>
                )}
              </CardContent>
            </Card>

            {started && (
              <Card className="min-w-0">
                <PanelHeader title="What it produced">
                  {run.kind === "running" && (
                    <>
                      <span className="text-xs text-faint" data-testid="run-streaming">
                        Streaming…
                      </span>
                      {run.step && (
                        <Badge
                          variant="progressing"
                          className="max-w-[12rem] overflow-hidden"
                          data-testid="run-step"
                        >
                          <span className="block truncate">{run.step}</span>
                        </Badge>
                      )}
                    </>
                  )}
                </PanelHeader>
                <CardContent>
                  {/* The agent's answer is prose as often as it is JSON, so this
                      well wraps (`pre-wrap` + `break-words`, never `break-all`)
                      rather than scrolling like the YAML well below — a paragraph
                      on a horizontal scrollbar is unreadable. It still owns its
                      own height. */}
                  <div
                    role="region"
                    aria-label="Agent response"
                    tabIndex={0}
                    className="max-h-[24rem] overflow-auto rounded-md bg-surface-3 p-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    {response ? (
                      <p className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed">
                        {response}
                      </p>
                    ) : (
                      <p className="font-mono text-xs text-ghost">
                        {run.kind === "running"
                          ? "waiting for the first token…"
                          : "the run returned no content"}
                      </p>
                    )}
                  </div>
                </CardContent>
              </Card>
            )}

            {consent.length > 0 && (
              // A 2px hold rule, not a hold fill: §2.2 allows exactly two
              // full-bleed semantic surfaces console-wide and this is not one.
              <Card className="min-w-0 border-l-2 border-l-hold">
                <PanelHeader title="Connect your account to continue" />
                <CardContent className="space-y-3">
                  <p className="text-sm text-secondary-foreground">
                    This run needs your own credentials, not the platform's.
                    Connect and it re-runs automatically — nothing has been lost.
                  </p>
                  <div className="flex flex-wrap gap-2">
                    {consent.map((server) => (
                      <Button
                        key={server}
                        size="sm"
                        variant="outline"
                        disabled={connecting !== null}
                        onClick={() => void onConnect(server)}
                        data-testid={`connect-${server}`}
                      >
                        {connecting === server ? "Connecting…" : `Connect ${server}`}
                      </Button>
                    ))}
                  </div>
                  {connectError && (
                    <p
                      className="font-mono text-xs text-destructive"
                      role="alert"
                      data-testid="connect-error"
                    >
                      {connectError}
                    </p>
                  )}
                </CardContent>
              </Card>
            )}

            {approval && (
              <Card
                className="min-w-0 border-l-2 border-l-hold"
                data-testid="approval-request"
              >
                <PanelHeader title="Your decision" />
                <CardContent className="space-y-4">
                  <p className="text-sm text-secondary-foreground">
                    This run is holding on one step. Approving lets it continue;
                    denying stops it for good. It is held, not failed — the tool
                    has not been called.
                  </p>
                  {approval.summary && (
                    <p className="border-l-2 border-l-hold bg-surface-2 px-4 py-3 font-serif text-md italic">
                      {approval.summary}
                    </p>
                  )}
                  <div className="flex flex-wrap gap-2">
                    <Button
                      size="sm"
                      disabled={running}
                      onClick={() => void onApprove(approval.runId)}
                      data-testid="approve-run"
                    >
                      Approve
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={running}
                      onClick={() => void onDeny(approval.runId)}
                      data-testid="deny-run"
                    >
                      Deny
                    </Button>
                  </div>
                </CardContent>
              </Card>
            )}

            {run.kind === "done" && run.nodes && run.nodes.length > 0 && (
              <WorkflowGraphSection
                nodes={run.nodes}
                currentNode={run.currentNode}
                runStatus={run.runStatus}
              />
            )}
          </div>

          <Card className="min-w-0">
            <PanelHeader title="The record" />
            <CardContent className="space-y-4">
              <KeyValueList items={facts} />
              {traceId ? (
                <NextStepLink
                  label="Open the trace"
                  to={`/traces/${encodeURIComponent(traceId)}`}
                  ariaLabel="Open the full trace for this run"
                  testId="view-full-trace"
                />
              ) : (
                <div data-testid="trace-building">
                  <QuietNote>
                    {record.started
                      ? "No trace id has come back yet. It appears the moment the run reports one; per-step timing and token counts live there, not here."
                      : "Per-step timing, token counts and spend are span-level facts — they live in the trace backend. A trace id appears here once a run reports one."}
                  </QuietNote>
                </div>
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      {/* ── Export to CRD ──────────────────────────────────────────────────
          A separate errand from running, so it sits BELOW both columns: in one
          stacked column it would otherwise land between the run and its
          outcome. */}
      <Card className="min-w-0">
        <PanelHeader title="Export to CRD">
          {exp.kind === "applied" ? (
            // `progressing`, not `ok`: the create call reports what it CREATED
            // and nothing about readiness, so a green tag here would be a claim
            // the backend never made (§7.1).
            <Badge variant="progressing">applied</Badge>
          ) : manifest ? (
            <Badge variant="muted">preview</Badge>
          ) : null}
        </PanelHeader>
        <CardContent className="space-y-4">
          {exp.kind === "applied" ? (
            <>
              <p className="font-serif text-md font-medium">Applied to the cluster</p>
              <p className="text-sm text-secondary-foreground">
                {exp.created.length === 1
                  ? "One object was"
                  : `${exp.created.length} objects were`}{" "}
                created. They are not serving yet — the controller reconciles
                them next.
              </p>
              <ul className="grid gap-2 sm:grid-cols-2" data-testid="crd-export-created">
                {exp.created.map((obj) => (
                  <li
                    key={`${obj.kind}/${obj.namespace}/${obj.name}`}
                    className="flex min-w-0 items-center justify-between gap-3 rounded-md border border-border bg-surface-2 p-3"
                  >
                    <div className="min-w-0">
                      <p className="truncate text-sm font-medium" title={obj.name}>
                        {obj.name}
                      </p>
                      <p
                        className="truncate font-mono text-xs text-faint"
                        title={obj.namespace}
                      >
                        {obj.namespace}
                      </p>
                    </div>
                    {/* A Kind is identity, not health — never the ok hue. */}
                    <Badge variant="muted">{obj.kind}</Badge>
                  </li>
                ))}
              </ul>
              <Button variant="outline" onClick={() => setExp({ kind: "idle" })}>
                Export again
              </Button>
            </>
          ) : (
            <>
              {exp.kind === "previewing" ? (
                <div className="rounded-md bg-surface-3 p-4">
                  <SkeletonText lines={8} />
                </div>
              ) : manifest ? (
                // The YAML well (§4.5/§4.6): `white-space: pre`, so indentation —
                // which in YAML is the structure — is never reflowed, and the
                // element owns its own scrollbars so a long line scrolls HERE
                // rather than widening the page. Keyboard-reachable, because a
                // scroll container a mouse can reach must be one (WCAG 2.1.1).
                <pre
                  role="region"
                  aria-label="Exported CRD preview"
                  data-testid="crd-export-preview"
                  tabIndex={0}
                  className="max-h-[24rem] overflow-auto rounded-md bg-surface-3 p-4 font-mono text-xs leading-relaxed text-secondary-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  {manifest}
                </pre>
              ) : (
                <QuietNote
                  title="The preview appears when the form is valid."
                  className="max-w-[72ch]"
                >
                  Take this definition to a CRD — the same server-side expand and
                  apply the config builder performs. Press{" "}
                  <span className="font-medium">Preview CRD</span> to read the
                  exact manifest; nothing reaches the cluster until you apply it.
                </QuietNote>
              )}
              {exp.kind === "error" && exp.forbidden ? (
                <ForbiddenInline
                  title="Not allowed to apply"
                  description="Your account can preview the manifest but can't create AgentDeployments in this cluster."
                  detail={exp.message}
                />
              ) : (
                exp.kind === "error" && (
                  <p className="font-mono text-xs text-destructive" role="alert">
                    {exp.message}
                    {exp.status ? ` (${exp.status})` : ""}
                  </p>
                )
              )}
              <div className="flex flex-wrap items-center gap-3">
                <Button onClick={onPreview} disabled={exporting} variant="outline">
                  {exp.kind === "previewing" ? "Expanding…" : "Preview CRD"}
                </Button>
                {/* Apply is a WRITE affordance — hidden for a viewer (Preview
                    stays available so they can still inspect the CRD). */}
                {canRun && (
                  <Button onClick={onApply} disabled={exp.kind !== "preview"}>
                    <Rocket className="h-4 w-4" />
                    {exp.kind === "applying" ? "Applying…" : "Apply to cluster"}
                  </Button>
                )}
              </div>
            </>
          )}
        </CardContent>
      </Card>

      {/* A chat turn's trace opens over the page, so the conversation is still
          there when it closes (the agent page does the same). */}
      <DetailDrawer
        open={inspectTrace !== null}
        onClose={() => setInspectTrace(null)}
        title="Run inspector"
        subtitle={inspectTrace ?? undefined}
        size="lg"
      >
        {inspectTrace && <RunInspector traceId={inspectTrace} />}
      </DetailDrawer>
    </div>
  );
}

// WorkflowGraphSection renders the per-node status panel for a workflow instance run (m67.15).
//
// The hues here follow §2.2 exactly, and two of them moved in M151:
//   pending  muted        declared, not in motion, not a problem
//   running  progressing  the machine converging on its own (was `secondary`,
//                         which said "idle" about a node doing work)
//   done     ok           it completed
//   failed   crit         it will not proceed
//
// The suspended tag is `progressing`, NOT warn and NOT hold: a suspended
// workflow instance is parked on a CHILD RUN and is machine-woken (ADR 0060 §3
// / internal/run/run.go). See the ruling in this file's header. Its tag reads
// one word so it stays inside the ≤16-character tag budget (§4.5); the sentence
// that explains it is the run panel's ClosingNote.
type NodeBadgeVariant = BadgeProps["variant"];
const NODE_STATUS_VARIANT: Record<WorkflowNodeStatus["status"], NodeBadgeVariant> = {
  pending: "muted",
  running: "progressing",
  done: "ok",
  failed: "crit",
};
const NODE_STATUS_LABEL: Record<WorkflowNodeStatus["status"], string> = {
  pending: "pending",
  running: "running",
  done: "done",
  failed: "failed",
};

function WorkflowGraphSection({
  nodes,
  currentNode,
  runStatus,
}: {
  nodes: WorkflowNodeStatus[];
  currentNode?: string;
  runStatus: string;
}) {
  return (
    <Card className="min-w-0" data-testid="workflow-graph-section">
      <PanelHeader title="Workflow" meta={`${nodes.length} nodes`}>
        {runStatus === "waiting" && (
          <Badge variant="progressing" data-testid="workflow-suspended-badge">
            Suspended
          </Badge>
        )}
      </PanelHeader>
      <CardContent>
        <ol className="space-y-2" data-testid="workflow-node-list">
          {nodes.map((node) => {
            const isCurrent = node.name === currentNode;
            return (
              <li
                key={node.name}
                data-testid={`workflow-node-${node.name}`}
                className={
                  // Selection is pine-family, never a status hue (§2.3).
                  isCurrent
                    ? "flex items-center justify-between gap-3 rounded-md border border-primary bg-accent px-3 py-2 text-sm"
                    : "flex items-center justify-between gap-3 rounded-md border border-border bg-surface-2 px-3 py-2 text-sm"
                }
              >
                <div className="flex min-w-0 items-center gap-2">
                  {isCurrent && (
                    <span
                      className="h-2 w-2 shrink-0 rounded-full bg-primary"
                      aria-label="current node"
                      data-testid={`workflow-node-current-${node.name}`}
                    />
                  )}
                  <span
                    className={
                      isCurrent
                        ? "truncate font-medium"
                        : "truncate font-medium text-secondary-foreground"
                    }
                  >
                    {node.name}
                  </span>
                  {node.agent && (
                    <span className="truncate font-mono text-xs text-faint">
                      {node.agent}
                    </span>
                  )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Badge
                    variant={NODE_STATUS_VARIANT[node.status]}
                    data-testid={`workflow-node-status-${node.name}`}
                  >
                    {NODE_STATUS_LABEL[node.status]}
                  </Badge>
                  {node.childRunId && (
                    <Link
                      to={`/traces/${encodeURIComponent(node.childRunId)}`}
                      data-testid={`workflow-node-run-link-${node.name}`}
                      className="truncate border-b border-accent font-mono text-xs text-primary hover:border-primary"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {node.childRunId.slice(0, 8)}…
                    </Link>
                  )}
                </div>
              </li>
            );
          })}
        </ol>
      </CardContent>
    </Card>
  );
}

// finalizedRun projects a finalized RunDetail (from the shared durable engine, ADR 0093) into
// this page's richer `Run` union — the traceId + response + consent/approval affordances +
// workflow nodes the run panel renders. A failed run maps to the error state (no trace
// panel). This is exactly the projection the page's old inline finalizeRun did, now fed by the
// hook's onFinalized callback. pause is what the run waits on, its own or a delegated sub-run's;
// an approval names the run that asked, which is the one approve and deny resolve.
function finalizedRun(detail: RunDetail, pause?: RunPause): Run {
  if (detail.status === "failed") {
    return { kind: "error", message: detail.error || "The run failed." };
  }
  const ra = pause?.action;
  const lastMessage = detail.messages?.length
    ? detail.messages[detail.messages.length - 1].content
    : "";
  // Derive failed node: when the run failed, the node at currentNode is the failed one. The BFF
  // exposes nodes with their stored status; we overlay "failed" for the current node when the run
  // itself is failed (UI-side derivation per m67.15).
  const nodes = detail.nodes?.map((n) => {
    if (detail.status === "failed" && n.name === detail.currentNode && n.status !== "done") {
      return { ...n, status: "failed" as const };
    }
    return n;
  });
  return {
    kind: "done",
    runId: detail.id,
    traceId: detail.traceId ?? "",
    response: lastMessage,
    runStatus: detail.status,
    consentRequired: ra?.kind === "consent_required" ? ra.servers : undefined,
    approval:
      ra?.kind === "approval"
        ? { runId: pause?.runId ?? detail.id, key: ra.key ?? "", summary: ra.message ?? "" }
        : undefined,
    took: runDuration(detail),
    workflowRef: detail.workflowRef,
    currentNode: detail.currentNode,
    nodes,
  };
}

// errorRun / errorExport map a thrown error to the respective error state,
// preserving the HTTP status when it is an ApiError so the user sees the real
// reason (e.g. a 403 RBAC denial, a 409 not-ready).
function errorRun(err: unknown): Run {
  if (err instanceof ApiError) {
    return { kind: "error", message: err.message, status: err.status, forbidden: err.isForbidden };
  }
  return { kind: "error", message: err instanceof Error ? err.message : "request failed" };
}

function errorExport(err: unknown): Export {
  if (err instanceof ApiError) {
    return { kind: "error", message: err.message, status: err.status, forbidden: err.isForbidden };
  }
  return { kind: "error", message: err instanceof Error ? err.message : "request failed" };
}
