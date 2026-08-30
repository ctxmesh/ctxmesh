import { useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Shield } from "lucide-react";

import type { AgentTeamSummary, AgentTeamRoster, AgentTeamSpawnBudget } from "@/lib/api";

// TeamSheet — the DECLARED lens of the delegation canvas (M144.9, ADR 0115).
//
// A team's declared structure drawn as an engineer's wiring schematic, rendered
// ONLY from the AgentTeamSummary the teams page already fetches (no new endpoint):
// the supervisor at the root, a label-free monotone edge fan to the roster of
// summonable sub-agents, the spawn budget drawn as HOLLOW ceilings (declared =
// nothing running), all inside the registry trust-boundary drawn as a fence.
//
// Honesty is the visual system (Fable design, ADR 0115): every capacity glyph is
// hollow/stroke-only because these are ceilings, not usage — the Live lens
// (m144.10) fills the SAME glyphs. Readiness is real: a supervisor/roster agent not
// in team.members is unresolved and degrades on three channels (dashed border,
// dimmed text, hollow-ring dot), so color is never the only signal.

const MAX_VISIBLE = 10;

// A resolved agent is one the controller confirmed Ready in the registry.
function isResolved(agentRef: string, members: string[]): boolean {
  return members.includes(agentRef);
}

function StatusDot({ resolved }: { resolved: boolean }) {
  return resolved ? (
    <span
      className="inline-block h-2 w-2 shrink-0 rounded-full bg-success"
      title="Ready in the registry"
      aria-hidden
    />
  ) : (
    <span
      className="inline-block h-2 w-2 shrink-0 rounded-full border border-destructive"
      title="Unresolved — not a Ready member of the registry"
      aria-hidden
    />
  );
}

// ── Budget glyphs — hollow, stroke-only (declared = ceilings, nothing running) ──

function FanGlyph({ n }: { n: number }) {
  const prongs = Math.min(n, 8);
  return (
    <svg width="30" height="22" viewBox="0 0 30 22" className="text-muted-foreground/70" aria-hidden>
      {Array.from({ length: prongs }).map((_, i) => {
        const spread = prongs === 1 ? 0 : i / (prongs - 1) - 0.5;
        return (
          <line
            key={i}
            x1="2"
            y1="11"
            x2="28"
            y2={11 + spread * 18}
            stroke="currentColor"
            strokeWidth="1.25"
          />
        );
      })}
    </svg>
  );
}

function DepthGlyph({ n }: { n: number }) {
  // ●━◌╌╌◌ — node 1 filled + solid segment (level 1 IS declared: this sheet);
  // the rest hollow + dashed (deeper nesting only emerges at runtime).
  const rungs = Math.min(n, 6);
  const cx = (i: number) => 6 + i * 16;
  return (
    <svg width={12 + (rungs - 1) * 16} height="14" className="text-muted-foreground/70" aria-hidden>
      {Array.from({ length: rungs - 1 }).map((_, i) => (
        <line
          key={`s${i}`}
          x1={cx(i)}
          y1="7"
          x2={cx(i + 1)}
          y2="7"
          stroke="currentColor"
          strokeWidth="1.25"
          strokeDasharray={i === 0 ? undefined : "3 2"}
        />
      ))}
      {Array.from({ length: rungs }).map((_, i) => (
        <circle
          key={`c${i}`}
          cx={cx(i)}
          cy="7"
          r="3.2"
          stroke="currentColor"
          strokeWidth="1.25"
          fill={i === 0 ? "currentColor" : "none"}
        />
      ))}
    </svg>
  );
}

function TotalGlyph({ n }: { n: number }) {
  const cap = 40;
  const dots = Math.min(n, cap);
  return (
    <div className="flex items-center gap-1.5">
      <div className="grid w-fit grid-cols-10 gap-[3px]" aria-hidden>
        {Array.from({ length: dots }).map((_, i) => (
          <span key={i} className="h-1.5 w-1.5 rounded-full border border-muted-foreground/70" />
        ))}
      </div>
      {n > cap && <span className="font-mono text-[10px] text-muted-foreground">×{n}</span>}
    </div>
  );
}

function BudgetStrip({ budget }: { budget: AgentTeamSpawnBudget }) {
  return (
    <div className="mt-4 border-t border-dashed border-muted-foreground/30 pt-3" data-testid="team-sheet-budget">
      <p className="mb-2 text-[10px] uppercase tracking-wide text-muted-foreground">
        spawn budget · ceilings, nothing running
      </p>
      <div className="flex flex-wrap gap-x-8 gap-y-3">
        <div className="flex flex-col gap-1">
          <FanGlyph n={budget.maxFanOut} />
          <span className="text-[11px] font-medium text-foreground">fan-out ≤ {budget.maxFanOut}</span>
          <span className="text-[10px] text-muted-foreground">parallel sub-runs per step</span>
        </div>
        <div className="flex flex-col gap-1">
          <div className="flex h-[22px] items-center">
            <DepthGlyph n={budget.maxSpawnDepth} />
          </div>
          <span className="text-[11px] font-medium text-foreground">depth ≤ {budget.maxSpawnDepth}</span>
          <span className="text-[10px] text-muted-foreground">level 1 declared · deeper at runtime</span>
        </div>
        <div className="flex flex-col gap-1">
          <div className="flex h-[22px] items-center">
            <TotalGlyph n={budget.maxTotalSpawns} />
          </div>
          <span className="text-[11px] font-medium text-foreground">total ≤ {budget.maxTotalSpawns}</span>
          <span className="text-[10px] text-muted-foreground">sub-runs per root run</span>
        </div>
      </div>
    </div>
  );
}

