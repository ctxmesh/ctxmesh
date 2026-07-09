"""batch-agent — run-once example for the `job` execution model (M7).

Execution contract
------------------
This agent is NOT an HTTP server. It runs to completion and exits:
  exit 0  — success; result line written to stdout.
  exit 1  — failure (BATCH_FAIL=1 set, or missing required input).

Input (in order of priority)
-----------------------------
1. BATCH_TASK environment variable  — the task description string.
2. BATCH_TASK_FILE environment variable — path to a file whose contents
   are read as the task (useful when the task is large or contains
   shell-special characters).
3. First command-line argument (sys.argv[1]).

If no input is found the agent exits 1 with an error on stderr.

Output
------
A single JSON line on stdout:
  {"agent":"batch-agent","result":"MOCK_OK <word-count> words","status":"complete","input_length":<n>}

Failure simulation
------------------
Set BATCH_FAIL=1 to force a non-zero exit (exit 2) — used by the harness
to verify the launcher propagates the child's exit code correctly.

Why stdlib-only?
----------------
Batch/CronJob runs should be reproducible with no network dependency on
the LLM gateway — the mock transform (word count) is deterministic and
can be asserted from pod logs in the m7.8 e2e without a live model.

How the launcher front-runs this
---------------------------------
For executionModel: job the launcher (PID 1) spawns this script as its
child, waits for it to exit, then exits with the same code. The pod
exits when the agent completes, which is the K8s Job/CronJob contract.
"""

from __future__ import annotations

import json
import os
import sys


# ---------------------------------------------------------------------------
# Input resolution
# ---------------------------------------------------------------------------

def _resolve_input() -> str | None:
    """Return the task string or None if no input source is configured."""
    # 1. Env var: BATCH_TASK
    task = os.environ.get("BATCH_TASK", "").strip()
    if task:
        return task

    # 2. Env var: BATCH_TASK_FILE
    task_file = os.environ.get("BATCH_TASK_FILE", "").strip()
    if task_file:
        try:
            with open(task_file) as fh:
                content = fh.read().strip()
            if content:
                return content
        except OSError as exc:
            print(f"batch-agent: cannot read BATCH_TASK_FILE {task_file!r}: {exc}", file=sys.stderr)
            return None

    # 3. CLI arg
    if len(sys.argv) > 1:
        return sys.argv[1].strip()

    return None


# ---------------------------------------------------------------------------
# Deterministic "unit of work" — mock transform, no LLM call
# ---------------------------------------------------------------------------

def _process(task: str) -> dict:
    """Run a deterministic word-count transform on *task*.

    Returns a result dict with all fields the e2e will assert.
    The result string is "MOCK_OK <n> words" so the harness can grep for it.
    """
    words = task.split()
    word_count = len(words)
    result_str = f"MOCK_OK {word_count} words"
    return {
        "agent": "batch-agent",
        "result": result_str,
        "status": "complete",
        "input_length": len(task),
    }


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------

def main() -> None:
    # Failure injection: BATCH_FAIL=1 → exit 2 immediately (for harness tests).
    if os.environ.get("BATCH_FAIL", "").strip() == "1":
        print("batch-agent: BATCH_FAIL=1: simulated failure", file=sys.stderr)
        sys.exit(2)

    task = _resolve_input()
    if task is None:
        print(
            "batch-agent: no input: set BATCH_TASK, BATCH_TASK_FILE, or pass a CLI arg",
            file=sys.stderr,
        )
        sys.exit(1)

    output = _process(task)

    # Single structured JSON line — e2e asserts this from pod logs.
    print(json.dumps(output, separators=(",", ":")), flush=True)
    sys.exit(0)


if __name__ == "__main__":
    main()
