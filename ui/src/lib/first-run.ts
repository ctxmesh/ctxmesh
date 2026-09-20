/**
 * The first-run checklist's "Run your agent" latch.
 *
 * The step's done-state is derived from the runs feed, which is Langfuse-backed. On a default
 * install that feed is READY and EMPTY — traces are not exported — so a user who runs an agent in
 * the Playground, sees a real response and a trace id, and returns to Home finds the circle still
 * open. Forever. The existing escape hatch only covers an UNAVAILABLE feed (a 501), not a feed
 * that answers correctly with nothing in it.
 *
 * So the browser records what it watched happen. This is a supplement to the feed, never a
 * replacement: a ready feed reporting runs still checks the step off on its own, and the latch
 * only ever turns the step ON — it cannot un-check a step the feed has confirmed.
 *
 * Keyed by workspace, because the checklist asks "have you run an agent HERE". A single global
 * flag would check the step off in a namespace the user has never run anything in, which is the
 * same wrong answer in the other direction.
 */

/** Namespaced so it cannot collide with the session token or a page's own key on this origin. */
const KEY_PREFIX = "ctxmesh.firstRun.ranAgent";

/** The empty namespace is the "all workspaces" scope the Home header calls that. */
function storageKey(namespace: string): string {
  return `${KEY_PREFIX}:${namespace || "*"}`;
}

/**
 * Record that a run completed in this workspace.
 *
 * Every storage write is guarded: Safari private mode throws on localStorage access, and losing a
 * completed Playground run because a bookkeeping write threw would be an absurd trade. A failed
 * write degrades to the previous behaviour — the step stays open — and nothing else breaks.
 */
export function markRunCompleted(namespace: string): void {
  try {
    window.localStorage.setItem(storageKey(namespace), "1");
  } catch {
    /* no-op: the feed remains the source of truth */
  }
}

/** Whether a run has been seen to complete in this workspace on this browser. */
export function hasCompletedRun(namespace: string): boolean {
  try {
    return window.localStorage.getItem(storageKey(namespace)) === "1";
  } catch {
    return false;
  }
}
