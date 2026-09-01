#!/usr/bin/env bash
# ui-no-internal-routes.sh — assert the production SPA bundle ships no
# internal-only surface.
#
# Why this is a build gate and not a unit test (M151 hardening, finding A2):
# the design gallery is gated by a statically-inlined constant, so there is
# nothing left to assert at runtime — a unit test can only prove the flag
# function returns false, which the old test did while the gallery shipped and
# `/design?design` served internal wireframes to anyone who could reach the
# console, unauthenticated. The only assertion that would have caught that is
# this one: look at what the build actually emitted.
#
# Usage: hack/ui-no-internal-routes.sh [dist-dir]   (default ui/dist)
set -euo pipefail

DIST="${1:-ui/dist}"

if [[ ! -d "$DIST/assets" ]]; then
  echo "FAIL: no build output at $DIST/assets — run 'make ui-build' first" >&2
  exit 1
fi

fail=0

# 1. No lazy chunk for the gallery. Rollup names chunks after their entry
#    module, so a surviving gallery is a file called gallery-<hash>.js.
if ls "$DIST"/assets/gallery-*.js >/dev/null 2>&1; then
  echo "FAIL: design gallery chunk shipped:" >&2
  ls -la "$DIST"/assets/gallery-*.js >&2
  fail=1
fi

# 2. No route registered for it, in any chunk.
if grep -l -e '"design/\*"' -e "'design/\*'" "$DIST"/assets/*.js >/dev/null 2>&1; then
  echo "FAIL: a 'design/*' route is registered in the production bundle:" >&2
  grep -l -e '"design/\*"' -e "'design/\*'" "$DIST"/assets/*.js >&2
  fail=1
fi

# 3. No runtime escape hatch that would re-enable an internal surface from a
#    query param — the specific shape of the A2 bug.
if grep -l -e 'has("design")' -e "has('design')" "$DIST"/assets/*.js >/dev/null 2>&1; then
  echo "FAIL: a runtime ?design escape hatch survives in the bundle:" >&2
  grep -l -e 'has("design")' -e "has('design')" "$DIST"/assets/*.js >&2
  fail=1
fi

if [[ $fail -ne 0 ]]; then
  echo "" >&2
  echo "The design gallery is an internal surface (M13/M14 wireframes). It must" >&2
  echo "be reachable ONLY from 'pnpm dev' or an explicit VITE_DESIGN_GALLERY=1" >&2
  echo "build, never from anything a visitor controls." >&2
  exit 1
fi

echo "PASS: no internal-only routes in $DIST"
