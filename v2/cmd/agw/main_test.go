package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestApplyAndRunUseAuthenticatedControlPlane(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runtime-token" {
			t.Fatalf("missing runtime authorization")
		}
		paths = append(paths, request.Method+" "+request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/runs") {
			_, _ = w.Write([]byte(`{"data":{"id":"run-1","status":"Pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"revision":1}}`))
	}))
	defer server.Close()
	t.Setenv("AGW_TOKEN", "runtime-token")
	manifest := t.TempDir() + "/organization.yaml"
	if err := writeTestFile(manifest, `apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
`); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"apply", "-server", server.URL, "-organization", "org-a", "-project", "project-a", "-f", manifest}, &stdout, &stderr); code != 0 {
		t.Fatalf("apply code=%d stderr=%s", code, stderr.String())
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run", "-server", server.URL, "-organization", "org-a", "-project", "project-a", "-kind", "AgentRun", "-ref", "fixer", "-revision", digest, "-idempotency-key", "issue-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, stderr.String())
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "/resources/Organization/acme") || !strings.HasSuffix(paths[1], "/runs") || !strings.Contains(stdout.String(), "run-1") {
		t.Fatalf("paths=%#v stdout=%s", paths, stdout.String())
	}
}

func TestGenerateSigningKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"generate-signing-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(strings.TrimSpace(stdout.String())) < 80 || !strings.Contains(stderr.String(), "public-key=") {
		t.Fatalf("unexpected signing key output")
	}
}

func TestGenerateLocalToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"generate-local-token"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(stdout.String()))
	if err != nil || len(decoded) != 32 {
		t.Fatalf("invalid generated token length=%d err=%v", len(decoded), err)
	}
}

func TestRunControlCommandsUseBoundedReferencesAndIdempotency(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runtime-token" || request.Header.Get("Idempotency-Key") == "" {
			t.Fatalf("missing control authentication/idempotency headers: %v", request.Header)
		}
		paths = append(paths, request.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"signal":"accepted"}}`))
	}))
	defer server.Close()
	t.Setenv("AGW_TOKEN", "runtime-token")
	base := []string{"-server", server.URL, "-organization", "org-a", "-project", "project-a", "-run-id", "run-1", "-idempotency-key"}
	tests := [][]string{
		append([]string{"cancel"}, append(base, "cancel-1", "-reason", "operator stop")...),
		append([]string{"approve"}, append(base, "approval-1", "-approval-id", "gate-1", "-step-id", "release", "-decision", "approved")...),
		append([]string{"reply"}, append(base, "reply-1", "-target", "agent", "-step-id", "test", "-task-id", "task-1", "-reply-ref", "s3://replies/one.json")...),
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
	if len(paths) != 3 || !strings.HasSuffix(paths[0], "/cancel") || !strings.HasSuffix(paths[1], "/approval") || !strings.HasSuffix(paths[2], "/reply") {
		t.Fatalf("control paths=%#v", paths)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"reply", "-server", server.URL, "-organization", "org-a", "-project", "project-a", "-run-id", "run-1", "-reply-ref", "raw content"}, &stdout, &stderr); code == 0 {
		t.Fatal("control command must require an idempotency key")
	}
}

func TestValidateCommand(t *testing.T) {
	manifest := t.TempDir() + "/resources.yaml"
	if err := writeTestFile(manifest, `apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
`); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "-f", manifest}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid: 1 resource(s)") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
}

func TestPlanCommandIsDeterministic(t *testing.T) {
	manifest := t.TempDir() + "/resources.yaml"
	if err := writeTestFile(manifest, `apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
`); err != nil {
		t.Fatal(err)
	}
	var first, firstErr bytes.Buffer
	if code := run([]string{"plan", "-f", manifest}, &first, &firstErr); code != 0 {
		t.Fatalf("first plan failed: %s", firstErr.String())
	}
	var second, secondErr bytes.Buffer
	if code := run([]string{"plan", "-f", manifest}, &second, &secondErr); code != 0 {
		t.Fatalf("second plan failed: %s", secondErr.String())
	}
	if first.String() != second.String() {
		t.Fatalf("plan output changed:\n%s\n%s", first.String(), second.String())
	}
}

func TestProductionFlagRejectsUnpinnedImage(t *testing.T) {
	manifest := t.TempDir() + "/resources.yaml"
	if err := writeTestFile(manifest, `apiVersion: agents.astatide.com/v1alpha1
kind: Agent
metadata:
  name: fixer
spec:
  runtime:
    harness: codex
    image: ghcr.io/example/agent:latest
  instructions:
    inline: fix
`); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "-production", "-f", manifest}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "sha256") {
		t.Fatalf("expected production digest error, code=%d stderr=%s", code, stderr.String())
	}
}

func TestMigrateManifestIsConservative(t *testing.T) {
	manifest := t.TempDir() + "/agent.yaml"
	if err := writeTestFile(manifest, `id: reviewer
name: Reviewer
description: Reviews changes
version: 1.0.0
runtime:
  type: process
  command: python run.py
skills: []
tools:
  - github.create_pull_request
permissions: {}
risk_level: medium
tags: [review]
author: test
`); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"migrate-manifest", "-f", manifest}, &stdout, &stderr); code != 0 {
		t.Fatalf("migration failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "kind: Agent") || !strings.Contains(stdout.String(), "approval: deny") {
		t.Fatalf("unsafe migration output:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unsafe_legacy_runtime") || !strings.Contains(stderr.String(), "TOOLS.md is not enforcement") {
		t.Fatalf("missing migration warnings:\n%s", stderr.String())
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