// ── Roster node ────────────────────────────────────────────────────────────────

function RosterRow({
  member,
  resolved,
  rowRef,
}: {
  member: AgentTeamRoster;
  resolved: boolean;
  rowRef: (el: HTMLDivElement | null) => void;
}) {
  // Dedupe: when the delegate_to handle equals the backing agent, print it once.
  const sameName = member.name === member.agentRef;
  return (
    <div
      ref={rowRef}
      className={`rounded-md border bg-card px-2.5 py-1.5 ${
        resolved ? "" : "border-dashed border-destructive/50"
      }`}
      data-testid={`team-member-${member.name}`}
    >
      <div className="flex items-center gap-2">
        <span
          className="max-w-[22ch] shrink-0 truncate rounded bg-muted px-1.5 py-0.5 font-mono text-[11px]"
          title={member.name}
        >
          {member.name}
        </span>
        {!sameName && (
          <span
            className={`truncate text-[13px] font-medium ${resolved ? "" : "text-muted-foreground"}`}
          >
            {member.agentRef}
          </span>
        )}
        <span className="ml-auto">
          <StatusDot resolved={resolved} />
        </span>
      </div>
      {resolved ? (
        member.description && (
          <p className="mt-0.5 truncate text-xs text-muted-foreground" title={member.description}>
            {member.description}
          </p>
        )
      ) : (
        <p className="mt-0.5 text-[10px] font-medium uppercase tracking-wide text-destructive">
          unresolved — not a Ready member of the registry
        </p>
      )}
    </div>
  );
}

// ── The team sheet ───────────────────────────────────────────────────────────

