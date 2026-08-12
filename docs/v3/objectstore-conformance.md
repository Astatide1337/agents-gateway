# Object-store conformance evidence

Status: provider-free local conformance slice, 2026-08-12

This is the evidence boundary for P0-F and the storage portions of ADR-016 and
ADR-027. It deliberately does not select a production provider or contain a
credential. The test fixture binds only to an ephemeral loopback TLS listener
and uses disposable in-process credentials so the real AWS SDK request path can
be exercised without an external service.

## What is proven locally

`v3/internal/objectstore/local_s3_conformance_test.go` runs the real AWS SDK
against a small S3-shaped fixture and wraps it in the production
`objectstore.Store`:

- conditional create has exactly one winner under concurrent writers;
- the winning bytes can be read exactly;
- a later conditional write returns an existing-object result and cannot
  overwrite the bytes;
- `artifacts.Writer.SaveResolvedSpec` stores a digest-addressed object and a
  new store/writer instance can load it after a simulated process restart;
- an object that is persisted before the response is dropped is reported by
  `Store.Put` as an error, never as success; a new store instance can reconcile
  the exact bytes afterward.

`v3/internal/objectstore/local_s3_release_gate_test.go` adds the smallest
integrated release-gate slice over that same fixture:

- the real Ed25519-signed Gate report is independently verified before the
  statement and bundle are persisted through `verificationattestation`; a
  tampered statement is rejected before any new object is written;
- a second lifecycle/store instance replays the same immutable attestation and
  proves both stored bytes' SHA-256 digests match their returned artifact refs;
- retention's real S3 adapter lists those objects, rejects a wrong ETag fence,
  deletes only the two target-run objects, and preserves a sentinel object from
  another run.

Run the bounded local gate with:

```text
cd v3
AGW_GOMAXPROCS=2 GOTOOLCHAIN=go1.26.5 ./scripts/objectstore-conformance.sh
```

It uses only an ephemeral loopback TLS fixture and in-process credentials; it
does not read cloud credentials, contact a provider, start a container, or
leave a service running. `--race` is available when the machine has enough
memory for the Go race build.

The focused package suite is also run under the race detector for:

```text
internal/objectstore
internal/artifacts
internal/artifactauth
internal/evidenceattestation
internal/cosignattestation
internal/verificationattestation
```

The existing unit suites additionally cover strict STS/static fallback
configuration, trusted Gate-report binding, canonical in-toto statement
construction, bounded cosign argv/output handling, tamper rejection, and
read-after-write attestation conflict handling.

## What remains external

This does not prove a production S3-compatible service. P0-F still requires an
operator-approved backend and a live, disposable bucket/IAM exercise covering
exact-prefix permissions, read-after-write, retention/restore, credentials or
STS expiry, and an outage/ambiguous transport drill. ADR-027 still requires a
self-managed or KMS trust root, independent verification, key rotation and
rollback evidence, and retained bundle recovery. Public Fulcio/keyless signing
is not assumed.

The Phase-0 probe remains the live-provider entry point. With no endpoint and
bucket supplied it must remain an explicit `SKIP`, not a simulated pass. Its
loopback fault fixture is separate from this Go SDK conformance test and does
not alter production configuration.
