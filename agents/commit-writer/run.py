#!/usr/bin/env python3
import json, sys, re
from collections import Counter

input_text = sys.stdin.read()
try: data = json.loads(input_text)
except json.JSONDecodeError: data = {"diff": input_text}

diff = data.get("diff", data.get("input", input_text))

type_scope = "feat"
scope = ""
description = "update"
body = []
breaking = False

for line in diff.split("\n"):
    stripped = line.strip()
    if re.match(r'^```', stripped):
        pass

file_changes = re.findall(r'^diff --git a/(.*?) b/(.*?)$', diff, re.M)
if file_changes:
    paths = [f[0] for f in file_changes]
    exts = [p.split(".")[-1] if "." in p else "" for p in paths]
    ext_counts = Counter(exts)

    if any("test" in p.lower() or "spec" in p.lower() or "__tests__" in p.lower() for p in paths):
        type_scope = "test"
    elif any(p.endswith((".md", ".rst", ".txt")) for p in paths):
        type_scope = "docs"
    elif any(p.endswith((".yaml", ".yml", ".toml", ".ini", ".cfg")) for p in paths):
        type_scope = "chore"
    elif ext_counts.get("py", 0) > 0:
        type_scope = "feat"
    elif ext_counts.get("js", 0) > 0 or ext_counts.get("ts", 0) > 0 or ext_counts.get("tsx", 0) > 0 or ext_counts.get("jsx", 0) > 0:
        type_scope = "feat"
    elif ext_counts.get("css", 0) > 0 or ext_counts.get("scss", 0) > 0:
        type_scope = "style"
    elif ext_counts.get("json", 0) > 0:
        type_scope = "chore"

    dirs = [p.split("/")[0] for p in paths]
    common_dir = max(set(dirs), key=dirs.count)
    if common_dir != ".":
        scope = common_dir

added_lines = [l for l in diff.split("\n") if l.startswith("+") and not l.startswith("+++")]
removed_lines = [l for l in diff.split("\n") if l.startswith("-") and not l.startswith("---")]

if type_scope == "feat" and not any(l.startswith("+def ") or l.startswith("+class ") or l.startswith("+function ") or l.startswith("+export ") or l.startswith("+pub ") for l in added_lines):
    type_scope = "fix"

if any("BREAKING CHANGE" in l or "!" in l.split(":")[0] for l in diff.split("\n") if l.startswith("+") or l.startswith("-")):
    breaking = True

removed_more = len(removed_lines) > len(added_lines) * 1.5
if not breaking:
    if removed_more and type_scope == "feat":
        type_scope = "refactor"
    elif removed_more and type_scope == "fix":
        pass

if added_lines:
    for l in added_lines:
        clean = l[1:].strip()
        if len(clean) > 10:
            description = clean[:72].rstrip(".,;:")
            if description.endswith("("):
                description = description[:-1].strip()
            break

if not description or description == "update":
    description = f"update {len(file_changes)} file(s)"

scope_str = f"({scope})" if scope else ""
breaking_str = "!" if breaking else ""
header = f"{type_scope}{scope_str}{breaking_str}: {description}"

if len(body) > 0:
    body_lines = body[:5]
else:
    stats = f"Files changed: {len(file_changes)} | +{len(added_lines)} -{len(removed_lines)}"
    body_lines = [stats]

commit_msg = header + "\n\n" + "\n".join(body_lines) if body_lines else header

print(json.dumps({
    "status": "completed",
    "commit_message": commit_msg,
    "parsed": {
        "type": type_scope,
        "scope": scope or None,
        "description": description,
        "breaking": breaking,
    },
    "stats": {"files": len(file_changes), "added": len(added_lines), "removed": len(removed_lines)},
}, indent=2))