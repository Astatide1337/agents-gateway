#!/usr/bin/env python3
import json, sys, re
from pathlib import Path

input_text = sys.stdin.read()
try: data = json.loads(input_text)
except json.JSONDecodeError: data = {"diff": input_text}

diff = data.get("diff", data.get("input", input_text))
if not diff.strip():
    print(json.dumps({"status": "completed", "findings": [], "summary": "No diff provided."}))
    sys.exit(0)

findings = []
lines = diff.split("\n")
added_lines = [l for l in lines if l.startswith("+") and not l.startswith("+++")]
removed_lines = [l for l in lines if l.startswith("-") and not l.startswith("---")]

if any("TODO" in l.upper() or "FIXME" in l.upper() or "HACK" in l.upper() for l in added_lines):
    findings.append({
        "severity": "warning", "category": "code-quality",
        "message": "Added lines contain TODO/FIXME/HACK markers that should be resolved before merge.",
        "lines": [l.strip() for l in added_lines if "TODO" in l.upper() or "FIXME" in l.upper() or "HACK" in l.upper()],
    })

if any(re.search(r'password|secret|api[-_]?key|token|credential', l, re.I) and '=' in l and not any(k in l.lower() for k in ['example', 'placeholder', 'your_', '<your', 'xxxx', '****', 'env.'])
        for l in added_lines if not l.strip().startswith("+#") and not l.strip().startswith("+//") and not l.strip().startswith("+/*")):
    findings.append({
        "severity": "error", "category": "security",
        "message": "Potential hardcoded secret detected in added lines.",
        "lines": [l.strip() for l in added_lines if re.search(r'password|secret|api[-_]?key|token|credential', l, re.I)],
    })

if any("print(" in l and not l.strip().startswith("+#") for l in added_lines if l.strip().startswith("+") and any(f in l for f in [".py", ".js", ".ts"]) is False):
    py_or_js_added = [l for l in added_lines if any(ext in data.get("diff", "") for ext in [".py", ".js", ".ts", ".jsx", ".tsx"]) or True]
    debug_stmts = [l.strip() for l in added_lines if re.search(r'\b(print|console\.log|debug|dd\b|pdb\.set_trace|binding\.pry)\b', l)]
    if debug_stmts:
        findings.append({
            "severity": "warning", "category": "code-quality",
            "message": "Debug statements left in added code. Remove before shipping.",
            "lines": debug_stmts,
        })

bare_excepts = [l.strip() for l in added_lines if re.search(r'^\s*except\s*:', l)]
if bare_excepts:
    findings.append({
        "severity": "error", "category": "bug-risk",
        "message": "Bare 'except:' clause catches all exceptions including SystemExit/KeyboardInterrupt. Specify exception type.",
        "lines": bare_excepts,
    })

long_lines = [f"line {i+1}: {l.strip()}" for i, l in enumerate(lines) if len(l) > 100 and any(c in l for c in ["+", "-"])]
if long_lines:
    findings.append({
        "severity": "warning", "category": "style",
        "message": f"Lines exceed 100 characters. Consider breaking into smaller expressions.",
        "lines": long_lines[:5],
    })

if len(added_lines) > 200:
    findings.append({
        "severity": "info", "category": "complexity",
        "message": f"Large diff ({len(added_lines)} added lines). Consider splitting into smaller PRs for easier review.",
    })

result = {
    "status": "completed",
    "summary": f"Reviewed {len(added_lines)} added, {len(removed_lines)} removed lines. Found {len(findings)} issue(s).",
    "findings": findings,
    "stats": {"added": len(added_lines), "removed": len(removed_lines)},
}
print(json.dumps(result, indent=2))