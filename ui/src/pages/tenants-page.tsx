import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Building2, Filter } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  formatMoney,
  CellEntity,
  ClosingNote,
  DataTable,
  FilterChipRow,
  KeyValueList,
  Meter,
  NextStepLink,
  PageHeader,
  QuantityValue,
  QuietNote,
  SectionHeader,
  StatusBadge,
  UNKNOWN,
  nextStepRank,
  type Column,
  type DataTableError,
  type EmptyStateProps,
  type FilterChip,
  type NextStepTone,
  type Quantity,
} from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { RES_TENANTS } from "@/lib/nav";
import {
  api,
  ApiError,
  type TenantSummary,
  type TenantDetail,
  type TenantModelDTO,
  type TenantUsage,
  type TenantCreateRequest,
} from "@/lib/api";

// TenantsPage — the multi-tenancy admin surface (M47, ADR 0046; re-housed on
// the editorial system in M151, spec §6.2: "near-cap rows get warn Tags + Meter
// cells").
//
// A Tenant is a cluster-scoped grouping of namespaces with compute and model
// quotas. The model budget is enforced for real: over the cap, the launcher
// fails the next model call closed with a typed budget_exceeded (402). That is
// what makes this page an operations surface rather than a config listing — the
// question it answers is "who is about to be throttled?".
//
// ── AMBER MEANS A BOUND IS NEAR OR CROSSED (§2.2), AND HERE IT LITERALLY IS ─
// A tenant at ≥80% of any cap wears the amber `warn` Tag and its Meter fill
// turns amber at the same tick. Over the cap it goes crit — not because crit is
// "worse amber", but because §2.2's crit is precisely "will not proceed without
// a change", and an over-budget tenant's next model call is refused. Those are
// the only two hues on this page beyond the readiness chip; pine stays what it
// is everywhere else — a thing you can press.
//
// ── NO CAP ⇒ NO BAR (§5.24) ────────────────────────────────────────────────
// A tenant without a budget has no denominator, so its cell shows the spend
// figure alone. A tenant whose live usage is unreadable (no state layer, or a
// viewer's 403) shows the honest dash, never a zero — "nothing measured" and
// "measured nothing" are different claims and this page makes both, separately.
// The QuietNote above the table says which case the reader is looking at.
//
// RBAC-aware: the whole surface degrades to the DataTable's forbidden state on
// a 403 (viewers and developers read; operators manage — enforced at the API
// server, ADR 0011). No end-user PII: a Tenant is config only.

// NEAR_CAP_RATIO is the fraction of any cap at which a tenant is flagged "near
// cap" (m54.5) — the at-a-glance "who's about to be throttled?", and the tick
// the Meter draws.
const NEAR_CAP_RATIO = 0.8;

type CapLevel = "over" | "near" | null;

// nearCapLevel classifies a tenant against its caps + live usage: "over" (≥100%
// of any cap), "near" (≥80%), or null (comfortable / no caps / no usage yet).
function nearCapLevel(
  model: TenantModelDTO | undefined,
  u: TenantUsage | undefined,
): CapLevel {
  if (!model || !u) return null;
  const ratios: number[] = [];
  const budget = capOf(model.budgetUSD);
  if (budget !== null) ratios.push(u.spendUSD / budget);
  if (model.rpm) ratios.push(u.rpm / model.rpm);
  if (model.maxConcurrent) ratios.push(u.inFlight / model.maxConcurrent);
  if (ratios.length === 0) return null;
  const max = Math.max(...ratios);
  if (max >= 1) return "over";
  if (max >= NEAR_CAP_RATIO) return "near";
  return null;
}

/**
 * The budget cap as a usable number, or null when there is none.
 *
 * The CRD stores it as a decimal STRING for exact money ("100.00"), so this is
 * the one place it becomes a float — and an unparseable or non-positive value
 * is treated as "no cap", never as a bound of zero.
 */
function capOf(budgetUSD?: string): number | null {
  if (!budgetUSD) return null;
  const n = Number.parseFloat(budgetUSD);
  return Number.isFinite(n) && n > 0 ? n : null;
}

