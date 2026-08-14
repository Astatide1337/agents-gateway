# v3 Codex adapter port

This directory is the v3 port of `v2/pkg/codexadapter`. The adapter remains a
small process boundary: it reads one `run.start` frame, launches Codex with a
private home, translates Codex JSONL into strict `agw.runtime.v1` events, and
emits `run.completed` only after all required artifact uploads have succeeded.

## Deliberate v2-to-v3 differences

These are limited to dependencies that do not exist in the standalone v3
module:

1. `v2/pkg/brokerbridge` and its Unix socket client were not copied. A v3 work
   Sandbox already has `agw-broker` as a loopback sidecar at
   `http://127.0.0.1:8081`. `AGW_BROKER` is used to derive `/v1`, `/mcp`,
   `/v1/artifacts/output`, and `/v1/artifacts/create`. The adapter sends no
   bearer credential; the broker owns credential injection and provider
   egress. All derived or explicit URLs still have to be literal loopback
   HTTP(S) endpoints with no URL credentials, query, or fragment.
2. `v2/pkg/workflow.ArtifactRef` was replaced by the current
   `v3/pkg/artifactcatalog.ArtifactRef`. The JSON shape is preserved, and the
   v3 catalog validation additionally rejects unsafe URI schemes and
   credential-bearing references.
3. `v2/pkg/artifact` was not copied. Its bounded descriptor envelope is kept
   locally as the same `X-AGW-Artifact-Metadata` raw-URL base64 JSON contract,
   while content-kind, renderer, capability, and media-type validation is
   delegated to `v3/pkg/artifactcatalog`.
4. The v2 container test depended on v2's model broker, run broker, MCP broker,
   and local artifact store. Those server implementations are not in v3 yet.
   It was replaced with a network-disabled runtime-image test using a mounted
   fake Codex executable. It verifies the adapter process boundary, private
   `CODEX_HOME`, non-inheritance of `OPENAI_API_KEY`, and strict completion.
   The unit tests still exercise the loopback artifact HTTP contract and
   authored-artifact ordering. A full broker-backed image E2E belongs with the
   v3 broker image acceptance suite.
5. The v3 command and runtime image now live at
   `v3/cmd/agw-runtime-codex/` and `v3/images/runtime-codex/`; this package
   document describes their contract but does not duplicate their build files.

## Runtime integration

`cmd/agw-runtime-codex` is the production wrapper. It creates `run.start`
from the immutable workload contract and synchronously POSTs each event to
the broker's loopback `/v1/runtime/events` endpoint. Kubernetes does not wire
stdout between sidecars, so stdin/stdout are retained only as the internal
adapter interface, not as the Pod transport.

The v3 work Sandbox should provide these values through the runtime image or
Pod environment:

```text
AGW_BROKER=http://127.0.0.1:8081
AGW_CODEX_WORKSPACE=/workspace/repo
AGW_CODEX_MODEL=<resolved model id>
AGW_CODEX_SANDBOX=workspace-write
```

The `internal/workload` builder supplies the broker address, selected model,
run identity, task, instructions, base SHA, and resolved-spec digest. The
adapter intentionally cannot read the broker container's
`AGW_MODEL_ROUTE_JSON` or infer a model from an endpoint.

`AGW_BROKER` derives the four loopback routes. Explicit
`AGW_CODEX_RESPONSES_URL`, `AGW_CODEX_MCP_URL`, `AGW_CODEX_ARTIFACT_URL`, and
`AGW_CODEX_ARTIFACT_CREATE_URL` may be used for tests or an equivalent broker,
but each must pass the same loopback/path validation. `AGW_CODEX_ENABLE_TOOLS`
and `AGW_CODEX_REQUIRE_ARTIFACT` are optional boolean overrides; when derived
from `AGW_BROKER`, tools and output artifacts are required by default.
`AGW_CODEX_SANDBOX` accepts `read-only`, `workspace-write`, or
`danger-full-access`. The Kubernetes work workload sets the last value because
the pod's hardened filesystem, UID egress airlock, dropped capabilities, and
disabled service-account token are the outer sandbox; nested user namespaces are
not available in that pod. Standalone runtimes should keep `workspace-write`.

The broker must expose:

- `POST /v1/responses` for the model proxy;
- `POST /mcp` for the selected ToolSet only;
- `PUT /v1/artifacts/output` for the immutable run output;
- `POST /v1/artifacts/create` for authored artifacts, validating the metadata
  header before storing bytes.

The adapter never reads `OPENAI_API_KEY`, `CODEX_HOME`, a host Codex auth file,
or any other provider credential. Its child environment is deliberately
allowlisted. The Pod still needs the Kubernetes-level `hostUsers: false`,
read-only-rootfs, seccomp, capability-drop, and UID/iptables airlock supplied
by `internal/workload`.

## Runtime image build

Build with `v3/images/runtime-codex/build.sh`. Its `Containerfile` pins the
reviewed base-image manifests, accepts an explicit Codex CLI version (the
wrapper resolves the current registry version only when one is not supplied),
and verifies that the installed package matches that version. Publish
the resulting image with an immutable registry digest, then put that digest in
`Agent.spec.runtime.image`; do not use a mutable tag in an `AgentRun` resolved
snapshot.

## Verification commands

From `v3/`:

```bash
go test ./pkg/codexadapter
go test -race ./pkg/codexadapter
go vet ./pkg/codexadapter
```

The Codex and container tests are opt-in:

```bash
AGW_CODEX_LIVE=1 go test ./pkg/codexadapter -run TestCodexCLILiveResponses -v
AGW_CODEX_CONTAINER_LIVE=1 \
  AGW_CODEX_RUNTIME_IMAGE=ghcr.io/astatide/agw-runtime-codex@sha256:<digest> \
  go test ./pkg/codexadapter -run TestCodexRuntimeContainerEndToEnd -v
```

The live Codex test uses a local fake Responses SSE server and no provider
credential. The container test uses `--network=none`; it is not a substitute
for the future two-container Kubernetes test that proves the real broker
sidecar path.
