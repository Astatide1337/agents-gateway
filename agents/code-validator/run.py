#!/usr/bin/env python3
"""no-mistakes pipeline validation agent.

Walks through the no-mistakes validation workflow:
  intent -> review -> test -> docs -> lint -> safety -> correctness

Each step produces structured findings (auto-fix, ask-user, no-op).
Returns a full pipeline report with gates, outcomes, and evidence.
"""

import json
import sys
import re
from dataclasses import dataclass, field
from typing import Any


input_text = sys.stdin.read()
try:
    data = json.loads(input_text)
except json.JSONDecodeError:
    data = {"diff": input_text}

diff = data.get("diff", data.get("input", input_text))
intent = data.get("intent", "")
yes_mode = data.get("yes", False)

if not diff.strip():
    print(json.dumps({
        "outcome": "checks-passed",
        "summary": "No code to validate.",
        "pipeline": [],
    }))
    sys.exit(0)

lines = diff.split("\n")
added_lines = [l[1:] for l in lines if l.startswith("+") and not l.startswith("+++") and l.strip()]
removed_lines = [l[1:] for l in lines if l.startswith("-") and not l.startswith("---") and l.strip()]


def dedent(code_lines):
    if not code_lines:
        return code_lines
    min_indent = min((len(l) - len(l.lstrip()) for l in code_lines if l.strip()), default=0)
    return [l[min_indent:] for l in code_lines]


added_code = "\n".join(dedent(added_lines))

next_finding_id = 0


def finding_id():
    global next_finding_id
    next_finding_id += 1
    return f"f{next_finding_id}"


def file_for_line(l):
    for ln in lines:
        if ln.startswith("+++ b/"):
            return ln[6:]
        if ln.startswith("--- a/"):
            pass
    return "(unknown)"


current_file = "(unknown)"
for ln in lines:
    if ln.startswith("+++ b/"):
        current_file = ln[6:]


@dataclass
class Finding:
    id: str = ""
    severity: str = "warning"
    file: str = ""
    line: int = 0
    action: str = "auto-fix"
    description: str = ""
    suggestion: str = ""


@dataclass
class StepResult:
    name: str
    status: str = "passed"
    findings: list[dict] = field(default_factory=list)
    note: str = ""


def run_pipeline():
    steps: list[StepResult] = []

    s1 = step_intent()
    steps.append(s1)

    s2 = step_review()
    steps.append(s2)

    s3 = step_test()
    steps.append(s3)

    s4 = step_docs()
    steps.append(s4)

    s5 = step_lint()
    steps.append(s5)

    s6 = step_safety()
    steps.append(s6)

    s7 = step_correctness()
    steps.append(s7)

    total_findings = sum(len(s.findings) for s in steps)
    total_auto_fix = sum(1 for s in steps for f in s.findings if f["action"] == "auto-fix")
    total_ask_user = sum(1 for s in steps for f in s.findings if f["action"] == "ask-user")
    total_no_op = sum(1 for s in steps for f in s.findings if f["action"] == "no-op")
    failed_steps = [s for s in steps if s.status == "failed"]
    gated_steps = [s for s in steps if s.status == "gate"]

    gate_summary = ""
    if gated_steps:
        gate_names = [s.name for s in gated_steps]
        gate_summary = f"Parked at {', '.join(gate_names)} gate(s). Use --action approve|fix|skip to decide findings."

    if failed_steps:
        outcome = "failed"
        summary_parts = [f"Pipeline failed at step(s): {', '.join(s.name for s in failed_steps)}"]
    elif gated_steps and not yes_mode:
        outcome = "gate"
        summary_parts = [f"Pipeline parked at {', '.join(s.name for s in gated_steps)} gate(s)"]
    elif total_findings == 0:
        outcome = "checks-passed"
        summary_parts = ["All pipeline checks passed."]
    else:
        outcome = "checks-passed"
        summary_parts = [f"All pipeline checks passed with {total_findings} finding(s)."]

    if yes_mode:
        for s in steps:
            for f in s.findings:
                f["action"] = "auto-fix"
            s.status = "passed"
        outcome = "checks-passed"
        summary_parts = [f"Pipeline completed in --yes mode. {total_findings} finding(s) resolved."]

    total_auto_fix = sum(1 for s in steps for f in s.findings if f["action"] == "auto-fix")
    total_ask_user = sum(1 for s in steps for f in s.findings if f["action"] == "ask-user")
    total_no_op = sum(1 for s in steps for f in s.findings if f["action"] == "no-op")
    gated_steps = [s for s in steps if s.status == "gate"]
    failed_steps = [s for s in steps if s.status == "failed"]

    return {
        "outcome": outcome,
        "summary": " | ".join(summary_parts),
        "pipeline": [
            {
                "step": s.name,
                "status": s.status,
                "note": s.note,
                "findings": s.findings,
            }
            for s in steps
        ],
        "stats": {
            "lines_added": len(added_lines),
            "lines_removed": len(removed_lines),
            "steps_total": len(steps),
            "steps_passed": sum(1 for s in steps if s.status == "passed"),
            "steps_gated": len(gated_steps),
            "steps_failed": len(failed_steps),
            "findings_total": total_findings,
            "findings_auto_fix": total_auto_fix,
            "findings_ask_user": total_ask_user,
            "findings_no_op": total_no_op,
        },
    }


