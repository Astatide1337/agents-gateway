# ADR-027 attestation trust-root contract

This document is the deployment contract for the optional verification
attestation path. It describes what the current code enforces and what still
requires a real target-cluster proof.

## Current boundary

`verificationAttestation.enabled` is `false` by default. When it is enabled,
the operator must be configured with:

- a digest-pinned cosign executable;
- an explicit `keyRef`, which is either a cosign provider/KMS URI or an
  absolute externally managed file path; and
- a verification reference. KMS-backed signing may use the same URI. A
  file-backed signing key must provide a separate absolute public-key path.

Private key bytes are not accepted in Helm values, command-line values, or the
attestation adapter. Relative paths, `env://`, and HTTP(S) key references are
rejected. The chart does not create or mount an external key path.

The standalone command exposes the same split with `--key-ref` and the
optional `--verify-key-ref`. The latter defaults to the signing reference only
when the caller intentionally omits it.

Public Fulcio/keyless issuance and ambient transparency configuration are not
implicit parts of this deployment. The current adapter uses an explicitly
selected key and disables transparency-log upload/lookup for the detached
bundle path. That is a deliberate self-managed/KMS boundary, not proof of a
public Sigstore identity.

## Persistence and retry contract

The attestation lifecycle authenticates the existing Gate report before invoking
cosign. It then creates the statement and bundle under content-addressed,
immutable object keys. Every write is followed by a bounded read-back and exact
byte comparison, including a replay where the provider reports that the object
already exists. A missing, conflicting, or ambiguous read fails closed. A
retry may therefore re-run the lifecycle without overwriting or silently
accepting a different artifact. The provider-free SDK/store portion of this
contract is exercised by
`v3/internal/objectstore/local_s3_conformance_test.go`; this does not replace a
live production-provider exercise.

This check is local provider-contract evidence. It does not make an eventually
consistent or incorrectly configured object store safe by itself; the
production store must still provide conditional create, bounded reads, and a
durable retention policy.

## Production choices

The supported production choices are:

1. a cosign-supported KMS/provider key whose URI identifies the signing and
   verification key, or
2. an externally managed key file plus a separately supplied public-key file,
   both mounted read-only through an independently reviewed mechanism.

KMS is the preferred custody model. The repository does not implement a KMS
client, Fulcio, Rekor, TUF root distribution, key rotation controller, or
secret-mount mechanism. Those remain deployment responsibilities and must be
proven before enabling attestation in a target cluster.

Rotation is not automatic. A production rotation procedure must record the new
key identity, verify new bundles with the new reference, retain the old
verification reference for the agreed rollback window, and independently
verify retained bundles before retiring the old key. No old report format or
trust root should be deleted based only on local tests.

## What local tests prove

The focused package tests prove explicit-key validation, separate verification
key wiring, Gate-report binding, bounded cosign invocation, fail-closed command
handling, immutable replay behavior, and read-after-write mismatch handling.
Helm tests prove the opt-in default, URI/absolute-path contract, and rejection
of file-backed signing without a separate verification path.

They do not prove a real KMS identity or policy, a real object store, key
rotation/rollback, production secret mounting, or an independent verifier on
k3s. Those are the remaining production-only gates.
