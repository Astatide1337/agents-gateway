"""_terminate_process_group: real incident regression.

npm run dev (and similar wrapper scripts) commonly fork a real server
as a grandchild rather than exec-replacing themselves — signalling
only the direct child leaves that grandchild running, orphaned, still
bound to the port. Confirmed live: leaked next-dev processes from
separate verification retries competed for the same port and produced
a crashed instance the next retry's crawl reported false failures
against.
"""
from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent
                       / "agents_gateway" / "harness" / "qa"))
import qa_crawl  # noqa: E402


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


class TestTerminateProcessGroup:
    def test_kills_grandchild_process_not_just_direct_child(self):
        # A shell wrapper that forks a real grandchild (mirrors `sh -c
        # "next dev"` spawning a node process) rather than exec'ing it.
        proc = subprocess.Popen(
            ["sh", "-c", "sleep 60 & echo $! > /tmp/qa_crawl_test_child.pid; wait"],
            start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        deadline = time.time() + 5
        child_pid = None
        while time.time() < deadline:
            if os.path.exists("/tmp/qa_crawl_test_child.pid"):
                child_pid = int(Path("/tmp/qa_crawl_test_child.pid").read_text().strip())
                break
            time.sleep(0.05)
        assert child_pid is not None, "grandchild never recorded its pid"
        assert _pid_alive(child_pid)

        qa_crawl._terminate_process_group(proc, timeout=3.0)

        assert not _pid_alive(proc.pid)
        assert not _pid_alive(child_pid), \
            "grandchild survived — only the direct child was signalled"
        os.remove("/tmp/qa_crawl_test_child.pid")

    def test_escalates_to_sigkill_if_group_ignores_sigterm(self):
        proc = subprocess.Popen(
            ["sh", "-c", "trap '' TERM; sleep 60"],
            start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        time.sleep(0.2)
        qa_crawl._terminate_process_group(proc, timeout=1.0)
        assert not _pid_alive(proc.pid)

    def test_already_dead_process_is_a_no_op(self):
        proc = subprocess.Popen(
            ["true"], start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        proc.wait()
        qa_crawl._terminate_process_group(proc, timeout=1.0)  # must not raise
