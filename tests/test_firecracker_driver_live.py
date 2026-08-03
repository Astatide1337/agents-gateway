"""Firecracker microVM driver — real tests against a real booted guest.

Requires /dev/kvm access (see conductor's Firecracker feasibility PoC —
this environment needs the current user in the `kvm` group, verified
working via `sg kvm -c "..."`) plus a prebuilt kernel+rootfs at the paths
below (produced once via the Firecracker quickstart CI artifacts). Skips
cleanly if either is missing.
"""

from __future__ import annotations

import os
import shutil

import pytest

from agents_gateway.harness.firecracker import FirecrackerConsoleDriver, FirecrackerDriverError

KERNEL_PATH = os.environ.get("FIRECRACKER_TEST_KERNEL", "/tmp/firecracker-poc/assets/vmlinux.bin")
ROOTFS_PATH = os.environ.get("FIRECRACKER_TEST_ROOTFS", "/tmp/firecracker-poc/assets/rootfs.ext4")


def _kvm_accessible() -> bool:
    try:
        fd = os.open("/dev/kvm", os.O_RDWR)
        os.close(fd)
        return True
    except Exception:
        return False


def _assets_present() -> bool:
    return (
        shutil.which("firecracker") is not None
        and os.path.exists(KERNEL_PATH)
        and os.path.exists(ROOTFS_PATH)
    )


pytestmark = pytest.mark.skipif(
    not (_kvm_accessible() and _assets_present()),
    reason="Firecracker assets/KVM access not present (run via: sg kvm -c 'uv run pytest ...')",
)


@pytest.fixture
def driver():
    return FirecrackerConsoleDriver(kernel_path=KERNEL_PATH, rootfs_path=ROOTFS_PATH,
                                    boot_timeout_seconds=20, command_timeout_seconds=20)


class TestSessionLifecycle:
    def test_create_session_boots_a_real_vm_and_starts_tmux(self, driver):
        ref = driver.create_session("fc-test-1", "/root", ["bash"])
        try:
            assert ref.session == "fc-test-1"
            assert driver.is_alive(ref) is True
        finally:
            driver.terminate(ref)

    def test_terminate_actually_kills_the_process(self, driver):
        ref = driver.create_session("fc-test-2", "/root", ["bash"])
        session = driver._sessions.get("fc-test-2")
        assert session is not None
        pid = session.proc.pid

        driver.terminate(ref)

        assert driver.is_alive(ref) is False
        # Real proof, not just internal bookkeeping: the OS process
        # itself must actually be gone.
        import signal
        with pytest.raises(ProcessLookupError):
            os.kill(pid, 0)

    def test_terminate_cleans_up_rootfs_copy(self, driver):
        ref = driver.create_session("fc-test-3", "/root", ["bash"])
        session = driver._sessions.get("fc-test-3")
        rootfs_copy = session.rootfs_copy_path
        assert os.path.exists(rootfs_copy)

        driver.terminate(ref)

        assert not os.path.exists(rootfs_copy), "terminate must remove the per-session rootfs copy"

    def test_terminate_cleans_up_config_and_socket_files(self, driver):
        """Found via manual inspection after a real test run: rootfs
        copies were being cleaned up but the per-session config JSON and
        API socket files were silently left behind in /tmp."""
        ref = driver.create_session("fc-test-3b", "/root", ["bash"])
        session = driver._sessions.get("fc-test-3b")
        config_path = session.config_path
        socket_path = session.socket_path
        assert os.path.exists(config_path)

        driver.terminate(ref)

        assert not os.path.exists(config_path)
        assert not os.path.exists(socket_path)


class TestCommandRoundTrip:
    def test_send_text_and_capture_real_output(self, driver):
        """The actual proof this matters: text sent from the host really
        executes INSIDE the booted guest's tmux pane, not just accepted
        and discarded — capture() must show real command output that
        could only have come from inside the VM."""
        ref = driver.create_session("fc-test-4", "/root", ["bash"])
        try:
            driver.send_text(ref, "echo FIRECRACKER_ROUNDTRIP_$(hostname)")
            driver.send_enter(ref)
            import time
            time.sleep(1)
            output = driver.capture(ref)
            assert "FIRECRACKER_ROUNDTRIP_ubuntu-fc-uvm" in output, (
                f"expected real guest output (hostname included) in capture, got:\n{output}"
            )
        finally:
            driver.terminate(ref)

    def test_send_text_literal_delivers_line_by_line(self, driver):
        ref = driver.create_session("fc-test-5", "/root", ["bash"])
        try:
            driver.send_text_literal(ref, "echo LITERAL_LINE_ONE")
            driver.send_enter(ref)
            import time
            time.sleep(1)
            output = driver.capture(ref)
            assert "LITERAL_LINE_ONE" in output
        finally:
            driver.terminate(ref)


class TestAdversarial:
    def test_capture_on_unknown_session_raises_cleanly(self, driver):
        from agents_gateway.harness.tmux import TmuxSessionRef
        fake_ref = TmuxSessionRef(session="never-created", window="main", pane="0")
        with pytest.raises(FirecrackerDriverError):
            driver.capture(fake_ref)

    def test_is_alive_false_for_unknown_session(self, driver):
        from agents_gateway.harness.tmux import TmuxSessionRef
        fake_ref = TmuxSessionRef(session="never-created", window="main", pane="0")
        assert driver.is_alive(fake_ref) is False

    def test_double_terminate_does_not_raise(self, driver):
        ref = driver.create_session("fc-test-6", "/root", ["bash"])
        driver.terminate(ref)
        driver.terminate(ref)  # must be a safe no-op, not an error
