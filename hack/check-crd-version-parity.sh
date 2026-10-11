#!/usr/bin/env bash
# CRD served-version guard (ADR 0100 amendment, m184.22; audit FUNC-8).
#
# Every CRD serves exactly one version, that version is its storage version, and every CRD in the
# directory serves the same one. Until GA there is one served API version (v1beta1), so no kind can
# admit through one version what another would reject: with conversion strategy None, two served
# versions are only safe while their validation is identical, which is the drift this gate first
# existed to catch (a 45+-char AgentDeployment name admitted via one version, wedging reconcile).
#
# Serving a second version is a decision, not a regeneration side effect: it needs an ADR, a
# conversion story, and a change to this gate. Run after `make manifests`.
set -euo pipefail

CRD_DIR="${1:-config/crd/bases}"

python3 - "$CRD_DIR" <<'PY'
import glob
import sys

import yaml

crd_dir = sys.argv[1]
failures = []
served_by_crd = {}
for f in sorted(glob.glob(f"{crd_dir}/*.yaml")):
    with open(f) as fh:
        docs = list(yaml.safe_load_all(fh))
    for d in docs:
        if not d or d.get("kind") != "CustomResourceDefinition":
            continue
        name = d["metadata"]["name"]
        versions = d["spec"]["versions"]
        served = [v["name"] for v in versions if v.get("served")]
        stored = [v["name"] for v in versions if v.get("storage")]
        if len(served) != 1:
            failures.append(f"{name} serves {len(served)} versions {served}; it must serve exactly one")
        if len(stored) != 1:
            failures.append(f"{name} has {len(stored)} storage versions {stored}; it must have exactly one")
        if len(served) == 1 and stored != served:
            failures.append(f"{name} serves {served[0]} but stores {stored}; the served version must be the storage version")
        served_by_crd[name] = tuple(served)

if not served_by_crd:
    print(f"FAIL: no CustomResourceDefinition found in {crd_dir}; the gate checked nothing")
    sys.exit(1)

distinct = sorted({s for s in served_by_crd.values()})
if len(distinct) > 1:
    failures.append("the CRDs do not serve the same version:")
    for name, served in sorted(served_by_crd.items()):
        failures.append(f"  {name}: {list(served)}")

if failures:
    for line in failures:
        print(f"FAIL: {line}")
    print("CRD served-version gate FAILED: one served version per CRD, the same for all (ADR 0100).")
    sys.exit(1)
print(f"CRD served-version OK: {len(served_by_crd)} CRDs each serve and store only {distinct[0][0]}.")
PY
