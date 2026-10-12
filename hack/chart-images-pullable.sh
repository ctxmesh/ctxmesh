#!/usr/bin/env bash
# chart-images-pullable — every third-party image the chart renders must pull anonymously.
#
# MinIO closed anonymous pulls on Docker Hub and then on quay.io, and both times the first sign was
# a stranger's `helm install` failing on a cold cluster: the image was digest-pinned and gone. A
# digest pins the bytes, not their availability. This pulls each image the way a fresh node does,
# with no registry credentials, so an upstream that closes its doors fails CI instead of an install.
#
# Images come from hack/chart_images.py: the default render, the README's production command and
# every optional component on. First-party images are left out: in a working tree they carry
# Chart.yaml's appVersion, which the release replaces with the tag, and the release's own stranger
# gate pulls the ones it publishes.
#
# Usage: hack/chart-images-pullable.sh [-- extra helm args for every render]
#   CHART               chart directory (default deploy/helm/ctxmesh)
#   CHART_IMAGES_PULLER a command run as `<cmd> <ref>` instead of `docker pull`, exit 0 = pullable
set -euo pipefail
cd "$(dirname "$0")/.."

CHART="${CHART:-deploy/helm/ctxmesh}"
refs="$(python3 hack/chart_images.py list --third-party --chart "$CHART" "$@")" \
  || { echo "FAIL: could not list the chart's images (above)" >&2; exit 1; }

anon="$(mktemp -d)"
trap 'rm -rf "$anon"' EXIT

# An empty DOCKER_CONFIG holds no credentials, so the pull is anonymous even on a machine that is
# logged in to the registry. A pull this script made is removed again; one already present is kept.
pull() {
  local ref="$1" had=0
  if [ -n "${CHART_IMAGES_PULLER:-}" ]; then
    # shellcheck disable=SC2086
    ${CHART_IMAGES_PULLER} "$ref"
    return
  fi
  docker image inspect "$ref" >/dev/null 2>&1 && had=1
  DOCKER_CONFIG="$anon" docker pull --quiet "$ref" >/dev/null || return 1
  if [ "$had" = 0 ]; then
    docker image rm "$ref" >/dev/null 2>&1 || true
  fi
}

checked=0
failed=0
while read -r ref; do
  [ -n "$ref" ] || continue
  checked=$((checked + 1))
  if err="$(pull "$ref" 2>&1)"; then
    echo "  ok: $ref"
  else
    failed=$((failed + 1))
    echo "FAIL: $ref does not pull anonymously" >&2
    printf '%s\n' "$err" | sed 's/^/      /' | tail -3 >&2
  fi
done <<< "$refs"

[ "$checked" -gt 0 ] || { echo "FAIL: no third-party images were listed; this checked nothing" >&2; exit 1; }
if [ "$failed" -gt 0 ]; then
  echo "FAIL: $failed of $checked third-party images do not pull. Mirror the image, move it to a registry that serves it, or replace it (ADR 0145)." >&2
  exit 1
fi
echo "PASS: all $checked third-party images the chart renders pull anonymously"
