import { Badge } from "@/components/ui/badge";

// StatusBadge — the ONE health-status chip across the console (M99 E1). Every resource list rendered a
// near-identical `<Badge variant={ready ? "success" : "warning"}>{phase || (ready ? "Ready" : "Pending")}</Badge>`
// but with DIVERGENT lexicons ("Ready" on agents/registries/routes vs "valid" on workflows/guardrails vs a
// lowercase "ready" on teams) — the audit's single biggest consistency defect. This unifies the vocabulary,
// casing, and color in one place: a healthy resource shows its `phase` (Title-cased upstream) or "Ready"
// (success green); a not-yet-healthy one shows its reason or "Pending" (warning amber). Callers pass the
// resource's `ready` boolean + optional `phase`/reason string.
export function StatusBadge({
  ready,
  phase,
  className,
}: {
  ready: boolean;
  phase?: string;
  className?: string;
}) {
  const label = (phase ?? "").trim() || (ready ? "Ready" : "Pending");
  return (
    <Badge variant={ready ? "success" : "warning"} className={className}>
      {label}
    </Badge>
  );
}
