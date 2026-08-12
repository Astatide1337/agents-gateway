// agw-broker is the per-run loopback policy sidecar for Agents Gateway v3.
// It has no public listener, no Kubernetes client, and no process-exit API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.LookupEnv, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agw-broker:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, lookup lookupEnv, diagnostics interface{ Write([]byte) (int, error) }) error {
	if ctx == nil || diagnostics == nil {
		return configError{code: "process_initialization_failed"}
	}
	config, err := loadConfig(lookup)
	if err != nil {
		return err
	}
	storage, err := newStorageDependencies(ctx, config.ObjectStore, config.Effects)
	if err != nil {
		return err
	}
	runtime, err := newBrokerRuntime(ctx, config, storage)
	if err != nil {
		return err
	}
	return runtime.serve(ctx)
}
