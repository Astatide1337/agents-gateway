// Command agw-codex-adapter is the bounded Codex harness entrypoint.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Astatide1337/agents-gateway/v2/pkg/codexadapter"
)

func main() {
	cfg, err := codexadapter.ConfigFromEnv(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "agw-codex-adapter: %s\n", codexadapter.Redact(err.Error()))
		os.Exit(2)
	}
	if err := codexadapter.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, cfg); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "agw-codex-adapter: %s\n", codexadapter.Redact(err.Error()))
		os.Exit(1)
	}
}
