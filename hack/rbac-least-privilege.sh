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

# ClusterRoles explicitly marked bind-only: a permission TEMPLATE meant to be attached with a
# per-namespace RoleBinding, never a ClusterRoleBinding. Kubernetes ships `admin` and `edit`
# this way — both grant secrets create/delete/patch/update and are documented for RoleBinding
# — so the shape is standard, and the danger the SEC-3 rule names is the BINDING, not the
# definition (M155, ADR 0136).
#
# The exemption is not a hole: everything marked bind-only is checked below against every
# ClusterRoleBinding we ship, which is a stronger assertion than this file made before.
bind_only = set()
cluster_bindings = []  # (file, binding name, referenced ClusterRole)
for path in sorted(glob.glob(os.path.join(rbac_dir, "*.yaml"))):
    for doc in yaml.safe_load_all(open(path)):
        if not doc:
            continue
        if doc.get("kind") == "ClusterRole" and (doc["metadata"].get("labels") or {}).get(
            "rbac.ctxmesh.ai/bind-only"
        ) == "true":
            bind_only.add(doc["metadata"]["name"])
        if doc.get("kind") == "ClusterRoleBinding":
            cluster_bindings.append(
                (os.path.basename(path), doc["metadata"]["name"], (doc.get("roleRef") or {}).get("name"))
            )

# A bind-only ClusterRole that something we ship binds CLUSTER-WIDE is the actual SEC-3
# hazard, and until now nothing looked for it.
for f, bname, ref in cluster_bindings:
    if ref in bind_only:
        bad.append(
            f"{f}: ClusterRoleBinding/{bname} binds bind-only ClusterRole/{ref} CLUSTER-WIDE — "
            f"that grants its Secret writes in every namespace. Use a RoleBinding."
        )

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
            if (
                kind == "ClusterRole"
                and name not in bind_only
                and "secrets" in resources
                and (writes & set(verbs))
            ):
                bad.append(
                    f"{os.path.basename(path)}: ClusterRole/{name} grants Secret WRITES "
                    f"{sorted(writes & set(verbs))} cluster-wide (SEC-3). Namespace them, or "
                    f"mark it rbac.ctxmesh.ai/bind-only if it is a RoleBinding template "
                    f"(ADR 0136) — bind-only roles are checked against every shipped "
                    f"ClusterRoleBinding."
                )
            # A bind-only role must not quietly acquire READ access to secrets: the console
            # never reads a key back, and a credential role that can list Secrets is a
            # credential-exfiltration role.
            if kind == "ClusterRole" and name in bind_only and "secrets" in resources:
                reads = {"get", "list", "watch"} & set(verbs)
                if reads:
                    bad.append(
                        f"{os.path.basename(path)}: bind-only ClusterRole/{name} grants Secret "
                        f"READS {sorted(reads)} — it exists to WRITE credentials, not read them."
                    )

for b in bad:
    print("  " + b, file=sys.stderr)
print(f"  checked {checked} rules across {rbac_dir}")
sys.exit(1 if bad else 0)
PY

echo "PASS: no verb wildcards, no cluster-scoped Secret writes, no bind-only role bound cluster-wide"
