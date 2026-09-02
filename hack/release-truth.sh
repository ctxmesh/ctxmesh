#!/usr/bin/env bash
# release-truth — a release must publish everything an install needs, at a version.
#
# Every verification this project runs starts from the working tree. A release is the
# first artifact that has to stand up without it, and four things were false when this
# gate was written:
#
#   • the chart's first-party image repositories were BARE NAMES — `controller`, `bff`,
#     `token-service`, `statelayer-proxy` — so a stock install pulled
#     docker.io/library/controller:latest. Not our image. Possibly someone's.
#   • appVersion was "latest". A chart that pins nothing cannot be upgraded FROM, and
#     `latest` on a registry is whatever was pushed most recently.
#   • release.yml built and pushed ONE image (the controller) and no chart, while the
#     chart references four and the controller injects a fifth at runtime.
#   • there were zero tags.
#
# None of that is visible to any test that installs from `config/` or a local build,
# which is every test we had. So this gate reads the RELEASE ARTIFACTS instead: the
# chart's own defaults and the workflow that publishes them. Static, no cluster,
# milliseconds — the same shape as install-truth.sh (M148) and provider-parity.sh (M153).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/helm/ctxmesh"
VALUES="$CHART/values.yaml"
CHART_YAML="$CHART/Chart.yaml"
RELEASE_WF="$ROOT/.github/workflows/release.yml"

for f in "$VALUES" "$CHART_YAML" "$RELEASE_WF"; do
  [ -f "$f" ] || { echo "FAIL: $f is missing"; exit 1; }
done

rc=0
note() { echo "  $*"; }
bad() { echo "FAIL: $*" >&2; rc=1; }

# ── 1. appVersion names a version ─────────────────────────────────────────────
appver="$(grep -E '^appVersion:' "$CHART_YAML" | head -1 | sed 's/^appVersion:[[:space:]]*//; s/"//g' | tr -d "'")"
case "$appver" in
  ""|latest|main|dev)
    bad "Chart appVersion is '$appver' — a chart that pins nothing cannot be upgraded FROM, and on a registry that tag is whatever was pushed most recently" ;;
  *) note "ok: appVersion is $appver" ;;
esac

# ── 2. first-party images name a registry ─────────────────────────────────────
# A repository with no host resolves to Docker Hub's library namespace. That is not a
# typo class — it is an install that silently pulls a stranger's image.
firstparty="$(python3 - "$VALUES" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
# repository: <value> lines that are NOT already host-qualified upstream images.
for m in re.finditer(r'^\s*repository:\s*([^\s#]+)', text, re.M):
    repo = m.group(1).strip().strip('"').strip("'")
    print(repo)
PY
)"
[ -n "$firstparty" ] || bad "could not read any image repository from values.yaml — the gate is not actually checking anything"
while read -r repo; do
  [ -n "$repo" ] || continue
  host="${repo%%/*}"
  # Host-qualified when the first segment carries a dot or a port (ghcr.io, localhost:5000).
  case "$repo" in
    */*) case "$host" in *.*|*:*) continue ;; esac ;;
  esac
  bad "image repository '$repo' names no registry — a stock install pulls docker.io/library/$repo"
done <<< "$firstparty"
[ "$rc" = "0" ] && note "ok: every image repository names a registry"

# ── 3. the release publishes every first-party image ──────────────────────────
# The chart's own image values ARE the list of what an install needs. Anything the
# workflow does not push is an image the installer cannot pull.
chart_images="$(python3 - "$VALUES" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
for m in re.finditer(r'^\s*repository:\s*([^\s#]+)', text, re.M):
    repo = m.group(1).strip().strip('"').strip("'")
    # First-party = published from this repo. Upstream pins (dex, litellm, pgvector…)
    # belong to their own projects and are correctly NOT ours to push.
    if '/ctxmesh/' in repo or repo.startswith('ctxmesh/'):
        print(repo.rsplit('/', 1)[-1])
PY
)"
if [ -z "$chart_images" ]; then
  bad "no first-party images found in values.yaml — either check 2 failed, or the chart no longer names our own images"
else
  while read -r img; do
    [ -n "$img" ] || continue
    grep -q -- "$img" "$RELEASE_WF" \
      || bad "the chart installs '$img' but release.yml never publishes it — an installer cannot pull it"
  done <<< "$chart_images"
fi

# ── 4. the chart itself is published ──────────────────────────────────────────
# Images with no chart is not an install. `helm push`/`oci://` is what makes the ✅'s
# "from published artifacts, not a git checkout" possible at all.
grep -qE 'helm (push|package)' "$RELEASE_WF" \
  || bad "release.yml never packages or pushes the chart — 'installable from published artifacts' has no chart to install"

# ── 5. the runtime-injected image ─────────────────────────────────────────────
# managed-agent is not in the chart's image values: the CONTROLLER injects it into
# every managed agent from a compiled-in default. It is still an image an install
# needs, and it is exactly the one that was unpullable (M153).
grep -q 'managed' "$RELEASE_WF" \
  || bad "release.yml never publishes managed-agent — the controller injects it into every managed agent, and an unpublished default is the M153 ContainerMissing defect shipped to users"

[ "$rc" = "0" ] || exit 1
echo "PASS: the release publishes what an install needs, at version $appver"
