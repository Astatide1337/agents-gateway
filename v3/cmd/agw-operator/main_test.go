package main

import (
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAllImagesPinnedFailsClosed(t *testing.T) {
	valid := "ghcr.io/astatide/helper@sha256:" + strings.Repeat("a", 64)
	if !allImagesPinned(valid, valid) {
		t.Fatal("valid digest-pinned images were rejected")
	}
	for _, images := range [][]string{
		nil,
		{valid, "ghcr.io/astatide/helper:latest"},
		{valid, "ghcr.io/astatide/helper@sha256:" + strings.Repeat("0", 64)},
	} {
		if allImagesPinned(images...) {
			t.Fatalf("unsafe images were accepted: %#v", images)
		}
	}
}

func TestSandboxBackendFlagParsingAndConstruction(t *testing.T) {
	if got, err := sandbox.ParseBackendKind(""); err == nil || got != "" {
		t.Fatal("empty backend was accepted")
	}
	for _, test := range []struct {
		value sandbox.BackendKind
		want  string
	}{
		{sandbox.BackendAgentSandbox, sandbox.ChildKindSandbox},
		{sandbox.BackendJob, sandbox.ChildKindJob},
	} {
		parsed, err := sandbox.ParseBackendKind(string(test.value))
		if err != nil || parsed != test.value || parsed.ChildKind() != test.want {
			t.Fatalf("parse %q = %q, %v; want %q/%q", test.value, parsed, err, test.value, test.want)
		}
		backend, err := newSandboxBackend(fake.NewClientBuilder().Build(), parsed)
		if err != nil || backend == nil {
			t.Fatalf("construct %q: backend=%T err=%v", parsed, backend, err)
		}
	}
	if _, err := newSandboxBackend(fake.NewClientBuilder().Build(), sandbox.BackendKind("bogus")); err == nil {
		t.Fatal("unknown backend was accepted by production construction")
	}
}

func TestParseCredentialSecretNames(t *testing.T) {
	got, ok := parseCredentialSecretNames("mcp-github, model-route,skills-provider")
	if !ok || strings.Join(got, ",") != "mcp-github,model-route,skills-provider" {
		t.Fatalf("parsed allowlist=%v ok=%v", got, ok)
	}
	for _, raw := range []string{"", "mcp-github,", "mcp-github,mcp-github", " ,mcp-github"} {
		if _, ok := parseCredentialSecretNames(raw); ok {
			t.Fatalf("unsafe credential allowlist accepted: %q", raw)
		}
	}
}

func TestParseOptionalNameList(t *testing.T) {
	got, ok := parseOptionalNameList(" gvisor, fast-local ")
	if !ok || strings.Join(got, ",") != "gvisor,fast-local" {
		t.Fatalf("parsed placement allowlist=%v ok=%v", got, ok)
	}
	if got, ok := parseOptionalNameList(""); !ok || got != nil {
		t.Fatalf("empty optional allowlist=%v ok=%v, want nil/true", got, ok)
	}
	for _, raw := range []string{"gvisor,", "gvisor,gvisor", " ,gvisor"} {
		if _, ok := parseOptionalNameList(raw); ok {
			t.Fatalf("unsafe optional allowlist accepted: %q", raw)
		}
	}
}
