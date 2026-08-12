package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.LookupEnv); err != nil {
		// stdout is intentionally unused. Never print the wrapped error here:
		// command, URL, Git, and provider implementations can evolve, while a
		// stable generic diagnostic guarantees that a secret cannot be echoed.
		_, _ = fmt.Fprintln(os.Stderr, "agw-verify-fetch: setup failed")
		os.Exit(1)
	}
}
