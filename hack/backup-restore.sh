#!/usr/bin/env bash
# backup-restore.sh — back up and restore ctxmesh's durable state (M48, ADR 0047).
#
# Three durable stores, backed up TOGETHER (a backup of one without the others is not a
# consistent restore):
#
#   1. Credential grants — the `kubernetes` backend (default) stores them as K8s Secrets in a
#      locked namespace; the `postgres` backend stores them AES-256-GCM-ENCRYPTED and is
#      CRYPTOGRAPHICALLY INERT without the KEK. So this script backs up BOTH the grants AND the
#      KEK Secret. A `postgres` grant dump restored WITHOUT its KEK (or with the external
#      OpenBao/KMS transit key rotated/deleted) is PERMANENTLY UNREADABLE (ADR 0047 §4).
#   2. Control-plane Postgres (CONTROLPLANE_DSN) — tenants, toolregistry, agent_memories,
#      grants (postgres backend), runs. A logical `pg_dump`.
#   3. Memory (Valkey) — session/shared/tenant-quota state. `BGSAVE` then a data-dir snapshot;
#      we POLL until the save completes (never copy /data mid-write — a torn file).
#
# Usage:
#   backup-restore.sh backup  <out-dir>
#   backup-restore.sh restore <in-dir>
#
# Env (override for your install):
#   NS=ctxmesh          control-plane namespace
#   CRED_NS=ctxmesh     credential (locked) namespace for kubernetes-backend Secrets
#   KEK_SECRET=ctxmesh-credstore-kek   the KEK Secret (postgres backend, LocalSealer)
#   PG_POD / PG_USER / PG_DB        control-plane Postgres (default: runstore-pg / postgres / runs)
#   VALKEY_STS=statelayer           the Valkey StatefulSet/pod name prefix
set -euo pipefail

log() { printf '>> %s\n' "$*" >&2; }

# EVERY default below is DISCOVERED from the cluster, not assumed.
#
# They used to be literals, and on 2026-09-14 all three were checked against a stock install and
# all three were wrong: PG_POD=runstore-pg did not exist (the chart ships ctxmesh-postgres),
# PG_USER/PG_DB=postgres/runs could not connect (the chart generates them into a Secret), and
# KEK_SECRET=ctxmesh-credstore-kek existed nowhere. So the disaster-recovery tool could not run
# against the product it recovers, and nothing had ever noticed, because nothing had ever run it.
#
# The KEK is the sharpest case. Three different names were in circulation -- cred-kek in the
# published runbook, ctxmesh-credstore-kek here, pg-kek in an integration test -- and none of them
# is real: the name is whatever the operator put in the CredentialStore's localKEKSecretRef. A
# guessed name fails as NotFound on the one asset without which credential ciphertext is
# permanently inert, at the exact moment somebody is restoring.
# KUBE_CONTEXT pins which cluster this talks to. Every kubectl below used the AMBIENT context,
# which is whatever the last `kind create cluster` set -- so running this while another cluster was
# current dumped THAT cluster instead, silently and with a plausible-looking dump. The M178 DR drill
# caught it exactly that way: it seeded a marker in one cluster, this tool backed up another, and
# the restore put nothing back. A disaster-recovery tool must never be ambiguous about which
# disaster it is recovering.
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
kubectl() { command kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} "$@"; }

NS="${NS:-ctxmesh}"
CRED_NS="${CRED_NS:-ctxmesh}"
VALKEY_STS="${VALKEY_STS:-statelayer}"

# secret_value — one key out of a Secret, through the context-pinned kubectl above.
secret_value() {
  kubectl -n "$NS" get secret "$1" -o jsonpath="{.data.$2}" 2>/dev/null | base64 -d 2>/dev/null
}

discover() {  # discover <var> <description> <command...>
  local var="$1" what="$2"; shift 2
  local cur="${!var:-}"
  [ -n "$cur" ] && return 0
  local val; val="$("$@" 2>/dev/null || true)"
  [ -n "$val" ] || return 1
  printf -v "$var" '%s' "$val"
  log "  discovered $what: $val"
}

discover PG_POD "control-plane Postgres pod" \
  kubectl -n "$NS" get pod -l control-plane=postgres -o jsonpath='{.items[0].metadata.name}' \
  || { echo "no Postgres pod labelled control-plane=postgres in $NS; set PG_POD" >&2; exit 1; }

PG_SECRET="${PG_SECRET:-$(kubectl -n "$NS" get secret -o name 2>/dev/null \
  | grep -m1 'postgres' | cut -d/ -f2 || true)}"
