"""Tmux driver layer used by HarnessDriver to control sessions.

Two implementations:

  * ``TmuxDriver``       - real tmux via ``subprocess.run([...])``
  * ``FakeTmuxDriver``   - in-memory fake used by unit tests and by
                            the local E2E script (when the harness is
                            the bundled ``fake-test`` profile).

Both implement the same 6 methods so the harness driver can depend on
either one without changing behaviour. Command arrays are passed
verbatim to subprocess; we never shell-interpolate untrusted strings.
"""

from __future__ import annotations

import shlex
import subprocess
import time
from dataclasses import dataclass, field
from typing import Any


@dataclass
class TmuxSessionRef:
    """Reference to a tmux session/window/pane tuple."""

    session: str
    window: str = "main"
    pane: str = "0"


class TmuxDriver:
    """Real tmux driver. Wraps `tmux` CLI calls.

    All invocations use command arrays (no shell interpolation). The
    driver never persists state — it only constructs CLI sessions
    backed by the host tmux daemon. Tests use FakeTmuxDriver instead.
    """

    def __init__(self, tmux_bin: str = "tmux") -> None:
        self.tmux_bin = tmux_bin

    # -- lifecycle ------------------------------------------------

    def create_session(self, session_name: str, cwd: str,
                       command: list[str]) -> TmuxSessionRef:
        """Create a detached session running `command`.

        We use ``tmux new-session -d`` with ``-c <cwd>``. The command
        is supplied as a single argv; tmux will spawn it inside the
        new window. Separators between argv elements become spaces in
        the shell command tmux runs, so the caller must pre-quote.
        """
        if not command:
            raise ValueError("tmux create_session requires a non-empty command")
        cmd_str = " ".join(shlex.quote(c) for c in command)
        argv = [
            self.tmux_bin, "new-session", "-d", "-s", session_name,
            "-c", cwd, "-n", "main", cmd_str,
        ]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        if proc.returncode != 0:
            raise RuntimeError(
                f"tmux create_session failed (rc={proc.returncode}): "
                f"{proc.stderr.strip() or proc.stdout.strip()}"
            )
        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def send_text(self, ref: TmuxSessionRef, text: str) -> None:
        """Send text into the pane without pressing Enter.

        We use plain ``send-keys`` (no ``-l``) because full-screen TUI
        harnesses (opencode, claude-code) use raw terminal mode and
        don't process literal bytes the same way a shell prompt does.
        Plain send-keys translates spaces and printable characters into
        key events which the TUI picks up correctly.

        For multi-line text, we send each line separately (without
        pressing Enter) so the receiving application can gather the
        complete text before the caller invokes ``send_enter``.

        Special characters that tmux interprets as key names (e.g.
        ``Enter``, ``Escape``, ``Space``) are sent via the ``-l`` flag
        to preserve their literal meaning.
        """
        target = self._target(ref)
        # Split on newlines and send each line separately. We use the
        # ``--`` separator so leading dashes (e.g. markdown list items
        # like "- " or argument flags) are not interpreted by tmux as
        # send-keys flags. Plain send-keys (no ``-l``) is preserved
        # because full-screen TUI harnesses (pi, opencode, claude-code)
        # use raw terminal mode and need key events, not literal text.
        for line in text.split("\n"):
            if line:
                argv = [self.tmux_bin, "send-keys", "-t", target, "--", line]
                proc = subprocess.run(
                    argv, capture_output=True, text=True, timeout=10)
                if proc.returncode != 0:
                    raise RuntimeError(
                        f"tmux send_text failed: {proc.stderr.strip()}")
            # Send a literal newline via -l to avoid tmux interpreting "Enter"
            # as a key name.
            argv = [self.tmux_bin, "send-keys", "-t", target, "-l", "\n"]
            proc = subprocess.run(
                argv, capture_output=True, text=True, timeout=10)
            if proc.returncode != 0:
                raise RuntimeError(
                    f"tmux send_text failed: {proc.stderr.strip()}")

    def send_enter(self, ref: TmuxSessionRef) -> None:
        target = self._target(ref)
        argv = [self.tmux_bin, "send-keys", "-t", target, "Enter"]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        if proc.returncode != 0:
            raise RuntimeError(f"tmux send_enter failed: {proc.stderr.strip()}")

    def capture(self, ref: TmuxSessionRef, lines: int = 2000) -> str:
        target = self._target(ref)
        argv = [
            self.tmux_bin, "capture-pane", "-t", target, "-p",
            "-S", str(-max(1, lines)), "-E", "-",
        ]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        if proc.returncode != 0:
            return ""
        return proc.stdout

    def is_alive(self, ref: TmuxSessionRef) -> bool:
        argv = [self.tmux_bin, "has-session", "-t", ref.session]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=5)
        return proc.returncode == 0

    def terminate(self, ref: TmuxSessionRef) -> None:
        argv = [self.tmux_bin, "kill-session", "-t", ref.session]
        subprocess.run(argv, capture_output=True, text=True, timeout=10)

    def _target(self, ref: TmuxSessionRef) -> str:
        return f"{ref.session}:{ref.window}.{ref.pane}"


