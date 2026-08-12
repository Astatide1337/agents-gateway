# Agents Gateway v3 quality architecture

Status: accepted implementation plan

This document extends the Kubernetes-native v3 design with ADR-015 through
ADR-024. It is intentionally narrower than the complete research proposal: it
defines the quality features that belong in the first stable v3 release and the
features that require evidence from real shadow runs before implementation.

## Product boundary

Agents Gateway owns the outer loop around an unmodified coding harness. Codex
and Claude Code retain their own planning and tool-use loops. The platform owns
only:

1. the immutable material placed in a sandbox before the harness starts;
2. the tools and bounded observations exposed through the broker;
3. independent verification and selection after the harness exits; and
4. structured feedback used by an explicitly bounded repair attempt.

The acceptance decision, rather than the model, is the trust boundary. A
coherent patch may be `Rejected`; `Failed` remains reserved for a run that did
not produce a coherent result.

## Current API surface

The original five-CRD ceiling is superseded by the quality contract. The first
stable release has seven Agents Gateway CRDs:

- `AgentRun`
- `Agent`
- `Gate`
- `ToolSet`
- `ModelRoute`
- `Policy`
- `ContextStrategy`

The upstream `Sandbox` CRD remains adopted and does not count as an AGW-owned
API. `EvalSuite` is deliberately deferred; adding it after real labelled shadow
runs would make it an eighth AGW CRD.

## One contract, three consumers

A `Policy` is the single source for project quality rules. At admission the
operator resolves it into the immutable run snapshot and compiles it into three
surfaces:

- context in `AGENTS.md`, so the harness knows the rule;
- a deterministic self-check exposed through the broker, so the harness can
  check its work; and
- the same deterministic check executed independently by the verify sandbox.

Rules have only two severities. A blocking rule must include a deterministic
check. Advisory rules may influence context and tools but cannot reject a run.
Policy scripts are read from the pristine `baseSHA`, copied into a read-only
pack, individually digested, and forbidden as patch targets. The verify sandbox
uses that pinned copy rather than an agent-modified checkout.

## ContextPack

`ContextStrategy` configures a bounded, deterministic context builder. Its
first-release tiers are:

- lexical search and inventory using ripgrep;
- a bounded structural repository map; the default is file-oriented, while a
  tree-sitter parser remains an optional adapter seam;
- language-server symbol information through an explicitly reviewed local
  provider where supplied; no Serena runtime is bundled by default;
- bounded Git history for touched paths; and
- conventions compiled from `Policy`.

Semantic embeddings are disabled in this release. The builder enforces byte and
token budgets, writes standard artifacts, uploads a manifest, and records the
pack digest in `AgentRun.status`:

```text
/workspace/AGENTS.md
/workspace/.agents/skills/...
/workspace/.agents/policies/...
/workspace/.agw/context/manifest.json
```

The work sandbox init order is `clone -> skills -> context -> lockdown`. The
agent sees the compiled pack read-only. Standard files are the harness-facing
interface; AGW does not invent a second prompt or skill language.

## Broker quality controls

`ToolSet` exposes bounded profiles for `explore`, `edit`, and `verify`, with no
more than twelve tools per phase. The work broker begins in `explore`; a
one-way, host-authorized transition enters `edit`. The verify profile is used
only by controller-owned verification work. The broker primitive and its
fail-closed authorization seam are locally tested, but the current broker
process does not yet bind a production runtime supervisor to that seam. Until
that adapter exists, an agent remains in `explore`; the implementation must not
claim that edit-phase MCP tools are available.

The deployment-side audit is recorded in
[`trusted-phase-supervisor-contract.md`](trusted-phase-supervisor-contract.md).
The current work pod keeps its PID namespace private and receives no phase
credential. Helm rejects an attempted supervisor enablement, and the bounded
Argo lifecycle image has no supervisor mode. Sandbox `Ready`/`Finished` polling
is lifecycle observation, not independent harness-process observation.

Large results are stored in the workspace and returned as a bounded summary
plus reference. Compiler and test errors are normalized to structured
file/line/message records. Full raw output remains an artifact. The broker does
not claim to rewrite a harness's private context: observation masking is only
enabled when a runtime adapter can implement it truthfully.

## Hybrid Gate

The first stable Gate combines two signal families:

1. deterministic execution evidence, which includes all blocking checks; and
2. an execution-free critic using a model family different from the worker.

Blocking rules are hard gates regardless of score. Weights rank only candidates
that passed every blocking requirement. Each critic finding must reference
machine-checkable evidence from a reproduction, static analysis, symbol query,
or Policy result to remain blocking. Model assertions without corroboration are
retained as clearly labelled advisory findings.

The critic runs in a fresh controller-owned sandbox and receives no direct
provider credential. Its broker owns provider access. It consumes the immutable
diff and ContextPack, never the agent's writable workspace.

## Output modes

`AgentRun.spec.output.mode` supports `patch`, `findings`, and `both`. Findings
mode is another result shape, not another controller or CRD. Findings-only
requires a pre-existing target pull request. In `both` mode, the patch
publication creates the pull request and the findings publication targets the
exact bounded pull-request number returned by that effect, including on
ledger replay. The Gate corroborates every proposed blocking finding before
publication.

## Deliberately deferred

The following are not first-release requirements:

- candidate fan-out and test-generation rollouts;
- mutation testing;
- semantic retrieval or a custom code graph;
- `EvalSuite` automation;
- auto-merge; and
- unbounded repair loops.

After approximately fifty human-labelled shadow runs for one repository and
Gate, the labels become the seed for `EvalSuite`. Only measured failure modes
may justify the deferred features. False accepts are the primary risk metric;
false rejects are treated as cost.

## Implementation sequence

1. Extend API types, generated CRDs, admission resolution, and canonical
   snapshots with `Policy` and `ContextStrategy`.
2. Compile Policy, skills, repository structure, symbols, and bounded history
   into a digest-addressed ContextPack.
3. Add ToolSet phase profiles, result references, and structured errors without
   weakening exact-JSON argument enforcement or the effect ledger.
4. Add execution-plus-critic Gate scoring and evidence demotion.
5. Add findings output mode and publication rendering.
6. Exercise all paths in unit, race, chart, API-server dry-run, container, and
   disposable-cluster tests.
7. Validate the security assumptions on the real two-node k3s topology, then
   run Codex Luna Max in shadow mode.

No v1/v2 deletion, production deployment, or enforcing-mode promotion is part
of these implementation steps.
