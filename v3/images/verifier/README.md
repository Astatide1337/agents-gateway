# Agents Gateway v3 offline verifier

`agw-verifier` is the main container in the independent verification Sandbox.
It runs after the fetch, safe-apply, and `lockdown` init containers. Its output
is observations only; the operator's Gate engine owns the verdict.

The container reads these controller-provided values:

| Variable | Meaning |
| --- | --- |
| `AGW_WORKSPACE` | Canonical workspace root containing the three paths below |
| `AGW_REPO_PATH` | Applied repository checkout |
| `AGW_BASE_PATH` | Immutable pristine checkout at the recorded base SHA |
| `AGW_PATCH_PATH` | Exact captured patch artifact |
| `AGW_RUN_UID` | Run identity |
| `AGW_SPEC_DIGEST` | Resolved-spec digest (`sha256:`) |
| `AGW_BASE_SHA` | Exact base commit SHA |
| `AGW_VERIFY_COMMANDS_JSON` | Strict v1 envelope containing trusted Gate commands |
| `AGW_VERIFY_TIMEOUT` | Overall bounded verification timeout |

Optional controller inputs are:

| Variable | Meaning |
| --- | --- |
| `AGW_VERIFY_REQUIREMENTS_JSON` | Strict v1 envelope for `testStrength`, `coverageDelta`, and optional `baseCommands` |
| `AGW_PATCH_DIGEST` | Expected patch digest; the verifier always hashes the patch itself |
| `AGW_VERIFY_INPUT_DIGEST` | Validated identity digest carried for future input attestation |
| `AGW_VERIFY_MAX_OUTPUT_BYTES` | Tighten per-attempt combined stdout/stderr output, default 64 KiB, hard maximum 256 KiB |
| `AGW_VERIFY_MAX_PATCH_BYTES` | Tighten patch input, default 128 MiB, hard maximum 1 GiB |
| `AGW_VERIFY_MAX_COPY_BYTES` | Tighten pristine-base copy for test strength, default 1 GiB, hard maximum 4 GiB |

`AGW_VERIFY_COMMANDS_JSON` is compatible with the existing Sandbox builder:

```json
{"version":1,"commands":[{"argv":["go","test","./..."]}]}
```

The optional requirements envelope is:

```json
{
  "version": 1,
  "testStrength": "newTestsMustFailOnBase",
  "coverageDelta": ">= 0",
  "baseCommands": [{"argv": ["go", "test", "./..."]}]
}
```

When `newTestsMustFailOnBase` is enabled, the verifier creates a second
pristine tree, copies only changed test files into it, and runs the explicitly
configured `baseCommands`. Without `baseCommands`, it selects recognizable
test commands such as `go test`, `pytest`, `cargo test`, and `npm test`. At
least one selected command must fail on that base tree; a timeout, output
overflow, missing suite, symlink, special file, or copied-path failure is
failure evidence and never success.

Coverage is an explicit command-output protocol. A trusted Gate command may
emit exactly one line of the form:

```text
AGW_COVERAGE_DELTA_V1 0.42
```

The verifier records the normalized decimal. If a Gate config requires a
coverage delta and no valid marker is observed, the evidence is incomplete.

## Trust boundary

Gate commands are policy authored by the trusted cluster owner. They may use
shell syntax only when the Gate explicitly uses `shell`; the verifier invokes
that text through the fixed `/bin/sh -c` entry point, with a fixed `PATH`,
`HOME=/nonexistent`, `GIT_*` protections, blank proxy variables, no inherited
credentials, no stdin, a bounded repository working directory, and bounded
stdout/stderr capture. Repository files and test code are not Gate policy.

This is not a substitute for the Sandbox airlock. The pod must have completed
the lockdown init container and run with `hostUsers: false`, no service-account
token, a read-only root filesystem, dropped capabilities, and an enforced
zero-egress policy. The verifier performs no network calls itself. If the
airlock or user namespace preflight is not proven, the operator must reject the
run before this image starts.

Stdout is reserved for exactly one line:

```text
AGW_VERIFY_EVIDENCE_V1 <unpadded-base64(strict-json-object)>\n
```

Child output is never forwarded. The evidence binds `runUID`, `specDigest`,
`baseSHA`, and the hash of the exact patch bytes, and contains sorted changed
paths, counts, binary detection, bounded command observations, coverage when
configured, and test-strength evidence. There is deliberately no `verdict`
field and no interpretation of the verifier process exit as acceptance.

## Build

Run from the repository root (the build context must contain `v3/`):

```sh
./v3/images/verifier/build.sh
```

Production manifests must replace the tag with a content-addressed image
digest after the image has passed the Phase-0 and container acceptance tests.
