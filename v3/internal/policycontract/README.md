# Policy contract compiler

This package is the pure compiler for the v3 quality contract. It accepts
immutable `resolved.PolicySnapshot` values, the required final
`resolvedSpecDigest`, and a caller-supplied loader for policy scripts at the
pinned pristine `baseSHA`. The digest binds every output manifest to the final
source-pinned run contract. The compiler does not read files, invoke Git,
access Kubernetes, or make network calls.

`Compile` creates one canonical `Rule` per globally unique rule ID. The same
rule then projects into:

1. `ContextPackRules`, preserving context, exact script bytes, source path, and
   SHA-256 for `internal/contextpack`;
2. `SelfChecks`, a bounded `argv` descriptor using `sh` plus the exact
   `.agents/policies/<rule-id>/<basename>` ContextPack output path; and
3. `GateChecks`, the independent verifier descriptor, where only blocking rules
   have `RejectsOnFailure=true`.

Advisory rules may omit a check. An advisory script is still loaded, digested,
and exposed to the two consumers, but its failure mode is always `advisory`.
Blocking rules require a regular, non-empty UTF-8 script and an `exit0`
expectation.

The loader must return the exact requested path and base SHA, explicit regular
file metadata, the bytes, and a `sha256:<lowercase-hex>` digest. The compiler
checks all of those values, rejects traversal and unsafe paths, rejects
symlinks/special files, and copies every returned byte. The canonical manifest
contains rule context, source Policy name/UID/resourceVersion/generation, check
paths, expectation, and script digest, but never script bytes or credentials.

`Compiled` accessors return defensive copies. `ManifestBytes` is compact,
deterministic JSON, and `Digest` is its SHA-256 content address.
