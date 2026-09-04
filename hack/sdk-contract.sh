#!/usr/bin/env bash
# sdk-contract — the SDKs must track the product: same wire contract, same version.
#
# WHY THIS EXISTS
# ---------------
# The SDKs are typed sugar over the launcher's localhost plane. Their entire value is being
# a FAITHFUL mirror of it, which makes drift uniquely expensive: a drifted SDK tells an
# author a capability exists, or is shaped a certain way, when the platform disagrees — and
# the failure lands in the user's agent at runtime, never in our CI.
#
# Nothing bound the two together. The launcher's routes and the SDKs' clients were kept in
# step by hand, and an audit on 2026-09-04 found what hand-keeping always eventually finds:
# `POST /a2a/{targetAgent}` — the M6 agent-mesh OUTBOUND call, served by the launcher and
# meant to be called by the agent — is exposed by NEITHER SDK. An author who wants an
# agent-to-agent call has to hand-roll an HTTP POST to a localhost port the SDK does not
# even tell them about.
#
# It also checks VERSION coherence. The SDKs ship at the product's version (ADR 0135), and
# the Python version is declared in two places that can silently disagree.
#
# Static, no cluster, milliseconds. Same shape as install-truth.sh (M148), provider-parity.sh
# (M153) and release-truth.sh (M154).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PY_SRC="$ROOT/sdk/python/src/ctxmesh"
TS_SRC="$ROOT/sdk/typescript/src"
LAUNCHER="$ROOT/cmd/launcher"

for d in "$PY_SRC" "$TS_SRC" "$LAUNCHER"; do
  [ -d "$d" ] || { echo "FAIL: $d is missing"; exit 1; }
done

rc=0
bad() { echo "FAIL: $*" >&2; rc=1; }

# ── 1. every localhost route the launcher serves is reachable from both SDKs ──
#
# WAIVERS carry a reason and a card. A waiver is a tracked gap, not a silenced check: the
# point of writing it down here is that the next person reads it in the same breath as the
# rule it bends.
#
#   /healthz — the launcher liveness probe. Not an agent-facing capability.
#
# /a2a was waived here until M156, when both SDKs gained client.mesh. The waiver is gone,
# which is the point of writing waivers down: it was a tracked gap with an owner, not a
# silenced check.
waived() {
  case "$1" in
    /healthz) return 0 ;;
    *) return 1 ;;
  esac
}

routes="$(grep -rhoE 'mux\.HandleFunc\("(GET |POST |PUT |DELETE |PATCH )?/[a-zA-Z0-9/_{}-]*"' "$LAUNCHER" \
  | sed -E 's/.*HandleFunc\("//; s/"$//; s/^(GET|POST|PUT|DELETE|PATCH) //' \
  | sed -E 's#^(/[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)?).*#\1#' \
  | grep -vE '\{' | sort -u)"

[ -n "$routes" ] || bad "read no routes out of $LAUNCHER — the gate is not actually checking anything"

checked=0
while read -r route; do
  [ -n "$route" ] || continue
  if waived "$route"; then
    echo "  waived: $route (see the waiver note in this script)"
    continue
  fi
  checked=$((checked + 1))
  grep -rqF -- "$route" "$PY_SRC" \
    || bad "the launcher serves '$route' and the PYTHON SDK never calls it — a platform capability with no SDK surface"
  grep -rqF -- "$route" "$TS_SRC" \
    || bad "the launcher serves '$route' and the TYPESCRIPT SDK never calls it — a platform capability with no SDK surface"
done <<< "$routes"
[ "$checked" -gt 0 ] || bad "no routes were actually checked"
[ "$rc" = "0" ] && echo "  ok: $checked launcher routes reachable from both SDKs"

# ── 2. the two SDKs stay at parity on the plane ───────────────────────────────
# A capability landing in one language only is a parity regression. Compare the module sets;
# the known, carded exceptions are named rather than hidden.
#   _http    — Python's transport helper; TS uses fetch. An implementation detail.
#   index    — the TS package entry point. Python uses __init__.
#
# `testing` was excluded here while the offline fakes were Python-only (m52.O13). M156
# shipped the TypeScript ones as the `ctxmesh/testing` subpath, so the exclusion is gone and
# the two are compared like every other module — which is the whole point of closing a
# parity gap rather than tolerating it.
py_mods="$(cd "$PY_SRC" && find . -maxdepth 1 -name "*.py" | sed 's#\./##; s#\.py$##' | grep -vE '^(__init__|_http)$' | sort)"
ts_mods="$(cd "$TS_SRC" && find . -maxdepth 1 -name "*.ts" | sed 's#\./##; s#\.ts$##' | grep -vE '^(index)$' | sort)"
if [ "$py_mods" != "$ts_mods" ]; then
  bad "the SDKs have diverged on modules (a capability in one language only):"
  diff <(echo "$py_mods") <(echo "$ts_mods") | sed 's/^/    /' >&2 || true
