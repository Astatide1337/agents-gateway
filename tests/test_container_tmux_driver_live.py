"""Live integration test for ContainerTmuxDriver against a real Docker
daemon — not mocked. Skips cleanly (never fakes success) when Docker
isn't available, matching this repo's existing real-E2E ethos (see
scripts/e2e-harness-runtime-real.sh).

Builds a minimal local image (alpine + tmux + bash) once per test
session rather than depending on a real coding-harness CLI being
installed — this validates the driver's container/tmux/bind-mount
mechanics, which is backend-agnostic; it does NOT validate a real
harness CLI (pi/opencode/claude/codex) running inside a container,
which still needs live validation separately before the "docker"
backend is trusted with real harness profiles in production.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import time

import pytest

from agents_gateway.harness.tmux import ContainerTmuxDriver

TEST_IMAGE = "agw-test-tmux:latest"


def _docker_available() -> bool:
    if not shutil.which("docker"):
        return False
    try:
        proc = subprocess.run(["docker", "info"], capture_output=True,
                              text=True, timeout=5)
        return proc.returncode == 0
    except Exception:
        return False


def _ensure_test_image() -> bool:
    """Build TEST_IMAGE if missing. Returns False (skip) if the build
    itself fails — never fakes success by falling back to a mock."""
    check = subprocess.run(["docker", "image", "inspect", TEST_IMAGE],
                           capture_output=True, text=True, timeout=10)
    if check.returncode == 0:
        return True
    with tempfile.TemporaryDirectory() as build_dir:
        with open(os.path.join(build_dir, "Dockerfile"), "w") as f:
            f.write("FROM alpine:latest\nRUN apk add --no-cache tmux bash\n")
        build = subprocess.run(
            ["docker", "build", "-t", TEST_IMAGE, build_dir],
            capture_output=True, text=True, timeout=120,
        )
        return build.returncode == 0


pytestmark = pytest.mark.skipif(
    not _docker_available(),
    reason="Docker daemon not available in this environment",
)


@pytest.fixture(scope="module", autouse=True)
def _test_image():
    if not _ensure_test_image():
        pytest.skip("could not build the tmux-capable test image")


@pytest.fixture
def workdir(tmp_path):
    return str(tmp_path)


@pytest.fixture
def driver():
    return ContainerTmuxDriver(docker_image=TEST_IMAGE, network="none")


class TestContainerTmuxDriverLive:
    def test_session_lifecycle(self, driver, workdir):
        ref = driver.create_session("agw-test-lifecycle", workdir, ["bash"])
        try:
            assert driver.is_alive(ref) is True
        finally:
            driver.terminate(ref)
        assert driver.is_alive(ref) is False

    def test_send_and_capture_output(self, driver, workdir):
        ref = driver.create_session("agw-test-io", workdir, ["bash"])
        try:
            driver.send_text(ref, "echo HELLO_FROM_CONTAINER")
            driver.send_enter(ref)
            time.sleep(1)
            out = driver.capture(ref)
            assert "HELLO_FROM_CONTAINER" in out
        finally:
            driver.terminate(ref)

    def test_bind_mount_working_directory_is_correct(self, driver, workdir):
        """Regression test for the UID-mismatch bug found during live
        validation: without --user matching the mount owner, container
        root gets EACCES on a bind-mounted host directory under a
        user-namespace-remapped Docker daemon, and tmux's -c cwd
        silently falls back to $HOME instead of failing loudly."""
        ref = driver.create_session("agw-test-cwd", workdir, ["bash"])
        try:
            driver.send_text(ref, "pwd")
            driver.send_enter(ref)
            time.sleep(1)
            out = driver.capture(ref)
            assert workdir in out
        finally:
            driver.terminate(ref)

    def test_host_written_file_readable_in_container(self, driver, workdir):
        with open(os.path.join(workdir, "note.txt"), "w") as f:
            f.write("hello from host\n")
        ref = driver.create_session("agw-test-read", workdir, ["bash"])
        try:
            driver.send_text(ref, "cat note.txt")
            driver.send_enter(ref)
            time.sleep(1)
            out = driver.capture(ref)
            assert "hello from host" in out
        finally:
            driver.terminate(ref)

    def test_container_written_file_visible_on_host(self, driver, workdir):
        ref = driver.create_session("agw-test-write", workdir, ["bash"])
        try:
            driver.send_text(ref, "echo WROTE > written_by_container.txt")
            driver.send_enter(ref)
            time.sleep(1)
            driver.capture(ref)  # drain, not asserted
        finally:
            driver.terminate(ref)
        host_path = os.path.join(workdir, "written_by_container.txt")
        assert os.path.exists(host_path)
        with open(host_path) as f:
            assert "WROTE" in f.read()

    def test_terminate_removes_the_container(self, driver, workdir):
        ref = driver.create_session("agw-test-cleanup", workdir, ["bash"])
        driver.terminate(ref)
        inspect = subprocess.run(
            ["docker", "inspect", ref.session],
            capture_output=True, text=True, timeout=10,
        )
        assert inspect.returncode != 0  # container no longer exists
