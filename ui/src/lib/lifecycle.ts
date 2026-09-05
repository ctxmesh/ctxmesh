import { resolveStatus } from "@/components/kit";

// The agent lifecycle, in one place.
//
// This is the spine Home's fleet bar and the Agents page's filter row both read.
// It used to exist twice — HALTED was copy-pasted verbatim into two pages, and the
// bucketing lived only on Home — so the two surfaces could disagree about what
// "failing" means while both looked right. A bar whose segments navigate to a
// filtered list has to agree with that list by construction, not by coincidence.

/** A phase/reason that says the work was deliberately stopped, not that it broke. */
export const HALTED = /(^|[^a-z])(suspend(ed)?|stopped|halted|killed)([^a-z]|$)/i;

export type Bucket = "serving" | "coming-up" | "draft" | "held" | "failing" | "halted";

/** The fields any bucketing needs — an AgentSummary and a census group both fit. */
export interface Statusish {
  ready: boolean;
  phase?: string;
  reason?: string;
  isDraft?: boolean;
}

/**
 * Precedence matters and is deliberate: halted beats every tone (a stopped agent
 * is not "failing"), a human gate beats ready, and draft is only reached once the
 * agent has no louder story to tell — a draft that is failing is failing.
 */
export function bucketOf(a: Statusish): Bucket {
  const { tone } = resolveStatus(a.ready, a.phase, a.reason);
  if (HALTED.test(`${a.phase ?? ""} ${a.reason ?? ""}`)) return "halted";
  if (tone === "failed") return "failing";
  if (tone === "waiting") return "held";
  if (a.isDraft) return "draft";
  return tone === "ready" ? "serving" : "coming-up";
}

/**
 * Render + navigation metadata per stage, in the order a person reads the fleet:
 * what is working, what is on its way, what is not started, then what wants a
 * person and what is wrong.
 *
 * `tint` is a semantic token class, never the brand: pine means "you can act
 * here", so it is spent on the label link, not on the state (design system §2.1).
 * The one exception is `coming-up`, which the system rules is the pine TINT —
 * converging is the machine doing its own work (§2.5).
 */
export const BUCKETS: {
  id: Bucket;
  label: string;
  /** Background class for the bar segment and legend pip. */
  tint: string;
  /** The Agents-page view this stage opens. */
  view: string;
}[] = [
  { id: "serving", label: "Serving", tint: "bg-success", view: "serving" },
  { id: "coming-up", label: "Coming up", tint: "bg-accent-foreground", view: "coming-up" },
  { id: "draft", label: "Draft", tint: "bg-ghost", view: "draft" },
  { id: "held", label: "Held", tint: "bg-info", view: "held" },
  { id: "failing", label: "Failing", tint: "bg-destructive", view: "failing" },
  { id: "halted", label: "Halted", tint: "bg-destructive/60", view: "halted" },
];
