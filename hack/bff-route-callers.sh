#!/usr/bin/env bash
# bff-route-callers — every /api route the BFF serves has a caller.
#
# The BFF's route table grew by accretion: a route added for a page that never shipped, or kept
# after its page moved, stays registered, authorized and reachable, and nothing notices. An audit
# in m184.38 found routes no shipped code had ever called. A route nobody calls is still attack
# surface and still has to be kept correct.
#
# A route passes when shipped code references its path:
#   console   ui/src (not tests)
#   sdk       sdk/<lang> sources (not tests)
#   platform  cmd/ and internal/ Go outside internal/bff (the launcher, the controller that writes
#             the BFF URLs into the launcher's environment, the CLI)
# Comment lines do not count. Wildcard segments match an interpolation or a literal; a literal
# reference is credited to the most specific route it matches, as the Go mux would route it.
#
# Where a path is served by more than one method, a reference counts for the methods named in its
# call (method: "PUT", http.MethodPut, postJSON, .delete( …: on its line, else within a few lines
# up to the next /api/ reference), or for GET when none is. A method named on an unrelated line
# that close can still credit a route it should not; the gate accepts that rather than parse six
# languages.
#
# A route with no such caller needs an entry in hack/bff-route-callers.allow:
#   documented  a public API on the docs site (CTXMESH_SITE, default ../ctxmesh.github.io),
#               checked there on every run
#   external    called from outside this repository (an OAuth server, the gateway)
#   kept        anything else, with the reason it must stay
# An entry for a route that is gone, or that now has a caller, fails too: the list only shrinks.
#
# Set BFF_ROUTE_CALLERS_ALLOW_PARTIAL=1 to skip the docs-site check, loudly, without a checkout.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ALLOW="$ROOT/hack/bff-route-callers.allow"
SITE="${CTXMESH_SITE:-$ROOT/../ctxmesh.github.io}"

[ -f "$ALLOW" ] || { echo "FAIL: $ALLOW is missing"; exit 1; }

python3 - "$ROOT" "$ALLOW" "$SITE" <<'PY'
import os
import re
import subprocess
import sys

root, allow_path, site = sys.argv[1:4]
allow_partial = os.environ.get("BFF_ROUTE_CALLERS_ALLOW_PARTIAL") == "1"
fails = []


def tracked(repo, *paths):
    out = subprocess.run(["git", "-C", repo, "ls-files", "--", *paths],
                         check=True, capture_output=True, text=True).stdout.split()
    return out


TEST = re.compile(r"(_test\.go$|\.(test|spec)\.(ts|tsx|js)$|/__tests__/|^ui/src/test/|/tests?/"
                  r"|(^|/)test_[^/]*\.py$|_test\.py$|/src/test/|/spec/)")
COMMENT = re.compile(r"^\s*(//|/\*|\*|#(?![{!])|--)")
METHODS = ("GET", "POST", "PUT", "DELETE", "PATCH")

# ── the route table ──────────────────────────────────────────────────────────
bff = [f for f in tracked(root, "internal/bff") if f.endswith(".go") and not TEST.search(f)]
consts = {}
for f in bff:
    for m in re.finditer(r'^\s*(?:const\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"(/api/[^"]*)"',
                         open(os.path.join(root, f)).read(), re.M):
        consts[m.group(1)] = m.group(2)

REG = re.compile(r"\b(api|authed|mux)\.(Handle|HandleFunc)\((.*)$")
LIT = re.compile(r'^"(?:(' + "|".join(METHODS) + r') )?(/api/[^"]*)"')
CAT = re.compile(r'^"(' + "|".join(METHODS) + r') "\s*\+\s*([A-Za-z_][A-Za-z0-9_]*)')

