#!/usr/bin/env bash
# ui-node.sh — deterministic Node toolchain bootstrap for the UI (M12).
#
# The UI (ctxmesh/ui) is built with a Node version PINNED by ui/.nvmrc and
# managed through nvm (user requirement, ADR 0010 addendum) — the build-time-only
# analogue of the M10 Python pinned-venv bootstrap. This script makes the Node
# toolchain reproducible on a CLEAN host (no node / pnpm / nvm) and in CI:
#
#   1. install nvm into $NVM_DIR (default ~/.nvm) if it is not already present;
#   2. `nvm install` / `nvm use` the version in ui/.nvmrc;
#   3. enable corepack + activate the pnpm version pinned by ui/package.json;
#   4. run the requested command (pnpm ...) inside that environment.
#
# nvm is a SHELL FUNCTION, not a binary — it only exists after sourcing
# "$NVM_DIR/nvm.sh". This script sources it explicitly so it works in the
# non-interactive `make` shell (which does not read the user's profile).
#
# Usage:
#   hack/ui-node.sh install            # bootstrap node+pnpm, `pnpm install`
#   hack/ui-node.sh run <pnpm-args...> # run pnpm inside the pinned env
#   hack/ui-node.sh exec <cmd...>      # run an arbitrary command inside the env
set -euo pipefail

# --- config -----------------------------------------------------------------
# Repo-root-relative regardless of CWD (the script lives in hack/).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
UI_DIR="$REPO_ROOT/ui"

# Pin nvm itself so the bootstrap is reproducible (not "whatever HEAD is").
NVM_VERSION="${NVM_VERSION:-v0.40.3}"
export NVM_DIR="${NVM_DIR:-$HOME/.nvm}"

log() { printf '>> %s\n' "$*" >&2; }

# --- 1. ensure nvm ----------------------------------------------------------
ensure_nvm() {
  if [ ! -s "$NVM_DIR/nvm.sh" ]; then
    log "nvm not found at $NVM_DIR — installing nvm $NVM_VERSION"
    mkdir -p "$NVM_DIR"
    # PROFILE=/dev/null: do NOT edit the user's shell rc files; this script
    # sources nvm explicitly, so profile mutation is unwanted and non-hermetic.
    if command -v curl >/dev/null 2>&1; then
      PROFILE=/dev/null bash -c \
        "curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh | bash"
    elif command -v wget >/dev/null 2>&1; then
      PROFILE=/dev/null bash -c \
        "wget -qO- https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh | bash"
    else
      log "ERROR: need curl or wget to install nvm on this clean host"
      exit 1
    fi
  fi
  # nvm is a shell function — source it into THIS shell.
  # shellcheck disable=SC1091
  . "$NVM_DIR/nvm.sh"
}

# --- 2. node from .nvmrc ----------------------------------------------------
ensure_node() {
  [ -f "$UI_DIR/.nvmrc" ] || { log "ERROR: missing $UI_DIR/.nvmrc"; exit 1; }
  cd "$UI_DIR"
  # `nvm install` reads .nvmrc; it is idempotent (no-op if already installed).
  nvm install >&2
  nvm use >&2
}

# --- 3. pnpm via corepack ---------------------------------------------------
ensure_pnpm() {
  # corepack ships with Node >=16.9; it activates the pnpm version pinned in
  # ui/package.json "packageManager", so pnpm is deterministic too.
  corepack enable >&2 2>/dev/null || true
  corepack prepare --activate >&2 2>/dev/null || true
  if ! command -v pnpm >/dev/null 2>&1; then
    log "pnpm not on PATH after corepack; activating explicitly"
    corepack prepare pnpm@latest --activate >&2
  fi
}

setup() {
  ensure_nvm
  ensure_node
  ensure_pnpm
}

# --- dispatch ---------------------------------------------------------------
cmd="${1:-}"
shift || true
case "$cmd" in
  install)
    setup
    cd "$UI_DIR"
    # Frozen lockfile: fail if pnpm-lock.yaml is out of sync — deterministic CI.
    pnpm install --frozen-lockfile
    ;;
  run)
    setup
    cd "$UI_DIR"
    pnpm "$@"
    ;;
  exec)
    setup
    cd "$UI_DIR"
    "$@"
    ;;
  print-versions)
    setup
    printf 'node %s\npnpm %s\n' "$(node --version)" "$(pnpm --version)"
    ;;
  *)
    log "usage: ui-node.sh <install|run|exec|print-versions> [args...]"
    exit 2
    ;;
esac
