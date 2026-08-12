# v3 shadow-mode measurability audit

Status: implemented locally; live cluster and object-store evidence are still
release gates.

This audit is intentionally separate from `completion-audit.md`. It covers
only §10 (shadow mode) and the Phase 5 measurement requirement in the
Kubernetes-native implementation plan. Retention, ADR-009, EvalSuite, and
automatic enforcing/auto-merge are out of scope.

## Finding before this change

The machine side was already measurable: the independent Gate report is
canonical, signed by the operator, and binds `runUID`, `specDigest`,
`baseSHA`, `patchDigest`, runtime/helper/skill digests, and the Gate UID and
generation. The AgentRun status exposed the machine verdict and report ref.

The owner side was missing. There was no append-only place to record
`diff-good`/`diff-bad`, no collision rule for a second opinion, and no
per-repository/per-Gate confusion matrix. PR labels were therefore not enough:
labels are mutable and do not bind a review to the exact run evidence.

## Implemented mechanism

`kubectl agw review RUN --diff-good|--diff-bad` now:

1. asks the Kubernetes API server for `kubectl auth whoami --output json`;
2. reads the terminal `AgentRun` and requires a shadow Gate result;
3. copies the controller-projected Gate name/UID/generation/mode/verdict and
   the run UID, spec digest, base SHA, patch digest, and report digest into a
   canonical review payload;
4. creates one deterministic `ConfigMap` slot for that run with
   `immutable: true` and `data["review.json"]` containing the canonical bytes;
5. treats an existing slot as success only when its bytes are identical, and
   rejects a different payload without attempting an update.

The payload is deliberately **unsigned**. Its provenance is the fixed value
`kubernetes-authenticated-subject`; the reviewer is the API server's reported
username, UID, and groups. The operator never signs a human assertion. This
records an authenticated Kubernetes subject, not proof that a particular
human was sitting at the keyboard. A stronger human identity or signature
scheme remains an explicit deployment decision.

`kubectl agw matrix` lists all `AgentRun` and ConfigMap objects in the selected
runs namespace and parses only canonical `review.json` payloads. It does not
trust review labels or annotations. It also treats every terminal shadow
`AgentRun` with a complete machine verdict as a required case: a missing label
is an excluded review and makes the command fail non-zero. This prevents a
partially labelled sample from looking like a complete confusion matrix. Each
present candidate must:

- have the deterministic name and `immutable: true`;
- correspond to exactly one live AgentRun UID;
- match the AgentRun's namespace/name/repository/Gate ref;
- match status `specDigest`, `baseSHA`, patch digest, report digest, Gate
  UID/generation/mode, and machine verdict;
- be terminal and shadow-mode.

Any malformed, duplicate, missing, or mismatched review is reported as
excluded and makes the command fail non-zero. A valid row contains the §10
cells:

| Machine verdict | Owner assessment | Cell |
| --- | --- | --- |
| Accepted | diff-good | true accept |
| Accepted | diff-bad | **false accept** |
| Rejected | diff-good | false reject |
| Rejected | diff-bad | true reject |

Rows are grouped by repository plus Gate ref, Gate UID, and Gate generation;
Gate revisions are never mixed. The JSON output includes `falseAccepts`
explicitly.

## ADR-024 compatibility

The review payload is append-only, canonical, and self-contained. It is
future-importable as a golden-set input for the proposed EvalSuite work: the
repository, exact Gate revision, machine verdict, owner classification, and
all evidence digests are present in each record. No EvalSuite CRD or other
new CRD is added before the 50-run threshold. Existing labels remain discovery
hints only and are not part of the trust decision.

## Remaining honest gaps

- The current mechanism stores the small review record in Kubernetes etcd as
  an immutable ConfigMap. There is no CLI credential path for writing directly
  to the production object store, and the operator currently has no review
  ingestion endpoint. Adding an operator mirror would require a separate
  authenticated design and RBAC decision; it is not faked here.
- There is not yet an offline case-bundle import/export command. `matrix
  --output json` is a derived confusion-matrix report, not a portable golden
  set. The canonical `review.json` payloads are individually self-contained
  and remain suitable as future EvalSuite inputs; adding a versioned bundle
  format is intentionally deferred rather than introducing a second source of
  truth before the labelled shadow window exists.
- ConfigMap evidence is not cryptographically signed. Kubernetes API
  authorization and immutable data prevent normal overwrite, while the
  canonical binding detects tampering or mismatched status. A human-signature
  design must specify the identity provider, key custody, and verification
  policy before it can be added.
- Matrix counts are evidence for shadow learning, not an enforcement switch.
  The CLI does not modify Gate mode, merge PRs, or infer auto-merge eligibility.
- The approximately 50-run zero-false-accept threshold still requires real
  shadow runs and human review. Tests prove the contract and rejection paths,
  not that the threshold has been earned.

## Local proof

The focused tests cover canonical round-trip and mutation rejection,
deterministic naming, idempotent review creation, conflicting review refusal,
explicit false-accept counting, matrix rejection when a payload's patch
binding is tampered with, and matrix rejection when a terminal shadow run has
no review record. The API-server smoke must also be rerun after CRD
regeneration; no live k3s or production object-store claim is made here.
