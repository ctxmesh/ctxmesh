#!/usr/bin/env python3
"""The images the Helm chart renders, and the checks over them.

  list [--third-party | --first-party]
                         every image reference the chart renders, one per line
  seamed                 fail on any image no chart value can move, or a third-party image without a
                         digest, or a mirror list out of step with thirdPartyImages
  mirror-plan            "<source> <destination>" for each image the mirror list marks for mirroring

An image counts wherever a pod would pull it: an `image:` field, or an env var named *_IMAGE (the
images the controller injects into agent pods). The chart is rendered in three configurations so an
image behind a switch is not missed: the defaults, the README's production command, and every
optional component on.

Usage: hack/chart_images.py <command> [--chart DIR] [-- extra helm args applied to every render]
"""
from __future__ import annotations

import argparse
import pathlib
import subprocess
import sys

import yaml

REPO = pathlib.Path(__file__).resolve().parent.parent
DEFAULT_CHART = REPO / "deploy/helm/ctxmesh"
MIRROR_LIST = REPO / "hack/third-party-images.txt"
MIRROR_REGISTRY = "ghcr.io/ctxmesh/mirror"

# First-party images carry Chart.yaml's appVersion, the product version bump-version.sh writes. On a
# milestone branch that is the upcoming release, published only when it is tagged, so they are
# listed apart and pulled where the version exists (the release, and nightly on main).
FIRST_PARTY_PREFIX = "ghcr.io/ctxmesh/"

# The README's production command, plus the two bff.mcp values the production guards also require
# (render-only placeholders, like the DSN, which the render never dials).
PRODUCTION = [
    "--set", "profile=production",
    "-f", "{chart}/values-production.yaml",
    "--set", "postgres.externalDsn=postgres://user:pass@db.example:5432/ctxmesh?sslmode=require",
    "--set", "bff.mcp.grantHmacKey=render-only",
    "--set", "bff.mcp.credentialNamespace=ctxmesh-credentials",
]
OPTIONAL_ON = [
    "--set", "devDataPlane.nats.enabled=true",
    "--set", "auth.oidc.enabled=true",
]
RENDERS = {"default": [], "production": PRODUCTION, "optional-on": OPTIONAL_ON}

# Every values key that names an image. `seamed` points all of them at SEAM and then expects every
# rendered image to start with it; one that does not is hardcoded in a template. A new image seam
# belongs here too, or this check reports its image as hardcoded.
SEAM = "seam.invalid"
FIRST_PARTY_SEAMS = {
    "controllerManager.image.repository": "controller",
    "bff.image.repository": "bff",
    "tokenService.image.repository": "token-service",
    "statelayerProxy.image.repository": "statelayer-proxy",
    "controllerManager.injectedImages.collector": "collector",
    "controllerManager.injectedImages.discovery": "discovery",
    "controllerManager.oboEgress.sidecarImage": "egress-sidecar",
    "bff.managedAgentImage": "managed-agent",
}


def render(chart: pathlib.Path, args: list[str]) -> str:
    cmd = ["helm", "template", "ctxmesh", str(chart), "-n", "ctxmesh"]
    cmd += [a.replace("{chart}", str(chart)) for a in args]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        sys.exit(f"FAIL: the chart does not render with {' '.join(args) or 'default values'}:\n{res.stderr}")
    return res.stdout


def images(text: str) -> set[str]:
    found: set[str] = set()

    def walk(node: object) -> None:
        if isinstance(node, dict):
            img = node.get("image")
            if isinstance(img, str) and img.strip():
                found.add(img.strip())
            name, value = node.get("name"), node.get("value")
            if isinstance(name, str) and name.endswith("_IMAGE") and isinstance(value, str) and value.strip():
                found.add(value.strip())
            for v in node.values():
                walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)

    for doc in yaml.safe_load_all(text):
        if isinstance(doc, dict) and doc.get("kind") == "CustomResourceDefinition":
            continue  # schemas describe images; they do not pull any
        walk(doc)
    return found


def rendered(chart: pathlib.Path, extra: list[str]) -> dict[str, set[str]]:
    return {label: images(render(chart, args + extra)) for label, args in RENDERS.items()}


def is_first_party(ref: str) -> bool:
    return ref.startswith(FIRST_PARTY_PREFIX) and not ref.startswith(MIRROR_REGISTRY + "/")


def chart_values(chart: pathlib.Path) -> dict:
    return yaml.safe_load((chart / "values.yaml").read_text())


