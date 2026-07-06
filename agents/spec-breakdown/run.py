#!/usr/bin/env python3
import json, sys, re

input_text = sys.stdin.read()
try: data = json.loads(input_text)
except json.JSONDecodeError: data = {"spec": input_text}

spec = data.get("spec", data.get("input", input_text))
if not spec.strip():
    print(json.dumps({"status": "completed", "tasks": [], "summary": "No spec provided."}))
    sys.exit(0)

phases = {
    "foundation": [],
    "core": [],
    "polish": [],
    "verification": [],
}

all_tasks = []
lines = spec.strip().split("\n")
spec_text = spec.lower()

task_patterns = [
    (r'\b(database|schema|migration|model|entity)\b', "foundation", "Set up database schema and data models", "implement", "medium"),
    (r'\b(api|endpoint|route|handler|controller)\b', "core", "Implement API endpoints with request validation", "implement", "medium"),
    (r'\b(auth|login|register|session|jwt|oauth)\b', "foundation", "Implement authentication and authorization", "implement", "high"),
    (r'\b(ui|page|component|view|template|frontend)\b', "core", "Build UI components for the feature", "implement", "medium"),
    (r'\b(test|spec|assert)\b', "verification", "Write unit and integration tests", "test", "medium"),
    (r'\b(cache|redis|memcache)\b', "core", "Add caching layer for performance", "implement", "low"),
    (r'\b(queue|worker|background|async|celery)\b', "core", "Set up background job processing", "implement", "medium"),
    (r'\b(validation|validate|sanitize)\b', "core", "Add input validation and sanitization", "implement", "high"),
    (r'\b(error|exception|logging|monitor)\b', "polish", "Add error handling and logging", "implement", "medium"),
    (r'\b(document|docs|readme|swagger|openapi)\b', "polish", "Write documentation for the feature", "docs", "low"),
    (r'\b(deploy|ci|cd|docker|kubernetes|helm)\b', "verification", "Configure deployment and CI/CD pipeline", "ops", "medium"),
    (r'\b(migration|upgrade|version|changelog)\b', "polish", "Create migration plan and changelog entry", "docs", "low"),
    (r'\b(metric|prometheus|grafana|alert|dashboard)\b', "polish", "Add observability metrics and dashboards", "implement", "low"),
]

seen = set()
for pattern, phase, desc, kind, effort in task_patterns:
    if re.search(pattern, spec_text):
        task_id = pattern.strip("\\b()")
        if task_id not in seen:
            seen.add(task_id)
            phases[phase].append({
                "id": f"{phase}-{len(phases[phase]) + 1}",
                "description": desc,
                "type": kind,
                "effort": effort,
                "dependencies": list(seen) if phase != "foundation" else [],
            })

spec_sentences = re.split(r'[.!?]+', spec)
for s in spec_sentences:
    s = s.strip()
    if len(s) > 30 and s.lower() not in seen:
        seen.add(s.lower()[:50])
        clean = s[:80]
        phases["core"].append({
            "id": f"core-{len(phases['core']) + 1}",
            "description": f"Implement: {clean}",
            "type": "implement",
            "effort": "medium",
            "dependencies": [t["id"] for t in phases["foundation"]],
        })

phase_order = ["foundation", "core", "polish", "verification"]
all_tasks = []
for phase in phase_order:
    for t in phases[phase]:
        t["phase"] = phase
        all_tasks.append(t)

if not all_tasks:
    all_tasks.append({
        "id": "core-1",
        "phase": "core",
        "description": f"Implement feature based on spec: {spec[:80].strip()}...",
        "type": "implement",
        "effort": "medium",
        "dependencies": [],
    })

summary = {
    "total_tasks": len(all_tasks),
    "by_phase": {p: len(phases[p]) for p in phase_order},
    "by_effort": {},
}
for t in all_tasks:
    e = t["effort"]
    summary["by_effort"][e] = summary["by_effort"].get(e, 0) + 1

result = {
    "status": "completed",
    "summary": f"Spec analyzed: {summary['total_tasks']} tasks identified across {len([p for p in phase_order if phases[p]])} phases.",
    "tasks": all_tasks,
    "stats": summary,
}
print(json.dumps(result, indent=2))