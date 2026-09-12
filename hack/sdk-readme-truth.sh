#!/usr/bin/env bash
# sdk-readme-truth — the page a stranger lands on must say what this is, and its install must work.
#
# WHY THIS EXISTS
# ---------------
# Measured 2026-09-12 against what the six registries actually render, not against the repo:
# four of six SDK READMEs (Go, Rust, Ruby, Java) never linked github.com/ctxmesh/ctxmesh at all,
# and Java pinned 0.1.0-beta.1 — a hard 404 on Central, so the copy-paste install failed outright.
#
# Two properties make this checkable only the hard way:
#
#   1. A pinned version can be perfectly well-formed and still not exist. Java's `0.1.0-beta.1`
#      would pass any string comparison against a CHANGELOG that also said beta.1. It 404s.
#      So --live asks the registry.
#   2. Each ecosystem SELECTS differently, and so do TOOLS WITHIN one ecosystem. `gem install
#      ctxmesh` refuses a prerelease-only gem while a Gemfile's `gem "ctxmesh"` resolves it,
#      so the same package is broken or fine depending on which command the README prints.
#      A generic "does any version exist" check calls everything fine and is worthless. The
#      first version of this gate failed Ruby for exactly that conflation.
#
# Registries also serve the README baked into the PUBLISHED artifact, so a green run here proves
# the NEXT publish will be right — never that the live page is. Task m175.6 re-reads the published
# artifacts; that is a different check and it is not this one.
#
# Usage:
#   ./hack/sdk-readme-truth.sh          structural only (no network) — wired into tier0
#   ./hack/sdk-readme-truth.sh --live   also resolve every install against its real registry
set -euo pipefail
cd "$(dirname "$0")/.."

LIVE=0
[ "${1:-}" = "--live" ] && LIVE=1

REPO="github.com/ctxmesh/ctxmesh"
SDKS="python typescript go rust ruby java"

product="$(grep -m1 -oE '^## v[0-9][^ ]*' CHANGELOG.md | sed 's/^## v//')"
[ -n "$product" ] || { echo "FAIL: could not read the product version from CHANGELOG.md" >&2; exit 1; }

rc=0
bad() { echo "FAIL: $*" >&2; rc=1; }
ok()  { echo "  ok: $*"; }

# The version a README pins, if it pins one at all. Python/TypeScript/Go/Ruby deliberately do not
# pin — an unpinned install is correct for them and is checked by its selection rule under --live.
pin_for() {
  case "$1" in
    rust) grep -oE '^ctxmesh = "[^"]+"' "$2" | head -1 | sed 's/.*"\(.*\)"/\1/' ;;
    # NOT sed 's|</\?version>||g' — \? is a GNU extension and BSD sed leaves the tags in,
    # which silently turned the Java pin into a string no registry could ever match.
    java) grep -oE '<version>[^<]+</version>' "$2" | head -1 | sed -e 's|<version>||' -e 's|</version>||' ;;
    *)    printf '' ;;
  esac
}

# The DESCRIPTION field, which is a different surface from the README and on two registries is the
# ONLY one. rubygems.org and Maven Central render no README at all — their page body is the gem
# description / POM <description>. So a README footer, however good, is invisible there: measured
# 2026-09-12, rubygems.org/gems/ctxmesh showed zero of it. A reader saw "typed clients for agents
# running on ctxmesh" and had no way to learn what ctxmesh is or where it lives.
description_of() {
  case "$1" in
    python)     grep -m1 '^description = ' sdk/python/pyproject.toml ;;
    typescript) python3 -c 'import json;print(json.load(open("sdk/typescript/package.json"))["description"])' ;;
    rust)       grep -m1 '^description = ' sdk/rust/Cargo.toml ;;
    ruby)       sed -n '/spec.description/,/^$/p' sdk/ruby/ctxmesh.gemspec ;;
    java)       python3 -c 'import re;s=open("sdk/java/pom.xml").read();m=re.search(r"<description>(.*?)</description>",s,re.S);print(m.group(1) if m else "")' ;;
    go)         sed -n '1,12p' sdk/go/config.go ;;
  esac
}

