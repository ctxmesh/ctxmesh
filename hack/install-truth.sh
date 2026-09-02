#!/usr/bin/env bash
# install-truth.sh — assert that the chart provisions what it consumes (M148).
#
# WHY THIS IS AN ENGINE-SIDE GATE, not only a harness one. M148's ✅ line is "the install
# proof runs in CI, so it cannot rot unnoticed the way G7/G11 did". Every assertion below
# is about the RENDER, so it needs no cluster and costs seconds — which is what makes it
# affordable on every PR. `harness/scripts/accept-m148.sh` calls this same script, so the
# thing CI checks and the thing acceptance checks cannot drift apart.
#
# WHAT IT CANNOT PROVE: that a fresh cluster comes up. That needs Knative + KEDA + a kind
# cluster (~10 minutes a run), and this project has already had its CI minutes exhausted
# once (m52.G3). The cluster-level bring-up is deliberately out of CI and carded for M150;
# the structural half — the class of defect that actually shipped — is here.
set -euo pipefail

CHART="${1:-deploy/helm/ctxmesh}"
[ -d "$CHART" ] || { echo "FAIL: no chart at $CHART" >&2; exit 1; }

RENDER="$(mktemp)"; PROD="$(mktemp)"
trap 'rm -f "$RENDER" "$PROD"' EXIT

helm template ctxmesh "$CHART" -n ctxmesh > "$RENDER" 2>/dev/null \
  || { echo "FAIL: the default render does not template" >&2; exit 1; }

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

# 1. Every Secret the chart CONSUMES, the chart CREATES.
#
# The M148 bug in one assertion: bff-adapters was referenced four times and created zero
# times. The pods STARTED (optional: true) and then os.Exit(1) on the empty DSN, so
# `kubectl get pods` showed CrashLoopBackOff with no missing-Secret event anywhere.
#
# bff-adapters stays the deliberate exception — it is the OPERATOR's Secret for optional
# adapter credentials, so the chart must not own it (ADR 0130). What the chart must own
# is the DSN, which is why that lives in its own Secret.
for want in ctxmesh-postgres-dsn ctxmesh-postgres-dev; do
  grep -q "name: $want" "$RENDER" || fail "the chart does not create $want"
done
ok "the chart creates the Secrets it depends on (the DSN is chart-owned, ADR 0130)"

# 2. The control plane's hard requirements have a source.
# cmd/main.go, cmd/bff/main.go and cmd/token-service/main.go all os.Exit(1) without this.
grep -q 'CONTROLPLANE_DSN' "$RENDER" || fail "CONTROLPLANE_DSN has no source in the render — three processes exit at startup without it (ADR 0044)"
grep -q 'RUN_STORE_DSN'    "$RENDER" || fail "RUN_STORE_DSN has no source in the render — the run worker cannot start"
ok "CONTROLPLANE_DSN and RUN_STORE_DSN are both sourced by the chart"

# 3. The database exists, is pgvector, and keeps its data.
grep -q 'name: ctxmesh-postgres$'      "$RENDER" || fail "no Postgres workload — the run store, control plane, cost rollups and knowledge chunks all require it"
grep -q 'image: pgvector/pgvector'     "$RENDER" || fail "the bundled database is not pgvector — vector search (ADR 0084) fails at the first knowledge query, not at install"
grep -q 'kind: PersistentVolumeClaim'  "$RENDER" || fail "the bundled database has no PVC — run history and cost rollups are real state"
ok "pgvector is deployed, with a PVC rather than an emptyDir"

# 4. Every feature the chart CLAIMS is reachable from values.yaml.
# Honest-when-unset (501s, unregistered edges) is correct and stays; what was wrong is
# that a stock install had no way to turn any of it ON.
for v in MODEL_GATEWAY_URL INGEST_OCR_URL KNOWLEDGE_RERANK_URL DISCOVERY_EMBEDDING_ROUTE \
         DISCOVERY_RERANK_URL ASYNC_BACKEND NATS_URL EGRESS_STREAM_IDLE_TIMEOUT \
         EGRESS_RESPONSE_HEADER_TIMEOUT KNOWLEDGE_SETTLE_WINDOW; do
  grep -q "name: $v" "$RENDER" || fail "$v is read by code and has no chart home — it cannot be enabled from values.yaml"
done
ok "the model-service, async and durability env all have a values home"

# 5. RAG is wired on BOTH sides. The token-service SERVES retrieval and hangs both
#    WithMemory and WithKnowledge off one "is the gateway set" condition, so a missing
#    gateway URL there does not degrade retrieval — it never wires it, and every search
#    returns empty with no error. That is how RAG shipped broken on every stock install.
[ "$(grep -c 'name: MODEL_GATEWAY_URL' "$RENDER")" -ge 2 ] \
  || fail "MODEL_GATEWAY_URL is set on only one component. The BFF ingests and the token-service RETRIEVES; both need it, and a missing one is silent."
ok "both the ingest and the retrieval halves have a gateway"

# 6. The gateway default names a Service this chart actually ships.
grep -q 'value: http://ctxmesh-gateway' "$RENDER" || fail "MODEL_GATEWAY_URL has no working default"
grep -q 'name: ctxmesh-gateway$'        "$RENDER" || fail "MODEL_GATEWAY_URL defaults to a gateway Service the chart does not create"
ok "the MODEL_GATEWAY_URL default names a Service this chart ships"

# 7. Cost aggregation is on by default (m52.G11e — already true when M148 checked;
#    asserted so it cannot silently regress).
grep -A1 'name: COST_ROLLUP_ENABLED' "$RENDER" | grep -q '"1"' \
  || fail "COST_ROLLUP_ENABLED no longer defaults on — cost_rollups stays empty even once spend is booked"
ok "COST_ROLLUP_ENABLED still defaults on"

# 8. A production render REFUSES rather than shipping something that cannot start.
if helm template ctxmesh "$CHART" -n ctxmesh --set profile=production \
     -f "$CHART/values-production.yaml" > "$PROD" 2>&1; then
  fail "a production render with no postgres.externalDsn SUCCEEDED — it would hand the control plane a DSN pointing at a Service that is not rendered (ADR 0130)"
fi
grep -q 'postgres.externalDsn' "$PROD" || fail "the production render failed for some reason other than the missing external DSN"
ok "profile=production refuses without an operator-managed database"

echo "PASS: the chart provisions what it consumes"
