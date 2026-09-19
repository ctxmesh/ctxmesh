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
    # `repository:` keys AND bare image-ref values. The controller-injected images
    # (injectedImages.collector / .discovery, oboEgress.sidecarImage) are PLAIN STRINGS, not
    # repository blocks, so a repository-only scan was structurally blind to them — which is
    # exactly how three images an install needs shipped unpublished while this gate stayed green.
for m in re.finditer(r'^\s*[a-zA-Z][a-zA-Z0-9_]*:\s*([^\s#]+)', text, re.M):
    repo = m.group(1).strip().strip('"').strip("'")
    # Only things that actually LOOK like a first-party image ref. Matching every YAML value
    # swept in `resources:`, `cpu:`, `memory:` and reported them as unregistered images.
    if '/ctxmesh/' not in repo and not repo.startswith('ctxmesh/'):
        continue
    repo = repo.split('@')[0]
    if ':' in repo and '/' in repo.rsplit(':', 1)[0]:
        repo = repo.rsplit(':', 1)[0]
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
# `repository:` keys AND bare image-ref VALUES. The controller-injected images
# (injectedImages.collector / .discovery, oboEgress.sidecarImage) are plain strings, not
# repository blocks, so a repository-only scan was structurally BLIND to them — which is exactly
# how three images an install needs shipped unpublished while this gate stayed green.
for m in re.finditer(r'^\s*[a-zA-Z][a-zA-Z0-9_]*:\s*([^\s#]+)', text, re.M):
    repo = m.group(1).strip().strip('"').strip("'")
    # Only things that LOOK like a first-party ref: matching every YAML value swept in
    # `resources:`, `cpu:` and `memory:` and reported them as unregistered images.
    if '/ctxmesh/' not in repo and not repo.startswith('ctxmesh/'):
        continue
    repo = repo.split('@')[0]
    if ':' in repo and '/' in repo.rsplit(':', 1)[0]:
        repo = repo.rsplit(':', 1)[0]
    print(repo.rsplit('/', 1)[-1])
PY
)"
if [ -z "$chart_images" ]; then
  bad "no first-party images found in values.yaml — either check 2 failed, or the chart no longer names our own images"
else
  while read -r img; do
    [ -n "$img" ] || continue
    # The MATRIX ENTRY, not any occurrence. A bare `grep "$img"` is satisfied by a COMMENT —
    # proven: removing egress-sidecar from the matrix left this GREEN, because the comment above
    # it still named the image. The same "a comment satisfied the check" defect M176 recorded,
    # reproduced inside the gate meant to prevent its consequences.
    grep -qE "^[[:space:]]*-[[:space:]]*image:[[:space:]]*${img}[[:space:]]*$" "$RELEASE_WF" \
      || bad "the chart installs '$img' but release.yml's image MATRIX never publishes it — an installer cannot pull it"
  done <<< "$chart_images"
fi

# ── 3b. the controller-injected image keys are NON-EMPTY ──────────────────────
# Enumerating needs FROM the artifact can be defeated by the artifact naming nothing. Section 3
# derives its list by scanning values.yaml for refs containing /ctxmesh/, so `collector: ""` holds
# no such ref, is SKIPPED rather than flagged, and vanishes from "what an install needs". That is
# exactly how the published beta.6 chart shipped all three controller-injected images empty while
# this gate stayed green -- run section 3's scan over those values and it returns an empty list.
# So these keys are checked BY NAME. A key that must be set cannot argue its way out of the list by
# being unset.
injected_bad="$(python3 "$(dirname "$0")/injected-image-keys.py" "$VALUES" 2>/dev/null || true)"
if [ -n "$injected_bad" ]; then
  while IFS='|' read -r key val; do
    [ -n "$key" ] || continue
    if [ "$val" = "EMPTY" ]; then
      bad "chart value '$key' is EMPTY — the controller injects this image into agent pods; empty means an unpullable dev.local default, and for the egress sidecar (which has NO fallback) a Service that Knative refuses by container index"
    else
      bad "chart value '$key' = '$val' names no registry — a stock install would pull docker.io/library/$val"
    fi
  done <<< "$injected_bad"
else
  note "ok: every controller-injected image key is set and registry-qualified"
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

