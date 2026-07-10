#!/usr/bin/env bash
# gen-helm-chart.sh — regenerate the agent-engine Helm chart templates from
# `kustomize build config/default`.
#
# THE NO-DRIFT CONTRACT (m12.2): the Helm chart MUST deploy the SAME resources
# as `make deploy` (`kustomize build config/default`). This script is the single
# source of that property: the control-plane + CRD templates are *generated* from
# config/, never hand-maintained. Run `make helm-generate` after any config/
# change; `make helm-verify` fails CI if the committed chart drifts from config/.
#
# Usage:
#   hack/gen-helm-chart.sh [OUT_TEMPLATES_DIR]
# Env:
#   KUSTOMIZE  path to the kustomize binary (default: bin/kustomize)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$REPO_ROOT/bin/kustomize}"
OUT_DIR="${1:-$REPO_ROOT/deploy/helm/agent-engine/templates}"

[ -x "$KUSTOMIZE" ] || { echo "kustomize not found at $KUSTOMIZE (run 'make kustomize')" >&2; exit 1; }

RENDER="$("$KUSTOMIZE" build "$REPO_ROOT/config/default")"

mkdir -p "$OUT_DIR"

# The generator lives in Python for reliable multi-doc YAML splitting and the
# precise Helm-template substitutions. It only performs *textual* wrapping — it
# never rewrites resource bodies — so the round-trip back to raw manifests is
# exact (proven by `make helm-verify`).
OUT_DIR="$OUT_DIR" python3 "$REPO_ROOT/hack/gen_helm_chart.py" <<<"$RENDER"

echo "generated Helm templates in $OUT_DIR"
