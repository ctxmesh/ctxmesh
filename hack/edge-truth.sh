#!/usr/bin/env bash
# edge-truth.sh — assert what the platform-edge chart (deploy/edge) renders. Render-only, no cluster.
#
# The console's MCP consent page trusts an opener at an agent hostname (ADR 0040 amendment). That is
# sound only while no tenant-controlled document is served there, and the one route that reaches a
# tenant's container is `agents` (/invoke). So that route must answer with
# `Content-Security-Policy: sandbox` (an opaque origin, no script) and `X-Content-Type-Options:
# nosniff`, with ext-auth on or off, and the chatbox route (`agents-app`) must not, or the chatbox
# cannot run.
set -euo pipefail
cd "$(dirname "$0")/.."

CHART="${1:-deploy/edge}"
[ -d "$CHART" ] || { echo "FAIL: no chart at $CHART" >&2; exit 1; }

helm lint "$CHART" >/dev/null || { helm lint "$CHART" >&2; echo "FAIL: helm lint $CHART" >&2; exit 1; }
echo "  ok: helm lint $CHART"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

render() { # <name> [helm args...]
  local name="$1"; shift
  helm template platform-edge "$CHART" "$@" > "$tmp/$name.yaml" \
    || { echo "FAIL: $CHART does not template with: $*" >&2; exit 1; }
}
render default
render noauth --set extAuth.enabled=false
render prod --set baseDomain=agents.example.com --set tls.enabled=true --set dns.externalDns=true

for r in default noauth prod; do
  python3 - "$tmp/$r.yaml" "$r" <<'PY'
import sys
import yaml

path, label = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(path)) if d]
routes = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "HTTPRoute"}

def fail(msg):
    print(f"FAIL ({label}): {msg}", file=sys.stderr)
    sys.exit(1)

def response_sets(route):
    """Per rule, the headers a ResponseHeaderModifier sets, lower-cased name -> value."""
    out = []
    for rule in route["spec"]["rules"]:
        got = {}
        for f in rule.get("filters") or []:
            if f.get("type") == "ResponseHeaderModifier":
                mod = f.get("responseHeaderModifier") or {}
                if mod.get("add"):
                    fail(f"route {route['metadata']['name']} adds response headers; it must set them, so the agent cannot send its own")
                for h in mod.get("set") or []:
                    got[h["name"].lower()] = h["value"]
        out.append(got)
    return out

for name in ("agents", "agents-app"):
    if name not in routes:
        fail(f"no {name} HTTPRoute in the render")

for i, got in enumerate(response_sets(routes["agents"])):
    if got.get("content-security-policy") != "sandbox":
        fail(f"agents rule {i} does not set Content-Security-Policy: sandbox (got {got.get('content-security-policy')!r}); "
             "a tenant's response at an agent hostname would be a document with that origin")
    if got.get("x-content-type-options") != "nosniff":
        fail(f"agents rule {i} does not set X-Content-Type-Options: nosniff")

for i, got in enumerate(response_sets(routes["agents-app"])):
    if "content-security-policy" in got:
        fail(f"agents-app rule {i} sets a Content-Security-Policy; the chatbox SPA needs its scripts and the BFF sets its own CSP")

print(f"  ok ({label}): agents sets CSP sandbox + nosniff; agents-app does not")
PY
done
echo "RESULT: PASS (edge-truth)"
