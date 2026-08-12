package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bridge "github.com/Astatide1337/agents-gateway/v3/phase0/argo-sandbox/bridge"
)

const (
	defaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultNSPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

func main() {
	var (
		name           = flag.String("name", "", "Sandbox name")
		namespace      = flag.String("namespace", "", "Sandbox namespace; defaults to the mounted ServiceAccount namespace")
		condition      = flag.String("condition", bridge.ConditionReady, "condition to wait for: Ready or Finished")
		timeout        = flag.Duration("timeout", 8*time.Minute, "maximum time to wait")
		pollInterval   = flag.Duration("poll-interval", 2*time.Second, "time between Kubernetes reads")
		marker         = flag.String("marker", "", "optional identity marker path to create after success")
		requiredMarker = flag.String("require-marker", "", "optional marker path that must identify this Sandbox before success")
		server         = flag.String("server", "", "Kubernetes API server; defaults to KUBERNETES_SERVICE_HOST/PORT_HTTPS")
		tokenPath      = flag.String("token-file", defaultTokenPath, "ServiceAccount token file")
		caPath         = flag.String("ca-file", defaultCAPath, "ServiceAccount CA file")
	)
	flag.Parse()

	if *name == "" {
		fatal("--name is required")
	}
	resolvedNamespace := *namespace
	if resolvedNamespace == "" {
		resolvedNamespace = readFile(defaultNSPath)
	}
	if resolvedNamespace == "" {
		fatal("--namespace is required when the mounted ServiceAccount namespace file is unavailable")
	}
	apiServer := *server
	if apiServer == "" {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if host == "" || port == "" {
			fatal("--server or KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT_HTTPS is required")
		}
		apiServer = "https://" + host + ":" + port
	}
	token, err := os.ReadFile(filepath.Clean(*tokenPath))
	if err != nil {
		fatal("read ServiceAccount token: %v", err)
	}
	ca, err := os.ReadFile(filepath.Clean(*caPath))
	if err != nil {
		fatal("read ServiceAccount CA: %v", err)
	}
	client, err := bridge.NewClient(apiServer, string(token), ca)
	if err != nil {
		fatal("configure Kubernetes client: %v", err)
	}
	err = bridge.Wait(context.Background(), client, bridge.Config{
		Namespace: resolvedNamespace, Name: *name, Condition: *condition,
		Timeout: *timeout, PollInterval: *pollInterval,
		MarkerPath: *marker, RequiredMarkerPath: *requiredMarker,
	}, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	if err != nil {
		fatal("Sandbox wait failed: %v", err)
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