if [ -n "$PG_SECRET" ]; then
  # NOT `sh -c`. A subshell does not inherit the kubectl() wrapper above, so these discoveries ran
  # against the AMBIENT context -- and when that context is empty or points elsewhere they fail and
  # fall back to postgres/postgres, which looks plausible and is wrong. That is the same
  # wrong-cluster failure KUBE_CONTEXT was added to close, reintroduced one line below the fix.
  PG_USER="${PG_USER:-$(secret_value "$PG_SECRET" username)}"
  PG_DB="${PG_DB:-$(secret_value "$PG_SECRET" database)}"
  [ -n "$PG_USER" ] && log "  discovered Postgres user: $PG_USER"
  [ -n "$PG_DB" ] && log "  discovered Postgres database: $PG_DB"
fi
PG_USER="${PG_USER:-postgres}"
PG_DB="${PG_DB:-postgres}"

# The KEK's name is on the CredentialStore, because the operator chose it there.
KEK_SECRET="${KEK_SECRET:-$(kubectl get clustercredentialstores,credentialstores -A \
  -o jsonpath='{.items[*].spec.provider.postgres.encryption.localKEKSecretRef.name}' 2>/dev/null \
  | awk '{print $1}' || true)}"


valkey_pod() { kubectl -n "$NS" get pod -l control-plane=statelayer -o jsonpath='{.items[0].metadata.name}'; }

backup() {
  local out="$1"; mkdir -p "$out"
  log "1/3 credential grants + KEK"
  # kubernetes backend: dump the credential Secrets (labelled by the credential plane).
  kubectl -n "$CRED_NS" get secret -l app.kubernetes.io/part-of=ctxmesh-credentials -o yaml \
    > "$out/credential-secrets.yaml" 2>/dev/null || log "  (no kubernetes-backend credential Secrets — postgres backend?)"
  # postgres backend: the KEK is REQUIRED to ever read the encrypted grants again.
  if kubectl -n "$NS" get secret "$KEK_SECRET" >/dev/null 2>&1; then
    kubectl -n "$NS" get secret "$KEK_SECRET" -o yaml > "$out/credstore-kek.yaml"
    log "  KEK backed up — a postgres-backend restore is UNREADABLE without it"
  fi
  log "2/3 control-plane Postgres ($PG_DB)"
  kubectl -n "$NS" exec "$PG_POD" -- pg_dump -U "$PG_USER" -Fc "$PG_DB" > "$out/controlplane.dump"
  log "3/3 Valkey memory (BGREWRITEAOF + poll + snapshot the whole /data)"
  local vp; vp="$(valkey_pod)"
  # With appendonly=yes the AOF is the source of truth (Valkey loads it on boot, NOT the RDB), so the
  # snapshot must capture the WHOLE /data (appendonlydir + dump.rdb). Compact the AOF then poll until the
  # rewrite finishes — never tar /data mid-write (a torn file).
  kubectl -n "$NS" exec "$vp" -- valkey-cli BGREWRITEAOF >/dev/null
  for _ in $(seq 1 30); do
    sleep 1
    [ "$(kubectl -n "$NS" exec "$vp" -- valkey-cli INFO persistence | tr -d '\r' | awk -F: '/aof_rewrite_in_progress/{print $2}')" = "0" ] && break
  done
  kubectl -n "$NS" exec "$vp" -- tar -C /data -cf - . > "$out/valkey-data.tar"
  log "backup complete → $out"
}

restore() {
  local in="$1"
  log "1/3 credential grants + KEK"
  [ -f "$in/credstore-kek.yaml" ]     && kubectl apply -f "$in/credstore-kek.yaml"
  [ -f "$in/credential-secrets.yaml" ] && kubectl apply -f "$in/credential-secrets.yaml"
  log "2/3 control-plane Postgres (pg_restore --clean)"
  kubectl -n "$NS" exec -i "$PG_POD" -- pg_restore -U "$PG_USER" -d "$PG_DB" --clean --if-exists < "$in/controlplane.dump"
  log "3/3 Valkey memory (replace /data with the snapshot, then restart to adopt the AOF)"
  local vp; vp="$(valkey_pod)"
  kubectl -n "$NS" exec -i "$vp" -- sh -c 'rm -rf /data/appendonlydir /data/dump.rdb; tar -C /data -xf -' < "$in/valkey-data.tar"
  kubectl -n "$NS" delete pod "$vp"   # restart → Valkey loads the restored AOF on boot
  log "restore complete (verify: valkey-cli DBSIZE; SELECT count(*) FROM tenants)"
}

case "${1:-}" in
  backup)  [ $# -eq 2 ] || { echo "usage: $0 backup <out-dir>" >&2; exit 2; }; backup "$2" ;;
  restore) [ $# -eq 2 ] || { echo "usage: $0 restore <in-dir>" >&2; exit 2; }; restore "$2" ;;
  *) echo "usage: $0 {backup|restore} <dir>" >&2; exit 2 ;;
esac
