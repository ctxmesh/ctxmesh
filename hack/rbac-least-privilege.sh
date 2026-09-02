#!/usr/bin/env bash
# rbac-least-privilege.sh — assert what the SHIPPED roles grant (M149 m149.3).
#
# WHY THIS IS A SHIP-ASSERTION, NOT A BEHAVIOURAL TEST. M146's acceptance found that
# `ctxmesh-operator` held `verbs: ["*"]` on agentdeployments, which silently implied the
# new `kill` permission the milestone was building — and it recorded the reason no
# behavioural test could have caught it: a handler test injects a fake authorizer, so it
# proves the handler ASKS, never that RBAC answers as intended. Only an assertion about
# what we ship can tell "the permission is gone" from "nothing exercised it today".
#
# Runs on the manifests, so it needs no cluster and costs milliseconds.
set -euo pipefail

RBAC="${1:-config/rbac}"
[ -d "$RBAC" ] || { echo "FAIL: no rbac dir at $RBAC" >&2; exit 1; }

python3 - "$RBAC" <<'PY'
import sys, glob, os, yaml

rbac_dir = sys.argv[1]
bad = []
checked = 0

for path in sorted(glob.glob(os.path.join(rbac_dir, "*.yaml"))):
    for doc in yaml.safe_load_all(open(path)):
        if not doc or doc.get("kind") not in ("Role", "ClusterRole"):
            continue
        name = doc["metadata"]["name"]
        kind = doc["kind"]
        ns = doc["metadata"].get("namespace")
        for rule in doc.get("rules") or []:
            checked += 1
            verbs = rule.get("verbs") or []
            resources = rule.get("resources") or []
            groups = rule.get("apiGroups") or []

            # 1. No verb wildcards anywhere. A '*' silently grants every FUTURE verb,
            #    including custom ones a later milestone adds — the exact hazard M146
            #    had to route around via a subresource.
            if "*" in verbs:
                bad.append(f"{os.path.basename(path)}: {kind}/{name} grants verbs ['*'] on {resources or groups}")

            # 2. No resource wildcards paired with write verbs.
            writes = {"create", "update", "patch", "delete", "deletecollection"}
            if "*" in resources and (writes & set(verbs)):
                bad.append(f"{os.path.basename(path)}: {kind}/{name} grants writes on resources ['*']")

            # 3. Secret WRITES must never be cluster-scoped. This is the remaining half of
            #    the Fable audit's SEC-3: a ClusterRole granting create/update on Secrets
            #    cluster-wide, when the only writer (the cert rotator) writes exactly one
            #    Secret into the install namespace.
            if kind == "ClusterRole" and "secrets" in resources and (writes & set(verbs)):
                bad.append(
                    f"{os.path.basename(path)}: ClusterRole/{name} grants Secret WRITES "
                    f"{sorted(writes & set(verbs))} cluster-wide (SEC-3). Namespace them."
                )

for b in bad:
    print("  " + b, file=sys.stderr)
print(f"  checked {checked} rules across {rbac_dir}")
sys.exit(1 if bad else 0)
PY

echo "PASS: no verb wildcards, no cluster-scoped Secret writes"
