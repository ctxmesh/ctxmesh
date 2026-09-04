#!/usr/bin/env bash
# flow-completability — every console flow must be completable by at least one shipped role.
#
# WHY THIS EXISTS
# ---------------
# The BFF writes with the CALLER's client (ADR 0011), so the API server makes the
# authorization decision. That is right — and it means a flow is only real if some shipped
# role actually grants the permission the flow needs. Nothing checked that, and the result
# was four flows no shipped persona could complete:
#
#   connect a provider · register a key-authed MCP server ·
#   complete an MCP OAuth grant · store an org-level MCP credential
#
# All four write a core Secret, and no shipped role granted `secrets: create`. They were
# found ONE AT A TIME over two months (m52.R10 in September, m52.G9 in August) and carded as
# separate bugs, because nothing connected them. Two were never carded at all.
#
# So this gate checks the CLASS. It reads the resources the BFF actually writes with the
# caller's client and asserts each is granted by at least one shipped ClusterRole — and it
# fails when the BFF grows a new caller-scoped write the allowlist does not know about, so
# the fifth flow cannot be discovered by a user in production.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BFF="$ROOT/internal/bff"
RBAC="$ROOT/config/rbac"

[ -d "$BFF" ] || { echo "FAIL: $BFF is missing"; exit 1; }
[ -d "$RBAC" ] || { echo "FAIL: $RBAC is missing"; exit 1; }

# The core/networking kinds the BFF is KNOWN to write with the caller's client, and the verb
# that makes the flow work. Kept explicit rather than inferred: the point of the list is that
# adding a row is a conscious act, and the drift check below makes it a required one.
#
#   Secret         create  — every credential flow (provider connect, MCP key, OAuth grant, org cred)
#   NetworkPolicy  create  — MCP-server registration's egress policy (m52.G9 first half)
#
# PodLogOptions and IPBlock are request/field types, not resources, and are excluded below.
declare -a FLOWS=(
  "secrets:create"
  "networkpolicies:create"
)

rc=0
bad() { echo "FAIL: $*" >&2; rc=1; }

# ── 1. every known flow is completable by at least one shipped role ───────────
for entry in "${FLOWS[@]}"; do
  res="${entry%%:*}"; verb="${entry##*:}"
  holder="$(python3 - "$RBAC" "$res" "$verb" <<'PY'
import glob, os, sys, yaml
rbac_dir, res, verb = sys.argv[1], sys.argv[2], sys.argv[3]
for path in sorted(glob.glob(os.path.join(rbac_dir, "*.yaml"))):
    for doc in yaml.safe_load_all(open(path)):
        if not doc or doc.get("kind") not in ("Role", "ClusterRole"):
            continue
        # The controller role is not a human persona. A flow completable only by the
        # controller is not completable by a user, which is the whole point of this gate.
        if doc["metadata"]["name"] in ("manager-role", "metrics-auth-role", "metrics-reader"):
            continue
        for rule in doc.get("rules") or []:
            if res in (rule.get("resources") or []) and verb in (rule.get("verbs") or []):
                print(doc["metadata"]["name"]); raise SystemExit
PY
)"
  if [ -z "$holder" ]; then
    bad "no shipped role grants '$verb $res' — the console flows that need it are uncompletable by every persona"
  else
    echo "  ok: $verb $res -> ClusterRole/$holder"
  fi
done

# ── 2. the BFF has not grown a caller-scoped write the list does not know ─────
# This is the drift half, and the reason the gate is worth having: without it the list is a
# snapshot of what someone once audited, and the fifth flow ships unnoticed.
found="$(find "$BFF" -maxdepth 1 -name '*.go' ! -name '*_test.go' -print0 \
  | xargs -0 grep -ohE '&(corev1|networkingv1)\.[A-Z][A-Za-z]+\{' \
  | sed -E 's/&(corev1|networkingv1)\.//; s/\{$//' | sort -u)"

[ -n "$found" ] || bad "read no constructed kinds out of $BFF — the gate is not actually checking anything"

# Request/field types, not API resources: they are never created against the API server.
not_a_resource() {
  case "$1" in PodLogOptions|PodList|IPBlock|ObjectReference|LocalObjectReference) return 0 ;; *) return 1 ;; esac
}
# Kind -> resource plural, for the kinds this gate governs.
plural() {
  case "$1" in
    Secret) echo secrets ;;
    NetworkPolicy) echo networkpolicies ;;
    Namespace) echo namespaces ;;
    *) echo "" ;;
  esac
}
while read -r kind; do
  [ -n "$kind" ] || continue
  not_a_resource "$kind" && continue
  p="$(plural "$kind")"
  if [ -z "$p" ]; then
    bad "the BFF constructs a $kind and this gate has no mapping for it — add it to FLOWS (with the verb the flow needs) or to the not-a-resource list, deliberately"
    continue
  fi
  printf '%s\n' "${FLOWS[@]}" | grep -q "^$p:" \
    || bad "the BFF writes '$p' with the caller's client and it is not in FLOWS — a new flow whose completability nobody checked"
done <<< "$found"

[ "$rc" = "0" ] || exit 1
echo "PASS: every caller-scoped BFF write is completable by a shipped role"
