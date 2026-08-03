"""Heuristic session-state classifier.

The classifier inspects recent tmux output and the session's
``last_output_at`` timestamp to make a best-effort guess about what
state the harness is in. The classifier is intentionally conservative:

  * It never marks a session ``completed`` from text alone — completion
    must be confirmed by passing verification. The closest classifier
    signal is ``completed_claimed`` which means the harness SAID it
    finished; the driver will then transition into ``verifying``.
  * It never marks a session ``failed`` from text alone — only a dead
    process or a hard error marker can classify as ``failed_claimed``.
  * When the classifier sees no output for ``stall_seconds`` and the
    harness hasn't claimed completion, it returns ``stalled`` (the
    supervisor then creates an ``ambiguous_harness_state`` Composer
    interaction so a human/Composer can decide; it does NOT auto-fail).

Heuristics can be augmented later; this is the first version.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Iterable


# ---------------------------------------------------------------------------
# Classifications + markers
# ---------------------------------------------------------------------------


class HarnessState:
    """String classification values."""

    running = "running"
    waiting_for_reply = "waiting_for_reply"
    completed_claimed = "completed_claimed"
    failed_claimed = "failed_claimed"
    stalled = "stalled"
    usage_limited = "usage_limited"
    unknown = "unknown"


# Lower-cased markers. The first hit wins (`failed` beats `completed`).
WAITING_MARKERS: tuple[str, ...] = (
    "i need clarification",
    "i need a clarification",
    "should i ",
    "should i continue",
    "should i proceed",
    "please provide",
    "please confirm",
    "could you clarify",
    "can you clarify",
    "what should i do",
    "asking for input",
    "would you like me to",
    "do you want me to",
    "waiting for input",
    "awaiting confirmation",
    "please specify",
)


COMPLETION_MARKERS: tuple[str, ...] = (
    # Markers are matched against whitespace-normalized text (see
    # _find_marker) — a real completion phrase was missed because the
    # terminal wrapped it across two lines. Space-delimited, not
    # newline-anchored, so wrapping can't split a marker apart.
    # The fake harness prints "DONE" for the local E2E flow.
    " done ",
    " done:",
    "done.",
    "completed.",
    "all tests passed",
    "verification passed",
    # Not "task complete" / "all required verification": both are
    # substrings of goal.py/verification.py's own injected boilerplate.
    "both tests pass",
    "tests pass (",
    # opencode echoes Composer's own (required)/(optional) tags rather
    # than pytest's raw summary line.
    "passed (required)",
    "tests passed",
    "passed in",
    "all tests pass",
    "task is complete",
    "i'm done",
    "finished.",
    "changes have been made",
    "implementation complete",
    # claude-code's own closing phrasing (observed live, verified via a
    # real integration task it correctly finished but never signaled
    # in a way the classifier recognized).
    "no further action needed",
    "no further action appears required",
    "task is done and verified",
    "working tree is committed and clean",
    "0 console errors, 0 page errors",
)


FAILURE_MARKERS: tuple[str, ...] = (
    "fatal error:",
    "critical failure",
    "traceback (most recent call last)",
    "panic:",
    "agent crashed",
)


# Usage-limit markers for subscription-tier harnesses (claude-code,
# codex — flat-rate CLI logins, not metered API keys; see profiles.py's
# billing_mode). Keyed by HarnessProfile.name. Specific multi-word
# phrases only, not generic words like "limit" — a false positive
# here reroutes a healthy session away from a working provider.
USAGE_LIMIT_MARKERS_BY_PROFILE: dict[str, tuple[str, ...]] = {
    "claude-code": (
        "usage limit reached",
        "you've hit your usage limit",
        "you have reached your usage limit",
        "claude usage limit",
        "limit resets at",
        "upgrade to continue using claude",
    ),
    "codex": (
        "usage limit reached",
        "you've hit your usage limit",
        "you have reached your usage limit",
        "rate limit exceeded",
        "try again after your limit resets",
    ),
}

# Fallback for any profile not listed above, and for metered profiles
# (opencode, pi-coding-agent) which hit standard API rate limits
# rather than subscription usage caps.
GENERIC_USAGE_LIMIT_MARKERS: tuple[str, ...] = (
    "usage limit reached",
    "quota exceeded",
    "rate limit exceeded",
    "rate limit reached",
    "too many requests",
)


def _lower(output: str) -> str:
    return output.lower()


def _find_marker(haystack_lower: str, markers: Iterable[str]) -> str:
    """Multi-word markers must survive terminal line-wrapping — a
    real completion phrase ("Working tree is committed and clean.")
    was missed because the pane wrapped it across two lines
    ("Working tree\\n  is committed and clean."), so the literal
    substring never appeared. Whitespace (including newlines) is
    collapsed to a single space before matching."""
    normalized = " ".join(haystack_lower.split())
    for m in markers:
        if m in normalized:
            return m
    return ""


@dataclass
class ClassifierResult:
    state: str
    excerpt: str = ""
    evidence: str = ""

    def to_dict(self) -> dict[str, str]:
        return {"state": self.state, "evidence": self.evidence}




def classify_state(output: str,
                   last_output_at: str | None = None,
                   now: str | None = None,
                   stall_seconds: int = 900,
                   process_alive: bool = True,
                   harness_profile: str = "") -> ClassifierResult:
    """Classify the current harness state from recent tmux output.

    harness_profile selects the usage-limit marker set for
    subscription-tier profiles; empty string uses the generic fallback.
    """
    usage_markers = USAGE_LIMIT_MARKERS_BY_PROFILE.get(
        harness_profile, GENERIC_USAGE_LIMIT_MARKERS,
    )

    if not process_alive:
        lower = _lower(output)
        usage_evidence = _find_marker(lower, usage_markers)
        if usage_evidence:
            return ClassifierResult(HarnessState.usage_limited,
                                    evidence=f"process exited + usage-limit marker: {usage_evidence!r}")
        if _find_marker(lower, COMPLETION_MARKERS):
            return ClassifierResult(HarnessState.completed_claimed,
                                    evidence="process exited + completion marker")
        if _find_marker(lower, FAILURE_MARKERS):
            return ClassifierResult(HarnessState.failed_claimed,
                                    evidence="process exited + failure marker")
        return ClassifierResult(HarnessState.failed_claimed,
                                evidence="process not alive")

    if not output.strip():
        return ClassifierResult(HarnessState.running,
                                evidence="no output yet")

    lower = _lower(output)
    # Restrict to the tail to avoid matching stale markers.
    tail_lower = lower[-1500:]

    # Priority: usage-limit > failure > completion > waiting > stall.
    usage_evidence = _find_marker(tail_lower, usage_markers)
    if usage_evidence:
        return ClassifierResult(HarnessState.usage_limited,
                                evidence=f"usage-limit marker: {usage_evidence!r}")

    fail_evidence = _find_marker(tail_lower, FAILURE_MARKERS)
    if fail_evidence:
        return ClassifierResult(HarnessState.failed_claimed,
                                evidence=f"failure marker: {fail_evidence!r}")

    complete_evidence = _find_marker(tail_lower, COMPLETION_MARKERS)
    if complete_evidence:
        return ClassifierResult(HarnessState.completed_claimed,
                                evidence=f"completion marker: {complete_evidence!r}")

    wait_evidence = _find_marker(tail_lower, WAITING_MARKERS)
    if wait_evidence:
        idx = tail_lower.find(wait_evidence)
        excerpt = output.strip()
        if idx >= 0:
            tail_start = max(0, len(output) - len(tail_lower))
            full_idx = tail_start + idx
            excerpt = output[full_idx:full_idx + 400]
        return ClassifierResult(HarnessState.waiting_for_reply,
                                excerpt=excerpt.strip(),
                                evidence=f"waiting marker: {wait_evidence!r}")

    if last_output_at:
        try:
            last_dt = datetime.fromisoformat(last_output_at.replace("Z", "+00:00"))
            now_dt = (datetime.fromisoformat(now.replace("Z", "+00:00"))
                      if now else datetime.now(timezone.utc))
            silence = (now_dt - last_dt).total_seconds()
            if silence > stall_seconds:
                return ClassifierResult(HarnessState.stalled,
                                        evidence=f"silent for {int(silence)}s")
        except Exception:
            pass

    return ClassifierResult(HarnessState.running,
                            evidence="no decisive marker")


def classify_json_transcript(text: str, process_alive: bool,
                             exit_code: int | None = None,
                             harness_profile: str = "") -> ClassifierResult:
    """Classify state for a one-shot JSON-mode session (opencode run
    --format json), as opposed to a persistent tmux/TUI session.

    Unlike classify_state, a clean exit (code 0) with no failure/
    waiting marker defaults to completed_claimed rather than
    failed_claimed — a real exit code is a much stronger signal than
    "the tmux pane is gone", and AGW's own VerificationRunner
    re-checks all required commands regardless of what's claimed here.

    Usage-limit markers are checked before the process_alive return: a
    retrying process may never exit on its own.
    """
    usage_markers = USAGE_LIMIT_MARKERS_BY_PROFILE.get(harness_profile, GENERIC_USAGE_LIMIT_MARKERS)
    lower = _lower(text)
    tail_lower = lower[-4000:]

    usage_evidence = _find_marker(tail_lower, usage_markers)
    if usage_evidence:
        return ClassifierResult(HarnessState.usage_limited,
                                evidence=f"usage-limit marker: {usage_evidence!r}")

    if process_alive:
        return ClassifierResult(HarnessState.running, evidence="process still running")

    fail_evidence = _find_marker(tail_lower, FAILURE_MARKERS)
    if exit_code not in (0, None):
        return ClassifierResult(
            HarnessState.failed_claimed,
            evidence=f"nonzero exit ({exit_code})"
                    + (f" + failure marker: {fail_evidence!r}" if fail_evidence else ""),
        )
    if fail_evidence:
        return ClassifierResult(HarnessState.failed_claimed,
                                evidence=f"failure marker: {fail_evidence!r}")

    wait_evidence = _find_marker(tail_lower, WAITING_MARKERS)
    if wait_evidence:
        idx = tail_lower.find(wait_evidence)
        excerpt = text.strip()
        if idx >= 0:
            tail_start = max(0, len(text) - len(tail_lower))
            full_idx = tail_start + idx
            excerpt = text[full_idx:full_idx + 400]
        return ClassifierResult(HarnessState.waiting_for_reply,
                                excerpt=excerpt.strip(),
                                evidence=f"waiting marker: {wait_evidence!r}")

    return ClassifierResult(HarnessState.completed_claimed,
                            evidence="clean process exit, no waiting/failure marker")


__all__ = [
    "ClassifierResult", "HarnessState", "classify_state",
    "classify_json_transcript",
    "USAGE_LIMIT_MARKERS_BY_PROFILE", "GENERIC_USAGE_LIMIT_MARKERS",
]
