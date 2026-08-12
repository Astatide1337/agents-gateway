# Agents Gateway broker image

`agw-broker` is the per-run, loopback-only sidecar for v3 work Sandboxes. It
is a policy boundary, not a public API and not a completion authority.

The image exposes only these exact in-pod routes:

| Route | Purpose |
| --- | --- |
| `POST /v1/responses` | allowlisted model proxy used below the Codex `/v1` base URL |
| `POST /mcp` | compiled local MCP ToolSet |
| `PUT /v1/artifacts/output` | bounded run-output upload |
| `POST /v1/artifacts/create` | bounded authored-artifact upload |
| `POST /v1/runtime/events` | strict `agw.runtime.v1` event stream |

The listener must be `127.0.0.1:<port>`. The router does not register
`/v1/runtime/process-exit`, even though the shared internal runtime handler
retains a constructor invariant for that legacy path. The broker creates a
throwaway server-side value to satisfy that constructor; it is never placed in
the agent environment, projected into a file, sent over HTTP, or logged.

The runtime event stream is not enough to complete a run. The operator observes
the Kubernetes agent-container termination and independently calls
`runtimeevents.Repository.FinalizeObservedExit` against the same object store.
This preserves the distinction between “the harness said completed” and “the
process actually exited successfully.”

## Required configuration

All values are bounded and parsed fail-closed. Credentials are never accepted
as raw environment values.

| Variable | Meaning |
| --- | --- |
| `AGW_RUN_UID` | exact server-bound run UID |
| `AGW_SPEC_DIGEST` | exact `sha256:` resolved-spec digest |
| `AGW_BASE_SHA` | exact lowercase Git object ID |
| `AGW_TOOLSET_JSON` | strict bounded `ToolSetSpec` JSON |
| `AGW_MODEL_ROUTE_JSON` | strict bounded `ModelRouteSpec` JSON |
| `AGW_BROKER_SECRET_FILES` | strict array of `{key,path}` projected logical-credential files |
| `AGW_OBJECT_STORE_BUCKET` | S3-compatible bucket |
| `AGW_OBJECT_STORE_REGION` | explicit signing region |
| `AGW_OBJECT_STORE_PREFIX` | relative object prefix |
| `AGW_OBJECT_STORE_ACCESS_KEY_FILE` | direct regular file containing access key |
| `AGW_OBJECT_STORE_SECRET_KEY_FILE` | direct regular file containing secret key |
| `AGW_OBJECT_STORE_SESSION_TOKEN_FILE` | optional direct regular session-token file |

Optional configuration includes `AGW_LISTEN_ADDRESS` (default
`127.0.0.1:8081`), `AGW_CREDENTIALS_DIR` (default
`/run/agw/credentials`), `AGW_OBJECT_STORE_ENDPOINT`,
`AGW_OBJECT_STORE_PATH_STYLE`, `AGW_OBJECT_STORE_MAX_BYTES`,
`AGW_EFFECTS_PREFIX`, `AGW_REQUIRE_APPROVAL_FOR_MUTATIONS`, and either
`AGW_PRICING_JSON` or `AGW_PRICING_FILE`.

`AGW_OBJECT_STORE_MAX_BYTES` defaults to 64 MiB and accepts values from 1 byte
through `objectstore.GeneralMaxObjectBytes`, the shared 64 MiB ceiling enforced
by the adapter, workload, broker, and lifecycle producer. The verifier's
patch-fetch and pristine-copy/archive limits are separate contracts; the
patch-fetch path may use its explicitly documented 1 GiB ceiling, but that
larger value is never accepted for general broker or lifecycle objects.

For a nonzero model cost budget, pricing is mandatory and must have one exact
entry per configured provider:

```json
{
  "openrouter": {
    "inputMicrosPerToken": 0,
    "outputMicrosPerToken": 0
  }
}
```

The broker rejects AWS default credential-chain variables and does not consult
metadata, profile, web-identity, or raw `AWS_*` credential inputs. The current
object-store package supports an explicit S3 client, so the executable uses
only the three fixed credential files above. If those files are not projected,
startup fails with `object_store_*_file_unreadable`; it does not fall back to
broad environment leakage.

Logical broker files must be direct regular files. Symlinks, special files,
size changes, non-printable bytes, and oversized values are rejected. This is
intentional: Kubernetes Secret atomic-writer item paths are symlinks on common
clusters. The v3 workload builder projects each selected key through a
read-only `subPath` bind mount over a regular `0400` image placeholder, so the
broker preserves this check without receiving an enumerable Secret directory.

## Build and release

The build uses the repository’s latest dependency policy for the build inputs:

```bash
v3/images/broker/validate.sh
IMAGE=ghcr.io/astatide/agw-broker:build-<revision> \
  CONTAINER_ENGINE=docker \
  v3/images/broker/build.sh
```

Resolve the pushed image’s registry manifest digest and place only the
`@sha256:...` reference in the resolved Agent snapshot. Do not deploy the local
tag or a floating `latest` tag.

The image contains no Kubernetes client, no public health/debug endpoint, no
provider credential, and no process-exit credential. Diagnostics are static
redacted configuration codes on stderr; request errors are the fixed JSON
`{"error":"request rejected"}`.