/**
 * Money in the §4.5 register: `< $1` keeps 4 decimals, `≥ $1` keeps 2. A
 * MEASURED zero renders `$0.00` — the four decimals exist to preserve
 * significant digits a sub-dollar amount would lose, and a zero has none;
 * `$0.0000` is also the exact string §7.1 reserves as "must never be mistaken
 * for an unknown". An unknown renders `—`, never a figure.
 *
 * Duplicated from cost-page rather than shared through `lib/format.ts`: that
 * module's `formatUSD` is the older 3/6-decimal register still used by surfaces
 * outside this page, and converging them is its own change.
 */


type Load =
  | { kind: "loading" }
  | { kind: "ready"; items: TenantSummary[] }
  | { kind: "error"; message: string; forbidden: boolean };

// The inline detail panel's own load state, so a row-click gives immediate
// feedback and a failed fetch is surfaced (never silently opens nothing).
type Detail =
  | { kind: "none" }
  | { kind: "loading"; name: string }
  | { kind: "ready"; detail: TenantDetail }
  | { kind: "error"; name: string; message: string };

// ── Triage ──────────────────────────────────────────────────────────────────

interface NextStep {
  /** Verb-first, ≤22 chars, no trailing arrow (§7.2). Absent when tone is "none". */
  label?: string;
  tone: NextStepTone;
}

interface Triaged {
  t: TenantSummary;
  level: CapLevel;
  next: NextStep;
}

/**
 * One tenant → (cap level, next step).
 *
 * Order matters and it is money first: a tenant over its cap is already having
 * calls refused, which outranks a tenant that has not finished reconciling.
 * Every step opens this page's own detail panel, which is where the caps, the
 * live usage, and any namespace conflict actually are — a link that goes
 * somewhere real, not a label that describes a state.
 */
function triage(t: TenantSummary, usage?: TenantUsage): Triaged {
  const level = nearCapLevel(t.model, usage);
  if (level === "over") {
    return { t, level, next: { label: "Raise the cap", tone: "crit" } };
  }
  if (!t.ready) {
    // Deliberately NOT crit: the list carries only `ready`, and a tenant that
    // has not reconciled may simply be new. Crit would claim a failure this
    // page cannot see — the readiness chip already says "Pending", and §5.19
    // reserves the crit link for a target that IS a failure or a stop.
    return { t, level, next: { label: "Check the tenant", tone: "default" } };
  }
  if (level === "near") {
    return { t, level, next: { label: "Review the cap", tone: "default" } };
  }
  return { t, level, next: { tone: "none" } };
}

// ── The chip views (§5.28): one question, one answer at a time ──────────────

type ViewId = "all" | "attention" | "pressure";

const VIEWS: { id: ViewId; label: string; match: (t: Triaged) => boolean }[] = [
  { id: "all", label: "Everything", match: () => true },
  { id: "attention", label: "Needs you", match: (t) => t.next.tone !== "none" },
  { id: "pressure", label: "Near or over cap", match: (t) => t.level !== null },
];

const VIEW_EMPTY: Record<Exclude<ViewId, "all">, { title: string; description: string }> = {
  attention: {
    title: "Nothing needs a person",
    description:
      "Every tenant is reconciled and comfortably inside its caps. Show everything to see them all.",
  },
  pressure: {
    title: "No tenant is near a cap",
    description:
      "Nothing is within 80% of a budget, rate, or concurrency ceiling. Show everything to see the full list.",
  },
};

/**
 * The §5.18 closing line: the honest ratio, in words, restating what the table
 * already showed. /api/tenants returns every tenant in one response, so these
 * counts are the whole truth rather than a page of it.
 */
export function closingLine(rows: Triaged[], usageKnown: boolean): string | null {
  const total = rows.length;
  if (total === 0) return null;
  const over = rows.filter((r) => r.level === "over").length;
  const near = rows.filter((r) => r.level === "near").length;
  const settling = rows.filter((r) => !r.t.ready).length;

  if (!usageKnown) {
    return total === 1
      ? "One tenant. How close it is to its caps is not something this install records."
      : `${total} tenants. How close they are to their caps is not something this install records.`;
  }
  if (over === 0 && near === 0 && settling === 0) {
    return total === 1
      ? "One tenant, comfortably inside every cap it has."
      : `All ${total} tenants are inside their caps and need nothing from you.`;
  }
  const parts: string[] = [];
  if (over > 0) parts.push(`${over} ${over === 1 ? "is" : "are"} over a cap`);
  if (near > 0) parts.push(`${near} ${near === 1 ? "is" : "are"} within 80% of one`);
  if (settling > 0)
    parts.push(`${settling} ${settling === 1 ? "has" : "have"} not reconciled yet`);
  const quiet = rows.filter((r) => r.level === null && r.t.ready).length;
  const tail =
    quiet === 0
      ? ""
      : ` The other ${quiet} ${quiet === 1 ? "is" : "are"} inside every cap.`;
  return `Of ${total} tenants, ${parts.join(", ")}.${tail}`;
}

