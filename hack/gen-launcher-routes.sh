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

routes="$(grep -rhoE 'mux\.Handle(Func)?\("(GET |POST |PUT |DELETE |PATCH )?[^"]+"' "$LAUNCHER"/*.go \
  | grep -v '_test' \
  | sed -E 's/.*mux\.Handle(Func)?\("//; s/"$//' \
  | grep -v '/healthz' \
  | sort -u)"
[ -n "$routes" ] || { echo "FAIL: read no routes out of $LAUNCHER" >&2; exit 1; }

json="$(python3 - <<PY
import json
raw = """$routes"""
out = []
for line in raw.strip().splitlines():
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
    else:
        listener, env, port = "memory", "MEMORY_PORT", 2998   # memory, knowledge and skills share one mux
    out.append({"method": method, "path": path, "listener": listener,
                "portEnv": env, "defaultPort": port})
out.sort(key=lambda r: (r["path"], r["method"]))
print(json.dumps({"routes": out}, indent=2))
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