routes = {}  # (method, path) -> "file:line"
for f in bff:
    for i, line in enumerate(open(os.path.join(root, f)), 1):
        if COMMENT.match(line):
            continue
        m = REG.search(line)
        if not m:
            continue
        arg = m.group(3).strip()
        lit, cat = LIT.match(arg), CAT.match(arg)
        if lit:
            method, path = lit.group(1) or "ANY", lit.group(2)
        elif cat and cat.group(2) in consts:
            method, path = cat.group(1), consts[cat.group(2)]
        else:
            fails.append(f"cannot read the route registered at {f}:{i} — register it with a literal "
                         f"\"METHOD /api/...\" pattern or a const this gate can resolve")
            continue
        if path == "/api/":
            continue  # the authenticated mount, not a route
        routes.setdefault((method, path), f"{f}:{i}")

if len(routes) < 100:
    fails.append(f"read only {len(routes)} routes from internal/bff — the gate is not actually checking anything")

paths = {}
for method, path in routes:
    paths.setdefault(path, set()).add(method)

WILD = (r"(?:\$\{[^}]*\}|#\{[^}]*\}|\{[^}/]*\}|%[sdvq]|:[A-Za-z_]+|<[^>/]+>"
        r"|\$\(?[A-Za-z_][A-Za-z0-9_]*\)?"
        r"|\"\s*\+\s*[^\"\n]+?\s*\+\s*\"|'\s*\+\s*[^'\n]+?\s*\+\s*'"
        r"|\"\s*\+\s*[A-Za-z0-9_.()\[\]]+"
        r"|[A-Za-z0-9_.\-~%@]+)")
END = r"(?=$|[\"'`?\s)$#,;\]>*}\\]|\"\s*\+)"


def path_regex(path):
    subtree = path.endswith("/")
    parts = []
    for seg in path.strip("/").split("/"):
        parts.append(WILD if seg.startswith("{") and seg.endswith("}") else re.escape(seg))
    return re.compile("/" + "/".join(parts) + ("/" if subtree else END))


def literal_count(path):
    return sum(1 for seg in path.strip("/").split("/") if not seg.startswith("{"))


rx = {p: path_regex(p) for p in paths}
MTOK = {
    "GET": re.compile(r"\bGET\b|MethodGet|getJSON|\.get\(|_get\(|::Get\b"),
    "POST": re.compile(r"\bPOST\b|MethodPost|postJSON|\.post\(|_post\(|::Post\b"),
    "PUT": re.compile(r"\bPUT\b|MethodPut|putJSON|\.put\(|_put\(|::Put\b"),
    "DELETE": re.compile(r"\bDELETE\b|MethodDelete|deleteJSON|\.delete\(|_delete\(|::Delete\b"),
    "PATCH": re.compile(r"\bPATCH\b|MethodPatch|patchJSON|\.patch\(|_patch\(|::Patch\b"),
}

# ── the callers ──────────────────────────────────────────────────────────────
SDK_SRC = ["sdk/python/src", "sdk/typescript/src", "sdk/go", "sdk/java/src/main", "sdk/rust/src", "sdk/ruby/lib"]
CODE = re.compile(r"\.(ts|tsx|js|go|py|java|rs|rb)$")
corpora = {
    "console": [f for f in tracked(root, "ui/src") if CODE.search(f) and not TEST.search(f)],
    "sdk": [f for f in tracked(root, *SDK_SRC) if CODE.search(f) and not TEST.search(f)],
    "platform": [f for f in tracked(root, "cmd", "internal")
                 if f.endswith(".go") and not TEST.search(f) and not f.startswith("internal/bff/")],
}
for name, files in corpora.items():
    if not files:
        fails.append(f"found no {name} sources — the gate is not actually checking anything")

callers = {}  # (method, path) -> first "corpus file:line"


def call_window(code, i):
    """The reference line, or when it names no method, the lines around it up to the nearest
    other /api/ reference: the extent of one call, as far as a line scan can tell."""
    if any(tok.search(code[i]) for tok in MTOK.values()):
        return code[i]
    lo = i
    while lo > max(0, i - 2) and "/api/" not in code[lo - 1]:
        lo -= 1
    hi = i
    while hi < min(len(code) - 1, i + 4) and "/api/" not in code[hi + 1]:
        hi += 1
    return "\n".join(code[lo:hi + 1])


def credit(method, path, where):
    if (method, path) in routes:
        callers.setdefault((method, path), where)


