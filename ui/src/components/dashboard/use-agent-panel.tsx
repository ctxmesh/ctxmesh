import * as React from "react";
import { Terminal } from "lucide-react";

// UseAgentPanel — the "easy running" payoff (M22 / U6; PROD-1). After an agent is
// deployed, tell the user HOW to call it, adapting to the execution model:
//   • serving  → the /invoke endpoint + a copy-paste curl AND a ctxmesh SDK snippet
//                (create + follow a run like the console — the door for code callers)
//   • eventing → trigger it by emitting a CloudEvent to the registry broker,
//                filtered on `type: <agentName>` (the m7.5 Trigger contract)
//   • job      → one-shot; read its result from the runs/logs
// It never goes quiet the moment the agent is Ready. The request body is {"input": ...}
// — the actual /invoke contract (serve.py / entrypoint read body["input"]).

function CodeBlock({ text, testId }: { text: string; testId?: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <div className="relative">
      <pre
        className="overflow-x-auto rounded-md bg-surface-3 p-3 text-xs leading-relaxed"
        data-testid={testId}
      >
        {text}
      </pre>
      <button
        type="button"
        aria-label={copied ? "copied to clipboard" : "copy to clipboard"}
        aria-live="polite"
        className="absolute right-2 top-2 rounded border bg-card px-2 py-0.5 text-[11px] text-muted-foreground hover:text-foreground"
        onClick={() => {
          void navigator.clipboard?.writeText(text).then(
            () => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            },
            () => {},
          );
        }}
      >
        {copied ? "copied" : "copy"}
      </button>
    </div>
  );
}

export function UseAgentPanel({
  name,
  executionModel,
  url,
  ns,
}: {
  name: string;
  executionModel: string;
  url: string;
  ns?: string;
}) {
  const model = executionModel || "serving";
  // The console/BFF origin the run API (ADR 0011, caller-authenticated) is served from —
  // the base_url for the ctxmesh RunsClient. window may be undefined under SSR/jsdom.
  const origin =
    typeof window !== "undefined" && window.location ? window.location.origin : "https://console.example";

  return (
    <div
      className="rounded-lg border bg-card p-4 shadow-card"
      data-testid="use-agent-panel"
    >
      <div className="mb-3 flex items-center gap-2">
        <Terminal className="h-4 w-4 text-primary" />
        <p className="text-sm font-medium">Use this agent</p>
        <span className="rounded bg-surface-3 px-1.5 py-0.5 text-[11px] text-muted-foreground">
          {model}
        </span>
      </div>

      {model === "eventing" ? (
        <div className="space-y-2 text-xs text-muted-foreground">
          <p>
            This agent is <span className="font-medium">event-triggered</span>.
            Emit a CloudEvent to its registry broker
            (<code>&lt;registry&gt;-broker</code> in{" "}
            <code>this namespace</code>) with the context attribute{" "}
            <code>type: {name}</code> — the agent&rsquo;s Trigger subscribes
            exactly that type.
          </p>
          <CodeBlock
            testId="use-agent-eventing"
            text={`curl -X POST http://<registry>-broker.<namespace>.svc.cluster.local \\
  -H "Ce-Id: $(uuidgen)" \\
  -H "Ce-Specversion: 1.0" \\
  -H "Ce-Type: ${name}" \\
  -H "Ce-Source: my-caller" \\
  -H "Content-Type: application/json" \\
  -d '{"input": "Hello, agent"}'`}
          />
          <p>
            The broker is created per registry. Async replies + oversized
            payloads follow the eventing contract (DLQ + blob offload).
          </p>
        </div>
      ) : model === "job" ? (
        <div className="space-y-2 text-xs text-muted-foreground">
          <p>
            This agent runs as a <span className="font-medium">one-shot job</span>
            — it executes to completion rather than serving traffic. Trigger a run
            and read its output from the <span className="font-medium">Runs</span>{" "}
            and <span className="font-medium">Logs</span> tabs above (or via{" "}
            <code>kubectl logs</code> on the job pod).
          </p>
        </div>
      ) : (
        <div className="space-y-2 text-xs text-muted-foreground">
          <p>
            This agent <span className="font-medium">serves requests</span>. POST
            your input to its endpoint:
          </p>
          {url ? (
            <CodeBlock
              testId="use-agent-serving"
              text={`curl -X POST ${url}/invoke \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"input": "Hello, agent"}'`}
            />
          ) : (
            <p>The endpoint appears once the agent is Ready.</p>
          )}
          <p>
            This is the agent&rsquo;s in-cluster address (reachable from workloads
            in the cluster); external access depends on your ingress. Or use the{" "}
            <span className="font-medium">Run</span> panel to try it from here.
          </p>
          <p className="pt-1">
            Or drive it from code with the{" "}
            <code>ctxmesh</code> SDK — create + follow a run like the console does
            (caller-authenticated, so it works from anywhere):
          </p>
          <CodeBlock
            testId="use-agent-sdk"
            text={`pip install ctxmesh

from ctxmesh import RunsClient

runs = RunsClient("${origin}", token="$TOKEN")
run = runs.run(agent="${name}",${ns ? ` namespace="${ns}",` : ""} input={"input": "Hello, agent"})
print(run.messages[-1]["content"])   # the final answer`}
          />
        </div>
      )}
    </div>
  );
}
