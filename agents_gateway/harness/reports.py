"""HTML review report generator.

Generates a single self-contained ``review-report.html`` per agent_run
that includes everything a human/Composer needs for the morning review:

  * task title + brief
  * repo / branch / worktree path
  * harness profile name
  * skills requested
  * tools requested
  * objective/repo timeline summary
  * verification commands and pass/fail status
  * links to every artifact (logs, captures, diff)
  * diff summary (changed files count + insertions/deletions)
  * commit SHA if committed
  * screenshots/videos if present (referenced, not embedded)
  * final status
  * known blockers (missing credentials etc.)

All text is HTML-escaped. No gateway tokens are ever written into the
report — we explicitly redact a known set of patterns before rendering.
"""

from __future__ import annotations

import html
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    HarnessSession,
    HarnessSessionStatus,
    VerificationRun,
    VerificationRunStatus,
    Worktree,
)
from agents_gateway.harness.storage import HarnessStorage


# Patterns to redact from any text that ends up in the report.
_REDACT_PATTERNS = [
    # Bearer tokens / API keys in headers (matches both plain-text and
    # JSON-serialized forms like {"Authorization": "Bearer xyz"}).
    # Captures everything from "Authorization" through the token value.
    re.compile(
        r'(Authorization[^,]*)Bearer\s+[A-Za-z0-9._\-]{4,}',
        re.I,
    ),
    re.compile(
        r"(X-Auth-Internal-Token[^,]*)"
        r"[A-Za-z0-9._\-]{8,}",
        re.I,
    ),
    re.compile(
        r"(Cf-Access-Jwt-Assertion[^,]*)"
        r"[A-Za-z0-9._\-\.]{8,}",
        re.I,
    ),
    # GitHub tokens (16+ chars after ghp_/ghs_/ghu_/ghr_/gho_ prefix)
    re.compile(r"\bgh[pousro]_[A-Za-z0-9]{16,}\b"),
    # Generic token=... assignments
    re.compile(r"(\btoken\s*=\s*)[\"']?[A-Za-z0-9._\-]{8,}[\"']?", re.I),
    re.compile(r"(\bsecret\s*=\s*)[\"']?[A-Za-z0-9._\-]{8,}[\"']?", re.I),
    # Passwords in URLs (https://user:pass@...)
    re.compile(r"(https?://[^:/@\s]+:)[^@/\s]+(@)", re.I),
]


def redact_text(text: str) -> str:
    if not text:
        return ""
    for pat in _REDACT_PATTERNS:
        text = pat.sub(lambda m: (m.group(1) if m.lastindex else "")
                       + "[REDACTED]"
                       + (m.group(2) if m.lastindex and m.lastindex >= 2
                          else ""), text)
    return text


def _esc(text: Any) -> str:
    return html.escape(str(text), quote=True)


def _short(text: str, max_len: int = 120) -> str:
    if not text:
        return ""
    if len(text) <= max_len:
        return text
    return text[:max_len].rsplit(" ", 1)[0] + "..."