else
  echo "  ok: the two SDKs expose the same module set"
fi

# ── 2b. the offline FAKES are at parity, stub by stub ────────────────────────
# Module-set parity (check 2) is too coarse to see this: both languages have a `testing`
# module, so a stub present in one and missing in the other is invisible to it. That is not
# hypothetical — TypeScript shipped a DelegateStub that Python never had, and the module
# check was perfectly happy (found by the M156 design review).
#
# The fakes are the surface an author tests against, so an asymmetry here means an agent
# testable in one language and not the other.
py_stubs="$(grep -oE '^class ([A-Za-z]+)Stub\(_BaseStub\)' "$PY_SRC/testing.py" \
  | sed -E 's/^class //; s/\(_BaseStub\)//' | sort -u)"
ts_stubs="$(grep -oE '^export class ([A-Za-z]+)Stub extends BaseStub' "$TS_SRC/testing.ts" \
  | sed -E 's/^export class //; s/ extends BaseStub//' | sort -u)"
if [ -z "$py_stubs" ] || [ -z "$ts_stubs" ]; then
  bad "could not read the stub sets out of both testing modules — the gate is not actually checking anything"
elif [ "$py_stubs" != "$ts_stubs" ]; then
  bad "the offline fakes have diverged (a stub an author can use in one language only):"
  diff <(echo "$py_stubs") <(echo "$ts_stubs") | sed 's/^/    /' >&2 || true
else
  echo "  ok: both testing modules expose the same stubs"
fi

# ── 3. version coherence ──────────────────────────────────────────────────────
# The SDKs ship at the PRODUCT's version (ADR 0135). Python declares it twice — in
# pyproject.toml and in __init__.py — and nothing kept them equal.
py_proj="$(grep -E '^version *= *"' "$ROOT/sdk/python/pyproject.toml" | head -1 | sed -E 's/.*"(.*)".*/\1/')"
py_init="$(grep -E '^__version__ *= *"' "$PY_SRC/__init__.py" | head -1 | sed -E 's/.*"(.*)".*/\1/')"
ts_pkg="$(python3 -c "import json;print(json.load(open('$ROOT/sdk/typescript/package.json'))['version'])")"
[ "$py_proj" = "$py_init" ] \
  || bad "python pyproject version ($py_proj) != ctxmesh.__version__ ($py_init) — an installed SDK would misreport itself"
[ "$py_proj" = "$ts_pkg" ] \
  || bad "python ($py_proj) and typescript ($ts_pkg) SDK versions differ — they ship together, at one version"
[ "$rc" = "0" ] && echo "  ok: both SDKs declare $py_proj, consistently"

# ── 4. the release publishes them at the PRODUCT's tag ────────────────────────
# ADR 0135: no separate sdk-v* namespace. A user must never have to work out which SDK
# release pairs with which cluster.
WF="$ROOT/.github/workflows/sdk-publish.yml"
if [ -f "$WF" ]; then
  # Read the TRIGGERS, not the prose around them. A gate that fails because a comment
  # mentions the thing it forbids is a gate people learn to ignore (the M150 lesson: a
  # comment beginning with the linter's own name was parsed as a directive).
  triggers="$(sed 's/#.*//' "$WF" | grep -E '^\s*tags:')"
  echo "$triggers" | grep -q 'sdk-v\*' \
    && bad "sdk-publish.yml still triggers on a separate sdk-v* tag — the SDKs ship at the product version (ADR 0135)"
  echo "$triggers" | grep -qE '\["v\*"\]' \
    || bad "sdk-publish.yml does not trigger on the product's v* tag — the SDKs would never be released with a release"
  [ "$rc" = "0" ] && echo "  ok: the SDKs publish on the product's own tag"
fi

[ "$rc" = "0" ] || exit 1
echo "PASS: the SDKs track the product (contract, parity, and version)"
