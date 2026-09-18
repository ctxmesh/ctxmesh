#!/usr/bin/env python3
"""Prove the Helm chart does not drift from `kustomize build config/default`.

Compares two multi-document YAML streams resource-by-resource:
  arg1: the kustomize render   (`kustomize build config/default`)
  arg2: the helm render        (`helm template ... <chart>` with defaults)

Pure stdlib (no PyYAML) so it runs on a clean CI host. Both streams originate
from the SAME source manifests, so a resource's body lines are identical in
order; the chart only reorders whole resources and adds comment banners. So the
check is: bucket each stream into resources keyed by (kind, namespace, name),
then compare each resource's NORMALIZED line list (comments/blank-lines/trailing
whitespace stripped, plus the cosmetic app.kubernetes.io/managed-by label which
differs kustomize=kustomize vs Helm=Helm — provenance only, not functional).

Exits non-zero and prints the delta on any drift — the m12.2 landmine.
"""
from __future__ import annotations

import re
import sys

# Helm HOOK resources (a `helm.sh/hook` annotation) are install/upgrade LIFECYCLE actions — a
# preflight check, a keygen bootstrap — NOT steady-state manifests that `kustomize build config/default`
# represents. They legitimately have no kustomize counterpart, so they are excluded from the no-drift
# diff (else an always-rendered hook shows as "EXTRA in helm"). Matches quoted + unquoted key forms.
_HOOK_RE = re.compile(r"""(?m)^\s*["']?helm\.sh/hook["']?\s*:""")


def split_docs(text: str) -> list[str]:
    docs = []
    for chunk in re.split(r"(?m)^---\s*$", text):
        if chunk.strip():
            docs.append(chunk)
    return docs


def normalize(doc: str) -> list[str]:
    lines = []
    for ln in doc.splitlines():
        # Drop full-line comments and blank lines (Helm banners / kustomize noise).
        stripped = ln.strip()
        if not stripped or stripped.startswith("#"):
            continue
        # Drop the cosmetic provenance label line entirely.
        if stripped.startswith("app.kubernetes.io/managed-by:"):
            continue
        # An env `value:` is a string either way, so quoting is not a difference. kustomize emits
        # image refs bare while the chart pipes them through `| quote`; comparing the two as text
        # reported that as drift. Scoped to `value:` deliberately -- elsewhere "true" and true are
        # not the same thing, and this must not start pretending they are.
        m = re.match(r'^(\s*value:\s*)(["\'])(.*)\2\s*$', ln)
        if m:
            lines.append(m.group(1).rstrip() + " " + m.group(3))
            continue
        lines.append(ln.rstrip())
    return lines


def field(doc: str, path_re: str) -> str:
    m = re.search(path_re, doc, re.MULTILINE)
    return m.group(1) if m else ""


def key_of(doc: str) -> tuple[str, str, str]:
    kind = field(doc, r"^kind:\s*(\S+)")
    name = field(doc, r"^  name:\s*(\S+)")
    ns = field(doc, r"^  namespace:\s*(\S+)")
    return (kind, ns, name)


def bucket(text: str) -> dict[tuple[str, str, str], list[str]]:
    out = {}
    for doc in split_docs(text):
        if not re.search(r"^kind:", doc, re.MULTILINE):
            continue
        if _HOOK_RE.search(doc):
            continue  # Helm hooks (preflight, keygen) are lifecycle actions, not steady-state resources
        out[key_of(doc)] = normalize(doc)
    return out


def main() -> None:
    with open(sys.argv[1]) as f:
        kustomize = bucket(f.read())
    with open(sys.argv[2]) as f:
        helm = bucket(f.read())

    problems = []
    # The RELEASE Namespace is deliberately kustomize-only (2026-09-18). The chart used to template
    # it, carrying `managed-by: kustomize` from config/, and with `--create-namespace` -- which the
    # public docs tell every user to pass -- Helm creates the namespace itself and then cannot adopt
    # its own copy, so the documented first command failed on a cold cluster. `--create-namespace`
    # owns it now, as Helm prescribes and as every mature chart does.
    #
    # This ALLOWANCE is narrow and the bar is net stronger, not weaker: the exception is keyed to
    # exactly one resource, and the assertion below turns it into a positive one -- the chart must
    # contain NO release-namespace object, so the thing that broke the install cannot creep back in
    # unnoticed. Every other resource is still compared byte for byte.
    release_ns = ("Namespace", "", "ctxmesh")
    if release_ns in helm:
        problems.append(
            "FORBIDDEN in helm: the chart templates the RELEASE Namespace again. That object broke "
            "`helm install --create-namespace` on a cold cluster; `--create-namespace` owns it."
        )
    ns_only_in_kustomize = release_ns in kustomize and release_ns not in helm

    for k in sorted(set(kustomize) - set(helm)):
        if k == release_ns and ns_only_in_kustomize:
            continue
        problems.append(f"MISSING in helm:  {k}")
    for k in sorted(set(helm) - set(kustomize)):
        problems.append(f"EXTRA in helm:    {k}")

    import difflib

    for key in sorted(set(kustomize) & set(helm)):
        a, b = kustomize[key], helm[key]
        if a != b:
            problems.append(f"DIFFERS: {key}")
            for line in difflib.unified_diff(a, b, "kustomize", "helm", lineterm=""):
                problems.append("    " + line)

    if problems:
        print("NO-DRIFT CHECK FAILED — chart diverges from `kustomize build config/default`:")
        print("\n".join(problems))
        sys.exit(1)

    print(
        f"NO-DRIFT OK: {len(kustomize)} resources match between "
        "`kustomize build config/default` and `helm template` with the dev image overrides."
    )


if __name__ == "__main__":
    main()
