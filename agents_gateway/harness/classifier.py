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
    # The fake harness prints "DONE" — keep that pattern as an explicit
    # marker so the local E2E flow can complete cleanly.
    "\ndone\n",
    "\ndone:",
    "done.\n",
    "completed.\n",
    "task complete",
    "all tests passed",
    "verification passed",
    "all required verification",
)


FAILURE_MARKERS: tuple[str, ...] = (
    "fatal error:",
    "critical failure",
    "traceback (most recent call last)",
    "panic:",
    "agent crashed",
)


def _lower(output: str) -> str:
    return output.lower()


@dataclass
class ClassifierResult:
    state: str
    excerpt: str = ""
    evidence: str = ""

    def to_dict(self) -> dict[str, str]:
        return {"state": self.state, "evidence": self.evidence}


def _find_marker(haystack_lower: str, markers: Iterable[str]) -> str:
    for m in markers:
        if m in haystack_lower:
            return m
    return ""


def classify_state(output: str,
                   last_output_at: str | None = None,
                   now: str | None = None,
                   stall_seconds: int = 900,
                   process_alive: bool = True) -> ClassifierResult:
    """Classify the current harness state from recent tmux output.

    Args:
      output:           recent tmux capture (last 2000 lines or so)
      last_output_at:  ISO timestamp of the last captured output
      now:              ISO timestamp of "now"; defaults to utcnow
      stall_seconds:    silence threshold for stalled classification
      process_alive:   whether the harness process is still alive

    Returns:
      ClassifierResult with one of HarnessState.* values.
    """
    if not process_alive:
        # A dead process can be claimed_failed (if markers present) or
        # completed_claimed (if it printed DONE before exiting) or
        # otherwise failed_claimed as the safest choice.
        lower = _lower(output)
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
    # Tail of the output governs current state — restrict to last ~1500
    # chars to avoid matching stale markers from earlier in the session.
    tail_lower = lower[-1500:]

    # Failure markers are highest priority: if the harness said "fatal
    # error" we should classify as failed_claimed regardless of other
    # signals.
    fail_evidence = _find_marker(tail_lower, FAILURE_MARKERS)
    if fail_evidence:
        return ClassifierResult(HarnessState.failed_claimed,
                                evidence=f"failure marker: {fail_evidence!r}")

    # A completion marker means the harness claims done — the driver
    # will transition to `verifying` rather than `completed` based on
    # the spec. We return `completed_claimed` here.
    complete_evidence = _find_marker(tail_lower, COMPLETION_MARKERS)
    if complete_evidence:
        return ClassifierResult(HarnessState.completed_claimed,
                                evidence=f"completion marker: {complete_evidence!r}")

    # Then waiting-for-reply.
    wait_evidence = _find_marker(tail_lower, WAITING_MARKERS)
    if wait_evidence:
        # Capture a short excerpt for the interaction prompt.
        idx = tail_lower.find(wait_evidence)
        excerpt = output.strip()
        if idx >= 0:
            # Map the tail index back into the full output (best effort).
            tail_start = max(0, len(output) - len(tail_lower))
            full_idx = tail_start + idx
            excerpt = output[full_idx:full_idx + 400]
        return ClassifierResult(HarnessState.waiting_for_reply,
                                excerpt=excerpt.strip(),
                                evidence=f"waiting marker: {wait_evidence!r}")

    # Stall detection: no output for stall_seconds. Convert ISO timestamps
    # to epoch seconds and compare; if parsing fails treat as running.
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


__all__ = ["ClassifierResult", "HarnessState", "classify_state"]