# ---------------------------------------------------------------------------
# FakeTmuxDriver — used by tests and the local E2E script
# ---------------------------------------------------------------------------


@dataclass
class _FakePane:
    output_lines: list[str] = field(default_factory=list)
    closed: bool = False
    started_at: float = field(default_factory=time.time)


class FakeTmuxDriver:
    """In-memory fake used by unit tests + the bundled fake-test harness.

    Behaviour:

      * ``create_session`` records the spawn command (so a test can
        assert on it) and marks the session alive. No real process is
        started — the test is expected to provide a stub "harness
        callback" via ``register_session_handler`` that drives output
        on demand.
      * ``send_text``/``send_enter`` append to the pane's input log
        and invoke the registered handler if any.
      * ``capture`` returns whatever the handler pushed into the
        pane's output buffer plus any text the test injected directly.
      * ``is_alive`` returns False after ``terminate`` or after the
        handler signals session end via ``mark_closed``.
    """

    def __init__(self) -> None:
        self._panes: dict[str, _FakePane] = {}
        self._spawn_commands: dict[str, list[str]] = {}
        self._inputs: dict[str, list[str]] = {}
        self._handlers: dict[str, Any] = {}
        self._closed: set[str] = set()

    # -- handlers ---------------------------------------------------

    def register_session_handler(self, session: str, handler: Any) -> None:
        """Register a callable invoked on each send_text/send_enter.

        Signature: ``handler(driver, session, text, is_enter) -> None``
        The handler can call ``push_output`` to populate the pane and
        ``mark_closed`` to end the session.
        """
        self._handlers[session] = handler

    def push_output(self, session: str, text: str) -> None:
        pane = self._panes.setdefault(session, _FakePane())
        # Treat each line of `text` as a captured line so substring
        # matching in the classifier works.
        for line in text.splitlines() or [""]:
            pane.output_lines.append(line)

    def mark_closed(self, session: str) -> None:
        self._closed.add(session)
        if session in self._panes:
            self._panes[session].closed = True

    @property
    def spawn_commands(self) -> dict[str, list[str]]:
        return dict(self._spawn_commands)

    @property
    def inputs(self) -> dict[str, list[str]]:
        return dict(self._inputs)

    # -- TmuxDriver protocol ---------------------------------------

    def create_session(self, session_name: str, cwd: str,
                       command: list[str]) -> TmuxSessionRef:
        self._spawn_commands[session_name] = list(command)
        self._panes[session_name] = _FakePane()
        self._inputs[session_name] = []
        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def send_text(self, ref: TmuxSessionRef, text: str) -> None:
        self._inputs.setdefault(ref.session, []).append(text)
        handler = self._handlers.get(ref.session)
        if handler is not None:
            handler(self, ref.session, text, is_enter=False)

    def send_enter(self, ref: TmuxSessionRef) -> None:
        self._inputs.setdefault(ref.session, []).append("<Enter>")
        handler = self._handlers.get(ref.session)
        if handler is not None:
            handler(self, ref.session, "<Enter>", is_enter=True)

    def capture(self, ref: TmuxSessionRef, lines: int = 2000) -> str:
        pane = self._panes.get(ref.session)
        if pane is None:
            return ""
        captured = pane.output_lines[-lines:]
        return "\n".join(captured) + ("\n" if captured else "")

    def is_alive(self, ref: TmuxSessionRef) -> bool:
        return ref.session in self._panes and ref.session not in self._closed

    def terminate(self, ref: TmuxSessionRef) -> None:
        self._closed.add(ref.session)


__all__ = ["FakeTmuxDriver", "TmuxDriver", "TmuxSessionRef"]
