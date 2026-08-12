# AGW preflight image

`agw-preflight` is the one-shot attestor used by the Helm chart's startup Job
and recurring CronJob. It has two modes:

- `attest` runs as UID 1337, receives only an explicitly projected short-lived
  ServiceAccount token and CA, performs the checks, and writes the fixed
  `agw-preflight` ConfigMap.
- `agent-probe` runs as UID 1000 without a token or Secret mount and proves that
  the UID airlock denies DNS and API-server egress.

The image is built from the repository root. The Go build dependency may follow
the latest stable stream, but deployed references must use the registry
manifest digest recorded after the release build. The image contains no
provider, GitHub, MCP, model, or object-store credentials.
