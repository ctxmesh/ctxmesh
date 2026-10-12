#!/usr/bin/env bash
# edge-truth.sh — assert what the platform-edge chart (deploy/edge) renders. Render-only, no cluster.
#
# The console's MCP consent page trusts an opener at an agent hostname (ADR 0040 amendment). That is
# sound only while no tenant-controlled document is served there, and the one route that reaches a
# tenant's container is `agents` (/invoke). So that route must answer with
# `Content-Security-Policy: sandbox` (an opaque origin, no script) and `X-Content-Type-Options:
# nosniff`, and must drop the agent's Set-Cookie (a sandbox does not stop the browser storing one,
# and the OAuth binding cookie lives on, or next to, the agent host), with ext-auth on or off. The
# chatbox route (`agents-app`) must not be sandboxed, or the chatbox cannot run.
#
# The same route must drop, from the request, the caller's bearer and every header that claims to be
# an agent (ADR 0153): the AMP envelope under both names and the attestation. A registry member admits
# an attested AMP call, so anything outside that could send them could claim to be one of its peers.
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

def request_removed(route):
    """Per rule, the lower-cased header names a RequestHeaderModifier removes."""
    out = []
    for rule in route["spec"]["rules"]:
        removed = set()
        for f in rule.get("filters") or []:
            if f.get("type") == "RequestHeaderModifier":
                removed |= {n.lower() for n in (f.get("requestHeaderModifier") or {}).get("remove") or []}
        out.append(removed)
    return out

def response_mods(route):
    """Per rule, (headers a ResponseHeaderModifier sets as lower-cased name -> value, lower-cased
    names it removes)."""
    out = []
    for rule in route["spec"]["rules"]:
        got, removed = {}, set()
        for f in rule.get("filters") or []:
            if f.get("type") == "ResponseHeaderModifier":
                mod = f.get("responseHeaderModifier") or {}
                if mod.get("add"):
                    fail(f"route {route['metadata']['name']} adds response headers; it must set them, so the agent cannot send its own")
                for h in mod.get("set") or []:
                    got[h["name"].lower()] = h["value"]
                removed |= {n.lower() for n in mod.get("remove") or []}
        out.append((got, removed))
    return out

for name in ("agents", "agents-app"):
    if name not in routes:
        fail(f"no {name} HTTPRoute in the render")

for i, (got, removed) in enumerate(response_mods(routes["agents"])):
    if got.get("content-security-policy") != "sandbox":
        fail(f"agents rule {i} does not set Content-Security-Policy: sandbox (got {got.get('content-security-policy')!r}); "
             "a tenant's response at an agent hostname would be a document with that origin")
    if got.get("x-content-type-options") != "nosniff":
        fail(f"agents rule {i} does not set X-Content-Type-Options: nosniff")
    if "set-cookie" not in removed:
        fail(f"agents rule {i} does not remove Set-Cookie; a sandboxed response can still plant a cookie, "
             "such as an OAuth binding cookie, at or beside the agent host")

must_strip = {"authorization", "x-amp-envelope", "x-a2a-envelope", "x-amp-attestation"}
for i, removed in enumerate(request_removed(routes["agents"])):
    missing = sorted(must_strip - removed)
    if missing:
        fail(f"agents rule {i} forwards {', '.join(missing)} from the caller to the agent; a caller outside "
             "the mesh could then present a bearer the agent must not see, or claim to be one of its AMP peers")

for i, (got, _) in enumerate(response_mods(routes["agents-app"])):
    if "content-security-policy" in got:
        fail(f"agents-app rule {i} sets a Content-Security-Policy; the chatbox SPA needs its scripts and the BFF sets its own CSP")

print(f"  ok ({label}): agents sets CSP sandbox + nosniff, drops Set-Cookie, and strips the bearer and the AMP "
      "headers from requests; agents-app is not sandboxed")
PY
done
echo "RESULT: PASS (edge-truth)"
