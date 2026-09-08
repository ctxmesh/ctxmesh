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

# ── 1. every launcher route is reachable from EVERY SDK, at every tier ───────
# /healthz is waived: the launcher's liveness probe, not an agent-facing capability.
routes="$(grep -rhoE 'mux\.HandleFunc\("(GET |POST |PUT |DELETE |PATCH )?/[a-zA-Z0-9/_{}-]*"' "$LAUNCHER" \
  | sed -E 's/.*HandleFunc\("//; s/"$//; s/^(GET|POST|PUT|DELETE|PATCH) //' \
  | sed -E 's#^(/[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)?).*#\1#' \
  | grep -vE '\{' | grep -v '^/healthz$' | sort -u)"
[ -n "$routes" ] || bad "read no routes out of $LAUNCHER — the gate is not actually checking anything"

checked=0
while IFS=: read -r lang tier s; do
  [ -n "$lang" ] || continue
  while read -r route; do
    [ -n "$route" ] || continue
    grep -rqF -- "$route" "$ROOT/$s" \
      || bad "the launcher serves '$route' and the $lang SDK ($tier) never calls it — a platform capability with no SDK surface"
    checked=$((checked + 1))
  done <<< "$routes"
done <<< "$sdks"
[ "$checked" -gt 0 ] || bad "no routes were actually checked"
[ "$rc" = "0" ] && echo "  ok: $(wc -l <<< "$routes" | tr -d ' ') launcher routes reachable from all $n_sdks SDKs (/healthz waived)"

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

# ── 3. every SDK ships at ONE version (ADR 0135) ─────────────────────────────
first_lang=""; first_ver=""
while IFS=: read -r lang tier s; do
  [ -n "$lang" ] || continue
  v="$(declared_version "$lang" 2>/dev/null || true)"
  [ -n "$v" ] || { bad "sdk/$lang declares no version where this gate looks — see declared_version()"; continue; }
  if [ -z "$first_lang" ]; then first_lang="$lang"; first_ver="$v"
  elif [ "$v" != "$first_ver" ]; then
    bad "$lang declares $v and $first_lang declares $first_ver — the SDKs ship together, at one version"
  fi
done <<< "$sdks"

# Python declares it twice and the two can silently disagree.
py_init="$(grep -E '^__version__ *= *"' "$ROOT/sdk/python/src/ctxmesh/__init__.py" | head -1 | sed -E 's/.*"(.*)".*/\1/')"
[ "$(declared_version python)" = "$py_init" ] \
  || bad "python pyproject version ($(declared_version python)) != ctxmesh.__version__ ($py_init) — an installed SDK would misreport itself"
[ "$rc" = "0" ] && echo "  ok: all $n_sdks SDKs declare $first_ver, consistently"

# ── 4. the SDKs publish on the product's tag, not their own ──────────────────
grep -qE '^\s+tags: \["sdk-v\*"\]' "$ROOT/.github/workflows/sdk-publish.yml" 2>/dev/null \
  && bad "sdk-publish.yml still triggers on a separate sdk-v* tag — the SDKs ship at the product version (ADR 0135)"
[ "$rc" = "0" ] && echo "  ok: the SDKs publish on the product's own tag"

[ "$rc" = "0" ] || exit 1
echo "PASS: the SDKs track the product (routes at every tier, authoring parity, one version)"