def mirror_list() -> dict[str, tuple[str, str]]:
    rows: dict[str, tuple[str, str]] = {}
    for n, line in enumerate(MIRROR_LIST.read_text().splitlines(), 1):
        line = line.split("#", 1)[0].strip()
        if not line:
            continue
        parts = line.split()
        if len(parts) != 3 or parts[2] not in ("yes", "no"):
            sys.exit(f"FAIL: {MIRROR_LIST.name}:{n}: expected '<key> <license> <yes|no>', got {line!r}")
        rows[parts[0]] = (parts[1], parts[2])
    return rows


def third_party_entries(chart: pathlib.Path) -> dict[str, dict]:
    tpi = chart_values(chart).get("thirdPartyImages") or {}
    return {k: v for k, v in tpi.items() if isinstance(v, dict)}


def cmd_list(chart: pathlib.Path, extra: list[str], third_party: bool, first_party: bool) -> int:
    refs = set().union(*rendered(chart, extra).values())
    if third_party:
        refs = {r for r in refs if not is_first_party(r)}
    if first_party:
        refs = {r for r in refs if is_first_party(r)}
    if not refs:
        print("FAIL: the chart rendered no images; this is not checking anything", file=sys.stderr)
        return 1
    print("\n".join(sorted(refs)))
    return 0


def cmd_seamed(chart: pathlib.Path, extra: list[str]) -> int:
    problems: list[str] = []
    seam_args = ["--set", f"thirdPartyImages.registryOverride={SEAM}"]
    for key, name in FIRST_PARTY_SEAMS.items():
        seam_args += ["--set", f"{key}={SEAM}/{name}"]
    checked = 0
    for label, refs in rendered(chart, seam_args + extra).items():
        for ref in sorted(refs):
            checked += 1
            if not ref.startswith(SEAM + "/"):
                problems.append(
                    f"{label} render: {ref} is hardcoded; no chart value moves it. Template it from "
                    "thirdPartyImages (third-party) or an image value listed in hack/chart_images.py."
                )
    if checked == 0:
        problems.append("the chart rendered no images; this is not checking anything")

    for label, refs in rendered(chart, extra).items():
        for ref in sorted(refs):
            if not is_first_party(ref) and "@sha256:" not in ref:
                problems.append(f"{label} render: third-party image {ref} has no digest")

    entries = third_party_entries(chart)
    rows = mirror_list()
    for key in sorted(set(entries) - set(rows)):
        problems.append(f"thirdPartyImages.{key} has no row in {MIRROR_LIST.name}; add its licence and mirror decision")
    for key in sorted(set(rows) - set(entries)):
        problems.append(f"{MIRROR_LIST.name} lists {key}, which is not in thirdPartyImages")

    if problems:
        for p in problems:
            print(f"FAIL: {p}", file=sys.stderr)
        return 1
    print(f"PASS: {checked} image references across {len(RENDERS)} renders all come from chart values; "
          f"every third-party image is digest-pinned and in {MIRROR_LIST.name}")
    return 0


def cmd_mirror_plan(chart: pathlib.Path) -> int:
    entries = third_party_entries(chart)
    plan = []
    for key, (_, mirror) in sorted(mirror_list().items()):
        if mirror != "yes":
            continue
        e = entries.get(key)
        if not e or not e.get("digest") or not e.get("tag"):
            print(f"FAIL: {key} is marked for mirroring and has no tag and digest in thirdPartyImages", file=sys.stderr)
            return 1
        src = f"{e['registry']}/{e['repository']}@{e['digest']}"
        dst = f"{MIRROR_REGISTRY}/{e['repository']}:{e['tag']}"
        plan.append(f"{src} {dst} {e['digest']}")
    if not plan:
        print("FAIL: nothing is marked for mirroring", file=sys.stderr)
        return 1
    print("\n".join(plan))
    return 0


def main() -> int:
    argv = sys.argv[1:]
    extra: list[str] = []
    if "--" in argv:
        i = argv.index("--")
        argv, extra = argv[:i], argv[i + 1 :]
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["list", "seamed", "mirror-plan"])
    ap.add_argument("--chart", default=str(DEFAULT_CHART))
    only = ap.add_mutually_exclusive_group()
    only.add_argument("--third-party", action="store_true", help="list: leave out first-party images")
    only.add_argument("--first-party", action="store_true", help="list: only ghcr.io/ctxmesh images")
    ns = ap.parse_args(argv)
    chart = pathlib.Path(ns.chart).resolve()
    if ns.command == "list":
        return cmd_list(chart, extra, ns.third_party, ns.first_party)
    if ns.command == "seamed":
        return cmd_seamed(chart, extra)
    return cmd_mirror_plan(chart)


if __name__ == "__main__":
    sys.exit(main())
