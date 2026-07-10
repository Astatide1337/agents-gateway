"""Tests for proof artifacts + HTML review report generation."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agents_gateway.harness.artifacts import ArtifactStore
from agents_gateway.harness.models import (
    ArtifactKind,
    HarnessSession,
    HarnessSessionStatus,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
)
from agents_gateway.harness.reports import generate_review_report, redact_text
from agents_gateway.harness.storage import HarnessStorage


@pytest.fixture
def storage(tmp_path):
    return HarnessStorage(str(tmp_path / "harness.db"))


@pytest.fixture
def store(storage, tmp_path):
    return ArtifactStore(storage=storage,
                         artifacts_root=str(tmp_path / "artifacts"))


class TestArtifactStoreBasics:
    def test_ensure_layout_creates_all_subdirs(self, store, tmp_path):
        root = store.ensure_layout("run_a")
        for d in ("logs", "captures", "screenshots", "videos",
                  "reports", "patches", "metadata"):
            assert (root / d).is_dir()

    def test_write_log_writes_file_and_records(self, store, tmp_path):
        a = store.write_log("run_b", "task_b", "session.log", "log content here")
        assert a["kind"] == ArtifactKind.log.value
        assert a["name"] == "session.log"
        assert Path(a["path"]).exists()
        assert Path(a["path"]).read_text() == "log content here"
        listed = store.list_artifacts(agent_run_id="run_b")
        assert any(art["name"] == "session.log" for art in listed)

    def test_write_capture_stores_terminal_capture(self, store, tmp_path):
        a = store.write_capture("run_c", "task_c", "terminal-final.txt",
                                 "tmux output")
        assert a["kind"] == ArtifactKind.terminal_capture.value
        assert Path(a["path"]).exists()

    def test_write_report_stores_html(self, store, tmp_path):
        a = store.write_report("run_d", "task_d", "<html>...</html>")
        assert a["kind"] == ArtifactKind.html_report.value
        assert a["mime_type"] == "text/html"
        assert Path(a["path"]).exists()

    def test_write_diff_stores_patch(self, store, tmp_path):
        a = store.write_diff("run_e", "task_e", "diff --git a/x b/x\n")
        assert a["kind"] == ArtifactKind.patch.value

    def test_write_result_writes_metadata_json(self, store, tmp_path):
        result = {"summary": "Task completed.", "status": "completed"}
        a = store.write_result("run_f", "task_f", result)
        assert a["kind"] == ArtifactKind.metadata.value
        body = json.loads(Path(a["path"]).read_text())
        assert body["status"] == "completed"

    def test_record_external_records_into_storage(self, store, tmp_path):
        some_file = tmp_path / "ext.txt"
        some_file.write_text("external artifact body")
        a = store.record_external(
            "run_g", "task_g", kind="test_output",
            name=some_file.name, path=str(some_file),
            mime_type="text/plain",
        )
        assert a["kind"] == "test_output"
        assert a["size_bytes"] == len("external artifact body")

    def test_list_artifacts_filters_by_task(self, store, tmp_path):
        store.write_log("run_h1", "task_h1", "session.log", "...")
        store.write_log("run_h2", "task_h2", "session.log", "...")
        by_task = store.list_artifacts(task_id="task_h1")
        assert all(a["task_id"] == "task_h1" for a in by_task)


class TestReportGeneration:
    def _build_session(self):
        return HarnessSession(
            id="sess_1", agent_run_id="run_1", task_id="task_1",
            harness_profile="opencode-deepseek", harness="opencode",
            runtime="tmux", tmux_session="agw_session",
            working_directory="/x", status="completed",
        )

    def _build_vr_passed(self):
        vr = VerificationRun(
            id="vr_1", agent_run_id="run_1", task_id="task_1",
            status=VerificationRunStatus.passed.value,
        )
        vr.commands.append(VerificationCommandResult(
            name="unit tests", command="uv run pytest -q", required=True,
            exit_code=0, passed=True, output_artifact="/x/log.txt",
        ))
        return vr

    def test_report_contains_basic_sections(self):
        session = self._build_session()
        vr = self._build_vr_passed()
        html = generate_review_report(
            task_title="Build timeline endpoint",
            task_brief="Implement GET /objectives/{id}/timeline.",
            repo_url="https://github.com/Astatide1337/conductor.git",
            branch="agent/task-1-timeline", base_branch="master",
            worktree_path="/var/lib/agw/worktrees/task_1",
            harness_profile="opencode-deepseek",
            skills_requested=["test-driven-development",
                              "verification-before-completion"],
            tools_requested=["github.read"],
            verification=vr,
            artifacts=[
                {"kind": "test_output", "name": "log.txt",
                 "path": "/x/log.txt", "size_bytes": 1234},
                {"kind": "html_report", "name": "review-report.html",
                 "path": "/x/report.html", "size_bytes": 4567},
            ],
            git_summary={"changed_files": ["src/x.py"],
                          "insertions": 42, "deletions": 3,
                          "commit_sha": "abc123",
                          "pushed": False, "pr_url": None,
                          "files": ["src/x.py"]},
            session=session,
            final_status="completed",
            summary_text="All required verification passed.",
            blockers=[],
        )
        assert "<h1>" in html
        assert "Build timeline endpoint" in html
        assert "Verification" in html
        assert "passed" in html.lower()
        assert "unit tests" in html
        assert "test-driven-development" in html
        assert "github.read" in html
        assert "abc123" in html
        assert "src/x.py" in html
        assert "log.txt" in html
        assert "review-report.html" in html
        assert "completed" in html

    def test_report_redacts_authorization_headers_in_timeline(self):
        timeline = [
            {"created_at": "2026-01-01T00:00:00Z",
             "event": "session.send_header",
             "data": {"Authorization": "Bearer ghp_secrettoken_12345"}},
        ]
        html = generate_review_report(
            task_title="X", task_brief="X", repo_url="x", branch="b",
            base_branch="master", worktree_path="/w",
            harness_profile="opencode-deepseek",
            timeline_events=timeline,
        )
        assert "ghp_secrettoken_12345" not in html
        assert "[REDACTED]" in html

    def test_report_redacts_github_tokens_anywhere(self):
        html = generate_review_report(
            task_title="Task X",
            task_brief="contains a fake github token ghp_abcdefghij0123456789abcdefghij0123456789x",
            repo_url="x", branch="b", base_branch="master",
            worktree_path="/w", harness_profile="opencode-deepseek",
        )
        assert "ghp_abcdefghij0123456789abcdefghij0123456789x" not in html

    def test_report_renders_blockers_block(self):
        html = generate_review_report(
            task_title="X", task_brief="Y", repo_url="x", branch="b",
            base_branch="master", worktree_path="/w",
            harness_profile="opencode-deepseek",
            blockers=[
                {"type": "missing_credentials",
                 "message": "Live E2E requires GITHUB_TOKEN",
                 "missing_env": ["GITHUB_TOKEN", "CONDUCTOR_TOKEN"]},
            ],
        )
        assert "blockers" in html.lower() or "Known blockers" in html
        assert "missing_credentials" in html
        assert "GITHUB_TOKEN" in html


class TestRedactText:
    def test_authorization_bearer_redacted(self):
        out = redact_text("Authorization: Bearer super-secret-token-abc")
        assert "super-secret-token-abc" not in out
        assert "[REDACTED]" in out

    def test_ghp_token_redacted(self):
        out = redact_text(
            "my token is ghp_0123456789abcdefghij0123456789abcdefghij0123456789abcdefghij"
        )
        assert "ghp_0123456789abcdefghij0123456789abcdefghij0123456789abcdefghij" not in out

    def test_url_credentials_redacted(self):
        out = redact_text("https://user:password@example.com/path")
        assert "password" not in out.lower() or "<redacted>" in out

    def test_safe_text_unchanged(self):
        out = redact_text("nothing sensitive here")
        assert "nothing sensitive here" in out

    def test_internal_token_redacted(self):
        out = redact_text(
            "X-Auth-Internal-Token: shared-secret-ABC123DEF456GHI789"
        )
        assert "shared-secret-ABC123DEF456GHI789" not in out
