# Publication controller contract

`Driver` is the non-Kubernetes boundary for the AgentRun `Publishing` phase.
It binds capture and verification artifacts to the immutable run identity, then
delegates the one external effect to `publish.Publisher`. The package never
reads a Secret, mints a token, calls Kubernetes, or exposes a merge operation.

## Construction

The operator wires the package once:

1. Construct the existing `objectstore.Store`, then wrap it with
   `NewStoreReader(store, bucket, prefix)`. `prefix` must be the same adapter
   prefix used by the store; it is removed before `store.Get`.
2. Construct `githubapp.Minter`, `githubpublish.Client`, and
   `effects.Ledger` in the operator. Pass the client and ledger to
   `publish.New`.
3. Pass the resulting publisher, the `StoreReader`, and the trusted Gate
   Ed25519 public key to `publishcontroller.New`.

The GitHub App private key and object-store credentials remain exclusively in
those operator-owned constructors. They never enter `Input`, `publish.Request`,
or a status projection.

## Controller integration

When an `AgentRun` reaches `Publishing`, construct one `Input` from the
immutable resolved snapshot and status:

| `Input` field | Source of truth |
| --- | --- |
| `RunUID`, `RunName` | `resolved.Snapshot.Run.UID`, `.Name` |
| `Repo`, `BaseRef`, `BaseSHA`, `SpecDigest` | snapshot source/repository, source base ref, source-pinned snapshot SHA, and resolved-spec digest |
| `GateUID`, `GateGeneration`, `GateMode` | resolved Gate object identity and `snapshot.Gate.Mode` |
| `Patch` | `AgentRun.status.patch.ref` |
| `Manifest` | the `AgentRun.status.artifacts` entry with kind `patch-manifest` and name `patch-manifest.json` for `patch`/`both`; findings-only does not publish the patch and may omit it |
| `GateReport` | `AgentRun.status.gate.reportRef` |
| `PublishMode`, `Title`, `Labels` | resolved `AgentRun.spec.publish` |

For `patch` and `both`, the manifest must be present even though the status patch
summary points at the diff. Do not synthesize it from the diff. Its URI must end
in the immutable capture key:

```text
runs/<runUID>/patches/<specDigest-hex>/<manifestDigest-hex>.manifest.json
```

The patch URI must end in the corresponding `.diff` key. The signed report URI
must end in:

```text
runs/<runUID>/verification/<specDigest-hex>/<patchDigest-hex>/<reportDigest-hex>.json
```

An object-store adapter prefix may precede these paths. The driver accepts only
canonical `s3://bucket/key` locations; HTTPS, presigned URLs, query strings,
userinfo, traversal, and alternate key identities are rejected.

Call:

```go
result, err := publicationDriver.Publish(ctx, input)
```

The driver loads each bounded object, checks the exact digest and size, parses
the canonical manifest, verifies the signed report with the configured trusted
public key, and binds `runUID`, `specDigest`, `baseSHA`, Gate identity, patch
digest, and changed paths before invoking the publisher.

## Result mapping

`result.Effect` is patch-owned and safe to copy directly into
`AgentRun.status.effect`. `result.PullRequestNumber` and
`result.PullRequestURL` are bounded together: for `OutputBoth`, the findings
publisher must use the number returned by the patch result, including after a
ledger replay. The Kubernetes adapter discards any pre-existing target when it
builds an `OutputBoth` input, so an unrelated target cannot be selected by
accident. Findings-only preserves its explicitly supplied pre-existing target
PR and has no patch effect; `status.published` and the patch `Published`
condition remain false while the separate findings condition records review
publication.

| Driver result | Controller action |
| --- | --- |
| `StatePending` | persist `effect.state=pending` and requeue according to the wrapper's normal pending policy |
| `StateSucceeded` | persist the patch effect when one exists; project findings separately; move to `Succeeded` |
| `StateRejected` | terminal `Rejected`; no publisher call occurred for enforcing Gates |
| `StateFailed` | terminal deterministic publication failure |
| `StateUnknown` / `ErrUnknownEffect` | terminal `UnknownEffect`; preserve any proven patch effect, do not retry automatically, and require reconciliation |
| `StateSkipped` | no effect projection; complete without a PR when publish mode is `none` |

The real `publish.Publisher` is synchronous and normally returns succeeded,
failed, rejected, or unknown. `StatePending` exists for a controller wrapper
that durably observes an in-progress external operation; it is represented by
`ErrPending` and must not be confused with an ambiguous GitHub result.

The driver never merges a branch. Pull-request creation is the terminal GitHub
operation represented by the injected publisher contract.
