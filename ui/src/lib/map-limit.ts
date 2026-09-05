// mapLimit — bounded-concurrency map, for the fan-outs that scan every workspace.
//
// The console has two places that ask every namespace a question at once: Home's
// approval queue and the Approvals page. Both used Promise.all over the namespace
// list, so an install with sixty workspaces opened sixty concurrent requests — on
// the landing page, on every load. The browser queues them, the BFF does not, and
// each one costs an API-server round trip against the caller's token.
//
// Results stay in input order and every item still runs, so callers that count
// refusals per namespace keep working unchanged. Rejection semantics match
// Promise.all: fn is expected to catch its own errors, as both callers already do.
export const NAMESPACE_SCAN_CONCURRENCY = 8;

export async function mapLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  const out = new Array<R>(items.length);
  if (items.length === 0) return out;

  let next = 0;
  const width = Math.max(1, Math.min(limit, items.length));
  await Promise.all(
    Array.from({ length: width }, async () => {
      for (;;) {
        const i = next++;
        if (i >= items.length) return;
        out[i] = await fn(items[i], i);
      }
    }),
  );
  return out;
}
