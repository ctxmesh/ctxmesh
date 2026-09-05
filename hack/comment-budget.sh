#!/usr/bin/env bash
# comment-budget — no new wall-of-text comments.
#
# Measured 2026-09-04: 28% of non-test Go source was comments — 31,704 lines. Most of the
# bulk is milestone archaeology ("this cost time in M155", "found by the M153 journey test")
# that the BRAIN already records in log/, reviews/ and ADRs, so the code carries a second copy
# that ages independently and buries the sentences a reader actually needs.
#
# WHAT THIS GATE IS AND IS NOT. It caps the size of a single comment BLOCK. That is a proxy,
# and a weak one: half the comment mass sits in ordinary 2-5 line comments, which no
# line-count rule can judge. It exists to stop the worst shape from coming BACK — the 40- and
# 50-line file headers that read like a changelog — while the style rule in CLAUDE.md handles
# the rest by judgment, as code is touched.
#
# api/*_types.go is exempt: those comments become CRD field descriptions through
# controller-gen, so they are user-facing API documentation, edited for quality rather than
# cut for length.
#
# The GRANDFATHERED list below is the honest part. These blocks exist today and are not
# silently tolerated — they are named, and the list may only shrink. A new entry is a
# deliberate act someone has to defend in review.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIMIT="${COMMENT_BLOCK_LIMIT:-25}"

python3 - "$ROOT" "$LIMIT" <<'PY'
import pathlib, sys

root, limit = pathlib.Path(sys.argv[1]), int(sys.argv[2])

# Blocks over the limit that predate the gate. Shrink this list; never grow it.
GRANDFATHERED = {
    "internal/controller/agentregistry_controller.go",
    "internal/bff/server.go",
    "internal/ingest/ingest.go",
    "internal/credresolve/doc.go",
    "cmd/launcher/objectstore.go",
    "cmd/launcher/guardrail_judge.go",
    "internal/gateway/render.go",
    "internal/asyncbus/asyncbus.go",
    "internal/bff/handlers.go",
    "internal/bff/promptversions.go",
    "internal/bff/evalsuites.go",
    "internal/runcap/doc.go",
    "internal/prompt/fixture.go",
    "internal/egress/record.go",
    "internal/controller/agentdeployment_controller.go",
    "internal/bff/workflow_executor.go",
    "internal/bff/langfuse.go",
    "internal/bff/dataset_export_executor.go",
    "internal/bff/agents_fork_refclosure.go",
    "cmd/launcher/guardrail.go",
}
SKIP = ("/vendor/", "/node_modules/", "zz_generated", "_test.go", "/api/")

offenders, still_grandfathered = [], set()
for f in sorted(root.rglob("*.go")):
    rel = str(f.relative_to(root))
    if any(s in "/" + rel for s in SKIP):
        continue
    try:
        lines = f.read_text().splitlines()
    except Exception:
        continue
    run, start = 0, 0
    for i, line in enumerate(lines + [""]):
        if line.strip().startswith("//"):
            if not run:
                start = i
            run += 1
        else:
            if run > limit:
                (still_grandfathered.add(rel) if rel in GRANDFATHERED
                 else offenders.append((run, rel, start + 1)))
            run = 0

for n, rel, ln in sorted(offenders, reverse=True):
    print(f"  {rel}:{ln} — a {n}-line comment block (limit {limit}).", file=sys.stderr)
    print("      Say what would break and link the ADR; the history lives in the brain.",
          file=sys.stderr)

# A name that no longer has an oversized block must leave the list, or the list becomes a
# permanent excuse rather than a shrinking debt.
stale = GRANDFATHERED - still_grandfathered
for rel in sorted(stale):
    print(f"  {rel} is grandfathered but no longer has an oversized block — remove it from "
          f"the list in hack/comment-budget.sh.", file=sys.stderr)

if offenders or stale:
    raise SystemExit(1)
print(f"PASS: no comment block over {limit} lines outside api/ "
      f"({len(still_grandfathered)} grandfathered, and the list may only shrink)")
PY