echo "product version: $product"
for d in $SDKS; do
  f="sdk/$d/README.md"
  if [ ! -f "$f" ]; then
    bad "$d has no README — its registry page would render blank"
    continue
  fi

  # (a) Can a stranger get from the registry page back to the project?
  if grep -q "$REPO" "$f"; then
    ok "$d README links the repository"
  else
    bad "$d README never links $REPO — someone landing on the registry page cannot find the project"
  fi

  # (b) The description must orient a stranger too — it is the whole page on two registries.
  if description_of "$d" 2>/dev/null | grep -q "$REPO"; then
    ok "$d description references the repository"
  else
    bad "$d description never mentions $REPO — on rubygems.org and Maven Central the description IS the page, and no README is rendered there"
  fi

  # (b2) npm SILENTLY TRUNCATES a description at 255 characters. A 342-char one published as 255,
  # cut mid-word, losing the repo URL that was at the end — and every local check passed, because
  # the file was fine. Only the registry knew. Checked here so the trailing reference survives.
  if [ "$d" = "typescript" ]; then
    len="$(python3 -c 'import json;print(len(json.load(open("sdk/typescript/package.json"))["description"]))')"
    if [ "$len" -le 255 ]; then
      ok "typescript description is ${len} chars (npm truncates above 255)"
    else
      bad "typescript description is ${len} chars — npm truncates at 255 and would cut off the repo URL at the end"
    fi
  fi

  # (c) If it pins a version, that version must be the one being shipped.
  pin="$(pin_for "$d" "$f")"
  if [ -n "$pin" ]; then
    if [ "$pin" = "$product" ]; then
      ok "$d pins $pin"
    else
      bad "$d README pins $pin but the product ships $product — a copy-paste install gets the wrong version"
    fi
  fi
done

[ "$LIVE" = "1" ] || {
  [ "$rc" = "0" ] || exit 1
  echo "PASS: every SDK README links the repo and pins the shipped version (structural; --live also resolves them)"
  exit 0
}

# ── live resolution ───────────────────────────────────────────────────────────
# Each ecosystem's own selection rule, because that is where Ruby and Java differ from the rest.
echo
echo "resolving every install against its real registry:"

fetch() { curl -sS --max-time 25 -H 'User-Agent: ctxmesh-readme-truth' "$1" 2>/dev/null; }

# pip install ctxmesh — pip selects a prerelease only when NO stable release exists.
py="$(fetch https://pypi.org/pypi/ctxmesh/json | python3 -c '
import json,sys
try: rel=json.load(sys.stdin)["releases"]
except Exception: print("ERR"); raise SystemExit
vs=[v for v,f in rel.items() if f]
stable=[v for v in vs if not any(c in v for c in ("a","b","rc","dev"))]
print("OK" if (stable or vs) else "NONE")' 2>/dev/null || echo ERR)"
[ "$py" = "OK" ] && ok "python: 'pip install ctxmesh' resolves" \
  || bad "python: 'pip install ctxmesh' does not resolve ($py)"

# npm install ctxmesh@beta — the dist-tag must exist.
ts="$(fetch https://registry.npmjs.org/ctxmesh | python3 -c '
import json,sys
try: print(json.load(sys.stdin).get("dist-tags",{}).get("beta") or "NONE")
except Exception: print("ERR")' 2>/dev/null || echo ERR)"
case "$ts" in
  NONE|ERR) bad "typescript: README says 'npm install ctxmesh@beta' and there is no 'beta' dist-tag ($ts)" ;;
  *)        ok "typescript: 'ctxmesh@beta' resolves to $ts" ;;
esac

# go get .../sdk/go — the module proxy must list a version.
gov="$(fetch https://proxy.golang.org/github.com/ctxmesh/ctxmesh/sdk/go/@v/list | tail -1)"
[ -n "$gov" ] && ok "go: the module proxy serves $gov" \
  || bad "go: proxy.golang.org lists no version for sdk/go — 'go get' cannot resolve it"

# cargo — a version satisfying the pin must exist on crates.io.
rpin="$(pin_for rust sdk/rust/README.md)"
rs="$(fetch https://crates.io/api/v1/crates/ctxmesh | python3 -c '
import json,sys
try: print(",".join(v["num"] for v in json.load(sys.stdin).get("versions",[])))
except Exception: print("ERR")' 2>/dev/null || echo ERR)"
if [ "$rs" = "ERR" ] || [ -z "$rs" ]; then
  bad "rust: could not read crates.io versions"
