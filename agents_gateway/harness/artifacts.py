"""Proof artifact storage layout per agent_run.

Layout on disk (rooted at ``artifacts_root``):

  artifacts/<agent_run_id>/
    logs/
      session.log           <- full session capture written at completion
      verification-<name>.txt  <- per-command verification output
      live-e2e.txt          <- live E2E output if applicable
    captures/
      terminal-final.txt   <- final tmux capture
    screenshots/           <- screenshots when configured
    videos/                <- screencast videos when configured
    reports/
      review-report.html   <- HTML review report (main human artifact)
    patches/
      diff.patch           <- captured git diff
    metadata/
      result.json          <- structured final result

All artifacts are recorded in the ``harness_artifacts`` SQLite table so
the HTTP API can list/serve them. Sensitive data is redacted before
writing into HTML reports (see ``reports.py``); raw artifact files may
contain verification output but never gateway tokens (verification
subprocesses are run with a stripped environment — see verification.py).
"""

from __future__ import annotations

import shutil
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    ArtifactKind,
    HarnessSession,
    VerificationRun,
)
from agents_gateway.harness.storage import HarnessStorage


class ArtifactStore:
    """Owns the per-run artifact directory tree + DB rows."""

    def __init__(self, storage: HarnessStorage,
                 artifacts_root: str = "/var/lib/agents-gateway/artifacts",
                 emit_event: Any | None = None) -> None:
        self.storage = storage
        self.root = Path(artifacts_root)
        self.emit_event = emit_event or (lambda *a, **kw: None)

    # -------------------------------------------------------------------
    # Layout helpers
    # -------------------------------------------------------------------

    def run_root(self, agent_run_id: str) -> Path:
        d = self.root / agent_run_id
        # If the run dir exists, keep it. We always (re-create) the
        # standard sub-tree so callers can rely on the layout.
        return d

    def ensure_layout(self, agent_run_id: str) -> Path:
        root = self.run_root(agent_run_id)
        for sub in ("logs", "captures", "screenshots", "videos",
                    "reports", "patches", "metadata"):
            (root / sub).mkdir(parents=True, exist_ok=True)
        return root

    def write_log(self, agent_run_id: str, task_id: str,
                  name: str, content: str,
                  kind: str = ArtifactKind.log.value) -> dict[str, Any]:
        """Write a log-text artifact and record it."""
        path = self.ensure_layout(agent_run_id) / "logs" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        return self._record(agent_run_id, task_id, kind, name, path,
                            mime="text/plain", content=content)

    def write_capture(self, agent_run_id: str, task_id: str,
                      name: str, content: str,
                      kind: str = ArtifactKind.terminal_capture.value
                      ) -> dict[str, Any]:
        """Write a tmux capture artifact."""
        path = self.ensure_layout(agent_run_id) / "captures" / name
        path.write_text(content)
        return self._record(agent_run_id, task_id, kind, name, path,
                            mime="text/plain", content=content)

    def write_report(self, agent_run_id: str, task_id: str,
                    html: str) -> dict[str, Any]:
        path = self.ensure_layout(agent_run_id) / "reports" / "review-report.html"
        path.write_text(html)
        return self._record(agent_run_id, task_id,
                            ArtifactKind.html_report.value,
                            "review-report.html", path,
                            mime="text/html", content=html)

    def write_diff(self, agent_run_id: str, task_id: str,
                  diff_text: str, name: str = "diff.patch") -> dict[str, Any]:
        path = self.ensure_layout(agent_run_id) / "patches" / name
        path.write_text(diff_text)
        return self._record(agent_run_id, task_id,
                            ArtifactKind.patch.value, name, path,
                            mime="text/plain", content=diff_text)

    def write_result(self, agent_run_id: str, task_id: str,
                    result: dict[str, Any]) -> dict[str, Any]:
        import json
        path = self.ensure_layout(agent_run_id) / "metadata" / "result.json"
        body = json.dumps(result, indent=2, default=str)
        path.write_text(body)
        return self._record(agent_run_id, task_id,
                            ArtifactKind.metadata.value, "result.json",
                            path, mime="application/json", content=body)

    def record_external(self, agent_run_id: str, task_id: str,
                        kind: str, name: str, path: str,
                        mime_type: str = "application/octet-stream",
                        metadata: dict[str, Any] | None = None
                        ) -> dict[str, Any]:
        """Record an artifact already written elsewhere on disk."""
        size = 0
        try:
            size = Path(path).stat().st_size
        except Exception:
            pass
        return self.storage.add_harness_artifact(
            agent_run_id=agent_run_id, task_id=task_id, kind=kind, name=name,
            path=path, mime_type=mime_type, size_bytes=size, metadata=metadata,
        )

    def copy_into(self, agent_run_id: str, task_id: str,
                  source: str, kind: str, name: str,
                  sub_dir: str = "captures",
                  mime_type: str = "application/octet-stream") -> dict[str, Any]:
        """Copy an existing file into the run artifacts tree."""
        src = Path(source)
        if not src.exists():
            raise FileNotFoundError(source)
        target_dir = self.ensure_layout(agent_run_id) / sub_dir
        target_dir.mkdir(parents=True, exist_ok=True)
        target = target_dir / name
        shutil.copy2(src, target)
        return self._record(agent_run_id, task_id, kind, name, target,
                            mime=mime_type, content=None, source=src)

    def list_artifacts(self, agent_run_id: str | None = None,
                       task_id: str | None = None) -> list[dict[str, Any]]:
        return self.storage.list_harness_artifacts(agent_run_id, task_id)

    # -------------------------------------------------------------------
    # helpers
    # -------------------------------------------------------------------

    def _record(self, agent_run_id: str, task_id: str, kind: str, name: str,
                path: Path, mime: str, content: str | None,
                source: Path | None = None) -> dict[str, Any]:
        size = 0
        src = source or path
        try:
            size = src.stat().st_size
        except Exception:
            if content is not None:
                size = len(content.encode())
        artifact = self.storage.add_harness_artifact(
            agent_run_id=agent_run_id, task_id=task_id, kind=kind, name=name,
            path=str(path), mime_type=mime, size_bytes=size,
        )
        # event hook for listeners
        try:
            self.emit_event(agent_run_id, task_id,
                            "artifact.created", artifact)
        except Exception:
            pass
        return artifact


__all__ = ["ArtifactStore"]
