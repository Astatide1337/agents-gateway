"""Tmux driver layer used by HarnessDriver to control sessions.

Three implementations:

  * ``TmuxDriver``          - real tmux via ``subprocess.run([...])``
                              on the host.
  * ``ContainerTmuxDriver`` - real tmux, but running inside a hardened,
                              long-lived Docker container instead of
                              the bare host — closes the "harness
                              sessions today run on host via tmux
                              (long-term containerization is roadmap)"
                              gap noted in README.md's Known
                              Limitations. See its docstring below.
  * ``FakeTmuxDriver``      - in-memory fake used by unit tests and by
                              the local E2E script (when the harness is
                              the bundled ``fake-test`` profile).

All three implement the same 6 methods so the harness driver can
depend on any of them without changing behaviour. Command arrays are
passed verbatim to subprocess/docker; we never shell-interpolate
untrusted strings.
"""

from __future__ import annotations

import os
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
        """Send (possibly multi-line) text into the pane as one paste,
        without pressing Enter.

        Live-found: the previous implementation sent each line via a
        separate ``send-keys`` call followed by a literal ``\\n`` key
        press, meaning a goal of even modest length (a couple dozen
        lines) became dozens of separate subprocess calls trickled in
        over real wall-clock time. Chat-style TUI input boxes (opencode
        included) commonly treat a bare Enter/newline as "submit" —
        only bracketed paste is trusted to carry literal newlines. That
        race intermittently caused the harness to submit an empty or
        partial message and land back on its blank welcome screen,
        with the rest of the goal typed into nowhere: reproduced twice
        across live E2E runs (once on an implementation task, once on
        integration), non-deterministically.

        ``tmux load-buffer`` + ``paste-buffer`` sends the whole text as
        a single bracketed-paste sequence in one shot instead — TUI
        frameworks that support bracketed paste (virtually all modern
        ones) then correctly treat every embedded newline as literal
        content, never as a submit keypress.
        """
        if not text:
            return
        target = self._target(ref)
        load = subprocess.run(
            [self.tmux_bin, "load-buffer", "-"],
            input=text, capture_output=True, text=True, timeout=10,
        )
        if load.returncode != 0:
            raise RuntimeError(
                f"tmux load-buffer failed: {load.stderr.strip()}")
        paste = subprocess.run(
            [self.tmux_bin, "paste-buffer", "-t", target],
            capture_output=True, text=True, timeout=10,
        )
        if paste.returncode != 0:
            raise RuntimeError(
                f"tmux paste-buffer failed: {paste.stderr.strip()}")

    def send_text_literal(self, ref: TmuxSessionRef, text: str) -> None:
        """Fallback send mechanism: one send-keys call per line plus a
        literal newline key, instead of a single bracketed paste.

        Used as the retry driver.py's start_session falls back to when
        the primary paste-buffer mechanism's injection doesn't
        register (see start_session's docstring) — a structurally
        different delivery path in case the failure is specific to
        bracketed paste (e.g. a TUI/terminal combination that doesn't
        actually enable bracketed-paste mode) rather than a pure
        timing race, where simply repeating the same mechanism would
        predictably fail again.
        """
        target = self._target(ref)
        for line in text.split("\n"):
            if line:
                argv = [self.tmux_bin, "send-keys", "-t", target, "--", line]
                proc = subprocess.run(
                    argv, capture_output=True, text=True, timeout=10)
                if proc.returncode != 0:
                    raise RuntimeError(
                        f"tmux send_text_literal failed: {proc.stderr.strip()}")
            argv = [self.tmux_bin, "send-keys", "-t", target, "-l", "\n"]
            proc = subprocess.run(
                argv, capture_output=True, text=True, timeout=10)
            if proc.returncode != 0:
                raise RuntimeError(
                    f"tmux send_text_literal failed: {proc.stderr.strip()}")

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
        try:
            proc = subprocess.run(argv, capture_output=True, text=True, timeout=5)
        except subprocess.TimeoutExpired:
            # Live-found: a transient timeout here (host under load —
            # e.g. concurrent tmux/docker/pytest activity) used to
            # propagate uncaught through classify_state's polling loop
            # and permanently crash the whole task. `has-session` being
            # slow says nothing about whether the session itself died,
            # so assume still-alive (checked again next poll) rather
            # than treat a monitoring hiccup as worse than an unknown.
            return True
        return proc.returncode == 0

    def terminate(self, ref: TmuxSessionRef) -> None:
        argv = [self.tmux_bin, "kill-session", "-t", ref.session]
        subprocess.run(argv, capture_output=True, text=True, timeout=10)

    def _target(self, ref: TmuxSessionRef) -> str:
        return f"{ref.session}:{ref.window}.{ref.pane}"