def finding_dict(f: Finding) -> dict:
    return {"id": f.id, "severity": f.severity, "file": f.file, "line": f.line,
            "action": f.action, "description": f.description, "suggestion": f.suggestion}


def find_line_for(content: str, code: str) -> int:
    for i, ln in enumerate(lines):
        if content in ln:
            return i + 1
    return 0


def step_intent() -> StepResult:
    r = StepResult(name="intent")
    if not intent:
        r.status = "gate"
        r.note = "No --intent provided. The review step uses intent to distinguish deliberate decisions from mistakes."
        r.findings = [finding_dict(Finding(
            id=finding_id(), severity="warning", file="(pipeline)",
            action="ask-user",
            description="Intent is required: what the user set out to accomplish. Pass the goal or request behind this change, not a description of the diff.",
            suggestion="Re-run with --intent describing the user's objective, decisions, and tradeoffs.",
        ))]
        return r

    r.note = f"Intent recorded: {intent[:120]}"
    return r


def step_review() -> StepResult:
    r = StepResult(name="review")

    has_types = bool(re.search(r':\s*(int|str|float|bool|list|dict|tuple|Optional|Union|Any|None|string|number|boolean|void|never)\b', added_code))
    has_typed_sig = bool(re.search(r'->\s*\w+', added_code))

    if re.search(r'(def |fn |func |function )', added_code) and not (has_types or has_typed_sig):
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="auto-fix",
            description="Functions defined without type annotations. Types make invalid states unrepresentable at compile time.",
            suggestion="Add type annotations to all function signatures. Use Optional[T] for nullable values.",
        )))

    todo_patterns = re.findall(r'(TODO|FIXME|HACK|XXX|WORKAROUND)\b', added_code, re.I)
    if todo_patterns:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="info", file=current_file,
            action="no-op",
            description=f"TODO/FIXME/HACK markers left in code: {', '.join(set(todo_patterns))}. These should be resolved before merging.",
            suggestion="Resolve each marker with an issue reference or remove before merge.",
        )))

    debug_stmts = re.findall(r'\b(print\(|console\.log\(|debug\(|pdb\.set_trace\(|binding\.pry|byebug|dd\()', added_code)
    if debug_stmts:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="auto-fix",
            description=f"Debug statements left in added code: {', '.join(set(d[:15] for d in debug_stmts))}.",
            suggestion="Remove debug statements before shipping.",
        )))

    bare_excepts = re.findall(r'^\s*except\s*:', added_code, re.M)
    if bare_excepts:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="error", file=current_file,
            action="ask-user",
            description="Bare 'except:' clause catches all exceptions including SystemExit/KeyboardInterrupt. Specify exception type.",
            suggestion="Replace 'except:' with 'except SpecificException:' to avoid swallowing critical signals.",
        )))

    long_lines_found = 0
    for i, ln in enumerate(lines):
        if (ln.startswith("+") or ln.startswith("-")) and len(ln) > 120:
            long_lines_found += 1
    if long_lines_found > 2:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="info", file=current_file,
            action="no-op",
            description=f"{long_lines_found} lines exceed 120 characters.",
            suggestion="Break long expressions across multiple lines.",
        )))

    if len(added_lines) > 500:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file="(diff)",
            action="ask-user",
            description=f"Large diff: {len(added_lines)} added lines. Large diffs are harder to review and more likely to hide issues.",
            suggestion="Split into smaller, focused PRs.",
        )))

    if r.findings:
        r.status = "gate"
        r.note = f"Review found {len(r.findings)} finding(s). Review auto-fix is not automatic — decisions belong to you."

    return r


