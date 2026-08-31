#!/usr/bin/env bash
# ui-colour-literals.sh — the colour doctrine, enforced (M151).
#
# tokens.css is the single source of truth for the brand (ADR 0010). That claim
# is only true if it is checked: a design system decays one hardcoded #hex at a
# time, and each one is invisible in review. This script fails the build if a
# colour literal appears anywhere in the UI source outside tokens.css.
#
# Allowed exceptions are listed explicitly below, each with a reason — an
# unexplained exception is how the rule dies.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UI_SRC="$(cd "$SCRIPT_DIR/../ui" && pwd)/src"

# A colour literal: #rgb / #rrggbb / #rrggbbaa, rgb(), rgba(), hsl(), hsla().
# `hsl(var(--x))` is the SANCTIONED form — that is a token being consumed, not a
# colour being invented — so it is excluded by the second filter.
#
# Lengths are pinned to 3 / 6 / 8 rather than a 3-8 range on purpose: the range
# matches "#4021" in ordinary prose (an order number in a wireframe's sample
# copy) and reports four false positives. The cost is that the rare 4-digit
# #rgba form would slip through; that trade is worth a gate people trust.
PATTERN='#([0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3})([^0-9a-fA-F]|$)|rgba?\(|hsla?\('

violations="$(
  grep -rnE "$PATTERN" "$UI_SRC" \
    --include='*.ts' --include='*.tsx' --include='*.css' \
    2>/dev/null \
  | grep -v '/styles/tokens\.css:' \
  | grep -vE 'hsla?\(\s*var\(--' \
  | grep -vE '^\S+\.(test|spec)\.tsx?:' \
  | grep -vE '#[0-9a-fA-F]{3,8}\b.*(eslint|https?://)' \
  || true
)"

if [ -n "$violations" ]; then
  echo "FAIL: colour literals found outside ui/src/styles/tokens.css" >&2
  echo "" >&2
  echo "$violations" >&2
  echo "" >&2
  echo "Colour lives in exactly one file. Add a semantic token in tokens.css," >&2
  echo "map it in tailwind.config.ts, and consume the utility." >&2
  exit 1
fi

echo "PASS: no colour literals outside tokens.css"
