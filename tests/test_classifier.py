"""Tests for the session-state classifier (`agents_gateway.harness.classifier`)."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from agents_gateway.harness.classifier import (
    ClassifierResult,
    HarnessState,
    classify_state,
)


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _iso(seconds_ago: int) -> str:
    return (datetime.now(timezone.utc) - timedelta(seconds=seconds_ago)).isoformat()


# ---------------------------------------------------------------------------
# Process-alive edge cases
# ---------------------------------------------------------------------------


class TestProcessDead:
    def test_dead_process_with_completion_marker_is_completed_claimed(self):
        r = classify_state(output="Done.\n",
                           last_output_at=_now_iso(),
                           process_alive=False)
        assert r.state == HarnessState.completed_claimed

    def test_dead_process_with_failure_marker_is_failed_claimed(self):
        r = classify_state(output="FATAL ERROR: boom\n",
                           last_output_at=_now_iso(),
                           process_alive=False)
        assert r.state == HarnessState.failed_claimed

    def test_dead_process_no_marker_is_failed_claimed(self):
        r = classify_state(output="some output\n",
                           last_output_at=_now_iso(),
                           process_alive=False)
        assert r.state == HarnessState.failed_claimed


# ---------------------------------------------------------------------------
# Live process — running state (no decisive marker)
# ---------------------------------------------------------------------------


class TestRunningState:
    def test_empty_output_is_running(self):
        r = classify_state(output="", process_alive=True)
        assert r.state == HarnessState.running

    def test_unrelated_text_is_running(self):
        r = classify_state(output="thinking through the design\n", process_alive=True)
        assert r.state == HarnessState.running

    def test_contains_no_decisive_marker_keeps_running(self):
        r = classify_state(
            output="Reading file src/foo.py\nConsidering refactor",
            last_output_at=_now_iso(),
            process_alive=True,
        )
        assert r.state == HarnessState.running


# ---------------------------------------------------------------------------
# Waiting-for-reply detection
# ---------------------------------------------------------------------------


class TestWaitingForReply:
    @pytest.mark.parametrize("text", [
        "I need clarification on whether to proceed with X or Y?",
        "should I continue with the refactor?",
        "please provide more context before I continue.",
        "could you clarify which approach to take?",
        "what should I do next?",
        "awaiting confirmation from project lead.",
    ])
    def test_recognises_waiting_markers(self, text):
        r = classify_state(output=text, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state == HarnessState.waiting_for_reply
        assert r.excerpt  # non-empty excerpt

    def test_waiting_excerpt_is_short(self):
        # Long lines still produce a bounded excerpt
        long_line = ("blah " * 200) + "I need clarification: should I redo the schema?"
        r = classify_state(output=long_line, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state == HarnessState.waiting_for_reply
        assert len(r.excerpt) <= 400

    def test_waiting_marker_in_stale_output_not_matched(self):
        # The classifier restricts matching to the last ~1500 chars so
        # an OLD waiting marker far back should NOT classify as waiting.
        old_marker = ("foo " * 500) + "I need clarification: meaning\n"
        # Pad with enough running text to push the marker outside the tail.
        recent = ("working\n" * 300)
        r = classify_state(output=old_marker + recent,
                          last_output_at=_now_iso(), process_alive=True)
        assert r.state == HarnessState.running


# ---------------------------------------------------------------------------
# Completion + failure markers in live output
# ---------------------------------------------------------------------------


class TestCompletionClaim:
    @pytest.mark.parametrize("text", [
        "Done.\n",
        "All tests passed.\n",
        "Verification passed.\n",
        "Task complete.\n",
    ])
    def test_completion_markers(self, text):
        r = classify_state(output=text, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state == HarnessState.completed_claimed
        # Completion never marks task completed directly — only claimed.
        assert r.state != "completed"


class TestFailureClaim:
    @pytest.mark.parametrize("text", [
        "FATAL ERROR: cannot proceed\n",
        "Traceback (most recent call last):\n  File ...",
        "panic: nil pointer dereference\n",
        "agent crashed\n",
    ])
    def test_failure_markers_beat_completion(self, text):
        # Mix a failure marker with a completion marker; failure wins.
        mixed = "Done.\n" + text
        r = classify_state(output=mixed, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state == HarnessState.failed_claimed


# ---------------------------------------------------------------------------
# Stall detection
# ---------------------------------------------------------------------------


class TestStalled:
    def test_silence_more_than_stall_seconds_marks_stalled(self):
        last = _iso(seconds_ago=1200)
        r = classify_state(output="some non-decisive text",
                          last_output_at=last, stall_seconds=900,
                          process_alive=True)
        assert r.state == HarnessState.stalled
        assert "1200" in r.evidence or "silent" in r.evidence.lower()

    def test_silence_below_threshold_not_stalled(self):
        last = _iso(seconds_ago=120)
        r = classify_state(output="thinking\n",
                          last_output_at=last, stall_seconds=900,
                          process_alive=True)
        assert r.state == HarnessState.running

    def test_invalid_timestamp_not_crash(self):
        r = classify_state(output="x", last_output_at="not-a-timestamp",
                          stall_seconds=900, process_alive=True)
        # Should not crash; default to running since stall detection fails
        assert r.state in (HarnessState.running, HarnessState.stalled)

    def test_no_last_output_at_no_stall(self):
        # If we never recorded output we cannot say it stalled
        r = classify_state(output="x", last_output_at=None,
                          stall_seconds=900, process_alive=True)
        assert r.state == HarnessState.running


# ---------------------------------------------------------------------------
# To-dict
# ---------------------------------------------------------------------------


class TestResultToDict:
    def test_to_dict_has_state_key(self):
        r = ClassifierResult(state=HarnessState.running, evidence="x")
        assert r.to_dict() == {"state": "running", "evidence": "x"}
