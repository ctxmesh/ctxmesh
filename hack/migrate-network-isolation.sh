#!/usr/bin/env bash
# migrate-network-isolation.sh — the ADR-0073 secure-default BACKFILL (m52.C4).
#
# Run this ORDERED STEP *before* upgrading to the chart version that flips
# Tenant.spec.networkIsolation to secure-by-default (*bool + kubebuilder:default=true).
# It grandfathers every tenant that is OPEN today (networkIsolation absent OR false) to an
# EXPLICIT `false` + the grandfather annotation, so an existing tenant keeps its exact network
# behavior after the flip (no upgrade incident). New tenants created after the flip omit the
# field → served as `true` → isolated from birth.
#
# WHY ORDERED + LOAD-BEARING: CRD structural defaults apply on READ (verified, ADR 0073 build-note),
# so the moment the defaulted CRD applies, every field-absent tenant is *served* as `true` and
# isolates on its next reconcile. Running this first makes them explicit-`false` so the default
# never reaches them. IDEMPOTENT — safe to re-run; re-patching an already-false tenant is a no-op.
#
# Usage: KUBECTL=kubectl ./hack/migrate-network-isolation.sh   (honors $KUBECTL for a --context wrapper)
set -euo pipefail
KUBECTL="${KUBECTL:-kubectl}"
ANNOTATION="agents.ctxmesh.ai/network-isolation-grandfathered"

echo "[migrate-netiso] grandfathering tenants that are open today (networkIsolation absent/false)…"
# Under the OLD CRD (networkIsolation bool + omitempty), an explicit false is OMITTED, so the
# jsonpath is empty for BOTH absent and false — exactly the set to grandfather. Only an explicit
# `true` serializes; skip those.
count=0
while IFS='=' read -r name iso; do
  [ -z "$name" ] && continue
  if [ "$iso" = "true" ]; then continue; fi   # already isolated — leave it
  "$KUBECTL" annotate tenant.agents.ctxmesh.ai "$name" "$ANNOTATION=true" --overwrite >/dev/null
  "$KUBECTL" patch tenant.agents.ctxmesh.ai "$name" --type=merge -p '{"spec":{"networkIsolation":false}}' >/dev/null
  echo "[migrate-netiso]   grandfathered: $name"
  count=$((count + 1))
done < <("$KUBECTL" get tenants.agents.ctxmesh.ai \
  -o 'jsonpath={range .items[*]}{.metadata.name}{"="}{.spec.networkIsolation}{"\n"}{end}')

echo "[migrate-netiso] done — grandfathered $count tenant(s). Now apply the secure-default CRD + roll the controller."
