#!/usr/bin/env bash
# prereqs-are-real — every third-party API the controller watches must be a declared prerequisite,
# and every declared prerequisite must be one the product actually needs.
#
# WHY THIS EXISTS
# ---------------
# Both directions of this had drifted at once, and each cost a newcomer something different.
#
# DEMANDED BUT NOT NEEDED: the install page said Knative Eventing was "required ... on a cluster
# without it the controller cannot start. It is not optional even if you never use the eventing
# execution model." Measured in envtest with no Eventing CRDs, both controllers registered, the
# manager started, and the cache SYNCED. The claim was false, and it cost every user a second
# Knative component to install and keep upgraded before they had deployed anything that used it.
# It survived because the Owns() call was read rather than the behaviour measured -- the same
# mistake that had the GA audit reporting G-C open through M177.
#
# NEEDED BUT NOT DECLARED: KEDA. agentscalingpolicy_controller.go Owns(&kedatypes.ScaledObject{})
# unconditionally, NOTES.txt and Chart.yaml both name KEDA a prerequisite, and it appeared in ZERO
# getting-started pages. A reader preparing a cluster from the docs simply did not install it.
#
# A prerequisite list is a promise about what you must have before the product works. Nothing
# checked it against the product.
#
# WHAT IT CHECKS
#   1. Every third-party API group the controllers watch is named on the install page.
#   2. A group whose watches are CONDITIONAL is described conditionally, not as flatly required --
#      because "required" for something that is not is exactly the defect above.
#
# It deliberately says nothing about whether a watch SHOULD be conditional. That is a design call
# per capability (ADR 0141); this only insists the docs match whatever was decided.
#
# Usage: ./hack/prereqs-are-real.sh [path-to-installation.md]
set -euo pipefail
cd "$(dirname "$0")/.."

PAGE="${1:-../ctxmesh.github.io/src/content/docs/getting-started/installation.md}"
[ -f "$PAGE" ] || { echo "SKIP: no install page at $PAGE (clone ctxmesh.github.io beside the engine)"; exit 0; }

python3 - "$PAGE" <<'PY'
import pathlib, re, sys

page = pathlib.Path(sys.argv[1]).read_text()
# The prerequisites section only -- a mention elsewhere on the page is not a declaration.
m = re.search(r'^##+\s*Prerequisites\s*$(.*?)^##', page, re.M | re.S)
prereqs = m.group(1) if m else page

# Go import alias -> (human name, how the page must name it)
GROUPS = {
    "servingv1":  ("Knative Serving",  r"Knative\s+Serving"),
    "eventingv1": ("Knative Eventing", r"Knative\s+Eventing"),
    "kedatypes":  ("KEDA",             r"KEDA"),
}

watches, conditional = {}, {}
for f in sorted(pathlib.Path("internal/controller").glob("*.go")):
    if f.name.endswith("_test.go"):
        continue
    lines = f.read_text().split("\n")
    for i, line in enumerate(lines):
        hit = re.search(r'(?:Owns|Watches|For)\(&([a-z][a-zA-Z0-9]*)\.', line)
        if not hit or hit.group(1) not in GROUPS:
            continue
        alias = hit.group(1)
        watches.setdefault(alias, []).append(f"{f.name}:{i+1}")
        # Conditional only when the watch is INSIDE an open `if …Available {` block. A fixed
        # lookback window is not enough: in agentregistry_controller.go the Serving watch sits two
        # lines after the eventing guard has already closed, and a 4-line window reported Knative
        # Serving as conditional -- which would have let the page describe the DEFAULT execution
        # model's dependency as optional. Walk back and stop at the closing brace.
        depth = 0
        for j in range(i - 1, max(-1, i - 12), -1):
            prev = lines[j]
            if re.match(r'^\t\}\s*$', prev):
                depth += 1
                continue
            if re.search(r'if\s+[\w.]*Available\s*\{', prev):
                if depth == 0:
                    conditional[alias] = True
                break

fails = 0
for alias, sites in sorted(watches.items()):
    name, pattern = GROUPS[alias]
    named = re.search(pattern, prereqs, re.I) is not None
    is_cond = conditional.get(alias, False)
    where = sites[0]
    if not named:
        print(f"  FAIL: the controllers watch {name} ({where}) and the install page's prerequisites never name it")
        fails += 1
        continue
    if is_cond:
        # the line that names it must not present it as unconditional
        line = next((l for l in prereqs.split("\n") if re.search(pattern, l, re.I)), "")
        if re.search(r'\b(required|not optional|must install)\b', line, re.I) and not re.search(r'only if|when you|optional', line, re.I):
            print(f"  FAIL: {name}'s watches are CONDITIONAL ({where}) but the page calls it required: {line.strip()[:90]}")
            fails += 1
            continue
        print(f"  ok: {name} is watched conditionally and described conditionally")
    else:
        print(f"  ok: {name} is watched unconditionally and declared a prerequisite")

if fails:
    print(f"FAIL: {fails} prerequisite(s) disagree with what the controllers actually watch.")
    sys.exit(1)
print("PASS: the declared prerequisites match what the controllers watch")
PY
