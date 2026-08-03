"""Process-based JSON-mode driver for `opencode run --format json`.

Avoids opencode's default TUI (alt-screen, so tmux capture-pane only
ever sees the current frame — a completed task's own output can scroll
out of view before the next poll). This driver runs one-shot
`opencode run --format json` processes instead; events accumulate in
an append-only NDJSON log and each tool call carries a real exit code.

Implements the same 6-method interface as TmuxDriver (see tmux.py) so
HarnessDriver can use it interchangeably. `send_text` + `send_enter` on
a session with no live process spawns `opencode run <message>
--format json ...`; once an opencode sessionID has been observed, a
later send_text/send_enter resumes it via `--session <id> --continue`.
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

from agents_gateway.harness.tmux import TmuxSessionRef

# opencode's internal log — retried 429s land here (level=ERROR), never in --format json stdout.
DEFAULT_OPENCODE_INTERNAL_LOG = str(
    Path.home() / ".local" / "share" / "opencode" / "log" / "opencode.log")


class OpencodeJsonDriverError(Exception):
    pass


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False
    except OSError:
        return False


@dataclass
class _JsonSessionState:
    cwd: str
    extra_args: list[str]
    log_path: str
    pending_text: str = ""
    proc: subprocess.Popen | None = None
    # Set on every spawn; used to reap via proc.poll(). Also the basis
    # for a reattached session's raw-PID liveness check when proc is
    # None (see reattach()) — a Popen handle cannot be reconstructed
    # for a process this instance didn't spawn.
    pid: int | None = None
    opencode_session_id: str = ""
    last_message_sent: str = ""
    exit_code: int | None = None
    internal_log_offset: int = 0


class OpencodeJsonDriver:
    """Spawns `opencode run --format json` as a one-shot process per turn."""

    def __init__(self, binary: str = "opencode",
                 log_dir: str = "/tmp/agents-gateway/opencode-json",
                 internal_log_path: str = DEFAULT_OPENCODE_INTERNAL_LOG) -> None:
        self.binary = binary
        self.log_dir = log_dir
        self.internal_log_path = internal_log_path
        os.makedirs(self.log_dir, exist_ok=True)
        self._sessions: dict[str, _JsonSessionState] = {}

    # -- lifecycle ------------------------------------------------

    def create_session(self, session_name: str, cwd: str,
                       command: list[str]) -> TmuxSessionRef:
        """Register session state; does not spawn — the message is a
        positional CLI arg, known only via a later send_enter call."""
        if not command:
            raise ValueError("create_session requires a non-empty command")
        extra_args = list(command[1:])
        log_path = os.path.join(self.log_dir, f"{session_name}.ndjson")
        open(log_path, "w").close()
        self._sessions[session_name] = _JsonSessionState(
            cwd=cwd, extra_args=extra_args, log_path=log_path,
        )
        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def send_text(self, ref: TmuxSessionRef, text: str) -> None:
        state = self._get(ref)
        state.pending_text += text if not state.pending_text else ("\n" + text)

    def send_text_literal(self, ref: TmuxSessionRef, text: str) -> None:
        self.send_text(ref, text)

    def send_enter(self, ref: TmuxSessionRef) -> None:
        state = self._get(ref)
        message = state.pending_text
        state.pending_text = ""
        if not message.strip():
            return
        self._spawn(ref.session, state, message)

    def capture(self, ref: TmuxSessionRef, lines: int = 2000) -> str:
        state = self._get(ref)
        self._reap(state)
        events = self._read_events(state.log_path)
        if not state.opencode_session_id:
            for e in events:
                sid = e.get("sessionID") or (e.get("part") or {}).get("sessionID")
                if sid:
                    state.opencode_session_id = sid
                    break
        text = self._render(events)
        internal_errors = self._tail_internal_log_errors(state)
        if internal_errors:
            text = f"{text}\n{internal_errors}" if text else internal_errors
        if state.last_message_sent:
            text = f"[sent] {state.last_message_sent}\n\n{text}"
        return text

    def _tail_internal_log_errors(self, state: _JsonSessionState) -> str:
        """ERROR-level lines from opencode's internal log since spawn."""
        try:
            with open(self.internal_log_path, "r") as f:
                f.seek(state.internal_log_offset)
                new_bytes = f.read()
        except OSError:
            return ""
        lines = [ln for ln in new_bytes.splitlines() if "level=ERROR" in ln]
        return "\n".join(lines[-50:])

    def is_alive(self, ref: TmuxSessionRef) -> bool:
        state = self._get(ref)
        self._reap(state)
        if state.proc is not None:
            return state.proc.poll() is None
        if state.pid is not None:
            return _pid_alive(state.pid)
        return False

    def terminate(self, ref: TmuxSessionRef) -> None:
        state = self._sessions.get(ref.session)
        if state is None:
            return
        if state.proc is not None and state.proc.poll() is None:
            try:
                state.proc.terminate()
                state.proc.wait(timeout=5)
            except Exception:
                try:
                    state.proc.kill()
                except Exception:
                    pass
        elif state.proc is None and state.pid is not None and _pid_alive(state.pid):
            # Reattached session — no Popen handle to .terminate(),
            # signal the raw PID directly.
            try:
                os.kill(state.pid, signal.SIGTERM)
            except OSError:
                pass
        try:
            os.remove(state.log_path)
        except OSError:
            pass
        self._sessions.pop(ref.session, None)

    def get_pid(self, ref: TmuxSessionRef) -> int | None:
        """Not part of the shared driver interface — HarnessDriver
        persists this into session.metadata so a later process (e.g.
        after an AGW restart, which loses all in-memory _sessions
        state) can reattach via reattach() below."""
        state = self._get(ref)
        return state.pid

    def reattach(self, session_name: str, cwd: str, pid: int,
                extra_args: list[str] | None = None) -> TmuxSessionRef:
        """Reconstruct enough state to resume polling a process this
        driver instance never spawned itself — no Popen handle exists
        for it, so liveness is checked via the raw PID instead of
        proc.poll(). capture() still works fully (pure log-file read);
        only replying (a fresh spawn) needs no special handling since
        _spawn always creates a brand new process regardless."""
        log_path = os.path.join(self.log_dir, f"{session_name}.ndjson")
        state = _JsonSessionState(
            cwd=cwd, extra_args=extra_args or [], log_path=log_path, pid=pid,
        )
        self._sessions[session_name] = state
        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def exit_code(self, ref: TmuxSessionRef) -> int | None:
        """Not part of the shared driver interface — used by
        classify_json_transcript instead of alive/dead alone."""
        state = self._get(ref)
        self._reap(state)
        return state.exit_code

    # -- internals ----------------------------------------------------

    def _get(self, ref: TmuxSessionRef) -> _JsonSessionState:
        state = self._sessions.get(ref.session)
        if state is None:
            raise OpencodeJsonDriverError(f"unknown session {ref.session!r}")
        return state

    def _reap(self, state: _JsonSessionState) -> None:
        if state.exit_code is not None:
            return
        if state.proc is not None:
            rc = state.proc.poll()
            if rc is not None:
                state.exit_code = rc
        # A reattached session (proc is None) whose PID has exited:
        # the real exit code isn't recoverable without the Popen
        # handle. Leave exit_code as None (classify_json_transcript
        # treats None the same as 0 — falls through to marker-based
        # classification rather than assuming success).

    def _spawn(self, session_name: str, state: _JsonSessionState, message: str) -> None:
        if state.proc is not None and state.proc.poll() is None:
            raise OpencodeJsonDriverError(
                f"session {session_name!r} already has a running process")
        if state.proc is None and state.pid is not None and _pid_alive(state.pid):
            raise OpencodeJsonDriverError(
                f"session {session_name!r} already has a running (reattached) process")

        # --dir is required, not just cwd=: opencode resolves its own
        # tool-call working directory independently of the process cwd
        # when that directory is a git worktree.
        argv = [self.binary, "run", message, "--format", "json",
                "--dir", state.cwd] + state.extra_args
        if state.opencode_session_id:
            argv = argv + ["--session", state.opencode_session_id, "--continue"]

        state.last_message_sent = message
        state.exit_code = None
        try:
            state.internal_log_offset = os.path.getsize(self.internal_log_path)
        except OSError:
            state.internal_log_offset = 0
        logf = open(state.log_path, "ab")
        try:
            state.proc = subprocess.Popen(
                argv, cwd=state.cwd, stdout=logf, stderr=subprocess.STDOUT,
            )
            state.pid = state.proc.pid
        finally:
            logf.close()

    def _read_events(self, log_path: str) -> list[dict]:
        events: list[dict] = []
        try:
            with open(log_path, "r") as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        events.append(json.loads(line))
                    except (TypeError, ValueError):
                        continue
        except FileNotFoundError:
            pass
        return events

    def _render(self, events: list[dict]) -> str:
        lines: list[str] = []
        for e in events:
            etype = e.get("type")
            part = e.get("part") or {}
            if etype == "text":
                t = part.get("text", "")
                if t:
                    lines.append(t)
            elif etype == "error":
                err = e.get("error") or {}
                data = err.get("data") or {}
                msg = data.get("message", "") or err.get("name", "")
                if msg:
                    lines.append(f"[error] {msg}")
            elif etype == "tool_use":
                tool = part.get("tool", "")
                st = part.get("state", {}) or {}
                out = st.get("output", "") or ""
                title = st.get("title", "")
                exit_code = (st.get("metadata") or {}).get("exit")
                if exit_code is not None:
                    lines.append(f"[tool:{tool}] {title} -> exit={exit_code}")
                if out:
                    lines.append(out[-2000:])
            elif etype == "step_finish":
                reason = part.get("reason", "")
                if reason:
                    lines.append(f"[step_finish reason={reason}]")
        return "\n".join(lines)


_default_driver: OpencodeJsonDriver | None = None


def get_default_json_driver() -> OpencodeJsonDriver:
    """Process-wide singleton. Unlike TmuxDriver (stateless — real
    session state lives in the external tmux daemon), this driver
    holds live Popen handles in Python-process memory, so every caller
    that constructs HarnessDriver without an explicit json_driver must
    share this instance or lose track of already-running sessions."""
    global _default_driver
    if _default_driver is None:
        _default_driver = OpencodeJsonDriver()
    return _default_driver


__all__ = ["OpencodeJsonDriver", "OpencodeJsonDriverError", "get_default_json_driver"]
