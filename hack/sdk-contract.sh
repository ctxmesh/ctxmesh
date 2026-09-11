#!/usr/bin/env bash
# sdk-contract — the SDKs must track the product: same wire contract, same version.
#
# An SDK's value is being a FAITHFUL mirror of the launcher's plane, which makes drift uniquely
# expensive: it tells an author a capability exists, or is shaped a certain way, when the
# platform disagrees — and the failure lands in the user's agent at runtime, never in CI. An
# audit on 2026-09-04 found exactly that: `POST /a2a/{targetAgent}` was served by the launcher
# and exposed by neither SDK.
#
# CONFORMANCE IS TIERED (ADR 0139). Every SDK declares its tier in `sdk/<lang>/.sdk-tier`:
# `plane-client` (every launcher route, typed errors, config, transport — the floor in every
# language) or `authoring` (that plus the managed loop, tools, model, serve, tracing).
#
# The route check applies at BOTH tiers: a launcher capability with no surface in some shipped
# SDK is a failure however thin that SDK is meant to be. Module-set and stub parity are compared
# only among authoring SDKs — a plane client is not supposed to have a managed loop, and failing
# it for that would make the tier meaningless.
#
# An undeclared or unknown tier is a HARD failure. Treating unknown as the weaker rule set is
# how a gate starts passing vacuously.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LAUNCHER="$ROOT/cmd/launcher"
[ -d "$LAUNCHER" ] || { echo "FAIL: $LAUNCHER is missing"; exit 1; }

rc=0
bad() { echo "FAIL: $*" >&2; rc=1; }

# Where each language keeps its source and declares its version. Adding a language means adding
# a case here plus a .sdk-tier file — nothing else.
src_dir() {
  case "$1" in
    python) echo "sdk/python/src/ctxmesh" ;;
    typescript) echo "sdk/typescript/src" ;;
    go) echo "sdk/go" ;;
    java) echo "sdk/java/src/main/java" ;;
    rust) echo "sdk/rust/src" ;;
    ruby) echo "sdk/ruby/lib" ;;
    *) echo "" ;;
  esac
}
declared_version() {
  case "$1" in
    python)     grep -E '^version *= *"' "$ROOT/sdk/python/pyproject.toml" | head -1 | sed -E 's/.*"(.*)".*/\1/' ;;
    typescript) python3 -c "import json;print(json.load(open('$ROOT/sdk/typescript/package.json'))['version'])" ;;
    go)         grep -E '^const Version = "' "$ROOT/sdk/go/version.go" 2>/dev/null | sed -E 's/.*"(.*)".*/\1/' ;;
    java)       python3 -c "
import re
t=open('$ROOT/sdk/java/pom.xml').read()
m=re.search(r'<artifactId>ctxmesh</artifactId>\s*<version>([^<]+)</version>', t)
print(m.group(1) if m else '')" 2>/dev/null ;;
    rust)       grep -E '^version *= *"' "$ROOT/sdk/rust/Cargo.toml" 2>/dev/null | head -1 | sed -E 's/.*"(.*)".*/\1/' ;;
    ruby)       grep -E 'VERSION *= *"' "$ROOT/sdk/ruby/lib/ctxmesh/version.rb" 2>/dev/null | head -1 | sed -E 's/.*"(.*)".*/\1/' ;;
    *) echo "" ;;
  esac
}

