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
partial=""
if [ -d "$SITE/src/content/docs" ]; then
  scanned="$scanned $SITE/src/content/docs"
else
  partial=" (docs site not checked out at $SITE — engine markdown only)"
fi

# shellcheck disable=SC2086
referenced="$(grep -rhoE "ghcr\.io/ctxmesh/[a-z0-9._/-]+" $scanned 2>/dev/null \
  | sed -E 's#(:[^/]*)?$##' | sed 's#^ghcr\.io/ctxmesh/##' | sed 's#[.:]$##' | sort -u)"

rc=0
while read -r img; do
  [ -n "$img" ] || continue
  if ! grep -qxF -- "$img" <<<"$published"; then
    echo "FAIL: the docs tell a user to pull ghcr.io/ctxmesh/$img, and no release publishes it." >&2
    echo "      Add it to the matrix in $RELEASE_WF, or stop documenting it." >&2
    rc=1
  fi
done <<<"$referenced"

[ "$rc" = "0" ] || exit 1
echo "PASS: every ghcr.io/ctxmesh image the docs reference is published by the release$partial"
