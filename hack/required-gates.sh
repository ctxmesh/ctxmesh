#!/usr/bin/env bash
# required-gates — compute which gates a change REQUIRES, from the change itself.
#
# WHY THIS EXISTS
# ---------------
# The close checklist asks whether the multi-agent UX review should run, and the answer has been
# "Not run — nothing touched the console" twice in a row. That judgement was mine, it went into an
# acceptance report as though it were a finding, and it was wrong: M177 shipped three console-facing
# defects — an empty namespace picker, a dead Connect provider, a gateway that met no PodSecurity
# standard — and the diff that caused them touched **zero files under ui/**.
#
# So the trigger cannot be a directory. A path→gate map keyed on `ui/` would have made exactly the
# same wrong call, with more authority for looking mechanical. What decides whether a gate is needed
# is the SURFACE a change reaches, and an RBAC binding reaches the console without touching it.
#
# TWO PROPERTIES THAT MAKE THIS WORTH HAVING
#
#   1. FAIL CLOSED. A path that matches no surface is an ERROR, not "no gates needed". A new
#      directory silently mapping to zero gates is the same defect class as a gate that cannot
#      fail. The fix for an unmapped path is to map it, deliberately, in this file.
#   2. THE MAP IS REVIEWABLE. The judgement moves out of my head and into a versioned table someone
#      can read and disagree with — "an RBAC change obviously affects the console, why isn't that
#      mapped?" That is the real win, more than the automation.
#
# WHAT IT DOES NOT DO, AND THIS MATTERS
#
#   • It does not judge gate QUALITY. flow-completability would have run on those RBAC changes and
#     still passed, because it checked that a role exists rather than that anyone holds it.
#     Selecting the right gate and the gate being right are different problems.
#   • The map is NOT CALIBRATED YET. Written in one sitting from one milestone's evidence, it is
#     deliberately broad — `control-plane.yaml` carries the RBAC roles, so most chart edits demand
#     ux-review. That is the right failure direction to start from, but a gate demanded on every
#     change gets skipped, and a skipped gate is where this whole problem began. Tighten it with
#     real use: when a required gate turns out not to have been needed, narrow the pattern here
#     rather than ignoring the output.
#



#
# Usage:
#   ./hack/required-gates.sh                 changes vs origin/main
#   ./hack/required-gates.sh <base>          changes vs an explicit base
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${1:-origin/main}"
git rev-parse --verify "$BASE" >/dev/null 2>&1 || { echo "FAIL: no such base '$BASE'" >&2; exit 1; }

CHANGED="$(git diff --name-only "$BASE"...HEAD)"
[ -n "$CHANGED" ] && echo "changed files vs $BASE: $(printf '%s\n' "$CHANGED" | wc -l | tr -d ' ')" \
                  || { echo "no changes vs $BASE — no gates required"; exit 0; }

# surface;path-regex;gates   (';' not '|': the patterns themselves contain alternation)
#
# A surface is what a USER experiences, not where the code lives. `deploy/.../dex.yaml` is an RBAC
# binding and reaches the console; `internal/controlplane/namespacetenant/` decides whether the
# namespace picker has anything in it. Both are console surfaces and neither is under ui/.
MAP='
console;^ui/;ux-review tier0 tier2-ui
console;^internal/bff/;ux-review tier0 tier1
console-authz;^config/rbac/;flow-completability ux-review tier1
console-authz;^deploy/helm/.*/(dex|control-plane)\.yaml$;flow-completability install-truth ux-review
console-authz;^internal/controlplane/namespacetenant/;flow-completability tier0 tier1
install;^deploy/helm/;install-truth helm-lint tier2
install;^config/(crd|default|gateway|manager)/;install-truth helm-generate tier1
install;^internal/controlplane/migrations/;tier1 install-truth
runtime;^cmd/launcher/;tier0 tier1 sdk-live
runtime;^internal/controller/;tier0 tier1 tier2
runtime;^internal/(run|gateway|egress|pki|statelayer)/;tier0 tier1
sdk;^sdk/;sdk-contract sdk-readme-truth tier0
release;^\.github/workflows/;release-truth tier0
release;^(CHANGELOG\.md|Makefile)$;release-truth tier0
docs;\.(md|mdx)$;docs-site-truth
build;^(Dockerfile.*|images/);tier0 tier2
meta;^(hack/|harness/|test/|examples/|\.gitignore|\.golangci\.yml|PROJECT|LICENSE|NOTICE|CODEOWNERS|SECURITY\.md|CONTRIBUTING\.md|CODE_OF_CONDUCT\.md|\.gitleaks\.toml);tier0
runtime;^cmd/;tier0 tier1 tier2
build;^(Dockerfile|images/);tier0 tier2
release;^(CHANGELOG\.md|Makefile|NOTICE)$;release-truth tier0
'

required=""; unmapped=""; surfaces=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  hit=0
  while IFS=';' read -r surface pattern gates; do
    [ -n "${pattern:-}" ] || continue
    if printf '%s' "$f" | grep -qE "$pattern"; then
      required="$required $gates"; surfaces="$surfaces $surface"; hit=1
    fi
  done <<< "$MAP"
  [ "$hit" = "0" ] && unmapped="$unmapped $f"
done <<< "$CHANGED"

echo
echo "surfaces reached:"
printf '%s\n' $surfaces | sort -u | sed 's/^/  /'
echo
echo "gates REQUIRED by this change:"
printf '%s\n' $required | sort -u | grep -v '^$' | sed 's/^/  /'

if [ -n "$unmapped" ]; then
  echo
  echo "FAIL: these paths match no surface, so the gates they need are unknown:" >&2
  printf '%s\n' $unmapped | sort -u | sed 's/^/  /' >&2
  echo "      Map them in hack/required-gates.sh. An unmapped path must never mean 'no gates'." >&2
  exit 1
fi

echo
echo "PASS: every changed path maps to a surface; the gates above are the ones this change requires"
