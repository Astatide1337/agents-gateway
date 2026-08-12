package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.LookupEnv); err != nil {
		// Do not print the wrapped error: Git diagnostics can contain remote
		// names or repository-controlled text. The verifier only needs a
		// non-zero init-container result and Kubernetes status.
		_, _ = fmt.Fprintln(os.Stderr, "agw-verify-apply: patch application failed")
		os.Exit(1)
	}
}
