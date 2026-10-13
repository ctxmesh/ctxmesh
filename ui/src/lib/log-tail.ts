// log-tail.ts — how the agent page waits for an agent to come alive.
//
// While an agent has no started container (no pod yet, scheduling, pulling its image) the BFF
// answers the live log tail with one "waiting" event and closes. The page asks again on this
// schedule, so the tail opens by itself once the agent starts, and stops asking after
// LOG_WAIT_GIVE_UP_MS so a page left open on an agent that never starts does not poll forever.

export const LOG_WAIT_RETRY_MS = [1_000, 2_000, 4_000, 8_000, 15_000] as const;

export const LOG_WAIT_GIVE_UP_MS = 10 * 60_000;

/** The delay before the next ask, after `retries` asks that each answered "waiting". */
export function logWaitDelay(retries: number): number {
  return LOG_WAIT_RETRY_MS[Math.min(Math.max(retries, 0), LOG_WAIT_RETRY_MS.length - 1)];
}