def generate_review_report(
    *,
    task_title: str,
    task_brief: str,
    repo_url: str,
    branch: str,
    base_branch: str,
    worktree_path: str,
    harness_profile: str,
    skills_requested: list[str] | None = None,
    tools_requested: list[str] | None = None,
    timeline_events: list[dict[str, Any]] | None = None,
    verification: VerificationRun | None = None,
    artifacts: list[dict[str, Any]] | None = None,
    git_summary: dict[str, Any] | None = None,
    session: HarnessSession | None = None,
    final_status: str = "",
    summary_text: str = "",
    blockers: list[dict[str, Any]] | None = None,
    storage: HarnessStorage | None = None,
) -> str:
    """Return the HTML body for the review report.

    The caller is responsible for writing the result into
    ``artifacts/<agent_run_id>/reports/review-report.html`` via
    ArtifactStore.write_report.
    """
    skills_requested = skills_requested or []
    tools_requested = tools_requested or []
    timeline_events = timeline_events or []
    artifacts = artifacts or []
    git_summary = git_summary or {}
    blockers = blockers or []
    final_status = final_status or (
        session.status if session else "unknown"
    )

    parts: list[str] = []
    parts.append("<!DOCTYPE html>")
    parts.append("<html lang='en'><head><meta charset='utf-8'>")
    parts.append(f"<title>Review Report — {_esc(task_title)}</title>")
    parts.append("<style>")
    parts.append(
        "body { font-family: -apple-system, system-ui, sans-serif; "
        "max-width: 960px; margin: 2em auto; padding: 0 1.5em; color: #1a1a1a; }"
        "h1 { font-size: 1.6em; border-bottom: 1px solid #ddd; padding-bottom: .3em; }"
        "h2 { font-size: 1.25em; margin-top: 1.6em; }"
        "table { border-collapse: collapse; width: 100%; margin: .8em 0; }"
        "th, td { border: 1px solid #eee; padding: .4em .6em; text-align: left; vertical-align: top; }"
        "th { background: #fafafa; }"
        "pre { background: #f6f6f6; padding: .8em; overflow-x: auto; }"
        ".pass { color: #1a7a1a; } .fail { color: #c00; } "
        ".blocked { color: #cc7a00; } .running { color: #4170c0; } "
        ".key { font-weight: 600; background: #f9f9f9; }"
        ".muted { color: #777; }"
        "ul.skills li { margin-bottom: .2em; }"
    )
    parts.append("</style></head><body>")

    # Header
    parts.append(f"<h1>Review Report — {_esc(task_title)}</h1>")
    parts.append(f"<p class='muted'>{_esc(summary_text)}</p>")

    # Summary table
    parts.append("<h2>Summary</h2><table>")
    parts.append(f"<tr><th>Final status</th><td><b class='{_status_class(final_status)}'>{_esc(final_status)}</b></td></tr>")
    parts.append(f"<tr><th>Harness profile</th><td>{_esc(harness_profile)}</td></tr>")
    parts.append(f"<tr><th>Repo</th><td><code>{_esc(repo_url)}</code></td></tr>")
    parts.append(f"<tr><th>Branch</th><td><code>{_esc(branch)}</code> <span class='muted'>(base: <code>{_esc(base_branch)}</code>)</span></td></tr>")
    parts.append(f"<tr><th>Worktree</th><td><code>{_esc(worktree_path)}</code></td></tr>")
    if git_summary.get("commit_sha"):
        parts.append(f"<tr><th>Commit</th><td><code>{_esc(git_summary['commit_sha'])}</code>"
                     f" <span class='muted'>pushed={_esc(git_summary.get('pushed', False))}</span></td></tr>")
    if session is not None:
        parts.append(f"<tr><th>Session id</th><td><code>{_esc(session.id)}</code></td></tr>")
        parts.append(f"<tr><th>tmux session</th><td><code>{_esc(session.tmux_session)}</code></td></tr>")
        parts.append(f"<tr><th>Started at</th><td>{_esc(session.started_at)}</td></tr>")
        if session.ended_at:
            parts.append(f"<tr><th>Ended at</th><td>{_esc(session.ended_at)}</td></tr>")
    parts.append("</table>")

    # Brief
    parts.append("<h2>Task Brief</h2>")
    parts.append(f"<pre>{_esc(redact_text(task_brief))}</pre>")

    # Skills + tools
    if skills_requested:
        parts.append("<h2>Required skills</h2><ul class='skills'>")
        for s in skills_requested:
            parts.append(f"<li>{_esc(s)}</li>")
        parts.append("</ul>")
    if tools_requested:
        parts.append("<h2>Required tools</h2><ul class='skills'>")
        for t in tools_requested:
            parts.append(f"<li>{_esc(t)}</li>")
        parts.append("</ul>")

    # Verification
    if verification is not None:
        parts.append("<h2>Verification</h2>")
        vclass = _status_class(verification.status)
        parts.append(f"<p>Status: <b class='{vclass}'>{_esc(verification.status)}</b></p>")
        parts.append("<table><thead><tr><th>Name</th><th>Required</th><th>Passed</th><th>Exit</th><th>Blocked</th><th>Artifact</th></tr></thead><tbody>")
        for c in verification.commands:
            passed_str = "yes" if c.passed else "no"
            req_str = "yes" if c.required else "no"
            blocked_str = c.blocked_reason if c.blocked else "no"
            art = c.output_artifact or ""
            art_cell = (f"<a href='file://{_esc(art)}'>"
                        f"{_esc(Path(art).name)}</a>"
                        if art else "")
            cls = "pass" if c.passed else ("blocked" if c.blocked else "fail")
            parts.append(f"<tr><td>{_esc(c.name)}</td><td>{req_str}</td>"
                        f"<td class='{cls}'>{passed_str}</td>"
                        f"<td><code>{_esc(c.exit_code)}</code></td>"
                        f"<td>{_esc(blocked_str)}</td>"
                        f"<td>{art_cell}</td></tr>")
        parts.append("</tbody></table>")
        if verification.metadata:
            parts.append("<details><summary>Verification metadata</summary>")
            parts.append(f"<pre>{_esc(redact_text(_dump_json(verification.metadata)))}</pre>")
            parts.append("</details>")

    # Git diff
    if git_summary:
        parts.append("<h2>Git changes</h2>")
        parts.append("<table>")
        parts.append(f"<tr><th>Changed files</th><td>{_esc(git_summary.get('changed_files', 0))}</td></tr>")
        parts.append(f"<tr><th>Insertions</th><td>{_esc(git_summary.get('insertions', 0))}</td></tr>")
        parts.append(f"<tr><th>Deletions</th><td>{_esc(git_summary.get('deletions', 0))}</td></tr>")
        if git_summary.get("commit_sha"):
            parts.append(f"<tr><th>Commit SHA</th><td><code>{_esc(git_summary['commit_sha'])}</code></td></tr>")
        if git_summary.get("pr_url"):
            parts.append(f"<tr><th>PR URL</th><td><a href='{_esc(git_summary['pr_url'])}'>{_esc(git_summary['pr_url'])}</a></td></tr>")
        parts.append("</table>")
        if git_summary.get("files"):
            parts.append("<details><summary>Changed files</summary><ul>")
            for f in git_summary["files"]:
                parts.append(f"<li><code>{_esc(f)}</code></li>")
            parts.append("</ul></details>")

    # Blockers
    if blockers:
        parts.append("<h2>Known blockers</h2><ul>")
        for b in blockers:
            message = b.get("message", "")
            missing = ", ".join(b.get("missing_env") or [])
            parts.append(
                f"<li><b>{_esc(b.get('type', 'unknown'))}</b>: "
                f"{_esc(message)}"
                + (f" (missing env: <code>{_esc(missing)}</code>)"
                   if missing else "")
                + "</li>"
            )
        parts.append("</ul>")

    # Artifacts
    if artifacts:
        parts.append("<h2>Artifacts</h2><table><thead><tr><th>Kind</th><th>Name</th><th>Path</th><th>Size</th></tr></thead><tbody>")
        for a in artifacts:
            parts.append("<tr><td><code>" + _esc(a["kind"]) + "</code></td>"
                        + f"<td>{_esc(a['name'])}</td>"
                        + f"<td><a href='file://{_esc(a.get('path', ''))}'>"
                        + _esc(_short(a.get("path", ""), 80))
                        + "</a></td>"
                        + f"<td>{_esc(a.get('size_bytes', 0))}</td></tr>")
        parts.append("</tbody></table>")

    # Timeline
    if timeline_events:
        parts.append("<h2>Timeline</h2><table><thead>"
                     "<tr><th>Timestamp</th><th>Event</th><th>Data</th></tr></thead><tbody>")
        for ev in timeline_events[:200]:
            data_str = _dump_json(ev.get("data", {}))
            parts.append(
                f"<tr><td>{_esc(ev.get('created_at', ''))}</td>"
                f"<td><code>{_esc(ev.get('event', ''))}</code></td>"
                f"<td><pre>{_esc(redact_text(data_str))}</pre></td></tr>"
            )
        parts.append("</tbody></table>")

    parts.append(f"<p class='muted'>Generated at {_esc(datetime.now(timezone.utc).isoformat())}")
    parts.append(" by agents-gateway harness-runtime report generator.</p>")
    parts.append("</body></html>")
    return "\n".join(parts)


def _status_class(status: str) -> str:
    status = (status or "").lower()
    if status in ("passed", "completed"):
        return "pass"
    if status in ("failed", "failed_claimed"):
        return "fail"
    if status in ("blocked", "blocked_external"):
        return "blocked"
    if status in ("running", "starting", "created"):
        return "running"
    return "muted"


def _dump_json(value: Any) -> str:
    import json
    try:
        return json.dumps(value, indent=2, default=str)
    except Exception:
        return str(value)


__all__ = ["generate_review_report", "redact_text"]