# ---------------------------------------------------------------------------
# ContainerTmuxDriver — tmux inside a long-lived, hardened container
# ---------------------------------------------------------------------------


class ContainerTmuxDriver:
    """tmux driver that runs the session inside a Docker container
    instead of on the bare host.

    One container per harness session, kept alive for the session's
    duration (unlike ``DockerRuntime`` in ``agents_gateway/runtime.py``,
    which runs one short-lived, ``--rm``'d container per task — a
    harness session is long-horizon and needs a persistent process to
    attach tmux commands to). Lifecycle:

      1. ``create_session`` starts the container detached, running
         ``sleep infinity`` as PID 1 so it stays alive, then
         ``docker exec``'s a ``tmux new-session`` inside it.
      2. ``send_text``/``send_enter``/``capture``/``is_alive`` are the
         same tmux commands as ``TmuxDriver``, each wrapped in
         ``docker exec <container>`` instead of running bare.
      3. ``terminate`` kills the tmux session then removes the
         container (``docker rm -f``).

    Sandbox flags mirror ``DockerRuntime._sandbox_flags()`` in
    ``agents_gateway/runtime.py`` (cap-drop, no-new-privileges,
    non-root user, memory/cpus/pids ceilings) with two deliberate
    differences a long-lived coding-agent session requires:

      * No ``--rm`` (the container must survive across many exec
        calls, not exit after one command) — cleanup is explicit in
        ``terminate()`` instead.
      * Network is **enabled by default** (``network: str | None`` —
        pass ``"none"`` to disable). Every harness profile calls out
        to an LLM provider (OpenRouter, or the harness CLI's own
        subscription backend for claude-code/codex) — unlike
        ``DockerRuntime``'s short trusted-script tasks, a harness
        session cannot function with no network at all. This is not a
        regression versus the host-tmux backend: TmuxDriver already
        runs with full host network access today, so a container with
        *any* network policy is equal-or-stricter isolation, not
        looser.
      * Root filesystem is left writable (not ``--read-only``): coding
        harness CLIs write their own config/session state outside the
        worktree (e.g. ``~/.claude``, ``~/.pi``) which a read-only
        root would break. The worktree bind mount is the only path
        that needs to survive container removal; everything else is
        disposable container-local state.

    Live-validated (see ``tests/test_container_tmux_driver_live.py``,
    run against a real local Docker daemon, not mocked) for the core
    container/tmux/bind-mount mechanics: session create/alive/
    terminate, send/capture round-trip, and — critically — that the
    worktree bind mount is actually readable/writable from inside the
    container. That last one caught a real bug during validation: on a
    Docker daemon with user-namespace remapping (confirmed present in
    this environment), container-"root" does NOT have access to a
    host-owned bind mount by default, and tmux's ``-c cwd`` silently
    falls back to $HOME instead of failing loudly. The fix is the
    ``--user`` flag above, defaulted to the current process's own
    uid:gid so it matches whatever host user actually owns the
    worktree — this is why the default differs from
    ``DockerRuntime``'s fixed ``65534:65534``.

    NOT YET validated against a real harness CLI (pi/opencode/claude/
    codex) inside a container — only against a plain shell (the live
    test suite) and the bundled ``fake-test`` harness's mechanics. The
    image supplied via ``docker_image`` must have ``tmux`` installed
    and, for a real (non-fake-test) profile, that harness's CLI on
    PATH and any subscription login state it needs already present
    (e.g. baked into the image or mounted in) — validate that
    end-to-end before trusting the "docker" backend with real harness
    profiles in production.
    """

    def __init__(self, *,
                 docker_image: str,
                 docker_bin: str = "docker",
                 memory: str = "2g",
                 cpus: str = "2.0",
                 pids_limit: int = 512,
                 network: str | None = None,
                 extra_env: dict[str, str] | None = None,
                 command_timeout_seconds: int = 15,
                 user: str | None = None) -> None:
        if not docker_image:
            raise ValueError("ContainerTmuxDriver requires docker_image")
        self.docker_image = docker_image
        self.docker_bin = docker_bin
        self.memory = memory
        self.cpus = cpus
        self.pids_limit = pids_limit
        self.network = network
        self.extra_env = extra_env or {}
        self.command_timeout_seconds = command_timeout_seconds
        # Bind-mounted worktree/workspace directories are owned by
        # whatever host UID runs this gateway process. On a Docker
        # daemon with user-namespace remapping (the modern default —
        # confirmed empirically: container-root mapped to a host UID
        # other than the mount owner gets EACCES on the bind mount
        # despite being "root" inside the container), the container
        # process MUST run as the same UID:GID as the mount owner or
        # it can't read/write the worktree at all. Default to the
        # current process's own uid:gid rather than a fixed non-root
        # id (unlike DockerRuntime's 65534:65534) because that fixed
        # id has no reason to match this host's actual mount owner.
        self.user = user or f"{os.getuid()}:{os.getgid()}"

    # -- lifecycle ------------------------------------------------

    def _sandbox_flags(self) -> list[str]:
        flags = [
            "-d",
            "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges",
            "--user", self.user,
            "--memory", self.memory,
            "--cpus", self.cpus,
            "--pids-limit", str(self.pids_limit),
        ]
        if self.network:
            flags += ["--network", self.network]
        for key, value in self.extra_env.items():
            flags += ["-e", f"{key}={value}"]
        return flags

    def create_session(self, session_name: str, cwd: str,
                       command: list[str]) -> TmuxSessionRef:
        if not command:
            raise ValueError("ContainerTmuxDriver create_session requires a non-empty command")

        run_argv = (
            [self.docker_bin, "run", "--name", session_name]
            + self._sandbox_flags()
            + ["-v", f"{cwd}:{cwd}", "-w", cwd, self.docker_image,
               "sleep", "infinity"]
        )
        proc = subprocess.run(run_argv, capture_output=True, text=True, timeout=30)
        if proc.returncode != 0:
            raise RuntimeError(
                f"container create_session failed to start container "
                f"(rc={proc.returncode}): {proc.stderr.strip() or proc.stdout.strip()}"
            )

        cmd_str = " ".join(shlex.quote(c) for c in command)
        tmux_argv = self._exec(session_name, [
            "tmux", "new-session", "-d", "-s", session_name,
            "-c", cwd, "-n", "main", cmd_str,
        ])
        proc = subprocess.run(tmux_argv, capture_output=True, text=True, timeout=10)
        if proc.returncode != 0:
            # Best-effort cleanup of the container we just started.
            subprocess.run([self.docker_bin, "rm", "-f", session_name],
                           capture_output=True, text=True, timeout=15)
            raise RuntimeError(
                f"container create_session failed to start tmux "
                f"(rc={proc.returncode}): {proc.stderr.strip() or proc.stdout.strip()}"
            )
        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def send_text(self, ref: TmuxSessionRef, text: str) -> None:
        """Same paste-buffer approach as TmuxDriver.send_text (see its
        docstring) — one bracketed paste instead of many per-line
        send-keys + literal-newline-key calls. ``docker exec -i`` is
        required here (unlike the host TmuxDriver's plain
        ``subprocess.run``) so the load-buffer step's stdin actually
        reaches the containerized tmux process."""
        if not text:
            return
        target = self._target(ref)
        load = subprocess.run(
            [self.docker_bin, "exec", "-i", ref.session, "tmux", "load-buffer", "-"],
            input=text, capture_output=True, text=True,
            timeout=self.command_timeout_seconds,
        )
        if load.returncode != 0:
            raise RuntimeError(f"container load-buffer failed: {load.stderr.strip()}")
        paste_argv = self._exec(ref.session, ["tmux", "paste-buffer", "-t", target])
        paste = subprocess.run(paste_argv, capture_output=True, text=True,
                               timeout=self.command_timeout_seconds)
        if paste.returncode != 0:
            raise RuntimeError(f"container paste-buffer failed: {paste.stderr.strip()}")

    def send_text_literal(self, ref: TmuxSessionRef, text: str) -> None:
        """See TmuxDriver.send_text_literal's docstring — fallback
        delivery mechanism used on retry."""
        target = self._target(ref)
        for line in text.split("\n"):
            if line:
                argv = self._exec(ref.session, ["tmux", "send-keys", "-t", target, "--", line])
                proc = subprocess.run(argv, capture_output=True, text=True,
                                      timeout=self.command_timeout_seconds)
                if proc.returncode != 0:
                    raise RuntimeError(f"container send_text_literal failed: {proc.stderr.strip()}")
            argv = self._exec(ref.session, ["tmux", "send-keys", "-t", target, "-l", "\n"])
            proc = subprocess.run(argv, capture_output=True, text=True,
                                  timeout=self.command_timeout_seconds)
            if proc.returncode != 0:
                raise RuntimeError(f"container send_text_literal failed: {proc.stderr.strip()}")

    def send_enter(self, ref: TmuxSessionRef) -> None:
        target = self._target(ref)
        argv = self._exec(ref.session, ["tmux", "send-keys", "-t", target, "Enter"])
        proc = subprocess.run(argv, capture_output=True, text=True,
                              timeout=self.command_timeout_seconds)
        if proc.returncode != 0:
            raise RuntimeError(f"container send_enter failed: {proc.stderr.strip()}")

    def capture(self, ref: TmuxSessionRef, lines: int = 2000) -> str:
        target = self._target(ref)
        argv = self._exec(ref.session, [
            "tmux", "capture-pane", "-t", target, "-p",
            "-S", str(-max(1, lines)), "-E", "-",
        ])
        proc = subprocess.run(argv, capture_output=True, text=True,
                              timeout=self.command_timeout_seconds)
        if proc.returncode != 0:
            return ""
        return proc.stdout

    def is_alive(self, ref: TmuxSessionRef) -> bool:
        # Container gone entirely -> definitely not alive. Checked
        # first since `docker exec` into a missing container also
        # returns nonzero but with a less specific error.
        try:
            inspect = subprocess.run(
                [self.docker_bin, "inspect", "-f", "{{.State.Running}}", ref.session],
                capture_output=True, text=True, timeout=10,
            )
        except subprocess.TimeoutExpired:
            # See TmuxDriver.is_alive's comment — a transient timeout
            # here means "unknown", not "dead"; assume alive so a
            # monitoring hiccup never crashes the whole task outright.
            return True
        if inspect.returncode != 0 or inspect.stdout.strip() != "true":
            return False
        argv = self._exec(ref.session, ["tmux", "has-session", "-t", ref.session])
        try:
            proc = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        except subprocess.TimeoutExpired:
            return True
        return proc.returncode == 0

    def terminate(self, ref: TmuxSessionRef) -> None:
        argv = self._exec(ref.session, ["tmux", "kill-session", "-t", ref.session])
        subprocess.run(argv, capture_output=True, text=True, timeout=10)
        subprocess.run([self.docker_bin, "rm", "-f", ref.session],
                       capture_output=True, text=True, timeout=15)

    # -- helpers ----------------------------------------------------

    def _exec(self, container: str, argv: list[str]) -> list[str]:
        return [self.docker_bin, "exec", container] + argv

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


def build_tmux_driver(config: Any) -> "TmuxDriver | FakeTmuxDriver | ContainerTmuxDriver":
    """Select a tmux driver from a HarnessRuntimeConfig-shaped object.

    Single source of truth for backend selection so
    ``HarnessRuntime.__init__`` and ``server.py``'s shared
    session-endpoint driver never disagree. ``use_fake_tmux=True``
    always wins (tests / local E2E), regardless of ``backend``.
    """
    if getattr(config, "use_fake_tmux", False):
        return FakeTmuxDriver()
    backend = getattr(config, "backend", "host-tmux")
    if backend == "docker":
        return ContainerTmuxDriver(
            docker_image=getattr(config, "docker_image", ""),
            memory=getattr(config, "docker_memory", "2g"),
            cpus=getattr(config, "docker_cpus", "2.0"),
            pids_limit=getattr(config, "docker_pids_limit", 512),
            network=getattr(config, "docker_network", None),
        )
    return TmuxDriver()


__all__ = ["FakeTmuxDriver", "TmuxDriver", "ContainerTmuxDriver", "TmuxSessionRef",
           "build_tmux_driver"]
