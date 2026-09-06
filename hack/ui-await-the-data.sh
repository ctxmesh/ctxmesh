#!/usr/bin/env bash
# ui-await-the-data.sh — forbid "await the shell, then assert the data synchronously".
#
# Every page renders its data-testid on the OUTERMOST div, unconditionally. So
#
#     await screen.findByTestId("workflows-page");
#     expect(screen.getByText("my-pipeline")).toBeInTheDocument();
#
# resolves the await on the FIRST render — before any fetch has settled — and then reads
# the fetched rows synchronously. On a fast machine the mocked fetch lands in the same
# microtask batch and it passes; under load it does not. workflows-page failed two tier0
# runs this way while passing 20/20 in isolation, which is the signature of exactly this.
#
# The fix is always the same: await the DATA. findBy is getBy plus waitFor, so it can only
# make an assertion more tolerant.
set -euo pipefail
cd "$(dirname "$0")/.."

hits="$(
  python3 - <<'PY'
import re, glob
SHELL = re.compile(r'await screen\.findByTestId\("[a-z0-9-]+-page"\)')
SYNC  = re.compile(r'(?<!await )screen\.getBy')
out = []
for f in sorted(glob.glob('ui/src/**/*.test.tsx', recursive=True)):
    lines = open(f).read().splitlines()
    for i, l in enumerate(lines):
        if not SHELL.search(l):
            continue
        n = 0
        for j in range(i + 1, min(i + 7, len(lines))):
            s = lines[j].strip()
            if SYNC.search(lines[j]):
                n += 1
            elif s and not s.startswith('//'):
                break
        if n >= 2:
            out.append(f"{f}:{i+1} — {n} synchronous screen.getBy after awaiting the page shell")
print("\n".join(out))
PY
)"

if [ -n "$hits" ]; then
  echo "$hits" | sed 's/^/  /'
  echo
  echo "FAIL: a test awaits the page shell, then reads fetched data synchronously."
  echo "      The shell renders before the fetch resolves, so this is a race that only"
  echo "      shows under load. Await the data: turn the first assertion into findBy."
  exit 1
fi
echo "PASS: no test awaits the page shell and then asserts fetched data synchronously"
