#!/usr/bin/env bash
# CRD version-parity guard (audit FUNC-8).
#
# The CRDs graduate v1alpha1 -> v1beta1 as "field-identical" copies with conversion
# strategy None (no webhook). That is only safe while the schemas' validation stays in
# lock-step: a value admitted via one version but rejected by the other wedges reconcile
# (e.g. a 45+-char name admitted via v1beta1 that v1alpha1's CEL rule would have rejected,
# then failing at the Knative revision-name webhook).
#
# This fails CI when a multi-version CRD's TOP-LEVEL metadata validations
# (x-kubernetes-validations, e.g. the size(self.metadata.name) <= 44 guard) differ across
# its served versions. Run after `make manifests`.
set -euo pipefail

CRD_DIR="${1:-config/crd/bases}"

python3 - "$CRD_DIR" <<'PY'
import sys, glob, yaml

crd_dir = sys.argv[1]
fail = False
for f in sorted(glob.glob(f"{crd_dir}/*.yaml")):
    for d in yaml.safe_load_all(open(f)):
        if not d or d.get("kind") != "CustomResourceDefinition":
            continue
        versions = d["spec"]["versions"]
        if len(versions) < 2:
            continue
        rulesets = {}
        for v in versions:
            schema = v["schema"]["openAPIV3Schema"]
            rulesets[v["name"]] = sorted(
                x["rule"] for x in schema.get("x-kubernetes-validations", [])
            )
        distinct = {tuple(r) for r in rulesets.values()}
        if len(distinct) > 1:
            fail = True
            print(f"DRIFT: {d['metadata']['name']} top-level CEL rules differ across versions:")
            for name, rules in rulesets.items():
                print(f"  {name}: {rules}")

if fail:
    print("CRD version-parity FAILED (audit FUNC-8) — align top-level validations across versions.")
    sys.exit(1)
print("CRD version-parity OK: every multi-version CRD has matching top-level CEL rules.")
PY
