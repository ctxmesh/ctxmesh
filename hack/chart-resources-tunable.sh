#!/usr/bin/env bash
# chart-resources-tunable — no container's resources may be baked into a template.
#
# WHY THIS EXISTS
# ---------------
# Until M178 an operator could not size any part of this install. 31 limits/requests blocks sat
# hardcoded across 8 templates, none of them reachable from values, and values.yaml had no
# `resources` key at all — while the same templates read `.Values` 219 times for everything else.
# Nobody noticed because nothing looked: a chart renders perfectly well with numbers a user cannot
# change.
#
# The failure mode this guards is not the existing templates; it is the NEXT workload. A new
# deployment copied from an old one carries the old one's literal block, renders green, ships, and
# the gap silently reopens.
#
# WHAT IT MUST NOT FLAG, AND WHY BOTH MATTER
#
#   • An RBAC rule's `resources:` is a list of API kinds, not a quantity map. Most `resources:`
#     matches in this chart are RBAC, which is exactly how the original audit miscounted the gap
#     in both directions.
#   • A PersistentVolumeClaim's `resources.requests.storage` is a volume size. Templating that as
#     a container resource would put a disk size where a CPU limit belongs.
#
# The discriminator is the same in both cases: a container block is followed by `limits:`/
# `requests:` AND carries a cpu/memory quantity.
#
# Usage: ./hack/chart-resources-tunable.sh
set -euo pipefail
cd "$(dirname "$0")/.."

TEMPLATES="deploy/helm/ctxmesh/templates"
[ -d "$TEMPLATES" ] || { echo "FAIL: no chart templates at $TEMPLATES" >&2; exit 1; }

python3 - "$TEMPLATES" <<'PY'
import pathlib, re, sys

templates = pathlib.Path(sys.argv[1])
bad, ok = [], 0

for f in sorted(templates.glob("*.yaml")):
    lines = f.read_text().split("\n")
    for i, line in enumerate(lines):
        if line.strip() != "resources:":
            continue
        indent = len(line) - len(line.lstrip())
        body = []
        j = i + 1
        while j < len(lines) and (
            not lines[j].strip() or (len(lines[j]) - len(lines[j].lstrip())) > indent
        ):
            body.append(lines[j])
            j += 1
        blob = "\n".join(body)
        # A templated block is the GOOD case, and it has to be recognised FIRST: its body is a
        # `{{- toYaml … }}` line, which carries no `limits:`/`requests:` for the RBAC filter below
        # to match on. The first version of this gate checked RBAC first, so every correct block
        # fell through the `continue` and the gate reported "0 tunable, 0 hardcoded — PASS" on a
        # chart it had not inspected at all.
        if "toYaml" in blob and ".Values" in blob:
            ok += 1
            continue
        # an RBAC rule: a list of API kinds, no quantity map
        if not re.search(r"^\s+(limits|requests):", blob, re.M):
            continue
        # a PVC claim: a volume size, not a container resource
        if "storage:" in blob and not re.search(r"\b(cpu|memory):", blob):
            continue
        bad.append((f.name, i + 1, " ".join(x.strip() for x in body if x.strip())[:70]))

for name, ln, preview in bad:
    print(f"  FAIL: {name}:{ln} hardcodes container resources — {preview}")
print(f"  {ok} container resource block(s) read from .Values, {len(bad)} hardcoded")
if bad:
    print("FAIL: a container's resources are baked into a template, so an operator cannot size it.")
    print("      Point the block at .Values.<component>.resources and ship today's numbers as the")
    print("      default, so `helm upgrade` at default values changes no limit on any cluster.")
    sys.exit(1)
print("PASS: every container in the chart takes its resources from values")
PY