// ── The list's usage cell ───────────────────────────────────────────────────

/**
 * Spend against the tenant's budget, as a bar when there is a bound to draw it
 * against and as a bare figure when there is not (§5.24's "no cap ⇒ no bar").
 *
 * The Meter's own no-cap and no-usage branches emit an explanatory paragraph,
 * which is right in a rail and wrong in a 44px table row — so this cell decides
 * up front and the reason is stated ONCE in the note above the table instead of
 * once per row.
 */
function SpendCell({
  model,
  usage,
}: {
  model?: TenantModelDTO;
  usage?: TenantUsage;
}) {
  const cap = capOf(model?.budgetUSD);
  const spend: Quantity = usage ? usage.spendUSD : UNKNOWN;

  if (cap === null || !usage) {
    return (
      <QuantityValue
        value={spend}
        format={formatMoney}
        title="Live usage isn’t recorded for this install — unknown, not zero."
        className="block text-right"
      />
    );
  }
  return (
    <Meter
      // Just the bound's name (§5.24: the key names the bound). Qualifying it
      // with the tenant repeats the first column AND widens the cell enough to
      // push the Next step column off the table's own scroll frame at 1280 —
      // and that column is the one §4.4 says is never dropped.
      label="budget"
      used={usage.spendUSD}
      cap={cap}
      threshold={cap * NEAR_CAP_RATIO}
      format={formatMoney}
      foot={null}
    />
  );
}

// ── The detail panel ────────────────────────────────────────────────────────

function computeSummary(quota: TenantDetail["quota"]): string | null {
  if (!quota) return null;
  const parts = [
    quota.cpu && `cpu ${quota.cpu}`,
    quota.memory && `mem ${quota.memory}`,
    quota.pods ? `pods ${quota.pods}` : undefined,
  ].filter(Boolean);
  return parts.length ? parts.join(" · ") : null;
}

// modelSummary is the tenant's DECLARED model caps, in words. Kept as prose (not
// a meter) because it describes the ceiling itself, not a position against it.
function modelSummary(model?: TenantModelDTO): string | null {
  if (!model) return null;
  const cap = capOf(model.budgetUSD);
  const parts = [
    cap !== null ? `${formatMoney(cap)} budget` : undefined,
    model.rpm ? `${model.rpm} req/min` : undefined,
    model.maxConcurrent ? `${model.maxConcurrent} concurrent` : undefined,
  ].filter(Boolean);
  return parts.length ? parts.join(" · ") : null;
}

