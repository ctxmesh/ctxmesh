#!/usr/bin/env bash
# backup-restore.sh — back up and restore agentry's durable state (M48, ADR 0047).
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
#   NS=agentry          control-plane namespace
#   CRED_NS=agentry     credential (locked) namespace for kubernetes-backend Secrets
#   KEK_SECRET=agentry-credstore-kek   the KEK Secret (postgres backend, LocalSealer)
#   PG_POD / PG_USER / PG_DB        control-plane Postgres (default: runstore-pg / postgres / runs)
#   VALKEY_STS=statelayer           the Valkey StatefulSet/pod name prefix
set -euo pipefail

NS="${NS:-agentry}"
CRED_NS="${CRED_NS:-agentry}"
KEK_SECRET="${KEK_SECRET:-agentry-credstore-kek}"
PG_POD="${PG_POD:-runstore-pg}"
PG_USER="${PG_USER:-postgres}"
PG_DB="${PG_DB:-runs}"
VALKEY_STS="${VALKEY_STS:-statelayer}"

log() { printf '>> %s\n' "$*" >&2; }

valkey_pod() { kubectl -n "$NS" get pod -l control-plane=statelayer -o jsonpath='{.items[0].metadata.name}'; }

backup() {
  local out="$1"; mkdir -p "$out"
  log "1/3 credential grants + KEK"
  # kubernetes backend: dump the credential Secrets (labelled by the credential plane).
  kubectl -n "$CRED_NS" get secret -l app.kubernetes.io/part-of=agentry-credentials -o yaml \
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