# ── 6. an install-critical fix must not sit unreleased ────────────────────────
# Close gate (d) found the PUBLISHED v0.1.0-beta.6 controller in CrashLoopBackOff on a cold cluster
# with the documented prerequisites. The code was fine; the guard that prevents it landed in
# b32a5b9 four days before the gate ran and was NOT an ancestor of the tag. A repair on a branch
# repairs nothing, and every day it sits, someone installs into the break.
#
# Scoped to the paths that decide whether an install BOOTS -- controller startup, the chart, the
# chart generator, and images/ (the base images and injected sidecars every agent pod runs). A fix
# elsewhere sitting unreleased is ordinary release cadence, not a known-broken artifact. Skipped
# when there is no tag yet, and when the newest tag IS HEAD.
#
# images/ is in the list because leaving it out missed one: the PUBLISHED
# ghcr.io/ctxmesh/managed-agent:v0.1.0-beta.6 reports User=[] (root) while the controller sets
# runAsNonRoot on every agent pod, so the kubelet refuses it with "image will run as root" and no
# managed agent can start from published artifacts at all. The local build reports 65532:65532. The
# repair is real and unshipped, same as the other seven.
newest_tag="$(git -C "$ROOT" tag --list 'v*' --sort=-v:refname 2>/dev/null | head -1 || true)"
# A RELEASE IN FLIGHT is not a stranded fix. Without this the gate makes the release PR itself
# unmergeable: the fixes cannot be in a tag until the PR merges, and the PR cannot merge until the
# fixes are in a tag. The state this check exists to catch is "fixes exist beyond the newest tag AND
# nothing is being cut" -- and a CHANGELOG naming a version newer than the newest tag is exactly the
# signal that something is. Say so and pass; the tag that follows closes the loop.
changelog_ver="$(grep -m1 -oE '^## v[0-9][^ ]*' "$ROOT/CHANGELOG.md" 2>/dev/null | sed 's/^## //' || true)"
if [ -n "$newest_tag" ] && [ -n "$changelog_ver" ] && [ "$changelog_ver" != "$newest_tag" ]; then
  echo "ok: a release is in flight — CHANGELOG names $changelog_ver, newest tag is $newest_tag; unreleased fixes are being carried, not stranded"
  newest_tag=""
fi
# The range start is normally the tag itself. It is NOT when history has been rewritten: main's
# commits then carry new SHAs while the tag still names the original, the two are disjoint, and
# `$newest_tag..HEAD` degenerates to EVERY commit — so every install-critical fix ever made reads
# as unreleased and the gate is permanently, uselessly red. That happened here when main was
# rewritten to correct an author identity (2026-09-19); the tags were deliberately left in place
# because proxy.golang.org has the sdk/go tags cached against their original commits.
#
# Ancestry was only ever a proxy for the real question: what is on HEAD that is not in the tree we
# published. So ask that directly — find the commit on HEAD whose TREE matches the tag's. It is the
# same answer when nothing was rewritten, exact when something was, and it fails CLOSED: with no
# tree-equivalent commit the tag is used unchanged and the gate reports as before.
range_start="$newest_tag"
if [ -n "$newest_tag" ] && ! git -C "$ROOT" merge-base --is-ancestor "$newest_tag" HEAD 2>/dev/null; then
  tag_tree="$(git -C "$ROOT" rev-parse "$newest_tag^{tree}" 2>/dev/null || true)"
  # No early `exit` in awk: it closes the pipe, git log takes SIGPIPE, and pipefail turns a
  # successful lookup into exit 141. Take the first match after awk has read the whole stream.
  twin="$(git -C "$ROOT" log HEAD --format='%H %T' 2>/dev/null | awk -v t="$tag_tree" '$2==t {print $1}' | head -1 || true)"
  if [ -n "$twin" ]; then
    echo "note: $newest_tag is not an ancestor of HEAD (history was rewritten); comparing against ${twin:0:8}, the commit carrying the same tree"
    range_start="$twin"
  fi
fi
if [ -n "$newest_tag" ] && [ "$(git -C "$ROOT" rev-parse "$range_start^{commit}" 2>/dev/null)" != "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null)" ]; then
  unreleased="$(git -C "$ROOT" log --format='%h %s' "$range_start..HEAD" \
      --  cmd/main.go internal/controller/eventing_available.go deploy/helm hack/gen_helm_chart.py images 2>/dev/null \
    | grep -E '^[0-9a-f]+ fix(\(|:)' || true)"
  if [ -n "$unreleased" ]; then
    echo "$unreleased" | sed 's/^/    /'
    bad "install-critical fix(es) are on HEAD but NOT in $newest_tag — the published artifact is known-broken until a release carries them"
  else
    echo "ok: no install-critical fix sits unreleased since $newest_tag"
  fi
fi

[ "$rc" = "0" ] || exit 1
echo "PASS: the release publishes what an install needs, at version $appver"