function TenantDetailPanel({
  tenant,
  onClose,
}: {
  tenant: TenantDetail;
  onClose: () => void;
}) {
  const conflict = tenant.conditions.find(
    (c) => c.type === "NamespaceConflict" && c.status === "True",
  );
  const [usage, setUsage] = useState<TenantUsage | null>(null);
  useEffect(() => {
    let live = true;
    api
      .tenantUsage(tenant.name)
      .then((u) => live && setUsage(u))
      // 501 (no state layer) or 403 → no usage to draw. The panel says so
      // rather than drawing an empty bar, which would read as "spent nothing".
      .catch(() => live && setUsage(null));
    return () => {
      live = false;
    };
  }, [tenant.name]);

  const cap = capOf(tenant.model?.budgetUSD);

  return (
    <section
      className="rounded-lg border border-border bg-card p-5"
      data-testid="tenant-detail"
      aria-label={`Tenant ${tenant.name}`}
    >
      <SectionHeader
        title={tenant.name}
        lede="Its member namespaces, the ceilings it enforces, and where its live usage sits against them."
        actions={
          <Button variant="ghost" size="sm" onClick={onClose} data-testid="tenant-detail-close">
            Close
          </Button>
        }
      />

      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_18rem]">
        <div className="min-w-0 space-y-4">
          <div>
            <p className="font-mono text-xs uppercase tracking-wide text-faint">
              Member namespaces
            </p>
            <div className="mt-1.5 flex flex-wrap gap-1" data-testid="tenant-namespaces">
              {tenant.namespaces.length === 0 ? (
                <span className="text-sm text-faint">
                  None yet — this tenant claims no namespace.
                </span>
              ) : (
                tenant.namespaces.map((ns) => (
                  <Badge key={ns} variant="muted">
                    {ns}
                  </Badge>
                ))
              )}
            </div>
          </div>

          <KeyValueList
            items={[
              {
                key: "Compute quota",
                value: computeSummary(tenant.quota),
                absent: "no compute quota set",
              },
              {
                key: "Model quota",
                value: modelSummary(tenant.model),
                absent: "no model quota set",
              },
            ]}
          />
        </div>

        {/* The rail: meters only (§4.7). Rendered only when there is live usage
            to place against the caps — an absent state layer gets a sentence,
            not three empty bars. */}
        <div className="min-w-0 space-y-4">
          {usage ? (
            <div className="space-y-4" data-testid="tenant-usage">
              <p className="font-mono text-xs uppercase tracking-wide text-faint">
                Live usage (this month)
              </p>
              <Meter
                label="spend"
                used={usage.spendUSD}
                cap={cap ?? UNKNOWN}
                threshold={cap !== null ? cap * NEAR_CAP_RATIO : undefined}
                format={formatMoney}
                thing="tenant"
                foot={null}
              />
              <Meter
                label="req/min"
                used={usage.rpm}
                cap={tenant.model?.rpm ?? UNKNOWN}
                threshold={
                  tenant.model?.rpm ? tenant.model.rpm * NEAR_CAP_RATIO : undefined
                }
                thing="tenant"
                foot={null}
              />
              <Meter
                label="in-flight"
                used={usage.inFlight}
                cap={tenant.model?.maxConcurrent ?? UNKNOWN}
                threshold={
                  tenant.model?.maxConcurrent
                    ? tenant.model.maxConcurrent * NEAR_CAP_RATIO
                    : undefined
                }
                thing="tenant"
                foot={null}
              />
              <p className="text-sm text-faint">
                The tick on each bar is 80% of the cap — where this console calls
                a tenant near cap. Over the cap, the next model call is refused.
              </p>
            </div>
          ) : (
            <QuietNote title="Live usage isn’t recorded for this install.">
              Spend, request rate, and in-flight counts come from the shared
              usage accumulator, and this platform has none wired up. The caps on
              the left are real; where this tenant sits against them is simply
              absent — not zero.
            </QuietNote>
          )}
        </div>
      </div>

      {conflict && (
        <div
          className="mt-5 rounded-md border border-destructive bg-card p-3"
          data-testid="tenant-conflict"
        >
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="crit">Namespace conflict</Badge>
            <p className="min-w-0 text-sm text-destructive">{conflict.message}</p>
          </div>
          <p className="mt-2 text-sm text-secondary-foreground">
            A namespace belongs to at most one tenant. An operator must remove
            the contested namespace from this tenant&rsquo;s{" "}
            <code className="font-mono text-xs">spec.namespaces</code> (or from
            the tenant that already owns it) — then this tenant re-reconciles
            clean.
          </p>
        </div>
      )}
    </section>
  );
}

