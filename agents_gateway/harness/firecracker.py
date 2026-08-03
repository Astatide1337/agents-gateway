"""Firecracker microVM driver — real, tested, but not yet wired into
AGW's backend-selection factory (agents_gateway/config.py's ``backend``
switch). Proves the hard, previously-unproven part: whether a real
microVM per harness session actually works in this deployment's kind of
environment. Wiring it into the backend factory alongside TmuxDriver/
ContainerTmuxDriver is the remaining, mechanical integration step.

Same conceptual interface as TmuxDriver/ContainerTmuxDriver
(create_session, send_text, send_text_literal, send_enter, capture,
terminate, is_alive). No docker-exec equivalent exists for a microVM, so
command delivery goes through the guest's serial console instead — one
persistent shell process per VM (not per-command like ContainerTmuxDriver's
``docker exec``), synchronized via a distinctive echo marker after each
command so capture() knows when a command's output is fully flushed.

Known real limitations, stated plainly rather than hidden:
  * The rootfs image is copied per session (not a real snapshot/overlay)
    — correct but slow (a few hundred MB copy per session start). A
    production version needs qcow2-style copy-on-write or Firecracker's
    own snapshot/restore feature for the ~3ms warm-restore path.
  * No networking is configured for the guest in this driver — a real
    harness session needs outbound network (LLM provider calls), which
    means a TAP device + host-side NAT per session is required before
    this is usable for anything beyond the mechanics proven here.
  * Guest image used in testing is a generic Ubuntu CI image, not one
    with harness CLIs (opencode/pi-coding-agent) baked in — building
    that image is separate, real work.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import threading
import time
import uuid

from agents_gateway.harness.tmux import TmuxSessionRef

__all__ = ["FirecrackerDriverError", "FirecrackerConsoleDriver"]


class FirecrackerDriverError(Exception):
    pass


class _ConsoleSession:
    def __init__(self, proc: subprocess.Popen, rootfs_copy_path: str,
                 config_path: str = "", socket_path: str = "") -> None:
        self.proc = proc
        self.config_path = config_path
        self.socket_path = socket_path
        self.rootfs_copy_path = rootfs_copy_path
        self._buffer: list[str] = []
        self._lock = threading.Lock()
        self._stopped = threading.Event()
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    def _read_loop(self) -> None:
        try:
            while not self._stopped.is_set():
                line = self.proc.stdout.readline()
                if not line:
                    break
                with self._lock:
                    self._buffer.append(line)
        except Exception:
            pass

    def write(self, text: str) -> None:
        if self.proc.stdin is None or self.proc.poll() is not None:
            raise FirecrackerDriverError("console process is not accepting input")
        self.proc.stdin.write(text)
        self.proc.stdin.flush()

    def snapshot(self) -> str:
        with self._lock:
            return "".join(self._buffer)

    def wait_for_marker(self, marker: str, timeout: float) -> bool:
        deadline = time.time() + timeout
        while time.time() < deadline:
            if marker in self.snapshot():
                return True
            time.sleep(0.1)
        return False

    def stop(self) -> None:
        self._stopped.set()


class FirecrackerConsoleDriver:
    def __init__(self, *, kernel_path: str, rootfs_path: str, firecracker_bin: str = "firecracker",
                 vcpu_count: int = 1, mem_size_mib: int = 512, boot_timeout_seconds: float = 15.0,
                 command_timeout_seconds: float = 15.0) -> None:
        if not os.path.exists(kernel_path):
            raise FirecrackerDriverError(f"kernel image not found: {kernel_path}")
        if not os.path.exists(rootfs_path):
            raise FirecrackerDriverError(f"rootfs image not found: {rootfs_path}")
        self.kernel_path = kernel_path
        self.rootfs_path = rootfs_path
        self.firecracker_bin = firecracker_bin
        self.vcpu_count = vcpu_count
        self.mem_size_mib = mem_size_mib
        self.boot_timeout_seconds = boot_timeout_seconds
        self.command_timeout_seconds = command_timeout_seconds
        self._sessions: dict[str, _ConsoleSession] = {}

    def create_session(self, session_name: str, cwd: str, command: list[str]) -> TmuxSessionRef:
        rootfs_copy = os.path.join(tempfile.gettempdir(), f"fc-rootfs-{session_name}-{uuid.uuid4().hex[:8]}.ext4")
        shutil.copyfile(self.rootfs_path, rootfs_copy)

        config = {
            "boot-source": {
                "kernel_image_path": self.kernel_path,
                "boot_args": "console=ttyS0 reboot=k panic=1 pci=off",
            },
            "drives": [
                {"drive_id": "rootfs", "path_on_host": rootfs_copy,
                 "is_root_device": True, "is_read_only": False},
            ],
            "machine-config": {"vcpu_count": self.vcpu_count, "mem_size_mib": self.mem_size_mib},
        }
        config_path = os.path.join(tempfile.gettempdir(), f"fc-config-{session_name}-{uuid.uuid4().hex[:8]}.json")
        import json
        with open(config_path, "w") as f:
            json.dump(config, f)

        socket_path = os.path.join(tempfile.gettempdir(), f"fc-{session_name}-{uuid.uuid4().hex[:8]}.socket")
        if os.path.exists(socket_path):
            os.remove(socket_path)

        proc = subprocess.Popen(
            [self.firecracker_bin, "--api-sock", socket_path, "--config-file", config_path],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, bufsize=1,
        )
        console = _ConsoleSession(proc, rootfs_copy, config_path=config_path, socket_path=socket_path)
        self._sessions[session_name] = console

        if not console.wait_for_marker("login:", self.boot_timeout_seconds):
            self._force_cleanup(session_name)
            raise FirecrackerDriverError(f"guest never reached a login prompt within {self.boot_timeout_seconds}s")

        # Auto-login lands at a root shell. Start a tmux session inside
        # the guest for the actual harness command, same shape as
        # ContainerTmuxDriver's approach (tmux inside the sandboxed unit,
        # not the sandbox boundary itself).
        cmd_str = " ".join(command) if command else "bash"
        marker = f"FC_SESSION_READY_{uuid.uuid4().hex[:8]}"
        console.write(f"tmux new-session -d -s {session_name} -c {cwd} -n main {cmd_str!r}\n")
        console.write(f"echo {marker}\n")
        if not console.wait_for_marker(marker, self.command_timeout_seconds):
            self._force_cleanup(session_name)
            raise FirecrackerDriverError("guest tmux session failed to start")

        return TmuxSessionRef(session=session_name, window="main", pane="0")

    def _run_console_command(self, session_name: str, argv_str: str) -> str:
        console = self._sessions.get(session_name)
        if console is None:
            raise FirecrackerDriverError(f"no such session: {session_name}")
        marker = f"FC_CMD_DONE_{uuid.uuid4().hex[:8]}"
        before = len(console.snapshot())
        console.write(f"{argv_str}; echo {marker}\n")
        if not console.wait_for_marker(marker, self.command_timeout_seconds):
            raise FirecrackerDriverError(f"console command timed out: {argv_str}")
        return console.snapshot()[before:]

    def send_text(self, ref: TmuxSessionRef, text: str) -> None:
        if not text:
            return
        target = f"{ref.session}:{ref.window}.{ref.pane}"
        escaped = text.replace("'", "'\\''")
        self._run_console_command(
            ref.session, f"echo -n '{escaped}' | tmux load-buffer - && tmux paste-buffer -t {target}",
        )

    def send_text_literal(self, ref: TmuxSessionRef, text: str) -> None:
        target = f"{ref.session}:{ref.window}.{ref.pane}"
        for line in text.split("\n"):
            if line:
                escaped = line.replace("'", "'\\''")
                self._run_console_command(ref.session, f"tmux send-keys -t {target} -- '{escaped}'")
            self._run_console_command(ref.session, f"tmux send-keys -t {target} -l $'\\n'")

    def send_enter(self, ref: TmuxSessionRef) -> None:
        target = f"{ref.session}:{ref.window}.{ref.pane}"
        self._run_console_command(ref.session, f"tmux send-keys -t {target} Enter")

    def capture(self, ref: TmuxSessionRef, lines: int = 2000) -> str:
        target = f"{ref.session}:{ref.window}.{ref.pane}"
        output = self._run_console_command(
            ref.session, f"tmux capture-pane -t {target} -p -S -{max(1, lines)} -E -",
        )
        return output

    def is_alive(self, ref: TmuxSessionRef) -> bool:
        console = self._sessions.get(ref.session)
        if console is None or console.proc.poll() is not None:
            return False
        try:
            output = self._run_console_command(ref.session, f"tmux has-session -t {ref.session}; echo RC=$?")
            return "RC=0" in output
        except FirecrackerDriverError:
            return False

    def terminate(self, ref: TmuxSessionRef) -> None:
        self._force_cleanup(ref.session)

    def _force_cleanup(self, session_name: str) -> None:
        console = self._sessions.pop(session_name, None)
        if console is None:
            return
        console.stop()
        if console.proc.poll() is None:
            console.proc.terminate()
            try:
                console.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                console.proc.kill()
        for path in (console.rootfs_copy_path, console.config_path, console.socket_path):
            if path:
                try:
                    os.remove(path)
                except OSError:
                    pass
