#!/usr/bin/env bash
# registry-copy — keep internal vocabulary off the pages strangers read.
#
# These files render on npmjs.com, pypi.org and Artifact Hub. A reader there has none of
# this project's context, so "typed sugar over the launcher localhost plane (ADR 0002)"
# tells them nothing — and "localhost" actively misleads, reading as a dev-only tool when
# the fact it encodes is the opposite.
#
# Scope is deliberately narrow: only files that SHIP to a registry. Internal docs, ADRs and
# specs are where this vocabulary belongs, and the gate must never push it out of them.
set -euo pipefail
cd "$(dirname "$0")/.."

FILES=(
  sdk/typescript/package.json
  sdk/typescript/README.md
  sdk/python/pyproject.toml
  sdk/python/README.md
  deploy/helm/ctxmesh/Chart.yaml
)

# Each entry is <regex>|<why>. Matching is case-insensitive.
BANNED=(
  'ADR[ -]?[0-9]{4}|an internal decision-record number means nothing on a registry page'
  '\bPRD\b|the PRD is not a document a reader can open'
  '\bM[0-9]{1,3}\.[0-9]|an internal milestone/task id'
  'typed sugar|says how it was built, not what it does'
  'localhost (platform )?plane|internal architecture vocabulary; it also reads as dev-only'
  'launcher plane|internal architecture vocabulary'
  'at parity with|an engineering goal, not a user benefit'
  'not published to (npm|pypi)|both SDKs ARE published — this was live on npm for two releases'
  '\bharness/|harness/ does not exist in this repo; it lives in agent-brain'
)

# What a registry actually RENDERS. A comment in pyproject.toml never reaches PyPI, and a
# gate that flags one is a gate someone waives — so the manifests are reduced to their
# rendered fields first. READMEs and Chart.yaml render whole.
rendered() {
  case "$1" in
    *package.json)
      python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d.get("description","")); print(" ".join(d.get("keywords",[])))' "$1" ;;
    *pyproject.toml)
      grep -E '^(description|keywords|classifiers) *=' -A6 "$1" | grep -v '^\s*#' ;;
    *) grep -v '^\s*#' "$1" ;;
  esac
}

rc=0
for f in "${FILES[@]}"; do
  [ -f "$f" ] || { echo "FAIL: $f is missing — the gate is not checking what it claims to" >&2; rc=1; continue; }
  body="$(rendered "$f")"
  for entry in "${BANNED[@]}"; do
    pat="${entry%%|*}"; why="${entry#*|}"
    if hits="$(grep -nEi -- "$pat" <<<"$body" 2>/dev/null)"; then
      while IFS= read -r line; do
        echo "FAIL: $f — $why" >&2
        echo "      $(printf '%s' "$line" | cut -d: -f2- | sed 's/^[[:space:]]*//' | cut -c1-100)" >&2
      done <<<"$hits"
      rc=1
    fi
  done
done

if [ "$rc" != "0" ]; then
  echo >&2
  echo "These files render on npmjs.com, pypi.org and Artifact Hub. Say what the thing DOES," >&2
  echo "for someone who has never seen this project. Keep the internal vocabulary in docs/ and" >&2
  echo "decisions/, where the reader has the context to decode it." >&2
  exit 1
fi
echo "PASS: no internal vocabulary in the ${#FILES[@]} files that ship to a registry"
