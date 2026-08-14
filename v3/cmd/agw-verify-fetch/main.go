package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
)

func main() {
	if err := run(context.Background(), os.LookupEnv); err != nil {
		// stdout is intentionally unused. Never print the wrapped error here:
		// command, URL, Git, and provider implementations can evolve, while a
		// bounded stage code makes failures diagnosable without echoing secrets.
		_, _ = fmt.Fprintf(os.Stderr, "agw-verify-fetch: setup failed (%s)\n", failureStage(err))
		os.Exit(1)
	}
}

func failureStage(err error) string {
	switch {
	case errors.Is(err, errInvalidContract):
		return "contract"
	case errors.Is(err, errWorkspace):
		return "workspace"
	case errors.Is(err, errGitOperation):
		return "git"
	case errors.Is(err, verifyfetch.ErrInvalidConfig):
		return "artifact_store_config"
	case errors.Is(err, verifyfetch.ErrInvalidSecret):
		return "artifact_store_credentials"
	case errors.Is(err, verifyfetch.ErrResponse), errors.Is(err, verifyfetch.ErrTooLarge), errors.Is(err, verifyfetch.ErrLengthMismatch):
		return "artifact_store_response"
	case errors.Is(err, errPatch):
		return "patch"
	case errors.Is(err, errArchive):
		return "archive"
	default:
		return "external_setup"
	}
}
