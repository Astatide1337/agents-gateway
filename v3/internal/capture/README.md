# Capture phase contract

`capture` is the short phase between the work Sandbox and independent
verification. It is intentionally a package-level boundary rather than a
controller implementation: callers provide the resolved snapshot, the
recorded `baseSHA`, the work PVC name, and digest-pinned capture image.

## Workload shape

`Build` returns:

- one `batch/v1 Job` with one container and one PVC mount (`/workspace`);
- one owner-bound, explicit deny-all `NetworkPolicy` for that pod selector;
- an output contract containing the bounded stdout-frame limit.

The pod has no init containers, sidecars, Secret volumes, Secret references,
service-account name, service-account token, image-pull Secret, or permitted
ingress/egress. `DNSNone`, `hostNetwork: false`, `hostUsers: false`,
RuntimeDefault seccomp, UID/GID 1000, read-only rootfs, no privilege escalation,
and dropped capabilities are all explicit. The controller should create the
NetworkPolicy before creating the Job and should refuse to use this component
on a cluster whose CNI does not enforce NetworkPolicy.

The work PVC is the actual claim created for the work Sandbox. Capture does not
create a second PVC, use a volume claim template, or mount the verify
workspace. `Job.Spec.TTLSecondsAfterFinished` is intentionally unset: the
controller must retrieve and persist evidence before deleting the Job and its
NetworkPolicy.

## Capture image responsibilities

The trusted image referenced by `Options.Image` must implement
`/agw/capture`. It receives the complete, bounded `AGW_CAPTURE_SPEC_JSON`
contract and must:

1. verify that `AGW_CAPTURE_REPO_PATH` is the expected worktree and that the
   recorded `AGW_CAPTURE_BASE_SHA` resolves to a commit;
2. build a temporary Git index from `baseSHA`, add the worktree (including
   untracked files) to that temporary index, and compute a binary-safe patch
   using the equivalent of:

   ```text
   git diff --cached --binary --full-index --no-ext-diff --no-textconv --no-renames <baseSHA> -- .
   ```

   The real index must not be changed. The temporary index must not include
   `.git` internals. The patch is the exact bytes later hashed and uploaded;
   no provider or object-store credential is available to this image.
3. derive sorted, unique changed paths from the same temporary index, count
   added/deleted content lines (not `---`/`+++` headers), and reject binary,
   symlink, submodule, or special-file changes;
4. read final regular-file bytes back from the temporary Git object store and
   produce the canonical `publish.MarshalPatchManifest` representation. Its
   entries are sorted and contain path, mode (`100644` or `100755`), deletion
   state, and final content; deletions have nil content;
5. write bounded regular staging files at `PatchPath`, `ManifestPath`, and
   `ResultPath` using no-follow file creation, then emit
   `EncodeOutputFrame(resultJSON, patch, manifest)` to stdout and exit zero
   only after the complete frame is written.

The controller independently parses the patch and compares its paths, line
count, binary marker, byte count, and digest to the envelope. A false or
truncated envelope therefore cannot become accepted evidence.

## Controller integration

The narrow retrieval path is `OutputReader.ReadCaptureOutput`. Its concrete
Kubernetes adapter should call the pod-log subresource for the fixed `capture`
container with `timestamps=false` and `follow=false`, stream no more than the
`maxBytes` argument, and return the raw bytes. It must not accept a command,
path, or container name from the AgentRun. The frame is:

```text
stdout-frame-v2\n
<raw-base64 payload>\n
```

`DecodeOutputFrame` authenticates independent SHA-256 values for result JSON,
patch, and manifest and enforces all three payload limits. `ReadAndValidate`
then binds the result to the resolved spec digest and recorded base SHA,
independently analyzes the patch, strictly calls `publish.DecodePatchManifest`,
proves manifest paths/count match patch evidence, and enforces scope, binary,
file-count, line-count, patch-byte, and manifest-byte rules.

Only after successful validation should the controller call:

```go
refs, err := capture.Persist(ctx, artifactStore, validated)
```

`ArtifactSink` is the controller-side immutable `Put`/`Get` seam. `Persist`
writes both `runs/<runUID>/patches/<specDigest>/<patchDigest>.diff` and the
digest-addressed `.manifest.json`, uses create-if-absent semantics, compares
existing bytes on an idempotent retry, and rejects an unsafe URI or conflicting
object. The capture pod never sees this sink or its credentials.

The normal reconcile sequence is:

```text
create deny policy -> create Job -> wait for pod completion
    -> ReadAndValidate -> Persist patch + manifest -> project PatchSummary
    -> delete Job/policy after the artifact is durable
```

A completed Job with no retrievable frame is a capture failure. A non-zero Job
exit is never interpreted as an empty patch or a successful capture.
