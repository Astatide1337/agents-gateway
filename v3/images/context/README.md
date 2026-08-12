# `agw-context`

`agw-context` materializes the immutable ContextPack from the sealed checkout.
The image includes the optional bounded local LSP client used by the symbol
tier; it does not include a language server, Serena, a code graph, credentials,
or a network client. The default image is intentionally symbol-disabled.

Symbols remain disabled unless all of the following are true:

1. the `ContextStrategy` requests `symbols.kind: lsp-serena`;
2. `AGW_CONTEXT_SYMBOL_ADAPTER` names an absolute, regular, non-symlink
   executable already present in this image; and
3. `AGW_CONTEXT_SYMBOL_ADAPTER_SHA256` exactly matches the SHA-256 digest of
   that executable's bytes; and
4. the executable speaks LSP over stdio and is supplied by a reviewed,
   digest-pinned image build.

Optional fixed arguments are supplied as canonical JSON in
`AGW_CONTEXT_SYMBOL_ADAPTER_ARGS_JSON`. The client launches the executable
directly, with no shell, a minimal environment, a bounded timeout, bounded
stdout/stderr, bounded message/item counts, and a private network namespace.
It asks only for `textDocument/documentSymbol`; malformed, unknown, remote, or
out-of-repository responses fail the init container closed.

The default image and sample do not enable symbols because no language-server
runtime is bundled by default. A deployment must build and test a separate
digest-pinned context image containing its chosen local server, calculate the
executable digest after the final image is assembled, and provide the adapter
variables from that reviewed image. The context init verifies the executable
digest at startup and fails closed on a missing, malformed, or mismatched
digest. The runtime image digest and executable digest are separate pins.

`lsp-serena` is an API selector for this bounded local LSP seam; it is not a
claim that Serena is installed or available. This repository does not bundle a
Serena runtime, language server, or provider image. With the default image, a
run that requests symbols fails during context materialization instead of
silently producing an empty symbol tier.
