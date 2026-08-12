package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.LookupEnv, os.Stdout); err != nil {
		// stdout is reserved for the single authenticated capture frame. All
		// diagnostics stay on stderr so the controller can reject any malformed
		// or partial frame without ambiguity.
		_, _ = fmt.Fprintln(os.Stderr, "agw-capture:", err)
		os.Exit(1)
	}
}