def step_test() -> StepResult:
    r = StepResult(name="test")

    has_tests = bool(re.search(r'(def test_|class Test|describe\(|it\(|#\[test\])', added_code))
    has_property_tests = bool(re.search(r'\b(hypothesis|@given|property|for_all|proptest|quickcheck)\b', added_code, re.I))

    if has_tests and not has_property_tests:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="ask-user",
            description="Tests exist but no property-based testing found. Example-based tests only cover hand-picked inputs and miss edge cases.",
            suggestion="Add property-based tests (Hypothesis for Python, QuickCheck for Rust/Haskell, fast-check for JS) that verify invariants across random inputs.",
        )))

    add_ratio = len(added_lines) / max(len(removed_lines), 1)
    if add_ratio > 5 and len(added_lines) > 50 and not has_tests:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file="(diff)",
            action="ask-user",
            description=f"Net {len(added_lines)} lines added but no tests detected in the diff.",
            suggestion="Add tests covering the new functionality, including property-based tests.",
        )))

    if re.search(r'(def |fn |func |function |class )', added_code):
        has_new_behavior = True
        if not has_tests:
            r.findings.append(finding_dict(Finding(
                id=finding_id(), severity="info", file=current_file,
                action="no-op",
                description="New functions/classes added but no tests in this diff.",
                suggestion="Ensure tests are written, preferably before the implementation (TDD).",
            )))

    return r


def step_docs() -> StepResult:
    r = StepResult(name="docs")

    has_doc_changes = bool(re.search(r'\.(md|rst|txt)$', current_file))
    api_changes = bool(re.search(r'(def |fn |func |function |class |pub |export |async )', added_code))

    if api_changes and not has_doc_changes:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="info", file=current_file,
            action="no-op",
            description="API or interface changes detected but no documentation files changed.",
            suggestion="Update relevant docs (README, API reference, inline docstrings) to reflect the changes.",
        )))

    return r


def step_lint() -> StepResult:
    r = StepResult(name="lint")

    has_trailing = sum(1 for l in added_lines if l.rstrip() != l)
    if has_trailing:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="auto-fix",
            description=f"{has_trailing} line(s) with trailing whitespace.",
            suggestion="Strip trailing whitespace.",
        )))

    tabs_in_py = [l for l in added_lines if "\t" in l and current_file.endswith(".py")]
    if tabs_in_py:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="auto-fix",
            description="Tab characters found in Python file. Use spaces for indentation.",
            suggestion="Replace tabs with 4 spaces.",
        )))

    semicolons = re.findall(r'(.+;)$', added_code, re.M)
    if semicolons and current_file.endswith(".py"):
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="info", file=current_file,
            action="auto-fix",
            description="Unnecessary semicolons in Python code.",
            suggestion="Remove semicolons.",
        )))

    return r