export function TeamSheet({ team }: { team: AgentTeamSummary }) {
  // Resolve + sort: unresolved first (declared order preserved within each group),
  // so truncation can only ever hide HEALTHY rows — a problem is never hidden.
  // Memoized so `visible` is stable across renders (else the measure effect loops).
  const { supervisorResolved, visible, hidden, hiddenUnresolved } = useMemo(() => {
    const supRes = isResolved(team.supervisor, team.members);
    const rr = team.roster.map((m) => ({
      member: m,
      resolved: isResolved(m.agentRef, team.members),
    }));
    const sorted = [...rr].sort((a, b) => Number(a.resolved) - Number(b.resolved));
    const vis = sorted.slice(0, MAX_VISIBLE);
    const hid = sorted.slice(MAX_VISIBLE);
    return {
      supervisorResolved: supRes,
      visible: vis,
      hidden: hid,
      hiddenUnresolved: hid.filter((r) => !r.resolved).length,
    };
  }, [team]);

  // Edge geometry: one monotone cubic bezier per visible row, measured relative to
  // the gutter. Monotone (one source, vertically-ordered targets) ⇒ crossings are
  // geometrically impossible at any count. Guarded for zero-measurement (jsdom):
  // the rows are the accessible encoding; the drawing is decorative reinforcement.
  const supRef = useRef<HTMLDivElement>(null);
  const gutterRef = useRef<HTMLDivElement>(null);
  const rowEls = useRef<(HTMLDivElement | null)[]>([]);
  const [edges, setEdges] = useState<{ d: string; resolved: boolean }[]>([]);
  const [gutter, setGutter] = useState({ w: 0, h: 0 });

  const measure = useCallback(() => {
    const g = gutterRef.current?.getBoundingClientRect();
    const s = supRef.current?.getBoundingClientRect();
    if (!g || !s || g.width === 0 || g.height === 0) {
      // No layout to measure (e.g. jsdom / gutter hidden) — the rows are the
      // accessible encoding; skip the decorative wires. Guard against churn.
      setEdges((prev) => (prev.length === 0 ? prev : []));
      return;
    }
    const sy = s.top + s.height / 2 - g.top;
    const w = g.width;
    const next: { d: string; resolved: boolean }[] = [];
    for (let i = 0; i < visible.length; i++) {
      const el = rowEls.current[i];
      if (!el) continue;
      const r = el.getBoundingClientRect();
      const ty = r.top + r.height / 2 - g.top;
      next.push({
        d: `M 0 ${sy} C ${w / 2} ${sy}, ${w / 2} ${ty}, ${w} ${ty}`,
        resolved: visible[i].resolved,
      });
    }
    setGutter((prev) => (prev.w === w && prev.h === g.height ? prev : { w, h: g.height }));
    setEdges((prev) => {
      const same =
        prev.length === next.length &&
        prev.every((p, i) => p.d === next[i].d && p.resolved === next[i].resolved);
      return same ? prev : next;
    });
  }, [visible]);

  useLayoutEffect(() => {
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => measure());
    if (gutterRef.current) ro.observe(gutterRef.current);
    rowEls.current.forEach((el) => el && ro.observe(el));
    return () => ro.disconnect();
  }, [measure]);

  return (
    <div className="space-y-2" data-testid="team-sheet">
      {/* The registry trust boundary drawn as a fence — every agent inside inherits
          it, every sub-run stays inside it. The legend tab breaks the top border. */}
      <div className="relative mt-4 rounded-lg border border-dashed border-muted-foreground/40 p-4">
        <div className="absolute -top-3 left-3 flex items-center gap-1.5 bg-card px-1.5">
          <Shield className="h-3 w-3 text-muted-foreground" />
          <span className="font-mono text-[11px] text-muted-foreground">
            registry: {team.registry}
          </span>
          <span className="text-[10px] uppercase tracking-wide text-muted-foreground/70">
            trust boundary
          </span>
        </div>

        {/* Wiring canvas: supervisor · edge gutter · roster. */}
        <div className="flex gap-0">
          {/* Supervisor — the only weighted node; the 2px left rule marks the
              orchestrator ROLE (identity), never status. */}
          <div
            ref={supRef}
            className="relative w-[164px] shrink-0 self-center rounded-lg border border-l-2 border-l-primary bg-card px-3 py-2.5"
            data-testid="team-sheet-supervisor"
          >
            <div className="text-[10px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
              Orchestrator
            </div>
            <div className="mt-0.5 flex items-center gap-1.5">
              <span className="truncate text-sm font-semibold">{team.supervisor}</span>
              <StatusDot resolved={supervisorResolved} />
            </div>
            <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">
              fan-out ≤ {team.budget.maxFanOut}
            </div>
            {/* Fan-out socket ticks at the output port. */}
            <div className="absolute -right-px top-1/2 flex -translate-y-1/2 flex-col gap-0.5">
              {Array.from({ length: Math.min(team.budget.maxFanOut, 6) }).map((_, i) => (
                <span key={i} className="h-px w-1.5 bg-muted-foreground/50" aria-hidden />
              ))}
            </div>
          </div>

          {/* Edge gutter (SVG) — hidden on narrow, where the roster falls to a spine. */}
          <div
            ref={gutterRef}
            className="relative hidden w-16 shrink-0 self-stretch sm:block"
            aria-hidden
          >
            {gutter.w > 0 && (
              <svg
                className="absolute inset-0 h-full w-full overflow-visible"
                width={gutter.w}
                height={gutter.h}
              >
                {edges.map((e, i) => (
                  <path
                    key={i}
                    d={e.d}
                    fill="none"
                    stroke={
                      e.resolved
                        ? "hsl(var(--muted-foreground) / 0.35)"
                        : "hsl(var(--destructive) / 0.5)"
                    }
                    strokeWidth="1.25"
                    strokeDasharray={e.resolved ? undefined : "4 3"}
                  />
                ))}
              </svg>
            )}
          </div>

          {/* Roster — one card per visible member; each card's handle chip IS the
              edge label (no text on the wires). Narrow: a left spine, tree-style. */}
          <div className="flex min-w-0 flex-1 flex-col gap-1.5 border-l border-muted-foreground/20 pl-3 sm:border-l-0 sm:pl-2.5">
            {visible.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No roster — this team summons no sub-agents.
              </p>
            ) : (
              <div className="flex flex-col gap-1.5" data-testid="team-sheet-roster">
                {visible.map((r, i) => (
                  <RosterRow
                    key={r.member.name}
                    member={r.member}
                    resolved={r.resolved}
                    rowRef={(el) => (rowEls.current[i] = el)}
                  />
                ))}
                {hidden.length > 0 && (
                  <div
                    className={`rounded-md border border-dashed px-2.5 py-1.5 text-[11px] ${
                      hiddenUnresolved > 0
                        ? "border-destructive/50 text-destructive"
                        : "border-muted-foreground/30 text-muted-foreground"
                    }`}
                    data-testid="team-sheet-overflow"
                  >
                    + {hidden.length} more
                    {hiddenUnresolved > 0
                      ? ` · ${hiddenUnresolved} unresolved`
                      : " · all ready"}
                  </div>
                )}
              </div>
            )}
          </div>
        </div>

        <BudgetStrip budget={team.budget} />

        {/* Inheritance chips on the fence's bottom edge. */}
        <div className="mt-3 flex flex-wrap items-center gap-x-2 gap-y-0.5 border-t border-dashed border-muted-foreground/30 pt-2 text-[11px] text-muted-foreground">
          <span>inherits from registry:</span>
          <span title="Sub-runs act with OBO credentials scoped to this registry.">OBO creds</span>
          <span aria-hidden>·</span>
          <span title="Egress is bounded by the registry's NetworkPolicy.">NetworkPolicy</span>
          <span aria-hidden>·</span>
          <span title="The `shared` memory scope is registry-scoped.">shared memory</span>
        </div>
      </div>
    </div>
  );
}
