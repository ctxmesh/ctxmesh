"""Unit tests for the batch-agent (M7 job execution model).

Tests cover:
  A. Success path — BATCH_TASK env var → exit 0 + structured JSON on stdout.
  B. Success path — CLI argument → exit 0 + structured JSON on stdout.
  C. Success path — BATCH_TASK_FILE → exit 0 + structured JSON on stdout.
  D. Failure: BATCH_FAIL=1 → exit 2.
  E. Failure: no input → exit 1.
  F. Result shape — JSON fields match the e2e assertion pattern.

These tests run the agent as a subprocess so they validate the full
run-once contract (exit code + stdout) without needing the launcher.

Run with:
  python3 examples/batch-agent/test_batch_agent.py
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest

# Path to the agent script — always resolved relative to this test file so the
# test is runnable from any working directory.
_AGENT = os.path.join(os.path.dirname(__file__), "agent.py")
_PYTHON = sys.executable


def _run(env_extra: dict | None = None, args: list[str] | None = None) -> subprocess.CompletedProcess:
    """Run the agent as a subprocess and capture output."""
    env = {k: v for k, v in os.environ.items()}
    # Strip any BATCH_* vars that might leak from the test environment.
    for key in ("BATCH_TASK", "BATCH_TASK_FILE", "BATCH_FAIL"):
        env.pop(key, None)
    if env_extra:
        env.update(env_extra)

    cmd = [_PYTHON, _AGENT] + (args or [])
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        env=env,
    )


class TestSuccessViaBatchTask(unittest.TestCase):
    """Scenario A: BATCH_TASK env var → exit 0, structured JSON output."""

    def test_exit_code_zero(self):
        result = _run(env_extra={"BATCH_TASK": "hello world"})
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_stdout_is_valid_json(self):
        result = _run(env_extra={"BATCH_TASK": "hello world"})
        line = result.stdout.strip()
        data = json.loads(line)
        self.assertIsInstance(data, dict)

    def test_output_fields(self):
        result = _run(env_extra={"BATCH_TASK": "hello world foo"})
        data = json.loads(result.stdout.strip())
        self.assertEqual(data["agent"], "batch-agent")
        self.assertEqual(data["status"], "complete")
        # "hello world foo" is 3 words
        self.assertEqual(data["result"], "MOCK_OK 3 words")
        self.assertEqual(data["input_length"], len("hello world foo"))

    def test_stderr_empty_on_success(self):
        result = _run(env_extra={"BATCH_TASK": "hello"})
        self.assertEqual(result.stderr, "")


class TestSuccessViaCLIArg(unittest.TestCase):
    """Scenario B: CLI argument → exit 0, structured JSON output."""

    def test_cli_arg_exit_zero(self):
        result = _run(args=["process this input"])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_cli_arg_result(self):
        result = _run(args=["one two three four"])
        data = json.loads(result.stdout.strip())
        self.assertEqual(data["result"], "MOCK_OK 4 words")

    def test_env_takes_priority_over_cli(self):
        """BATCH_TASK env overrides CLI arg."""
        result = _run(
            env_extra={"BATCH_TASK": "env wins"},
            args=["cli loses"],
        )
        data = json.loads(result.stdout.strip())
        # "env wins" = 2 words
        self.assertEqual(data["result"], "MOCK_OK 2 words")


class TestSuccessViaBatchTaskFile(unittest.TestCase):
    """Scenario C: BATCH_TASK_FILE → exit 0, structured JSON output."""

    def test_file_input(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".txt", delete=False) as fh:
            fh.write("from a file five words")
            tmp_path = fh.name
        try:
            result = _run(env_extra={"BATCH_TASK_FILE": tmp_path})
            self.assertEqual(result.returncode, 0, result.stderr)
            data = json.loads(result.stdout.strip())
            self.assertEqual(data["result"], "MOCK_OK 5 words")
        finally:
            os.unlink(tmp_path)

    def test_missing_file_exits_one(self):
        result = _run(env_extra={"BATCH_TASK_FILE": "/nonexistent/path/task.txt"})
        self.assertEqual(result.returncode, 1)
        self.assertIn("batch-agent:", result.stderr)


class TestFailureInjection(unittest.TestCase):
    """Scenario D: BATCH_FAIL=1 → exit 2."""

    def test_batch_fail_exit_two(self):
        result = _run(env_extra={"BATCH_FAIL": "1", "BATCH_TASK": "irrelevant"})
        self.assertEqual(result.returncode, 2)

    def test_batch_fail_writes_stderr(self):
        result = _run(env_extra={"BATCH_FAIL": "1"})
        self.assertIn("BATCH_FAIL=1", result.stderr)

    def test_batch_fail_no_stdout(self):
        result = _run(env_extra={"BATCH_FAIL": "1"})
        self.assertEqual(result.stdout, "")


class TestNoInput(unittest.TestCase):
    """Scenario E: no input source → exit 1."""

    def test_no_input_exits_one(self):
        result = _run()
        self.assertEqual(result.returncode, 1)

    def test_no_input_writes_usage_to_stderr(self):
        result = _run()
        self.assertIn("batch-agent:", result.stderr)
        self.assertIn("no input", result.stderr)

    def test_no_input_no_stdout(self):
        result = _run()
        self.assertEqual(result.stdout, "")


class TestOutputShape(unittest.TestCase):
    """Scenario F: JSON output shape satisfies the e2e grep pattern."""

    def test_result_contains_mock_ok(self):
        result = _run(env_extra={"BATCH_TASK": "any task"})
        data = json.loads(result.stdout.strip())
        self.assertTrue(data["result"].startswith("MOCK_OK "))

    def test_single_json_line(self):
        """Output is exactly one line of JSON — no extra newlines or preamble."""
        result = _run(env_extra={"BATCH_TASK": "single line check"})
        lines = [l for l in result.stdout.splitlines() if l.strip()]
        self.assertEqual(len(lines), 1)
        json.loads(lines[0])  # must parse cleanly

    def test_agent_name_in_output(self):
        result = _run(env_extra={"BATCH_TASK": "name check"})
        data = json.loads(result.stdout.strip())
        self.assertEqual(data["agent"], "batch-agent")

    def test_empty_task_word_count_zero(self):
        """An all-whitespace BATCH_TASK resolves to zero words."""
        # Whitespace strips to empty → no input; exit 1
        result = _run(env_extra={"BATCH_TASK": "   "})
        # A blank task resolves to None (stripped to "")
        self.assertEqual(result.returncode, 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)
