# Disposable local two-node k3s smoke

v3/scripts/local-k3s-smoke.sh is a bounded local harness for exercising the
Kubernetes-facing Phase-0 assumptions without installing k3s on the host or
touching the production Docker/Coolify stack.

It runs one k3s server and one k3s agent in Docker using a unique bridge
network. The server is limited to 1 CPU and 1536 MiB RAM; the agent is limited
to 0.75 CPU and 1024 MiB RAM. Only the Kubernetes API is published, on a
dynamically allocated 127.0.0.1 port. Coolify's public ports are not used.

The script verifies:

- the host-published k3s API reaches /readyz;
- exactly two nodes exist and both become Ready;
- exactly one node has agw.astatide.com/agents=true;
- that node has agw.astatide.com/agents=true:NoSchedule;
- the generated kubeconfig uses a unique context and the loopback API port.

## Usage

Plan mode is offline and is the default:

    bash v3/scripts/local-k3s-smoke.sh --plan

An actual run requires an explicit mutation confirmation:

    bash v3/scripts/local-k3s-smoke.sh --apply --yes

The cached rancher/k3s:v1.35.0-k3s1 image is used when available. If it is
missing, the apply run pulls exactly the requested image. The image is not
removed afterward so a later local run does not need to download it again.

To run the existing Phase-0 probes while the disposable cluster is alive:

    bash v3/scripts/local-k3s-smoke.sh --apply --yes --phase0

To install the pinned Agent Sandbox release into that disposable cluster as
part of the probe, request it explicitly and provide the release-manifest
SHA-256:

    AGENT_SANDBOX_MANIFEST_SHA256=<64-hex-digest> \\
    AGENT_SANDBOX_CONTROLLER_IMAGE=registry.k8s.io/agent-sandbox/agent-sandbox-controller@sha256:<64-hex-digest> \\
      bash v3/scripts/local-k3s-smoke.sh --apply --yes --phase0 --install-upstream

The harness creates an isolated temporary kubeconfig and passes it to the
Phase-0 wrapper through KUBECONFIG; it does not alter ~/.kube/config or the
current kubectl context. The Phase-0 wrapper receives the generated unique
context and namespace. Upstream installation is disabled by default so a
probe cannot silently fetch a controller; `--install-upstream` forwards the
explicit request to the pinned Phase-0 installer. This is the supported way to
use the short-lived cluster for probes because the cluster is intentionally
destroyed before the command exits.

## Cleanup boundary

The apply path installs an EXIT, INT, and TERM cleanup boundary after creating
a unique temporary directory. Cleanup removes only:

- the exact generated agent container, after checking its ownership labels;
- the exact generated server container, after checking its ownership labels;
- the exact generated network, after checking its ownership labels;
- the exact generated temporary directory, after validating its mktemp prefix.

It never runs docker system prune, never deletes by a broad label selector,
never removes an existing network, and never deletes a namespace from another
cluster. If an ownership check fails, the script refuses that deletion and
returns a cleanup failure rather than widening the target.

## Resource and security limits

The harness refuses to start unless it sees at least two CPUs, 3000 MiB of
available RAM, and 2048 MiB of free disk. The Docker containers are privileged
because k3s must manage namespaces, cgroups, networking, and its embedded
container runtime inside the container. The two-node result therefore proves
the API, scheduling labels, and taint wiring in a disposable Docker topology;
it does not prove that hostUsers:false, UID-owner iptables rules, or rootless
isolation work on the eventual production k3s node. Those remain target-node
Phase-0 gates.

The harness does not install k3s on the host, create host services, publish an
external port, push images, invoke GitHub Actions, or retain a cluster after
the run. A downloaded k3s image remains in Docker's image cache by design.

Exit status `3` means the required two-node smoke and required Phase-0 checks
passed, but an explicitly optional check was skipped (for example, no
object-store endpoint was supplied). Exit status `1` or `2` means a required
check or harness safety condition failed.
