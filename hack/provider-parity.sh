#!/usr/bin/env bash
# provider-parity — the console may not offer a provider the API rejects.
#
# The connect wizard renders a fixed list of providers (ui/src/pages/connect-provider-page.tsx,
# `PROVIDERS`). The BFF validates the posted provider against its own switch
# (internal/bff/provider_client.go, `providerModels`). Nothing tied the two together, and
# they drifted: the console offered "Custom / OpenAI-compatible" and Google, the BFF
# supported only anthropic and openai. A user picked one of the other two, typed their API
# key, and got `unsupported provider` — after the key was already in the form.
#
# That defect is invisible to every per-screen test. The connect page renders its four
# choices correctly; the BFF rejects "custom" correctly; only WALKING from one to the other
# reveals that the product promises something it cannot do. The M153 journey test found it,
# and this gate is what stops it coming back — the list of offers and the list of
# capabilities are now checked against each other on every build.
#
# Fails on divergence in EITHER direction:
#   • offered but unsupported → a user hits a wall after committing their key;
#   • supported but not offered → dead capability nobody can reach from the console.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI_PAGE="$ROOT/ui/src/pages/connect-provider-page.tsx"
BFF_CLIENT="$ROOT/internal/bff/provider_client.go"

[ -f "$UI_PAGE" ] || { echo "FAIL: $UI_PAGE is missing"; exit 1; }
[ -f "$BFF_CLIENT" ] || { echo "FAIL: $BFF_CLIENT is missing"; exit 1; }

# What the console OFFERS: the `id:` fields of the PROVIDERS literal.
offered="$(awk '/^const PROVIDERS = \[/,/^\] as const;/' "$UI_PAGE" \
  | grep -oE 'id: "[a-z0-9-]+"' | sed 's/id: "//; s/"//' | sort -u)"
# What the BFF SUPPORTS: the provider* constants the connect switch dispatches on.
# Read the case arms, not the constant block — a declared-but-unrouted constant is
# not support, and this gate is about what a POST actually accepts.
supported="$(awk '/^func providerModels\(/,/^}$/' "$BFF_CLIENT" \
  | grep -oE 'case provider[A-Za-z]+:' | sed 's/case provider//; s/://' \
  | tr '[:upper:]' '[:lower:]' | sort -u)"

[ -n "$offered" ] || { echo "FAIL: could not read the console's PROVIDERS list — the gate is not actually checking anything"; exit 1; }
[ -n "$supported" ] || { echo "FAIL: could not read the BFF's provider switch — the gate is not actually checking anything"; exit 1; }

rc=0
while read -r p; do
  [ -n "$p" ] || continue
  echo "$supported" | grep -qx "$p" || {
    echo "FAIL: the console offers provider '$p' but the BFF rejects it — a user picks it, types their key, and hits 'unsupported provider'"
    rc=1
  }
done <<< "$offered"
while read -r p; do
  [ -n "$p" ] || continue
  echo "$offered" | grep -qx "$p" || {
    echo "FAIL: the BFF supports provider '$p' but the console never offers it — a capability nobody can reach"
    rc=1
  }
done <<< "$supported"

[ "$rc" = "0" ] || exit 1
echo "PASS: the console offers exactly what the BFF supports ($(echo "$offered" | tr '\n' ' '))"
