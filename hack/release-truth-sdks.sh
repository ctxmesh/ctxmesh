#!/usr/bin/env bash
# release-truth-sdks — answer "what actually got published for version X?" in one command.
#
# WHY THIS EXISTS
# ---------------
# ADR 0139 asked for this in its Consequences after v0.1.0-beta.2 shipped 8 of 9 artifacts:
# "the release must report per-artifact status rather than a single green." It was never built, and
# on 2026-09-11 the same failure ran again across six registries — sdk-publish.yml reported success
# while npm and RubyGems served functionless stubs, PyPI failed on a missing artifact, and Maven
# failed AFTER its upload had already succeeded. Establishing the truth took six manual queries.
#
# Worse is the quiet version: v0.1.0-beta.3 published a crate whose README told every visitor to
# install 0.1.0-beta.1, and nothing noticed for three releases, because "the workflow was green"
# and no one asked the registries.
#
# Two things make this harder than a loop over six URLs:
#
#   1. EVERY ECOSYSTEM SPELLS THE VERSION DIFFERENTLY. 0.1.0-beta.5 is `0.1.0b5` on PyPI and
#      `0.1.0.pre.beta.5` on RubyGems. A naive grep for the tag reports both as missing.
#   2. A REGISTRY CAN 200 AND STILL BE WRONG. Maven serves a directory listing for a version that
#      does not exist on some mirrors; crates.io answers for the crate, not the version. So each
#      check asserts the VERSION, never just the package.
#
# Usage:
#   ./hack/release-truth-sdks.sh              the version in CHANGELOG.md
#   ./hack/release-truth-sdks.sh 0.1.0-beta.5 an explicit version
#
# Writes a per-artifact table to $GITHUB_STEP_SUMMARY when set. Exits non-zero if ANY artifact is
# missing — that is the whole point, and it is proven by running it against a version that was
# never published.
set -uo pipefail
cd "$(dirname "$0")/.."

want="${1:-}"
if [ -z "$want" ]; then
  want="$(grep -m1 -oE '^## v[0-9][^ ]*' CHANGELOG.md | sed 's/^## v//')"
fi
[ -n "$want" ] || { echo "FAIL: no version given and none readable from CHANGELOG.md" >&2; exit 1; }
want="${want#v}"

# Per-ecosystem spellings of the same release.
pypi_v="$(printf '%s' "$want"  | sed -E 's/-alpha\./a/; s/-beta\./b/; s/-rc\./rc/')"
ruby_v="$(printf '%s' "$want"  | sed 's/-/.pre./')"
go_v="v$want"

UA='User-Agent: ctxmesh-release-truth'
rows=""
rc=0

record() { # name, status(ok|MISSING|ERROR), detail
  rows="${rows}| $1 | $2 | $3 |\n"
  if [ "$2" = "ok" ]; then
    printf '  ok      %-14s %s\n' "$1" "$3"
  else
    printf '  %-7s %-14s %s\n' "$2" "$1" "$3" >&2
    rc=1
  fi
}

# Each probe asserts the VERSION exists, not merely that the package does.
have() { curl -sS --max-time 30 -H "$UA" "$1" 2>/dev/null; }

echo "release-truth for $want"

# ── PyPI ──────────────────────────────────────────────────────────────────────
out="$(have https://pypi.org/pypi/ctxmesh/json | python3 -c "
import json,sys
try: rel=json.load(sys.stdin)['releases']
except Exception: print('ERROR'); raise SystemExit
print('ok' if '$pypi_v' in rel and rel['$pypi_v'] else 'MISSING')" 2>/dev/null || echo ERROR)"
record "PyPI" "${out:-ERROR}" "$pypi_v"

# ── npm ───────────────────────────────────────────────────────────────────────
out="$(have https://registry.npmjs.org/ctxmesh | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('ERROR'); raise SystemExit
print('ok' if '$want' in d.get('versions',{}) else 'MISSING')" 2>/dev/null || echo ERROR)"
record "npm" "${out:-ERROR}" "$want"

# ── crates.io ─────────────────────────────────────────────────────────────────
out="$(have https://crates.io/api/v1/crates/ctxmesh | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('ERROR'); raise SystemExit
print('ok' if any(v['num']=='$want' for v in d.get('versions',[])) else 'MISSING')" 2>/dev/null || echo ERROR)"
record "crates.io" "${out:-ERROR}" "$want"

# ── RubyGems ──────────────────────────────────────────────────────────────────
out="$(have https://rubygems.org/api/v1/versions/ctxmesh.json | python3 -c "
import json,sys
try: vs=[v['number'] for v in json.load(sys.stdin)]
except Exception: print('ERROR'); raise SystemExit
print('ok' if '$ruby_v' in vs else 'MISSING')" 2>/dev/null || echo ERROR)"
record "RubyGems" "${out:-ERROR}" "$ruby_v"

# ── Maven Central ─────────────────────────────────────────────────────────────
# The POM, not a directory listing: repo1 serves no listings, so `.../$v/` 404s even when published.
code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 \
  "https://repo1.maven.org/maven2/ai/ctxmesh/ctxmesh/$want/ctxmesh-$want.pom" 2>/dev/null || echo 000)"
[ "$code" = "200" ] && record "Maven Central" "ok" "$want" || record "Maven Central" "MISSING" "pom http=$code"

# ── Go module proxy ───────────────────────────────────────────────────────────
# Go has no registry: `go get` resolves from the sdk/go/vX.Y.Z tag via the proxy.
code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 \
  "https://proxy.golang.org/github.com/ctxmesh/ctxmesh/sdk/go/@v/$go_v.info" 2>/dev/null || echo 000)"
[ "$code" = "200" ] && record "Go proxy" "ok" "$go_v" || record "Go proxy" "MISSING" "http=$code"

# ── Helm chart on ghcr ────────────────────────────────────────────────────────
# An anonymous pull token — the chart must be installable by a stranger, logged out.
tok="$(have "https://ghcr.io/token?scope=repository:ctxmesh/charts/ctxmesh:pull&service=ghcr.io" \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])' 2>/dev/null || true)"
if [ -n "$tok" ]; then
  out="$(curl -sS --max-time 30 -H "Authorization: Bearer $tok" \
    https://ghcr.io/v2/ctxmesh/charts/ctxmesh/tags/list 2>/dev/null | python3 -c "
import json,sys
try: print('ok' if '$want' in json.load(sys.stdin).get('tags',[]) else 'MISSING')
except Exception: print('ERROR')" 2>/dev/null || echo ERROR)"
  record "Helm chart" "${out:-ERROR}" "$want"
else
  record "Helm chart" "ERROR" "could not obtain an anonymous ghcr pull token"
fi

# ── report ────────────────────────────────────────────────────────────────────
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Release truth — \`$want\`"
    echo
    echo "| Artifact | Status | Version served |"
    echo "|---|---|---|"
    printf '%b' "$rows"
    echo
    [ "$rc" = "0" ] && echo "All seven artifacts published." \
                    || echo "**Some artifacts are missing — this release is partial.**"
  } >> "$GITHUB_STEP_SUMMARY"
fi

if [ "$rc" != "0" ]; then
  echo "FAIL: $want is not fully published — see the MISSING rows above" >&2
  exit 1
fi
echo "PASS: all seven artifacts serve $want"
