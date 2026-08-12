# ContextPack symbol-provider contract

Status: default-disabled; optional provider seam implemented locally

The ContextPack builder has working lexical, structural, history, and Policy
projections. Its symbol implementation is only a bounded local LSP client. It
is not a language server, Serena installation, semantic index, or remote
service.

## Default behavior

The default `agw-context` image contains no language server and no
Serena runtime. The default image does not set symbol-provider environment
variables. If a resolved `ContextStrategy` requests `symbols.kind: lsp-serena`
without a provider, `agw-context` returns an error and the init container fails
closed. It never emits an empty successful symbol tier to hide a missing
provider.

The name `lsp-serena` is an API selector for the compatible symbol tier. It is
not evidence that Serena is installed or available.

## Explicit provider contract

A separately reviewed, derived context image may opt in to a local provider.
The image must be immutable in the chart (`runtimeImages.context` is pinned by
image digest), and the executable must be configured with all of these values:

```text
AGW_CONTEXT_SYMBOL_ADAPTER=/opt/agw/providers/<server>
AGW_CONTEXT_SYMBOL_ADAPTER_SHA256=sha256:<64 lowercase hex characters>
AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON=[]
```

The executable path must be absolute, canonical, regular, non-symlink, and
already present in the image. At startup the context process hashes the exact
executable bytes and refuses to run if the digest does not match. Arguments are
strict canonical JSON, fixed at process start, and cannot contain task or
repository substitutions.

The client launches only that local executable over stdio. It uses a bounded
timeout and output/message/item budgets, a minimal environment, no shell, and
a private network namespace. It requests only document symbols and rejects
malformed, remote, credential-like, or out-of-repository results.

The executable digest is separate from the image digest: the image digest pins
the complete filesystem, while the executable digest makes the provider
identity explicit and detects a changed file at the configured path.

## Production gap

No reviewed language-server or Serena provider image is bundled, built, pushed,
or deployed by this repository change. Until one exists and is tested against
the target runtime, keep the symbol tier omitted from production
`ContextStrategy` objects. The lexical/tree-sitter/history/Policy path remains
the supported default.

Focused local proof covers configuration parsing, digest mismatch rejection,
LSP framing/result validation, path containment, budgets, and the default image
contract. It does not prove a real provider, Serena availability, or a live
k3s materialization.
