import * as React from "react";
import { Copy, Check, Bell, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useFocusTrap } from "@/components/kit/use-focus-trap";

// NewAlertPolicyDialog — the "New alert policy" create path (M144.7).
//
// AlertPolicy has no dedicated BFF create endpoint yet (unlike agents, which the
// config-builder POSTs). Rather than a dead-end ("AlertPolicy rules fire…" with
// no way to make one), this gives operators an honest, actionable path: a
// ready-to-edit AlertPolicy manifest seeded with the current namespace, a Copy
// button, and the one-liner to apply it. This is how kubectl-native platforms
// expose less-common CRDs; a form-driven POST endpoint is a tracked follow-up.
//
// The template is intentionally minimal-but-complete: one errorRate condition +
// a webhook route, with the condition-type enum and threshold semantics inline as
// comments so an operator can adapt it without leaving the dialog.

function templateFor(namespace: string): string {
  const ns = namespace || "<namespace>";
  return `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AlertPolicy
metadata:
  name: high-error-rate
  namespace: ${ns}
spec:
  # Which agents this policy watches. Empty selector = every agent in the namespace.
  selector: {}
  # names: [my-agent]        # or watch specific AgentDeployments by name
  conditions:
    - name: errors-over-5pct
      # type: errorRate | p95Latency | budgetSoft | forecastExceeded |
      #       regressionDetected | runFailureRate | approvalWaiting
      type: errorRate
      threshold: "0.05"      # errorRate: 5xx fraction 0..1  (0.05 = 5%)
      window: 5m             # look-back window
  route:
    channels:
      - type: webhook
        webhook:
          url: https://example.com/alerts
          # secretRef: alert-webhook-token   # optional bearer-token Secret
`;
}

export interface NewAlertPolicyDialogProps {
  open: boolean;
  onClose: () => void;
  namespace: string;
}

export function NewAlertPolicyDialog({ open, onClose, namespace }: NewAlertPolicyDialogProps) {
  const [yaml, setYaml] = React.useState(() => templateFor(namespace));
  const [copied, setCopied] = React.useState(false);
  // Focus-trap + Escape-to-close, matching the other console dialogs.
  const containerRef = useFocusTrap<HTMLDivElement>({ active: open, onEscape: onClose });

  // Re-seed the namespace whenever the dialog is (re)opened.
  React.useEffect(() => {
    if (open) setYaml(templateFor(namespace));
  }, [open, namespace]);

  if (!open) return null;

  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard blocked (insecure context / permissions) — the textarea is
      // still selectable, so this is a non-fatal best-effort.
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-label="New alert policy"
      data-testid="new-alert-policy-dialog"
    >
      <button
        type="button"
        aria-label="Close"
        className="absolute inset-0 cursor-default bg-black/50"
        onClick={onClose}
      />
      <div
        ref={containerRef}
        className="relative z-10 flex max-h-[85vh] w-full max-w-2xl flex-col overflow-hidden rounded-lg border bg-card shadow-lg"
      >
        <div className="flex items-start justify-between gap-4 border-b px-5 py-4">
          <div className="flex items-start gap-2">
            <Bell className="mt-0.5 h-4 w-4 text-muted-foreground" />
            <div>
              <h3 className="text-base font-semibold">New alert policy</h3>
              <p className="text-sm text-muted-foreground">
                Edit this manifest and apply it to the cluster. Alerts fire when any condition
                breaches its threshold.
              </p>
            </div>
          </div>
          <button
            type="button"
            aria-label="Close"
            onClick={onClose}
            className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-5 py-4">
          <label className="block text-xs font-medium text-muted-foreground" htmlFor="alertpolicy-yaml">
            AlertPolicy manifest
          </label>
          <textarea
            id="alertpolicy-yaml"
            spellCheck={false}
            value={yaml}
            onChange={(e) => setYaml(e.target.value)}
            className="h-72 w-full resize-y rounded border bg-surface-3 p-3 font-mono text-xs leading-relaxed focus:outline-none focus:ring-2 focus:ring-ring"
            data-testid="new-alert-policy-yaml"
          />
          <div className="rounded border bg-surface-3 p-3">
            <p className="mb-1 text-xs font-medium text-muted-foreground">Apply it</p>
            <code className="block break-all font-mono text-xs">
              kubectl apply -f alertpolicy.yaml
            </code>
            <p className="mt-2 text-xs text-muted-foreground">
              Or pipe the copied manifest straight in:{" "}
              <code className="font-mono">pbpaste | kubectl apply -f -</code>
            </p>
          </div>
        </div>

        <div className="flex items-center justify-end gap-2 border-t px-5 py-4">
          <Button variant="ghost" size="sm" onClick={onClose}>
            Close
          </Button>
          <Button
            size="sm"
            onClick={() => copy(yaml)}
            data-testid="new-alert-policy-copy"
          >
            {copied ? <Check className="mr-2 h-4 w-4" /> : <Copy className="mr-2 h-4 w-4" />}
            {copied ? "Copied" : "Copy manifest"}
          </Button>
        </div>
      </div>
    </div>
  );
}
