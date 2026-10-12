"""Every workload that holds the run-capability key carries the same capability audience.

The BFF and the run-worker mint run capabilities for an audience, and the controller (for the egress
sidecar and agents) and the token service verify them against one. When one of them takes the audience
from a value the others do not, setting that value makes every capability fail verification
(invalid_capability) with nothing in the chart to say why.

  helm template ... --set controllerManager.oboEgress.capabilityAudience=<probe> | \\
    capability_audience.py <probe>

reads a render on stdin and fails unless every container that reads the bff-capability Secret (by
envFrom or secretKeyRef) sets MCP_CAPABILITY_AUDIENCE to the probe value, and at least the BFF, the
controller and the token service are among them.
"""
from __future__ import annotations

import sys

import yaml

KEY_SECRET = "bff-capability"
AUDIENCE = "MCP_CAPABILITY_AUDIENCE"
MUST_HOLD = ("ctxmesh-bff", "ctxmesh-controller-manager", "ctxmesh-token-service")


def reads_key(container: dict) -> bool:
    for src in container.get("envFrom") or []:
        if (src.get("secretRef") or {}).get("name") == KEY_SECRET:
            return True
    for env in container.get("env") or []:
        ref = ((env.get("valueFrom") or {}).get("secretKeyRef") or {})
        if ref.get("name") == KEY_SECRET:
            return True
    return False


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: capability_audience.py <probe-audience> < render.yaml", file=sys.stderr)
        return 2
    probe = sys.argv[1]
    holders, bad = [], []
    for doc in yaml.safe_load_all(sys.stdin):
        # Long-running workloads only: the preflight hook Job reads the Secret to check the keypair
        # and mints nothing.
        if not isinstance(doc, dict) or doc.get("kind") not in ("Deployment", "StatefulSet"):
            continue
        name = doc["metadata"]["name"]
        for c in doc["spec"]["template"]["spec"].get("containers") or []:
            if not reads_key(c):
                continue
            holders.append(name)
            values = [e.get("value") for e in c.get("env") or [] if e.get("name") == AUDIENCE]
            if values != [probe]:
                bad.append(f"{name}/{c['name']}: {AUDIENCE}={values or 'unset'}, want [{probe!r}]")
    missing = [n for n in MUST_HOLD if n not in holders]
    for m in missing:
        bad.append(f"{m}: does not read the {KEY_SECRET} Secret, so this check cannot see it")
    if bad:
        print("FAIL: the run-capability audience is not the same everywhere it is minted or verified:", file=sys.stderr)
        for b in bad:
            print(f"  {b}", file=sys.stderr)
        return 1
    print(f"  ok: {len(holders)} workloads that hold the capability key all use audience {probe!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
