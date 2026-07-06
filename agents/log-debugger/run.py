#!/usr/bin/env python3
import json, sys, re
from collections import defaultdict

input_text = sys.stdin.read()
try: data = json.loads(input_text)
except json.JSONDecodeError: data = {"logs": input_text}

logs = data.get("logs", data.get("input", input_text))
if not logs.strip():
    print(json.dumps({"status": "completed", "issues": [], "summary": "No logs provided."}))
    sys.exit(0)

stack_traces = []
exceptions = []
error_patterns = set()
lines = logs.split("\n")

for i, line in enumerate(lines):
    stripped = line.strip()
    m = re.match(r'^Traceback \(most recent call last\):', stripped)
    if m:
        trace_lines = [stripped]
        for j in range(i + 1, min(i + 40, len(lines))):
            tl = lines[j].strip()
            trace_lines.append(tl)
            if re.match(r'^\w+(Error|Exception|Warning|Failure|Fault):', tl):
                exceptions.append(tl)
                break
        stack_traces.append("\n".join(trace_lines))

    for pattern in [
        r'\b(Error|ERROR|FATAL|CRITICAL)\b',
        r'\b(Timeout|timeout|timed?\s*out)\b',
        r'\b(failed|Failure|FAILED)\b',
        r'\b(ConnectionRefused|connection refused|ECONNREFUSED)\b',
        r'\b(KeyError|IndexError|TypeError|ValueError|AttributeError|ImportError|ModuleNotFoundError)\b',
        r'\b(Segmentation fault|segfault|core dumped)\b',
        r'\b(OOMKilled|OutOfMemory|memory.*exhausted)\b',
        r'\b(exit code|exited with)\b',
    ]:
        if re.search(pattern, stripped, re.I):
            error_patterns.add(stripped[:120])

issues = []

if stack_traces:
    last_trace = stack_traces[-1]
    exc_match = re.search(r'(\w+(?:Error|Exception|Warning|Failure|Fault)):\s*(.*)', last_trace)
    if exc_match:
        exc_type = exc_match.group(1)
        exc_msg = exc_match.group(2)
        severity = "error"
        if any(k in exc_type.lower() for k in ["timeout", "connection", "network"]):
            category = "infrastructure"
            fix = "Check network connectivity, service availability, and timeout settings."
        elif any(k in exc_type.lower() for k in ["key", "index", "attribute", "import", "module"]):
            category = "code-bug"
            fix = "Verify variable/import names exist in the expected scope. Add null/undefined checks."
        elif any(k in exc_type.lower() for k in ["type", "value"]):
            category = "type-mismatch"
            fix = "Add type validation and sanitize inputs before use."
        elif any(k in exc_type.lower() for k in ["memory", "oom", "segfault"]):
            category = "resource"
            fix = "Increase memory limits, optimize data structures, or add streaming."
        else:
            category = "runtime-error"
            fix = "Review the failing code path. Add defensive checks and error handling."

        issues.append({
            "severity": severity,
            "category": category,
            "exception": exc_type,
            "message": exc_msg.strip(),
            "suggested_fix": fix,
        })

for pattern in error_patterns:
    if not any(pattern in i["message"] for i in issues):
        issues.append({
            "severity": "warning",
            "category": "log-anomaly",
            "message": pattern[:200],
            "suggested_fix": "Review surrounding context for this log line to determine root cause.",
        })

summary_parts = []
if stack_traces:
    summary_parts.append(f"{len(stack_traces)} stack trace(s)")
if exceptions:
    summary_parts.append(f"{len(exceptions)} exception(s)")
if error_patterns:
    summary_parts.append(f"{len(error_patterns)} error pattern(s)")

result = {
    "status": "completed",
    "summary": f"Analyzed {len(lines)} log lines. Found {len(issues)} issue(s): {' | '.join(summary_parts)}." if summary_parts else f"Analyzed {len(lines)} log lines. No critical issues detected.",
    "issues": issues,
    "stats": {"log_lines": len(lines), "stack_traces": len(stack_traces), "exceptions": len(exceptions)},
}
print(json.dumps(result, indent=2))