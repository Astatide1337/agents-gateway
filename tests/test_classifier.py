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


class TestUsageLimited:
    def test_claude_code_marker_detected(self):
        r = classify_state(
            output="You've hit your usage limit. Limit resets at 5pm.\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="claude-code",
        )
        assert r.state == HarnessState.usage_limited

    def test_codex_marker_detected(self):
        r = classify_state(
            output="Rate limit exceeded, try again after your limit resets.\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="codex",
        )
        assert r.state == HarnessState.usage_limited

    def test_no_profile_uses_generic_markers(self):
        r = classify_state(
            output="usage limit reached\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="",
        )
        assert r.state == HarnessState.usage_limited

    def test_unrelated_profile_word_limit_alone_is_not_usage_limited(self):
        r = classify_state(
            output="Set the connection limit to 10.\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="pi-coding-agent",
        )
        assert r.state != HarnessState.usage_limited

    def test_usage_limit_preempts_failure_marker(self):
        r = classify_state(
            output="Rate limit exceeded — panic: aborting.\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="codex",
        )
        assert r.state == HarnessState.usage_limited

    def test_usage_limit_preempts_completion_marker(self):
        r = classify_state(
            output="verification passed but you've hit your usage limit\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="claude-code",
        )
        assert r.state == HarnessState.usage_limited

    def test_dead_process_with_usage_limit_marker(self):
        r = classify_state(
            output="You've hit your usage limit.\n",
            last_output_at=_now_iso(), process_alive=False,
            harness_profile="claude-code",
        )
        assert r.state == HarnessState.usage_limited

    def test_evidence_names_the_marker(self):
        r = classify_state(
            output="claude usage limit\n",
            last_output_at=_now_iso(), process_alive=True,
            harness_profile="claude-code",
        )
        assert "usage-limit marker" in r.evidence


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
        "The task is complete.\n",
        "uvx pytest -q -k multiply -> 1 passed (required)",
    ])
    def test_completion_markers(self, text):
        r = classify_state(output=text, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state == HarnessState.completed_claimed
        # Completion never marks task completed directly — only claimed.
        assert r.state != "completed"

    def test_claude_code_closing_phrasing_recognized(self):
        """Real incident: claude-code declared genuine, verified work
        done ("No further action needed — the task is done and
        verified.") but this phrasing matched none of the existing
        completion markers, so the session sat idle indefinitely
        instead of transitioning to verifying."""
        text = ("No further action needed — the task is done and verified.")
        r = classify_state(output=text, last_output_at=_now_iso(), process_alive=True)
        assert r.state == HarnessState.completed_claimed

    def test_claude_code_qa_crawl_pass_summary_recognized(self):
        """Real incident: a second real integration task declared
        completion via a genuine, verified QA crawl summary
        ("qa_crawl ... PASS — 11 views, 0 console errors, 0 page
        errors, 0 slop violations. ... Working tree is committed and
        clean.") that also matched none of the existing markers."""
        text = (
            "- qa_crawl (bash .agent-task/qa/qa_crawl.sh .): PASS — 11 views, "
            "0 console errors, 0 page errors, 0 slop violations\n"
            "Working tree is committed and clean."
        )
        r = classify_state(output=text, last_output_at=_now_iso(), process_alive=True)
        assert r.state == HarnessState.completed_claimed

    def test_marker_split_across_a_wrapped_terminal_line(self):
        """Real incident: a genuine completion phrase was missed
        because the terminal wrapped it across two lines
        ("Working tree\\n  is committed and clean.") — the literal
        substring never appeared in the raw captured text at all."""
        text = "Working tree\n  is committed and clean."
        r = classify_state(output=text, last_output_at=_now_iso(), process_alive=True)
        assert r.state == HarnessState.completed_claimed

    def test_opencode_style_verification_summary_not_missed(self):
        """opencode echoes Composer's (required)/(optional) tags rather
        than pytest's raw summary line."""
        text = (
            "Verification:\n\n"
            "- uvx pytest -q -k multiply -> 1 passed (required)\n"
            "- Lint: skipped (ruff not available)\n"
        )
        r = classify_state(output=text, last_output_at=_now_iso(), process_alive=True)
        assert r.state == HarnessState.completed_claimed


class TestSystemBoilerplateDoesNotSelfTrigger:
    """System-injected boilerplate must not self-classify as completion."""

    def test_plain_prompt_goal_text_not_completion_claimed(self):
        from agents_gateway.harness.goal import _plain_prompt
        text = _plain_prompt("Add a divide function with tests.")
        r = classify_state(output=text, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state != HarnessState.completed_claimed

    def test_verification_failure_feedback_not_completion_claimed(self):
        feedback = (
            "VERIFICATION FEEDBACK (from Agents Gateway):\n"
            "1 required verification command(s) failed:\n\n"
            "- divide unit tests: exit_code=5\n"
            "  command: uvx pytest -q -k divide\n\n"
            "Continue fixing until all required verification commands pass.\n"
            "Do not mark this task complete until they do."
        )
        r = classify_state(output=feedback, last_output_at=_now_iso(),
                          process_alive=True)
        assert r.state != HarnessState.completed_claimed


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
