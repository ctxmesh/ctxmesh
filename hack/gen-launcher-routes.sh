#!/usr/bin/env bash
# gen-launcher-routes — emit the launcher's agent-facing route table as JSON.
#
# This file is the single source of truth every SDK is measured against. It exists because the
# previous gate grepped for route SUBSTRINGS in SDK source and was defeated three ways at once:
# it truncated paths to two segments (so `/memory/agent/remember` became `/memory/agent`, a path
# the launcher never serves, and four SDKs were written against that fiction); it dropped every
# `{param}` route; and a literal in a COMMENT satisfied it, so an SDK replaced by six lines of
# comments passed with "10 launcher routes reachable from all 6 SDKs".
#
# Emitted as a committed fixture rather than computed at check time so that a launcher change
# which the SDKs have not followed shows up as a DIFF a human reads, not as a silently smaller
# route set. `--check` proves the committed copy still matches the source.
set -euo pipefail
cd "$(dirname "$0")/.."

LAUNCHER="cmd/launcher"
OUT="sdk/launcher-routes.json"

# Test files are excluded by NAME. The previous filter was a `grep -v '_test'` on grep -h output, which
# carries no filenames, so it filtered nothing, and a test's fake BFF read as launcher routes.
routes="$(grep -rhoE --exclude='*_test.go' 'mux\.Handle(Func)?\("(GET |POST |PUT |DELETE |PATCH )?[^"]+"' "$LAUNCHER"/*.go \
  | sed -E 's/.*mux\.Handle(Func)?\("//; s/"$//' \
  | grep -v '/healthz' \
  | sort -u)"
[ -n "$routes" ] || { echo "FAIL: read no routes out of $LAUNCHER" >&2; exit 1; }

# The script owns each route's IDENTITY only (method, path, listener, portEnv, defaultPort). Every
# other key (the SDK tier that owes the route, waivers, schemas, goldens) is written by hand and
# carried across a regeneration by route key, so a new route lands with nothing hand-written and a
# removed route takes its keys with it.
json="$(ROUTES="$routes" OUT="$OUT" python3 - <<'PY'
import json, os
existing = {}
if os.path.exists(os.environ["OUT"]):
    with open(os.environ["OUT"]) as f:
        existing = json.load(f)
by_key = {(r["method"], r["path"]): r for r in existing.get("routes", [])}
identity = ("method", "path", "listener", "portEnv", "defaultPort")
out = []
for line in os.environ["ROUTES"].strip().splitlines():
    line = line.strip()
    if not line:
        continue
    parts = line.split(" ", 1)
    if len(parts) == 2 and parts[0] in ("GET", "POST", "PUT", "DELETE", "PATCH"):
        method, path = parts
    else:
        # feedback.go registers without a method prefix: any method reaches it.
        method, path = "ANY", parts[-1]
    # Routes reach DIFFERENT listeners. Putting /delegate and /handoff on the memory port is
    # exactly the defect this fixture exists to make impossible.
    if path.startswith(("/amp/", "/a2a/")):
        listener, env, port = "amp", "A2A_PORT", 2997
    elif path in ("/delegate", "/handoff"):
        listener, env, port = "delegate", "DELEGATE_PORT", 2994
    elif path == "/feedback":
        listener, env, port = "feedback", "FEEDBACK_PORT", 2995
    elif path in ("/tools", "/events"):
        # Tool discovery binds a FIXED port that no env var names, so portEnv is null rather than
        # an invented variable an SDK would read and the launcher would ignore.
        listener, env, port = "discovery", None, 2999
    else:
        listener, env, port = "memory", "MEMORY_PORT", 2998   # memory, knowledge and skills share one mux
    route = {"method": method, "path": path, "listener": listener, "portEnv": env, "defaultPort": port}
    for k, v in by_key.get((method, path), {}).items():
        if k not in identity:
            route[k] = v
    out.append(route)
out.sort(key=lambda r: (r["path"], r["method"]))
doc = {k: v for k, v in existing.items() if k != "routes"}
result = {}
for k in ("description",):
    if k in doc:
        result[k] = doc.pop(k)
result["routes"] = out
result.update(doc)
print(json.dumps(result, indent=2, ensure_ascii=False))
PY
)"

if [ "${1:-}" = "--check" ]; then
  [ -f "$OUT" ] || { echo "FAIL: $OUT is missing — run $0 to create it" >&2; exit 1; }
  if ! diff -u "$OUT" <(printf '%s\n' "$json") > /tmp/routes.diff 2>&1; then
    echo "FAIL: $OUT is stale — the launcher's routes changed and the fixture did not." >&2
    sed 's/^/  /' /tmp/routes.diff >&2
    echo "      Run hack/gen-launcher-routes.sh, then make every SDK cover the new route." >&2
    exit 1
  fi
  echo "PASS: $OUT matches the launcher ($(python3 -c "import json;print(len(json.load(open('$OUT'))['routes']))") routes)"
  exit 0
fi

printf '%s\n' "$json" > "$OUT"
echo "wrote $OUT ($(python3 -c "import json;print(len(json.load(open('$OUT'))['routes']))") routes)"
