#!/usr/bin/env python3
"""Deterministic fake harness for the harness worktree runtime.

This script is what the `fake-test` harness profile spawns inside a
tmux session. It continuously reads lines from stdin (driven by the
harness runtime's `send_text` / `send_enter` calls) and performs
scripted behaviour so tests + the local E2E script can drive the full
lifecycle without needing a real LLM-backed harness.

Behaviour is controlled by "directives" embedded in the goal text:

  - When the goal contains the substring `AGENT_ASK_QUESTION:true`,
    the harness prints a clarifying question and waits for a reply
    before continuing.
  - When the goal contains `AGENT_FAIL_ONCE:true`, the harness writes a
    broken file first, then on the FIRST verification pass the tests
    fail (because the file is broken); the runtime feeds the failure
    back into the session, the harness sees the feedback, and on the
    SECOND iteration it fixes the file so verification passes.
  - When the goal contains `AGENT_SCRATCH_FILE:<filename>`, the harness
    writes that filename into the worktree at completion (default:
    `AGENT_RESULT.txt`).
  - When the goal contains `AGENT_FAIL_CLAIM:true`, the harness prints
    a fatal-error marker and exits so the classifier can detect a
    failed_claimed state.

Default behaviour (no special directive): the harness writes
`AGENT_RESULT.txt`, prints `Done.`, and loops until terminate so the
classifier sees the completion marker.

It is robust to being run from any CWD inside the worktree.
"""

from __future__ import annotations

import os
import re
import sys
import time
from pathlib import Path

# We must be QUIET about credentials. The harness never sees the
# gateway env directly (the verifier strips them), but we go further
# and refuse to log anything that looks like a token.
_SENSITIVE_RE = re.compile(
    r"(token|secret|Authorization|Bearer)[^ \n]{8,}", re.I,
)


def _redact(text: str) -> str:
    return _SENSITIVE_RE.sub("<redacted>", text or "")


def _read_directive(line: str, name: str, default: str = "") -> str:
    """Extract a `NAME:value` directive from a line of goal text."""
    m = re.search(rf"{name}\s*:\s*([^\s]+)", line)
    return m.group(1) if m else default


def _write_scratch(cwd: Path, filename: str, body: str) -> Path:
    target = cwd / filename
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body)
    return target


def _ask_question(question: str) -> None:
    print(f"QUESTION: {question}", flush=True)
    print("I need clarification before I can continue.", flush=True)


def _fail_claim(reason: str) -> None:
    print(f"FATAL ERROR: {reason}", flush=True)
    sys.exit(1)


def main() -> None:
    cwd = Path(os.environ.get("AGENT_WORKDIR", os.getcwd())).resolve()
    cwd.mkdir(parents=True, exist_ok=True)
    scratch = "AGENT_RESULT.txt"
    ask_question = False
    fail_once = False
    fail_claim = False
    already_failed = False
    iterations = 0
    saw_goal = False

    # Print a banner so the classifier can see we're alive.
    print("Fake harness starting.", flush=True)
    print("Waiting for goal.", flush=True)

    while True:
        try:
            line = sys.stdin.readline()
        except (KeyboardInterrupt, EOFError):
            print("DONE.", flush=True)
            return
        if not line:
            # stdin closed (e.g. the tmux session was killed)
            time.sleep(0.1)
            continue
        line = line.strip()
        if not line:
            continue
        # Echo the line so the classifier captures what was sent.
        # Redact anything token-like first.
        echoed = _redact(line)
        print(f"> {echoed}", flush=True)

        # Look for directives only inside goal lines (start with "/goal"
        # or with a plain prompt header).
        is_goal_line = (line.startswith("/goal") or "ASSISTANT REPLY" in line
                        or "Plain prompt" in line or saw_goal is False)
        if "ASSISTANT REPLY" in line:
            # Composer reply — we treat any reply as "continue".
            saw_goal = True
            print("Understood. Continuing.", flush=True)
            # If we were asking a question, the answer to that is the
            # NEXT line (or has already come in the reply). For the
            # fake harness we just consume it and move on.
            ask_question = False
            # If the agent previously failed once, fix the file now.
            if fail_once and not already_failed:
                # We already failed once, this is a verification retry
                # signal — write the fixed file.
                _write_scratch(cwd, scratch,
                                "Fake-harness completed file (fixed).\n")
                print("DONE.", flush=True)
                continue
            continue
        if is_goal_line:
            saw_goal = True
            ask_question = _read_directive(line, "AGENT_ASK_QUESTION") == "true"
            fail_once = _read_directive(line, "AGENT_FAIL_ONCE") == "true"
            fail_claim = _read_directive(line, "AGENT_FAIL_CLAIM") == "true"
            scratch = _read_directive(line, "AGENT_SCRATCH_FILE", scratch) or scratch
            iterations += 1
            # Walk through the scenario
            if fail_claim:
                _fail_claim("AGENT_FAIL_CLAIM requested")
            if ask_question:
                _ask_question(
                    "Should I create the scratch file with the default content?"
                )
                continue
            if fail_once and not already_failed:
                # Write a broken file (a python file with a syntax error)
                _write_scratch(cwd, "broken.py", "def broken(:\n")
                _write_scratch(cwd, scratch,
                                "Fake-harness ran but is broken.\n")
                print("DONE.", flush=True)
                already_failed = True
                continue
            # Default success path
            _write_scratch(cwd, scratch,
                            "Fake-harness completed successfully.\n")
            print("DONE.", flush=True)
            continue

        # Otherwise we're reading additional free-text input.
        # The fake harness ignores non-directive lines after a goal.
        if "VERIFICATION FEEDBACK" in line:
            # The runtime fed back a verification failure. Fix the file
            if fail_once and already_failed:
                _write_scratch(cwd, "broken.py", "def broken():\n    return 0\n")
                _write_scratch(cwd, scratch,
                                "Fake-harness completed file (fixed).\n")
                print("Fixed based on verification feedback.", flush=True)
                print("DONE.", flush=True)
                continue
        # If no special directive, ignore.
        time.sleep(0.1)


if __name__ == "__main__":
    main()
