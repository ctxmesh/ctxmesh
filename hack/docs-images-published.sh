#!/usr/bin/env bash
# docs-images-published — every ghcr.io/ctxmesh image the docs tell a user to pull must be
# one the release actually publishes.
#
# v0.1.0-beta.2 documented `FROM ghcr.io/ctxmesh/base-python` and `image:
# ghcr.io/ctxmesh/example-agent`, and the release matrix published neither. Following the
# SDK guide gave an auth failure; following the QUICKSTART gave ImagePullBackOff. Nothing
# caught it, because every gate compared the chart to the release and no gate compared the
# DOCS to the release.
#
# The docs site is a separate repo. When it is not checked out this reads only the engine's
# own markdown and says so — a partial run is reported, never silently passed.
set -euo pipefail
cd "$(dirname "$0")/.."

RELEASE_WF=".github/workflows/release.yml"
SITE="${CTXMESH_SITE:-../ctxmesh.github.io}"

published="$(python3 - "$RELEASE_WF" <<'PY'
import re, sys
t = open(sys.argv[1]).read()
print("\n".join(sorted(set(re.findall(r'^\s+- image:\s*(\S+)', t, re.M)))))
PY
)"
[ -n "$published" ] || { echo "FAIL: read no images out of $RELEASE_WF — the gate is not checking anything"; exit 1; }

# The chart is published by its own job, not the image matrix.
published="$published
charts/ctxmesh"

scanned="README.md"
[ -d docs ] && scanned="$scanned docs"

# The docs site is where the motivating defect lived: `FROM ghcr.io/ctxmesh/base-python` and the
# quickstart's `example-agent` are both there, and NEITHER is in the engine's markdown. Without
# the site this gate scans one file whose only ghcr reference is the chart — which the block
# above adds to `published` unconditionally — so it checks one image against a list that contains
# it and cannot fail. That is worse than not running.
#
# So a missing site is a FAILURE unless the caller says otherwise. CI sets CTXMESH_SITE; a
# developer without the sibling checkout sets DOCS_IMAGES_ALLOW_PARTIAL=1 and gets a loud SKIP,
# not a PASS.
if [ -d "$SITE/src/content/docs" ]; then
  scanned="$scanned $SITE/src/content/docs"
elif [ -n "${DOCS_IMAGES_ALLOW_PARTIAL:-}" ]; then
  echo "SKIP: the docs site is not at $SITE — this gate checked NOTHING that matters." >&2
  echo "      The images it exists to catch (base-python, echo-agent) live there." >&2
  exit 0
else
  echo "FAIL: the docs site is not checked out at $SITE." >&2
  echo "      Every ghcr.io/ctxmesh image the docs reference lives in that repo, so scanning" >&2
  echo "      only the engine's markdown checks one image against a list already containing it." >&2
  echo "      Set CTXMESH_SITE, or DOCS_IMAGES_ALLOW_PARTIAL=1 to skip loudly." >&2
  exit 1
fi

# shellcheck disable=SC2086
referenced="$(grep -rhoE "ghcr\.io/ctxmesh/[a-z0-9._/-]+" $scanned 2>/dev/null \
  | sed -E 's#(:[^/]*)?$##' | sed 's#^ghcr\.io/ctxmesh/##' | sed 's#[.:]$##' | sort -u)"

checked=0
rc=0
while read -r img; do
  [ -n "$img" ] || continue
  if ! grep -qxF -- "$img" <<<"$published"; then
    echo "FAIL: the docs tell a user to pull ghcr.io/ctxmesh/$img, and no release publishes it." >&2
    echo "      Add it to the matrix in $RELEASE_WF, or stop documenting it." >&2
    rc=1
  fi
  checked=$((checked + 1))
done <<<"$referenced"

# Without this, a grep that stops matching makes the loop run zero times and the gate pass.
[ "$checked" -gt 0 ] || { echo "FAIL: found no ghcr.io/ctxmesh references at all — the gate is not checking anything" >&2; rc=1; }

[ "$rc" = "0" ] || exit 1
echo "PASS: all $checked ghcr.io/ctxmesh images the docs reference are published by the release"
