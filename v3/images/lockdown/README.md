# AGW lockdown image

This deliberately small image supplies only a POSIX shell and iptables for
the fixed ADR-006 script embedded by `internal/workload` and
`internal/verifyworkload`. It receives no Secret or workspace mount. The pod
grants only namespaced `NET_ADMIN` while `hostUsers: false`; Phase 0 must prove
the owner-match rules work before the operator admits execution.

The deployed image reference must be resolved to a sha256 digest even though
the build recipe follows the current Alpine package stream.
