package spec

import (
	"strings"
	"testing"
)

func TestDecodeAllDispatchesMultiDocumentStream(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
---
apiVersion: agents.astatide.com/v1alpha1
kind: Project
metadata:
  name: agents
spec:
  organizationRef: acme
`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(resources) != 2 || ResourceKind(resources[0]) != KindOrganization || ResourceKind(resources[1]) != KindProject {
		t.Fatalf("unexpected resources: %#v", resources)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: Acme
  typo: rejected
`))
	if err == nil || !strings.Contains(err.Error(), "field typo not found") {
		t.Fatalf("expected strict unknown-field error, got %v", err)
	}
}

func TestValidateProductionRequiresDigests(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Agent
metadata:
  name: fixer
spec:
  runtime:
    harness: codex
    image: ghcr.io/example/agent:latest
  instructions:
    inline: fix it
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(resources, ValidationOptions{Production: true}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected digest validation error, got %v", err)
	}
}

func TestValidateWorkflowRejectsCyclesAndUnknownDependencies(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Workflow
metadata:
  name: cycle
spec:
  steps:
    - id: first
      agent: one
      needs: [second]
    - id: second
      agent: two
      needs: [first, missing]
`))
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateAll(resources, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("expected cycle and dependency errors, got %v", err)
	}
}

func TestRevisionDigestIsStableForMapAndUnorderedFieldOrder(t *testing.T) {
	first, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: SandboxProfile
metadata:
  name: coding
  labels:
    z: last
    a: first
spec:
  backend: podman
  image: ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  filesystem:
    root: read-only
  network:
    mode: brokered
    routes: [tools, models]
`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: SandboxProfile
metadata:
  name: coding
  labels:
    a: first
    z: last
spec:
  backend: podman
  image: ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  filesystem:
    root: read-only
  network:
    mode: brokered
    routes: [models, tools]
`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := RevisionDigest(first[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := RevisionDigest(second[0])
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("digest changed for normalized equivalent resources: %s != %s", a, b)
	}
}

func TestArtifactDigestMustBeSha256(t *testing.T) {
	resources, err := Decode([]byte(`apiVersion: agents.astatide.com/v1alpha1
kind: Artifact
metadata:
  name: report
spec:
  runRef: run
  uri: s3://bucket/report
  digest: md5:bad
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(resources, ValidationOptions{}); err == nil {
		t.Fatal("expected artifact digest validation error")
	}
}
