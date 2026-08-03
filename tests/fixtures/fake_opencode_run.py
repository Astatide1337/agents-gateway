#!/usr/bin/env python3
"""Fake `opencode run --format json` binary for OpencodeJsonDriver tests.

Mimics the real NDJSON shape observed live: step_start, tool_use (with
a real exit code under part.state.metadata.exit), text, step_finish.
Scripted by keywords in the message so tests can exercise completion,
failure, and waiting-for-reply paths deterministically and offline.
"""
import json
import sys
import time

args = sys.argv[1:]
assert args[0] == "run", args
message = args[1]
resuming = "--continue" in args

session_id = "ses_fake0001"


def emit(obj):
    print(json.dumps(obj))
    sys.stdout.flush()


emit({"type": "step_start", "timestamp": int(time.time() * 1000),
      "sessionID": session_id, "part": {"type": "step-start"}})

if "trigger_waiting" in message:
    emit({"type": "text", "sessionID": session_id,
          "part": {"type": "text",
                   "text": "Should I use float or int division for divide()? Please confirm."}})
    emit({"type": "step_finish", "sessionID": session_id,
          "part": {"type": "step-finish", "reason": "stop"}})
    sys.exit(0)

if "trigger_fail" in message:
    emit({"type": "text", "sessionID": session_id,
          "part": {"type": "text", "text": "Fatal error: could not apply patch."}})
    emit({"type": "step_finish", "sessionID": session_id,
          "part": {"type": "step-finish", "reason": "stop"}})
    sys.exit(1)

if "trigger_hang_ratelimit" in message:
    # stays alive, silent on stdout
    time.sleep(2)
    sys.exit(0)

if "trigger_ratelimit" in message:
    # Shape matches real live-observed opencode error events (see
    # process_json.py's _render): {"type":"error","error":{"data":
    # {"message":...}}}. Real OpenRouter 429 phrasing, not the
    # subscription-CLI "usage limit reached" wording.
    emit({"type": "error", "sessionID": session_id,
          "error": {"name": "UnknownError",
                    "data": {"message": "Rate limit exceeded. Please try again later.",
                              "ref": "err_fake_ratelimit"}}})
    sys.exit(1)

prefix = "Resumed. " if resuming else ""
emit({"type": "tool_use", "sessionID": session_id,
      "part": {"type": "tool", "tool": "bash",
               "state": {"status": "completed",
                        "title": "uvx pytest -q -k divide",
                        "output": ".\n1 passed in 0.01s\n",
                        "metadata": {"exit": 0}}}})
emit({"type": "text", "sessionID": session_id,
      "part": {"type": "text",
               "text": f"{prefix}divide(a,b) implemented and verified: 1 passed (required)."}})
emit({"type": "step_finish", "sessionID": session_id,
      "part": {"type": "step-finish", "reason": "stop"}})
sys.exit(0)
