#!/usr/bin/env python3
"""Interactive QA crawl for a freshly-integrated app.

Starts the app (auto-detecting how), then drives it with a real headless
browser: visits every reachable view, clicks primary interactive elements,
collects console/page errors, and scans the rendered chrome for the
concrete anti-slop violations from the frontend-design-quality skill
(emoji standing in for icons, flat-color placeholder "art" instead of real
imagery). This exists because static unit tests and a couple of
screenshots do not catch functional-but-ugly, or worse, functional-but-
broken UI — an agent's own two-screenshot verification missed a
completely blank homepage and would have missed every one of these too.

Exit code 0: crawl completed, no console/page errors, no hard slop
             violations.
Exit code 1: a real functional error (uncaught exception, console error)
             or a confirmed hard slop violation (emoji-as-icon, flat-
             color placeholder art matching the exact pattern that
             shipped in the last build) was found.
Exit code 2: could not even start or reach the app.

Writes verification/qa/report.json and verification/qa/*.png into the
worktree so the findings are inspectable evidence, not just a pass/fail.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

MAX_VIEWS = 15
STARTUP_TIMEOUT_SECONDS = 45
CRAWL_STEP_TIMEOUT_MS = 8000

# Common emoji ranges used as icon stand-ins. Deliberately does not match
# every emoji in Unicode — this targets pictographs, symbols, and
# transport/map symbols, the ranges actually used as ad hoc UI icons.
EMOJI_PATTERN = re.compile(
    "[\U0001F300-\U0001FAFF\U00002600-\U000027BF\U0001F000-\U0001F0FF"
    "\U0001F100-\U0001F1FF\U00002190-\U000021FF\U00002B00-\U00002BFF]"
)


def _wait_for_http(url: str, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            urllib.request.urlopen(url, timeout=2)
            return True
        except Exception:
            time.sleep(0.5)
    return False


def _detect_start_command(repo_root: Path) -> tuple[list[str], int, str] | None:
    """Returns (argv, port, kind) or None if nothing recognizable found."""
    pkg = repo_root / "package.json"
    if pkg.exists():
        try:
            data = json.loads(pkg.read_text())
        except (json.JSONDecodeError, OSError):
            data = {}
        scripts = data.get("scripts", {})
        node_modules = repo_root / "node_modules"
        argv_prefix = []
        if not node_modules.exists():
            argv_prefix = ["sh", "-c", "npm install --silent && "]
        script = "dev" if "dev" in scripts else ("start" if "start" in scripts else None)
        if script:
            cmd = f"npm run {script}"
            if argv_prefix:
                return (["sh", "-c", f"npm install --silent && {cmd}"], 3000, "node")
            return (["npm", "run", script], 3000, "node")

    for candidate in ("app.py", "main.py", "server.py"):
        f = repo_root / candidate
        if f.exists() and "fastapi" in f.read_text(errors="ignore").lower():
            module = candidate[:-3]
            return (
                ["uvx", "--with", "fastapi", "--with", "uvicorn", "--with", "pydantic",
                 "--with", "httpx", "--with", "aiofiles",
                 "uvicorn", f"{module}:app", "--host", "127.0.0.1", "--port", "8099"],
                8099, "fastapi",
            )
    return None


def _terminate_process_group(proc: subprocess.Popen, timeout: float = 5.0) -> None:
    """Kill the whole process group, not just ``proc`` itself.

    ``npm run dev`` (and similar wrapper scripts) commonly fork a real
    server as a grandchild rather than exec-replacing themselves —
    signalling only the direct child leaves that grandchild running,
    orphaned, still bound to the port. Confirmed live: three-plus
    leaked ``next dev`` processes from separate verification retries
    ended up alive simultaneously, competing for the same port and
    producing a crashed/conflicting instance the next retry's crawl
    then reported false 404s and unstyled pages against.
    """
    try:
        pgid = os.getpgid(proc.pid)
    except ProcessLookupError:
        return
    try:
        os.killpg(pgid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(pgid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        try:
            proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            pass


def start_app(repo_root: Path) -> tuple[subprocess.Popen, int] | None:
    detected = _detect_start_command(repo_root)
    if detected is None:
        return None
    argv, port, kind = detected
    print(f"[qa_crawl] starting app ({kind}): {' '.join(argv)}")
    proc = subprocess.Popen(
        argv, cwd=str(repo_root),
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    base = f"http://127.0.0.1:{port}"
    if _wait_for_http(base, STARTUP_TIMEOUT_SECONDS):
        return proc, port
    _terminate_process_group(proc)
    return None


def scan_slop(html: str) -> list[dict]:
    """Static scan for the concrete anti-slop violations we can detect
    without a vision model: emoji standing in for icons in chrome
    elements, and the exact flat-color-SVG-placeholder-art pattern."""
    violations = []

    # Emoji inside short interactive elements (button/nav-link text),
    # not inside long-form body copy where emoji in user content is fine.
    for tag_pattern, label in (
        (r"<button[^>]*>([^<]{0,12})</button>", "button"),
        (r"<a[^>]*class=\"[^\"]*nav[^\"]*\"[^>]*>([^<]{0,12})</a>", "nav link"),
    ):
        for m in re.finditer(tag_pattern, html, re.IGNORECASE):
            text = m.group(1)
            emoji_found = EMOJI_PATTERN.findall(text)
            if emoji_found and not re.search(r"[a-zA-Z]{3,}", text):
                violations.append({
                    "type": "emoji_as_icon",
                    "severity": "hard",
                    "detail": f"{label} text is emoji-only: {text!r}",
                })

    # The exact flat-rect + centered-text SVG placeholder pattern.
    svg_blocks = re.findall(r"<svg[^>]*>.*?</svg>", html, re.IGNORECASE | re.DOTALL)
    for svg in svg_blocks:
        has_full_rect = re.search(r"<rect[^>]*width=\"?100%", svg) or re.search(
            r"<rect[^>]*width=\"(\d+)\"[^>]*height=\"\1\"", svg)
        text_nodes = re.findall(r"<text[^>]*>(.*?)</text>", svg, re.DOTALL)
        if has_full_rect and len(text_nodes) == 1:
            violations.append({
                "type": "flat_placeholder_art",
                "severity": "hard",
                "detail": "SVG is a single full-size rect + one centered "
                          "text/emoji glyph — flat placeholder art, not "
                          "real imagery.",
            })
    return violations


def crawl(base_url: str, out_dir: Path) -> dict:
    from playwright.sync_api import sync_playwright

    console_errors: list[str] = []
    page_errors: list[str] = []
    screenshots: list[str] = []
    slop_violations: list[dict] = []
    views_visited: list[str] = []

    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1280, "height": 800})
        page.on("console", lambda m: console_errors.append(m.text)
                if m.type == "error" else None)
        page.on("pageerror", lambda exc: page_errors.append(str(exc)))

        page.goto(base_url, wait_until="networkidle", timeout=CRAWL_STEP_TIMEOUT_MS)
        page.wait_for_timeout(1200)

        nav_selector = ("nav a, [role=navigation] a, nav button, "
                        "[data-view], .sidebar a, .nav-item")
        to_visit = ["__root__"]
        step = 0
        while to_visit and step < MAX_VIEWS:
            step += 1
            target = to_visit.pop(0)
            if target != "__root__":
                try:
                    page.locator(target).first.click(timeout=3000)
                    page.wait_for_timeout(900)
                except Exception:
                    continue
            views_visited.append(target)

            html = page.content()
            slop_violations.extend(scan_slop(html))

            shot_path = out_dir / f"view_{step:02d}.png"
            try:
                page.screenshot(path=str(shot_path), full_page=False)
                screenshots.append(shot_path.name)
            except Exception:
                pass

            if step == 1:
                count = min(page.locator(nav_selector).count(), MAX_VIEWS)
                to_visit = [
                    f":nth-match({nav_selector}, {i + 1})"
                    for i in range(count)
                ]

        browser.close()

    return {
        "base_url": base_url,
        "views_visited": len(views_visited),
        "console_errors": console_errors,
        "page_errors": page_errors,
        "slop_violations": slop_violations,
        "screenshots": screenshots,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo-root", default=".")
    args = ap.parse_args()
    repo_root = Path(args.repo_root).resolve()
    out_dir = repo_root / "verification" / "qa"
    out_dir.mkdir(parents=True, exist_ok=True)

    started = start_app(repo_root)
    if started is None:
        report = {"error": "could not detect or start the app"}
        (out_dir / "report.json").write_text(json.dumps(report, indent=2))
        print("[qa_crawl] FAIL: could not detect or start the app")
        return 2

    proc, port = started
    try:
        report = crawl(f"http://127.0.0.1:{port}", out_dir)
    except Exception as exc:
        report = {"error": f"crawl crashed: {exc}"}
        (out_dir / "report.json").write_text(json.dumps(report, indent=2))
        print(f"[qa_crawl] FAIL: crawl crashed: {exc}")
        return 2
    finally:
        _terminate_process_group(proc)

    (out_dir / "report.json").write_text(json.dumps(report, indent=2))

    hard_violations = [v for v in report["slop_violations"] if v["severity"] == "hard"]
    print(f"[qa_crawl] visited {report['views_visited']} view(s), "
          f"{len(report['screenshots'])} screenshot(s) saved")
    print(f"[qa_crawl] console errors: {len(report['console_errors'])}, "
          f"page errors: {len(report['page_errors'])}, "
          f"slop violations: {len(hard_violations)}")
    for e in report["console_errors"][:10]:
        print(f"  [console error] {e}")
    for e in report["page_errors"][:10]:
        print(f"  [page error] {e}")
    for v in hard_violations[:10]:
        print(f"  [slop] {v['type']}: {v['detail']}")

    if report["page_errors"] or report["console_errors"] or hard_violations:
        print("[qa_crawl] FAIL — see verification/qa/report.json and screenshots")
        return 1
    if report["views_visited"] <= 1:
        print("[qa_crawl] FAIL — crawl only reached the initial view, "
              "no navigable UI was found")
        return 1
    print("[qa_crawl] PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
