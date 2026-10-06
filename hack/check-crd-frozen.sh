#!/usr/bin/env bash
# CRD freeze guard (ADR 0152 §4).
#
# AgentTeam and Workflow are frozen: additive bug fixes only, and no new fields until close gate (d),
# the stranger's install path, is green. A freeze that is only a sentence is not a freeze, so this
# compares the generated CRDs with the checksums committed in hack/crd-freeze.sum. Any change to a
# frozen kind's schema, description included, fails until that file is edited on purpose.
#
# Unfreezing, or landing a sanctioned bug fix, is a deliberate edit of hack/crd-freeze.sum:
#   (cd config/crd/bases && sha256sum <crd>.yaml) and replace that line.
# Run after `make manifests`.
set -euo pipefail
cd "$(dirname "$0")/.."

SUMS="hack/crd-freeze.sum"
CRD_DIR="${1:-config/crd/bases}"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

[ -s "$SUMS" ] || { echo "FAIL: $SUMS is missing or empty; the freeze has nothing to compare" >&2; exit 1; }

fail=0
checked=0
while read -r want file; do
  [ -n "${want:-}" ] || continue
  case "$want" in \#*) continue ;; esac
  path="$CRD_DIR/$file"
  if [ ! -f "$path" ]; then
    echo "FAIL: $path is frozen in $SUMS but does not exist" >&2
    fail=1
    continue
  fi
  got="$(sha256 "$path")"
  checked=$((checked + 1))
  if [ "$got" != "$want" ]; then
    echo "FAIL: $file changed, but this kind is frozen until close gate (d) is green (ADR 0152)." >&2
    echo "      Only additive bug fixes may land, and unfreezing is a deliberate edit of $SUMS:" >&2
    echo "      want $want" >&2
    echo "      got  $got" >&2
    fail=1
  fi
done < "$SUMS"

[ "$checked" -gt 0 ] || { echo "FAIL: $SUMS names no CRD; the freeze checks nothing" >&2; exit 1; }
[ "$fail" -eq 0 ] || exit 1
echo "CRD freeze OK: $checked frozen CRD(s) match $SUMS."