for corpus, files in corpora.items():
    for f in files:
        lines = open(os.path.join(root, f), errors="replace").read().split("\n")
        code = ["" if COMMENT.match(l) else l for l in lines]
        for i, line in enumerate(code):
            if "/api/" not in line:
                continue
            spans = {}
            for p, r in rx.items():
                for m in r.finditer(line):
                    spans.setdefault((m.start(), m.end()), []).append(p)
            for found in spans.values():
                best = max(literal_count(p) for p in found)
                for p in (p for p in found if literal_count(p) == best):
                    where = f"{corpus} {f}:{i + 1}"
                    served = paths[p]
                    if len(served) == 1:
                        credit(next(iter(served)), p, where)
                        continue
                    named = {mt for mt, tok in MTOK.items() if tok.search(call_window(code, i))} & served
                    for mt in named or {"GET"}:
                        credit(mt, p, where)

# ── the allowlist ────────────────────────────────────────────────────────────
KINDS = ("documented", "external", "kept")
allow = {}
for n, raw in enumerate(open(allow_path), 1):
    line = raw.strip()
    if not line or line.startswith("#"):
        continue
    cells = [c.strip() for c in line.split("|")]
    if len(cells) != 3 or not cells[2]:
        fails.append(f"{os.path.basename(allow_path)}:{n}: want 'METHOD /api/path | kind | reason'")
        continue
    route, kind, reason = cells
    method, _, path = route.partition(" ")
    if kind not in KINDS:
        fails.append(f"{os.path.basename(allow_path)}:{n}: kind '{kind}' is not one of {', '.join(KINDS)}")
    allow[(method, path)] = (kind, n)

site_docs = []
if any(kind == "documented" for kind, _ in allow.values()):
    docs = os.path.join(site, "src/content/docs")
    if os.path.isdir(docs):
        for dirpath, _, names in os.walk(docs):
            site_docs += [open(os.path.join(dirpath, x), errors="replace").read()
                          for x in names if x.endswith((".md", ".mdx"))]
    elif allow_partial:
        print(f"SKIP: no docs site at {site}; 'documented' entries are NOT checked (BFF_ROUTE_CALLERS_ALLOW_PARTIAL=1)")
    else:
        fails.append(f"the docs site is not checked out at {site}, so the 'documented' entries cannot be "
                     f"checked (set CTXMESH_SITE; BFF_ROUTE_CALLERS_ALLOW_PARTIAL=1 skips them loudly)")

for (method, path), (kind, n) in sorted(allow.items(), key=lambda kv: kv[1][1]):
    entry = f"{os.path.basename(allow_path)}:{n} ({method} {path})"
    if (method, path) not in routes:
        fails.append(f"{entry} names a route the BFF no longer serves — remove the entry")
    elif (method, path) in callers:
        fails.append(f"{entry} now has a caller ({callers[(method, path)]}) — remove the entry")
    elif kind == "documented" and site_docs and not any(path_regex(path).search(d) for d in site_docs):
        fails.append(f"{entry} is marked documented but the docs site does not mention {path}")

# ── verdict ──────────────────────────────────────────────────────────────────
orphans = sorted(r for r in routes if r not in callers and r not in allow)
for method, path in orphans:
    fails.append(f"{method} {path} (registered at {routes[(method, path)]}) has no caller in the console, "
                 f"an SDK or the platform. Delete it, or allowlist it in hack/bff-route-callers.allow "
                 f"with the reason it must stay")

if os.environ.get("BFF_ROUTE_CALLERS_VERBOSE") == "1":
    for r in sorted(callers):
        print(f"  {r[0]:6} {r[1]:50} {callers[r]}")

if fails:
    for f in fails:
        print(f"FAIL: {f}")
    sys.exit(1)

kinds = {k: sum(1 for kind, _ in allow.values() if kind == k) for k in KINDS}
print(f"PASS: {len(routes)} BFF routes; {len(callers)} called by the console, an SDK or the platform; "
      f"{len(allow)} allowlisted ({', '.join(f'{v} {k}' for k, v in kinds.items())})")
PY