# ── discover the SDKs and their declared tiers ───────────────────────────────
sdks=""
for d in "$ROOT"/sdk/*/; do
  lang="$(basename "$d")"
  if [ ! -f "$d.sdk-tier" ]; then
    bad "sdk/$lang has no .sdk-tier — every SDK declares 'plane-client' or 'authoring' (ADR 0139)"
    continue
  fi
  tier="$(tr -d '[:space:]' < "$d.sdk-tier")"
  case "$tier" in
    plane-client|authoring) ;;
    *) bad "sdk/$lang declares tier '$tier', which is not a tier — use plane-client or authoring"; continue ;;
  esac
  s="$(src_dir "$lang")"
  [ -n "$s" ] || { bad "sdk/$lang has no layout in this gate — add it to src_dir() and declared_version()"; continue; }
  [ -d "$ROOT/$s" ] || { bad "sdk/$lang declares tier $tier but $s does not exist"; continue; }
  sdks="$sdks$lang:$tier:$s"$'\n'
done
sdks="$(printf '%s' "$sdks" | sed '/^$/d')"
[ -n "$sdks" ] || { echo "FAIL: found no SDKs — the gate is not checking anything"; exit 1; }
n_sdks="$(wc -l <<< "$sdks" | tr -d ' ')"

# ── 1. the committed route fixture still matches the launcher ────────────────
# The previous version of this check grepped the SDK source for route SUBSTRINGS and was
# defeated three ways: it truncated paths to two segments (so /memory/agent/remember became
# /memory/agent, a path the launcher never serves — and four SDKs were written against that
# fiction), it dropped every {param} route, and a literal in a COMMENT satisfied it. An SDK
# replaced by six lines of comments passed with "10 launcher routes reachable from all 6 SDKs".
#
# Route COVERAGE is no longer asserted here at all. A static grep cannot tell a call from a
# comment, so each SDK's test suite proves it by driving a fake launcher built from this same
# fixture, which 404s anything unregistered. This gate's job is only to keep the fixture honest.
./hack/gen-launcher-routes.sh --check || rc=1

# ── 2. authoring SDKs stay at parity with each other ─────────────────────────
n_auth="$(grep -c ':authoring:' <<< "$sdks" || true)"
if [ "${n_auth:-0}" -ge 2 ]; then
  py_mods="$(cd "$ROOT/sdk/python/src/ctxmesh" && find . -maxdepth 1 -name "*.py" | sed 's#\./##; s#\.py$##' | grep -vE '^(__init__|_http)$' | sort)"
  ts_mods="$(cd "$ROOT/sdk/typescript/src" && find . -maxdepth 1 -name "*.ts" | sed 's#\./##; s#\.ts$##' | grep -vE '^(index)$' | sort)"
  if [ "$py_mods" != "$ts_mods" ]; then
    bad "the authoring SDKs' module sets have diverged:"
    diff <(echo "$py_mods") <(echo "$ts_mods") | sed 's/^/    /' >&2 || true
  else
    echo "  ok: the authoring SDKs expose the same module set"
  fi

  py_stubs="$(grep -oE '^class ([A-Za-z]+)Stub\(_BaseStub\)' "$ROOT/sdk/python/src/ctxmesh/testing.py" | sed -E 's/^class ([A-Za-z]+)Stub.*/\1/' | sort)"
  ts_stubs="$(grep -oE '^export class ([A-Za-z]+)Stub extends BaseStub' "$ROOT/sdk/typescript/src/testing.ts" | sed -E 's/^export class ([A-Za-z]+)Stub.*/\1/' | sort)"
  if [ -z "$py_stubs" ] || [ -z "$ts_stubs" ]; then
    bad "could not read the stub sets out of both testing modules — the gate is not actually checking anything"
  elif [ "$py_stubs" != "$ts_stubs" ]; then
    bad "the offline fakes have diverged (a stub an author can use in one language only):"
    diff <(echo "$py_stubs") <(echo "$ts_stubs") | sed 's/^/    /' >&2 || true
  else
    echo "  ok: both authoring testing modules expose the same stubs"
  fi
fi

# ── 3. every SDK ships at the PRODUCT's version (ADR 0135) ───────────────────
# The old check compared the SDKs only to EACH OTHER, so uniform staleness was indistinguishable
# from correctness: it printed "all 6 SDKs declare 0.1.0-beta.1, consistently" while the product
# was on beta.3, three releases later, and that output was quoted as proof.
#
# The distinction it erased: release stamps python and typescript from the tag, so their
# committed value is genuinely decorative. NOTHING stamps go, java, rust or ruby — their
# committed constant is what a user gets. One rule cannot cover both.
STAMPED_AT_RELEASE="python typescript"
product="$(grep -m1 -oE '^## v[0-9][^ ]*' "$ROOT/CHANGELOG.md" | sed 's/^## v//')"
[ -n "$product" ] || bad "could not read the product version from CHANGELOG.md — the gate is not actually checking anything"

while IFS=: read -r lang tier s; do
  [ -n "$lang" ] || continue
  v="$(declared_version "$lang" 2>/dev/null || true)"
  [ -n "$v" ] || { bad "sdk/$lang declares no version where this gate looks — see declared_version()"; continue; }
  if grep -qw -- "$lang" <<< "$STAMPED_AT_RELEASE"; then
    # May lag: the release rewrites it. It must still agree with the other stamped SDKs, or the
    # stamping step is papering over a real divergence.
    [ -z "${stamped_ver:-}" ] && stamped_ver="$v" && stamped_lang="$lang"
    [ "$v" = "$stamped_ver" ] \
      || bad "$lang declares $v and $stamped_lang declares $stamped_ver — the stamped SDKs must still agree"
  else
    # Shipped verbatim. Anything but the product version is what the user installs.
    [ "$v" = "$product" ] \
      || bad "sdk/$lang declares $v but the product is $product, and NOTHING stamps $lang at release — a user would install a version that lies about itself"
  fi
done <<< "$sdks"

py_init="$(grep -E '^__version__ *= *"' "$ROOT/sdk/python/src/ctxmesh/__init__.py" | head -1 | sed -E 's/.*"(.*)".*/\1/')"
[ "$(declared_version python)" = "$py_init" ] \
  || bad "python pyproject version ($(declared_version python)) != ctxmesh.__version__ ($py_init) — an installed SDK would misreport itself"
[ "$rc" = "0" ] && echo "  ok: unstamped SDKs are at the product version ($product); stamped SDKs agree at $stamped_ver"

# ── 4. the SDKs publish on the product's tag, not their own ──────────────────
grep -qE '^\s+tags: \["sdk-v\*"\]' "$ROOT/.github/workflows/sdk-publish.yml" 2>/dev/null \
  && bad "sdk-publish.yml still triggers on a separate sdk-v* tag — the SDKs ship at the product version (ADR 0135)"
[ "$rc" = "0" ] && echo "  ok: the SDKs publish on the product's own tag"

[ "$rc" = "0" ] || exit 1
echo "PASS: the SDKs track the product (routes at every tier, authoring parity, one version)"