elif printf '%s' "$rs" | tr ',' '\n' | grep -qx "$rpin"; then
  ok "rust: crates.io serves the pinned $rpin"
else
  bad "rust: README pins $rpin and crates.io serves [$rs] — cargo may still resolve it via caret, but the page lies"
fi

# Ruby has TWO install forms with DIFFERENT prerelease rules, and conflating them is a false
# positive either way (measured 2026-09-12):
#   • Gemfile `gem "ctxmesh"` via bundler  -> RESOLVES to 0.1.0.pre.beta.3. Bundler takes a
#     prerelease when it is the only version published.
#   • CLI `gem install ctxmesh`            -> FAILS, "Could not find a valid gem". The CLI will
#     not select a prerelease without --pre or an exact version.
# So the check depends on which form the README actually shows.
rb="$(fetch https://rubygems.org/api/v1/versions/ctxmesh.json | python3 -c '
import json,sys
try: vs=[v["number"] for v in json.load(sys.stdin)]
except Exception: print("ERR"); raise SystemExit
stable=[v for v in vs if "pre" not in v and not any(c.isalpha() for c in v)]
print("STABLE:"+stable[0] if stable else "PRERELEASE_ONLY:"+",".join(vs))' 2>/dev/null || echo ERR)"
case "$rb" in
  STABLE:*) ok "ruby: a stable gem exists, so a bare gem \"ctxmesh\" resolves (${rb#STABLE:})" ;;
  PRERELEASE_ONLY:*)
    vs="${rb#PRERELEASE_ONLY:}"
    if grep -qE '^gem install ctxmesh\s*$' sdk/ruby/README.md; then
      bad "ruby: only prereleases are published ($vs) and the README shows a bare 'gem install ctxmesh', which fails with 'Could not find a valid gem' — it needs --pre or an exact version"
    elif grep -qE '^gem "ctxmesh"' sdk/ruby/README.md; then
      ok "ruby: only prereleases are published ($vs); the README shows a Gemfile line, which bundler resolves"
    else
      ok "ruby: only prereleases are published ($vs); the README names a version explicitly"
    fi ;;
  *) bad "ruby: could not read RubyGems versions ($rb)" ;;
esac

# Maven resolves the EXACT version. This is the one that was a 404.
jpin="$(pin_for java sdk/java/README.md)"
jcode="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 25 \
  "https://repo1.maven.org/maven2/ai/ctxmesh/ctxmesh/$jpin/ctxmesh-$jpin.pom" 2>/dev/null || echo 000)"
[ "$jcode" = "200" ] && ok "java: Central serves the pinned $jpin" \
  || bad "java: README pins $jpin and Central answers HTTP $jcode for its pom — the copy-paste install fails outright"

# Every link the README offers must actually go somewhere. Added after writing the footers found
# /sdk/go/, /sdk/rust/, /sdk/ruby/ and /sdk/java/ all 404 — four published SDKs with no docs page.
# A footer pointing at a dead page is the same defect as a pin naming a version that does not exist.
echo
echo "resolving every documentation link:"
for d in $SDKS; do
  f="sdk/$d/README.md"
  urls="$(grep -oE 'https://(ctxmesh\.github\.io|github\.com/ctxmesh)[^ )"]*' "$f" | sed 's/[.,]$//' | sort -u)"
  [ -n "$urls" ] || continue
  dead=""
  while read -r u; do
    [ -n "$u" ] || continue
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -L "$u" 2>/dev/null || echo 000)"
    case "$code" in 2*|3*) ;; *) dead="$dead\n      $code  $u" ;; esac
  done <<< "$urls"
  if [ -n "$dead" ]; then
    bad "$d README links pages that do not resolve:$(printf "$dead")"
  else
    ok "$d: every documentation link resolves ($(printf '%s' "$urls" | grep -c . ) links)"
  fi
done

[ "$rc" = "0" ] || exit 1
echo "PASS: every SDK README links the repo, pins $product, resolves on its own registry, and every doc link is live"
