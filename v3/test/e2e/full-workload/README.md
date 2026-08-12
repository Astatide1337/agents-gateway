# Full production-builder direct-backend contract

`full_workload_test.go` is the provider-free integration slice that sits above
the BusyBox direct gate. It uses the real `workload.Build`,
`capture.Build`/`capturecontroller.Driver`, `verifyworkload.Build`,
`verifycontroller.Driver`, and `sandbox.JobBackend` together in one disposable
in-memory Kubernetes client.

It proves, without model/MCP/GitHub/object-store credentials:

- the production work Sandbox becomes an idempotent direct Job plus an
  AgentRun-owned workspace PVC;
- active and terminal direct-backend observations are distinct;
- capture consumes the fixed capture container frame and persists the exact
  patch and manifest before capture cleanup;
- the verify builder creates a separate Job with no work PVC;
- verification remains pending until the Job is finished and independently
  bound machine evidence is available;
- a signed Accepted Gate report is persisted and replay is idempotent; and
- cleanup removes execution Jobs while leaving the AgentRun-owned workspace for
  the retention controller.

Run it from the v3 module:

```sh
GOTOOLCHAIN=go1.26.5 GOMAXPROCS=2 go test -p 1 -count=1 ./test/e2e/full-workload
```

This is intentionally not a claim that the real container images executed.
The test does not have a repository clone token, provider endpoint, artifact
lease, or a live Kubernetes scheduler. The remaining boundary is a disposable
live run with the pinned clone/context/runtime/capture/verify images (and then
real artifact credentials for publication); the BusyBox direct gate and the
separate k3s/Agent Sandbox preflight cover the lower Kubernetes lifecycle
boundaries until that environment is available.
