#!/usr/bin/env bash
# bump-version — one writer for every version this repo states about itself.
#
# WHY THIS EXISTS
# ---------------
# ADR 0135 splits the SDKs in two: Python and TypeScript are STAMPED by the release workflow, and
# the other four ship VERBATIM — whatever is in the tree is what a user installs. Nothing wrote
# those four, so a version bump meant hand-editing four manifests and two README pins, and
# `hack/sdk-contract.sh` only caught the manifests.
#
# The READMEs drifted exactly there: Rust and Java sat at 0.1.0-beta.1 through three releases.
# Java's was a hard 404 on Maven Central, so its copy-paste install failed outright, and crates.io
# spent that whole time telling people to install a version the crate had moved past.
#
# So: one script writes them all, and --check asserts they agree with CHANGELOG.md.
#
#   ./hack/bump-version.sh 0.1.0-beta.4    write every file
#   ./hack/bump-version.sh --check         assert they already agree (wired into tier0)
#
# NOT handled here, on purpose:
#   • sdk/python and sdk/typescript — the release workflow stamps those (ADR 0135). Writing them
#     here would fight it.
#   • CHANGELOG.md — a release note is prose a human writes; this reads it as the source of truth.
set -euo pipefail
cd "$(dirname "$0")/.."

CHECK=0
case "${1:-}" in
  --check) CHECK=1 ;;
  "")      echo "usage: $0 <version>|--check   e.g. $0 0.1.0-beta.4" >&2; exit 1 ;;
esac

product="$(grep -m1 -oE '^## v[0-9][^ ]*' CHANGELOG.md | sed 's/^## v//')"
[ -n "$product" ] || { echo "FAIL: could not read the product version from CHANGELOG.md" >&2; exit 1; }
target="$([ "$CHECK" = "1" ] && printf '%s' "$product" || printf '%s' "$1")"

python3 - "$target" "$CHECK" "$product" <<'PY'
import pathlib, re, sys

target, check, product = sys.argv[1], sys.argv[2] == "1", sys.argv[3]

# (file, regex, replacement-template). Every pattern is anchored so it cannot match a dependency's
# version — the Java pom carries seven <version> tags and only the first is the project's.
EDITS = [
    ("sdk/rust/Cargo.toml",              r'(?m)^version = "[^"]*"',        'version = "{v}"'),
    ("sdk/go/version.go",                r'(?m)^const Version = "[^"]*"',  'const Version = "{v}"'),
    ("sdk/ruby/lib/ctxmesh/version.rb",  r'(?m)^  VERSION = "[^"]*"',      '  VERSION = "{v}"'),
    ("sdk/java/pom.xml",                 r'<version>[^<]*</version>',      '<version>{v}</version>'),
    # The install snippets a stranger copies. These are the ones that drifted.
    ("sdk/rust/README.md",               r'(?m)^ctxmesh = "[^"]*"',        'ctxmesh = "{v}"'),
    ("sdk/java/README.md",               r'<version>[^<]*</version>',      '<version>{v}</version>'),
]

rc = 0
for rel, pat, tmpl in EDITS:
    p = pathlib.Path(rel)
    if not p.exists():
        print(f"FAIL: {rel} is missing", file=sys.stderr); rc = 1; continue
    text = p.read_text()
    m = re.search(pat, text)
    if not m:
        print(f"FAIL: {rel} has no version where this script looks (pattern: {pat})", file=sys.stderr)
        rc = 1; continue
    found = re.search(r'"([^"]*)"|<version>([^<]*)</version>', m.group(0))
    current = (found.group(1) or found.group(2)) if found else "?"
    if check:
        if current != product:
            print(f"FAIL: {rel} declares {current} but the product is {product} — "
                  f"nothing stamps it at release, so that is what a user gets", file=sys.stderr)
            rc = 1
        else:
            print(f"  ok: {rel} at {current}")
    else:
        if current == target:
            print(f"  ok: {rel} already at {target}")
        else:
            p.write_text(re.sub(pat, tmpl.format(v=target), text, count=1))
            print(f"  {rel}: {current} -> {target}")

if rc:
    sys.exit(1)
if check:
    print(f"PASS: every verbatim-shipped version and README pin is at {product}")
else:
    print(f"wrote {target}. RubyGems will normalise it to {target.replace('-', '.pre.', 1)} on push.")
PY