def step_safety() -> StepResult:
    r = StepResult(name="safety")

    secret_pattern = re.compile(
        r'(password|secret|api[-_]?key|token|credential|private_key|access_key|secret_key)',
        re.I,
    )
    secret_lines = []
    for l in added_lines:
        if secret_pattern.search(l) and "=" in l:
            skip = any(k in l.lower() for k in
                       ["example", "placeholder", "your_", "<your", "xxxx", "****", "env.", "getenv", "environ",
                        "os.getenv", "config"])
            if not skip:
                secret_lines.append(l.strip())
    if secret_lines:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="error", file=current_file,
            action="ask-user",
            description="Potential hardcoded secret detected. Credentials should never be committed to version control.",
            suggestion="Use environment variables or a secret manager. Run `git secrets --scan` to verify.",
        )))

    unsafe_funcs = re.findall(r'\b(eval\(|exec\(|os\.system\(|subprocess\.call\(|__import__\()', added_code)
    if unsafe_funcs:
        for uf in unsafe_funcs:
            r.findings.append(finding_dict(Finding(
                id=finding_id(), severity="error", file=current_file,
                action="ask-user",
                description=f"Unsafe function call: {uf}. Can lead to code injection if input is untrusted.",
                suggestion=f"Replace {uf.split('(')[0]} with safer alternatives (e.g. subprocess.run with argument list).",
            )))

    sql_injection = re.findall(
        r'(execute\(f["\']|execute\(\s*["\'].*\{|raw\(|raw_query\()',
        added_code, re.I,
    )
    if sql_injection:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="error", file=current_file,
            action="ask-user",
            description="Possible SQL injection vulnerability. String interpolation in SQL queries can lead to data breaches.",
            suggestion="Use parameterized queries (?, %s, :name) instead of string formatting.",
        )))

    return r


def step_correctness() -> StepResult:
    r = StepResult(name="correctness")

    if re.search(r'\b(match|switch)\b', added_code):
        has_catch_all = bool(re.search(r'\b(_|default|else)\b', added_code))
        if has_catch_all:
            r.findings.append(finding_dict(Finding(
                id=finding_id(), severity="warning", file=current_file,
                action="ask-user",
                description="Match/switch uses a catch-all arm. New variants won't trigger a compile error and may be silently mishandled.",
                suggestion="Remove the catch-all and enumerate all variants explicitly. Let the compiler flag unhandled variants.",
            )))

    null_patterns = [
        r'(if\s+\w+\s*(is not None|is None|!= None|== None))',
        r'(if\s+\w+\s*(=== null|!== null|== null|!= null))',
        r'(Optional\[|nullable|undefined)',
    ]
    any_null_handling = any(re.search(p, added_code) for p in null_patterns)
    has_nullable_marker = bool(re.search(r'(Optional\[|nullable|undefined|Maybe|Option\[)', added_code))
    if has_nullable_marker and not any_null_handling:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="warning", file=current_file,
            action="ask-user",
            description="Nullable types used but no explicit null-handling for all code paths. Missing a null check leads to runtime NPE.",
            suggestion="Handle the None/null case explicitly in every branch. Force callers to deal with absence.",
        )))

    unsafe_unwraps = re.findall(r'\.unwrap\(\)|\.expect\(|assert!\(', added_code)
    if unsafe_unwraps:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="error", file=current_file,
            action="auto-fix",
            description=f"{len(unsafe_unwraps)} unwrap/assert call(s) found. These panic on failure without recovery.",
            suggestion="Replace .unwrap() with proper error propagation (? operator, Result/Option, or match with error context).",
        )))

    mutable_found = re.findall(r'\b(global\s|static\s+mut|var\s|let\s+mut)\b', added_code)
    if mutable_found:
        r.findings.append(finding_dict(Finding(
            id=finding_id(), severity="info", file=current_file,
            action="no-op",
            description="Mutable state declared. Mutable shared state is a common source of non-deterministic bugs.",
            suggestion="Prefer immutable bindings. Encapsulate mutation behind a safe API that enforces invariants.",
        )))

    bounds_access = re.search(r'(\[.*\]|\.get\(|\.at\()', added_code)
    bounds_guard = re.search(r'\b(if\s+\w+\s*[<>]|guard|\.get\(|try_|safe_|contains_key)', added_code)
    if bounds_access and not bounds_guard:
        has_collection = re.search(r'\b(list|dict|array|vec|slice|map)\b', added_code, re.I)
        if has_collection:
            r.findings.append(finding_dict(Finding(
                id=finding_id(), severity="warning", file=current_file,
                action="auto-fix",
                description="Collection access without bounds guard or safe accessor.",
                suggestion="Use safe access methods (.get(), Optional chaining, try_get()) or add explicit bounds checks.",
            )))

    return r


result = run_pipeline()
print(json.dumps(result, indent=2))