# Agents Gateway v2 acceptance matrix

Last updated: 2026-08-09

This matrix turns the target architecture's release criteria into explicit,
repeatable evidence. `Pass` means the implemented slice has a current automated
or live test. `Partial` means an implementation path or enforcement primitive
exists but a required live or profile-specific test is still outstanding.
`Blocked` means the platform must not make the claim.

| Acceptance criterion | Status | Current evidence | Remaining release gate |
| --- | --- | --- | --- |
| Standalone configuration exists without Cloudflare, Kubernetes, Temporal, S3, or OIDC | Pass | Standalone Compose profile, local auth, PostgreSQL local engine, local artifacts, rootless-Podman runner config, and installer are present | Repeat on a clean host before a general release |
| Standalone installer is idempotent and preserves secrets | Pass | Installer validates managed files, refuses accidental replacement, supports explicit config/binary update modes, creates backups, never rewrites the standalone secrets file, and passed update/restart/E2E on the target host | Repeat on a clean host before a general release |
| Standalone cross-sandbox file/process/socket/credential isolation | Pass | Live rootless-Podman E2E covers two concurrent sandboxes, host file/socket/credential absence, non-root execution, dropped capabilities, read-only root, and cleanup | Repeat on a clean host before a general release |
| Direct internet, DNS, metadata, and direct-IP egress denied | Pass | Live rootless-Podman E2E verifies network, DNS, metadata, direct-IP, and proxy-bypass denial; brokered routes remain explicit | Keep the adversarial egress suite in release CI |
| Unauthorized tools/resources denied and audited | Pass | Connected MCP test verifies exact catalog authorization, a read, one approved write, and duplicate denial; durable audit sink tests and adapter assertions verify persisted, privacy-preserving outcomes | Include the connected test and durable-audit checks in release CI |
| Pinned model/tool/skill revisions are checked before execution | Partial | Immutable resource resolution, digest checks, exact MCP catalogs, Skills Gateway canonical digests, read-only skill staging, and broker policy digests are implemented | Add a full local-engine run that exercises every selected capability and records revisions in run history |
| Environment references are safely materialized | Pass | Opaque `secret://` references resolve only on the trusted runner; private `0600` source files are materialized into a per-run `0600` env file below `/run`, then removed on success, failure, or cancellation; installer creates the private roots | Keep source-file and materialization cleanup tests in release CI |
| Failure recovery has accurate terminal/unknown states | Pass | Runner, broker, artifact, local-engine, and disposable PostgreSQL fault-injection tests cover crash recovery, single-claim delivery, and immutable unknown outcomes | Keep fault-injection jobs in release CI |
| External writes are not duplicated after ambiguous failure | Pass | Connected MCP write test exercises the effect ledger and duplicate denial; fault tests cover ambiguous outcomes and artifact compensation | Keep provider-specific reconciliation workflows separate |
| Optional hosted profile: cross-tenant API, SSE, and PostgreSQL access denied | Pass | Authorization/API tests and disposable PostgreSQL forced-RLS test | Keep as a hosted-profile CI job |
| Optional hosted profile: object storage and Temporal isolation | Partial | Tenant-scoped artifact interfaces and Temporal metadata boundaries exist | Wire both paths and run adversarial cross-tenant integration tests |
| Model credentials stay outside the sandbox | Pass | Responses proxy resolves credentials at the host boundary; broker client files contain scoped session data but not provider secrets; Docker E2E verifies upstream injection and output redaction | Add equivalent evidence for every future provider adapter |
| Codex adapter reaches a scoped Responses broker and creates immutable output | Pass | Docker container E2E exercises the adapter, loopback broker bridge, upload, and terminal output with a fake local Responses provider; the separate live rootless-Podman E2E proves runner isolation, secret handling, quota, and cleanup | Complete the combined real-model chain before claiming provider-backed agent execution |
| Local artifact storage is immutable and bounded | Pass | Local object client, upload handler, content API, and Docker E2E cover tenant/run scope, traversal, symlinks, no-overwrite commits, size, digest, media type, and full digest verification before response | Keep local and S3-compatible backends in the release regression suite |
| Claude-style artifact creation and viewing is safe by default | Pass | Strict sandbox descriptors, private authored-artifact broker, PostgreSQL catalog, authenticated API, and React workspace cover creation, immutable versions, source, safe Markdown/HTML preview, and download; latest-Chrome desktop/mobile E2E verifies sandbox/CSP, accessibility, and browser errors | Edit/remix mutation and public sharing remain separate future features |
| Owner-bound model subscriptions and explicit fallback enforced | Partial | Model route/broker policies and subscription-boundary tests exist; API-provider credentials are explicit | Implement any subscription adapter without pooling or credential extraction |
| Optional OTLP profile correlates workflow, agent, model, tool, approval, sandbox, verification, and artifact events | Blocked | OTLP providers and HTTP propagation are configured | Add semantic spans/metrics to every named boundary and assert one trace |
| Conductor can stop without affecting v2 | Pass | V2 Go module and deployment have no Conductor runtime dependency | Keep dependency check in release review |
| Public cancellation, approval, and immutable reply controls | Partial | Authenticated API/CLI wiring, durable claims, and Temporal signal tests exist | Add concurrent live API/Temporal fault-injection tests |
| Optional remote profile: reassignment cannot accept stale completion | Blocked | Single-runner owner-bound local lease only | Authoritative control-plane lease store and cross-runner fencing tests |
| Requested workspace and compute limits are enforced | Pass | Rootless-Podman provisions a quota-sized workspace tmpfs; the dedicated-service live E2E proves excess writes fail and reads exact kernel CPU, memory, and PID cgroup values before cleanup | Keep quota and cgroup assertions in release CI |
| Explicit OpenAI/OpenRouter Responses providers are supported | Pass | The local broker factory accepts only explicit OpenAI or OpenRouter Responses provider kinds, uses their respective default endpoints, and resolves credentials at the host boundary | Complete a combined run against a real provider |
| Optional team profile: concurrency, spend, tool-call, and retention quotas | Blocked | Declarative quota fields and validation exist | Add admission/accounting enforcement and cross-tenant starvation tests |

## Current release interpretation

The owner-operated implementation now includes the local durable engine,
standalone installer, rootless-Podman runner, enforced tmpfs workspace quota,
opaque secret materialization, brokered model/tool/skill/artifact path, Codex
adapter, durable MCP audit, and local artifact storage. Live rootless-Podman,
Skills Gateway, and connected MCP tests are verified. The console is
live-by-default and accepts an owner token through an in-memory runtime
provider. The Coolify Git-backed Compose profile is documented but has not
yet been deployed or verified on the live Coolify instance.

The combined local-engine chain using a real OpenAI or OpenRouter provider,
Skills Gateway, MCP read/write, artifact authoring, and audited durable state
is still pending. That is the remaining critical end-to-end evidence gate;
the current provider and component tests do not imply that combined claim.

Temporal, OIDC, S3, OTLP backends, stronger isolation, and hosted
multi-tenancy are optional profiles and must not be treated as standalone
requirements. Optional capabilities fail closed when selected but
misconfigured.

The authoritative current feature ledger is
[implementation-status.md](implementation-status.md). The target design is
[architecture-plan.md](architecture-plan.md), and its security assumptions are
[threat-model.md](threat-model.md).
