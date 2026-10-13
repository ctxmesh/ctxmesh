#!/usr/bin/env python3
"""The README's production install command, as the arguments a render needs.

install-truth.sh renders it and chart_images.py lists its images, so both check the command a
reader copies. A hand-kept copy of it is how the README's command went on failing to render while
every check passed: the copy carried the values the README left out.

The command is the `helm install` in the first fenced block after the marker below, with its
continuation lines. `$(...)` is not run: its text stands in for the value, which a render accepts
as long as it is not empty.

  readme_install.py production [--readme README.md] [--chart DIR]

prints the chart directory and then each helm flag, one per line, with the README's chart path
replaced by DIR when given.
"""
from __future__ import annotations

import argparse
import pathlib
import re
import shlex
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
MARKER = "<!-- ctxmesh:install production -->"
README_CHART = "deploy/helm/ctxmesh"

# Flags that change how helm waits or where it installs, not what it renders.
DROP_WITH_VALUE = {"--timeout", "-n", "--namespace"}
DROP = {"--wait", "--atomic", "--create-namespace"}


def production_install(readme: pathlib.Path, chart: str | None = None) -> tuple[str, list[str]]:
    text = readme.read_text()
    at = text.find(MARKER)
    if at < 0:
        sys.exit(f"FAIL: {readme} has no {MARKER} line before its production install command")
    block = re.search(r"```(?:sh|bash)\n(.*?)```", text[at:], re.S)
    if not block:
        sys.exit(f"FAIL: {readme}: no fenced sh block follows {MARKER}")

    command, collecting = [], False
    for line in block.group(1).splitlines():
        if line.lstrip().startswith("helm install "):
            collecting = True
        if collecting:
            command.append(line.rstrip().removesuffix("\\"))
            if not line.rstrip().endswith("\\"):
                break
    if not command:
        sys.exit(f"FAIL: {readme}: the block after {MARKER} has no `helm install` command")

    argv = shlex.split(" ".join(command))
    if len(argv) < 4:
        sys.exit(f"FAIL: {readme}: `{' '.join(argv)}` names no release and chart")
    chart_ref, flags = argv[3], argv[4:]

    out: list[str] = []
    skip = False
    for arg in flags:
        if skip:
            skip = False
            continue
        name = arg.split("=", 1)[0]
        if name in DROP:
            continue
        if name in DROP_WITH_VALUE:
            skip = "=" not in arg
            continue
        out.append(arg)

    if chart:
        def move(value: str) -> str:
            return re.sub(r"(?:\./)?" + re.escape(README_CHART), chart, value)

        chart_ref, out = move(chart_ref), [move(a) for a in out]
    return chart_ref, out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["production"])
    ap.add_argument("--readme", default=str(REPO / "README.md"))
    ap.add_argument("--chart", default=None)
    ns = ap.parse_args()
    chart_ref, flags = production_install(pathlib.Path(ns.readme), ns.chart)
    print("\n".join([chart_ref, *flags]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