// CreateTenantPanel is the minimal admin create form (M99 C4): a name + member
// namespaces (comma/space separated) + a network-isolation toggle
// (secure-by-default). Compute/model/storage quotas are set elsewhere (kubectl,
// or a future edit surface). On success it calls onCreated(name) to refresh.
function CreateTenantPanel({
  onCreated,
  onClose,
}: {
  onCreated: (name: string) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  const [namespaces, setNamespaces] = useState("");
  const [isolate, setIsolate] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit() {
    const nm = name.trim();
    if (!nm) {
      setError("Name is required.");
      return;
    }
    setBusy(true);
    setError(null);
    const req: TenantCreateRequest = {
      name: nm,
      namespaces: namespaces
        .split(/[\s,]+/)
        .map((s) => s.trim())
        .filter(Boolean),
      networkIsolation: isolate,
    };
    try {
      await api.createTenant(req);
      onCreated(nm);
    } catch (e) {
      setError(
        e instanceof ApiError && e.isForbidden
          ? "You don't have permission to create tenants — ask an admin for access."
          : e instanceof Error
            ? e.message
            : "Create failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <section
      className="space-y-4 rounded-lg border border-border bg-card p-5"
      data-testid="create-tenant-panel"
    >
      <SectionHeader
        title="New tenant"
        lede="A name and the namespaces it claims. Quotas are added afterwards."
        actions={
          <Button variant="ghost" size="sm" onClick={onClose}>
            Cancel
          </Button>
        }
      />
      <div className="grid gap-3 sm:grid-cols-2">
        <label className="space-y-1.5">
          <span className="font-mono text-xs uppercase tracking-wide text-faint">
            Name
          </span>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="acme"
            data-testid="new-tenant-name"
          />
        </label>
        <label className="space-y-1.5">
          <span className="font-mono text-xs uppercase tracking-wide text-faint">
            Member namespaces (comma-separated)
          </span>
          <Input
            value={namespaces}
            onChange={(e) => setNamespaces(e.target.value)}
            placeholder="team-a, team-b"
            data-testid="new-tenant-namespaces"
          />
        </label>
      </div>
      <label className="flex items-center gap-2 text-sm text-secondary-foreground">
        <input
          type="checkbox"
          checked={isolate}
          onChange={(e) => setIsolate(e.target.checked)}
          data-testid="new-tenant-isolation"
        />
        Network isolation (recommended — deny cross-tenant pod traffic)
      </label>
      {error && (
        <p className="text-sm text-destructive" role="alert" data-testid="new-tenant-error">
          {error}
        </p>
      )}
      <Button size="sm" onClick={submit} disabled={busy} data-testid="new-tenant-submit">
        {busy ? "Creating…" : "Create tenant"}
      </Button>
    </section>
  );
}

export function TenantsPage() {
  const { can } = useCapabilities();
  const canCreate = can(RES_TENANTS, "create");
  const [showCreate, setShowCreate] = useState(false);
  // Pre-fill the filter from ?q= so an agent's namespace link (agent detail →
  // /tenants?q=<namespace>) lands with the owning tenant already filtered (m49.4).
  const [searchParams] = useSearchParams();
  const [query, setQuery] = useState(searchParams.get("q") ?? "");
  const [view, setView] = useState<ViewId>("all");
  const [state, setState] = useState<Load>({ kind: "loading" });
  const [detail, setDetail] = useState<Detail>({ kind: "none" });
  // Live usage per tenant for the near-cap indicator (m54.5) — one batched
  // call, best-effort (501 no state layer / 403 → the column says so).
  const [usageByTenant, setUsageByTenant] = useState<Record<string, TenantUsage>>({});
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(() => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setState({ kind: "loading" });
    // Batched live usage for the near-cap column — best-effort, never blocks or
    // fails the list (no state layer → 501, a viewer → 403).
    setUsageByTenant({});
    api
      .listTenantUsage(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setUsageByTenant(Object.fromEntries(res.items.map((u) => [u.name, u])));
      })
      .catch(() => {
        /* usage is optional — the cells and the note say it is absent */
      });
    api
      .listTenants(controller.signal)
      .then((res) => {
        if (controller.signal.aborted) return;
        setState({ kind: "ready", items: res.items });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : "couldn't load tenants",
          forbidden: err instanceof ApiError && err.isForbidden,
        });
      });
  }, []);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const openDetail = useCallback((name: string) => {
    setDetail({ kind: "loading", name });
    api
      .tenantDetail(name)
      .then((d) => setDetail({ kind: "ready", detail: d }))
      .catch((err: unknown) =>
        setDetail({
          kind: "error",
          name,
          message: err instanceof Error ? err.message : "couldn't load tenant detail",
        }),
      );
  }, []);

  const items = useMemo(
    () => (state.kind === "ready" ? state.items : []),
    [state],
  );

  // Triage once, sort once. Attention-first (§6.1): anything asking for a
  // person sits above everything that is fine, then alphabetically.
  const sorted = useMemo(() => {
    const rows = items.map((t) => triage(t, usageByTenant[t.name]));
    rows.sort(
      (a, b) =>
        nextStepRank(a.next.tone) - nextStepRank(b.next.tone) ||
        a.t.name.localeCompare(b.t.name),
    );
    return rows;
  }, [items, usageByTenant]);

  const activeView = VIEWS.find((v) => v.id === view) ?? VIEWS[0];
  const q = query.trim().toLowerCase();
  const visible = useMemo(() => {
    const byView = sorted.filter(activeView.match);
    return q
      ? byView.filter(
          (r) =>
            // Match the tenant name OR any of its namespaces — "which tenant
            // owns X?" (M47 review).
            r.t.name.toLowerCase().includes(q) ||
            r.t.namespaces.some((ns) => ns.toLowerCase().includes(q)),
        )
      : byView;
  }, [sorted, activeView, q]);

  // /api/tenants returns every tenant in ONE response (no cursor), so a count
  // here describes the whole set — the condition the FilterChipRow contract
  // requires before a chip may carry a number.
  const chips: FilterChip[] = VIEWS.map((v) => ({
    id: v.id,
    label: v.label,
    count: state.kind === "ready" ? sorted.filter(v.match).length : undefined,
  }));

  const usageKnown = Object.keys(usageByTenant).length > 0;
  const cappedCount = items.filter((t) => t.model && capOf(t.model.budgetUSD) !== null).length;

  const error: DataTableError | null =
    state.kind === "error"
      ? {
          message: state.message,
          forbidden: state.forbidden,
          resource: "tenants",
          onRetry: state.forbidden ? undefined : load,
        }
      : null;

  // The §4.4 resource-list budget, in visual order. Tenant, State and Next step
  // survive every width; the spend meter and the namespace count leave first.
  const columns: Column<Triaged>[] = [
    {
      id: "name",
      header: "Tenant",
      priority: 1,
      className: "max-w-[20rem]",
      cell: ({ t }) => (
        <CellEntity
          name={t.name}
          title={t.name}
          // The namespaces are the tenant's identity, not a number — they
          // belong on the second line, where a namespace never shares the
          // name's line (§4.5).
          namespace={t.namespaces.length > 0 ? t.namespaces.join(" · ") : undefined}
        />
      ),
    },
    {
      id: "namespaces",
      header: "Namespaces",
      priority: 3,
      numeric: true,
      cell: ({ t }) => <QuantityValue value={t.memberNamespaces ?? UNKNOWN} />,
    },
    {
      id: "spend",
      header: "Spend vs budget",
      priority: 3,
      className: "w-[14rem]",
      cell: ({ t }) => (
        <SpendCell model={t.model} usage={usageByTenant[t.name]} />
      ),
    },
    {
      id: "status",
      header: "State",
      priority: 1,
      className: "w-[11rem]",
      cell: ({ t, level }) => (
        <div className="flex flex-wrap items-center gap-1.5">
          <StatusBadge ready={t.ready} />
          {/* "Over cap" (not "At cap") makes clear that calls are ALREADY being
              refused, not merely at the boundary (m54.6 UX). Amber is §2.2's
              "a bound is near"; crit is §2.2's "will not proceed". */}
          {level && (
            <Badge
              variant={level === "over" ? "crit" : "warn"}
              data-testid={`tenant-nearcap-${t.name}`}
              title={
                level === "over"
                  ? "Live usage is at or over one of this tenant's caps — the next model call is refused."
                  : `Live usage is within ${Math.round(NEAR_CAP_RATIO * 100)}% of one of this tenant's caps.`
              }
            >
              {level === "over" ? "Over cap" : "Near cap"}
            </Badge>
          )}
        </div>
      ),
    },
    {
      id: "next",
      header: "Next step",
      // Never dropped and never truncated (§4.4).
      priority: 1,
      className: "w-[10rem]",
      cell: ({ t, next }) => (
        <NextStepLink
          label={next.label}
          tone={next.tone}
          onClick={next.tone === "none" ? undefined : () => openDetail(t.name)}
          ariaLabel={next.label ? `${next.label} — ${t.name}` : undefined}
          testId={`tenant-next-${t.name}`}
        />
      ),
    },
  ];

  const viewEmptied =
    items.length > 0 && visible.length === 0 && view !== "all" && q === "";
  const empty: EmptyStateProps = viewEmptied
    ? {
        intent: "filtered",
        icon: Filter,
        title: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].title,
        description: VIEW_EMPTY[activeView.id as Exclude<ViewId, "all">].description,
        action: {
          label: "Show everything",
          variant: "outline",
          onClick: () => setView("all"),
        },
        totalCount: items.length,
        countNoun: "tenants",
      }
    : {
        icon: Building2,
        title: "No tenants yet",
        description:
          "A tenant groups namespaces and caps their compute and model usage — over a model budget, the next call is refused rather than billed. An operator creates one to enforce that.",
        action: canCreate
          ? { label: "New tenant", onClick: () => setShowCreate(true) }
          : undefined,
      };

  const closing = state.kind === "ready" ? closingLine(sorted, usageKnown) : null;
  const metaLine =
    state.kind === "ready"
      ? `${items.length} tenant${items.length === 1 ? "" : "s"}`
      : undefined;

  return (
    <div className="min-w-0 space-y-6" data-testid="tenants-page">
      <PageHeader
        title="Tenants"
        meta={metaLine}
        lede="Cluster-scoped groupings of namespaces with compute and model quotas. Sorted by what is closest to a ceiling."
        actionsSlot={
          canCreate && !showCreate ? (
            <Button
              size="sm"
              className="text-sm"
              onClick={() => setShowCreate(true)}
              data-testid="new-tenant-button"
            >
              New tenant
            </Button>
          ) : undefined
        }
      />

      {showCreate && (
        <CreateTenantPanel
          onClose={() => setShowCreate(false)}
          onCreated={() => {
            setShowCreate(false);
            load();
          }}
        />
      )}

      {(state.kind === "loading" || items.length > 0) && (
        <FilterChipRow
          chips={chips}
          value={view}
          onChange={(id) => setView(id as ViewId)}
          label="Filter tenants"
          className="min-w-0"
        />
      )}

      {state.kind === "ready" && items.length > 0 && (
        <div data-testid="tenants-usage-note">
          {!usageKnown ? (
            <QuietNote title="Live usage isn’t recorded for this install.">
              Spend, request rate, and in-flight counts come from the shared
              usage accumulator, and this platform has none wired up — so no row
              can say how close it is to its caps. The Spend column reads{" "}
              <span className="font-mono">—</span> rather than{" "}
              <span className="font-mono">$0.00</span>, because zero would be a
              claim we can&rsquo;t make.
            </QuietNote>
          ) : cappedCount < items.length ? (
            <QuietNote title="A tenant without a budget gets no bar.">
              {cappedCount === 0
                ? "No tenant here sets a model budget, so there is no bound to draw spend against — the figures are the real usage, with nothing to measure them by."
                : `${items.length - cappedCount} of these ${items.length} tenants set no model budget. Their spend is shown as a figure rather than a bar: a full bar against a cap nobody set would be a claim, not a measurement.`}
            </QuietNote>
          ) : null}
        </div>
      )}

      <DataTable<Triaged>
        columns={columns}
        rows={visible}
        rowKey={({ t }) => t.name}
        loading={state.kind === "loading"}
        error={error}
        query={query}
        onQueryChange={setQuery}
        queryPlaceholder="Filter by name or namespace…"
        ariaLabel="Tenants"
        onRowClick={({ t }) => openDetail(t.name)}
        empty={empty}
      />

      {closing && <ClosingNote>{closing}</ClosingNote>}

      {detail.kind === "loading" && (
        <div className="rounded-lg border border-border bg-card p-5" data-testid="tenant-detail">
          <p className="text-sm text-faint" data-testid="tenant-detail-loading">
            Loading {detail.name}…
          </p>
        </div>
      )}
      {detail.kind === "error" && (
        <div className="rounded-lg border border-border bg-card p-5" data-testid="tenant-detail">
          <div className="mb-2 flex items-center justify-between gap-3">
            <h2 className="font-serif text-lg font-medium">{detail.name}</h2>
            <Button variant="ghost" size="sm" onClick={() => setDetail({ kind: "none" })}>
              Close
            </Button>
          </div>
          <p className="text-sm text-destructive" role="alert" data-testid="tenant-detail-error">
            {detail.message}
          </p>
        </div>
      )}
      {detail.kind === "ready" && (
        <TenantDetailPanel
          tenant={detail.detail}
          onClose={() => setDetail({ kind: "none" })}
        />
      )}
    </div>
  );
}
