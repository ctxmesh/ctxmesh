// Shared formatting helpers for the observability surfaces (dashboard, runs, cost).
// Kept dependency-free and locale-aware; every surface formats identically.

// formatUSD renders a USD cost. Sub-cent amounts (typical per-run LLM spend) keep
// enough significant digits to be meaningful ($0.0003), whole-dollar amounts round to
// cents ($12.40). Exactly 0 renders "$0.00" (a real zero, not "—").
export function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return "$0.00";
  if (n === 0) return "$0.00";
  if (n >= 1) return `$${n.toFixed(2)}`;
  if (n >= 0.01) return `$${n.toFixed(3)}`;
  // Sub-cent: show up to 6 decimals, trimming trailing zeros so $0.000297 reads clean.
  return `$${n.toFixed(6).replace(/0+$/, "").replace(/\.$/, "")}`;
}

// formatTokens renders an LLM token count, distinguishing "not captured" from a real zero (M99 A1).
// The Langfuse traces-LIST API doesn't carry per-trace token usage, so a runs/trace row's tokens are 0
// whenever they weren't captured — showing a bare "0" reads as "used no tokens", which is false for any
// real LLM call. Render "—" for 0/non-finite (not captured) and the locale count otherwise. Real token
// capture (per-trace enrichment) is carded — see m52.
export function formatTokens(n: number): string {
  return Number.isFinite(n) && n > 0 ? n.toLocaleString() : "—";
}

// formatCompact renders a large count compactly: 426986 → "427K", 1_200_000 → "1.2M".
// Pinned to en-US so the suffix is the globally-legible K/M/B — NOT the locale's numbering
// (e.g. en-IN would render "4.3L" lakhs), which reads as a bug on a platform dashboard.
export function formatCompact(n: number): string {
  if (!Number.isFinite(n)) return "0";
  return new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n);
}

// formatLatency renders a millisecond latency in the most readable unit: sub-second as
// "450ms", seconds as "1.4s", minutes as "1m 5s". A non-positive value renders "—"
// (unknown), never a misleading "0ms".
export function formatLatency(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${Math.round(s - m * 60)}s`;
}

// formatRelativeTime renders an ISO timestamp as a short "time ago": "just now", "5m ago",
// "3h ago", "2d ago", falling back to an absolute date beyond a week. Returns "" for an
// empty/unparseable input so callers can omit it.
export function formatRelativeTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const secs = Math.round((Date.now() - t) / 1000);
  if (secs < 0) return "just now";
  if (secs < 45) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  const days = Math.round(hrs / 24);
  if (days < 7) return `${days}d ago`;
  return new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

// formatDateTime renders an ISO timestamp as an absolute local date-time (for the
// title/tooltip on a relative-time label). Returns "" for an unparseable input.
export function formatDateTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  return new Date(t).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// shortTraceId truncates a trace id for compact display, keeping the leading chars that
// make it recognizable/searchable. The full id stays available via a title attribute.
export function shortTraceId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 12)}…` : id;
}

// RunLatencyStats is a small aggregate over a set of run latencies (ms).
export interface RunLatencyStats {
  count: number;
  avgMs: number;
  p95Ms: number;
}

// latencyStats computes count + average + p95 latency (ms) over runs, ignoring
// non-positive latencies (unknown). Empty/all-unknown → zeros. p95 uses
// nearest-rank on the sorted positive latencies.
export function latencyStats(latenciesMs: number[]): RunLatencyStats {
  const vals = latenciesMs.filter((v) => Number.isFinite(v) && v > 0).sort((a, b) => a - b);
  if (vals.length === 0) return { count: 0, avgMs: 0, p95Ms: 0 };
  const sum = vals.reduce((a, b) => a + b, 0);
  const rank = Math.min(vals.length - 1, Math.ceil(0.95 * vals.length) - 1);
  return { count: vals.length, avgMs: sum / vals.length, p95Ms: vals[Math.max(0, rank)] };
}
