package codexadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
)

func TestCollectAuthoredArtifactsUsesStrictWorkspaceRelativeDescriptors(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, ".agw", "artifacts")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	body := `<main><button>Run</button></main>`
	if err := os.WriteFile(filepath.Join(workspace, "report.html"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	descriptor := `{"schema":"agents-gateway.artifact.v1","title":"Interactive report","description":"Local UI","content_kind":"interactive_component","media_type":"text/html","source":"report.html","capabilities":["sandboxed_scripts"]}`
	if err := os.WriteFile(filepath.Join(directory, "report.json"), []byte(descriptor), 0600); err != nil {
		t.Fatal(err)
	}

	artifacts, err := collectAuthoredArtifacts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || string(artifacts[0].body) != body || artifacts[0].mediaType != "text/html" || artifacts[0].descriptor.ContentKind != artifactcatalog.ContentKindInteractive || len(artifacts[0].descriptor.Capabilities) != 1 {
		t.Fatalf("unexpected authored artifacts: %#v", artifacts)
	}
}

func TestCollectAuthoredArtifactsRejectsTraversalSymlinksAndUnknownFields(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		descriptor string
		prepare    func(t *testing.T, workspace string)
	}{
		{
			name:       "traversal",
			descriptor: `{"schema":"agents-gateway.artifact.v1","title":"Bad","content_kind":"document","media_type":"text/plain","source":"../secret"}`,
		},
		{
			name:       "symlink",
			descriptor: `{"schema":"agents-gateway.artifact.v1","title":"Bad","content_kind":"document","media_type":"text/plain","source":"linked.txt"}`,
			prepare: func(t *testing.T, workspace string) {
				outside := filepath.Join(t.TempDir(), "secret.txt")
				if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(workspace, "linked.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "unknown field",
			descriptor: `{"schema":"agents-gateway.artifact.v1","title":"Bad","content_kind":"document","media_type":"text/plain","source":"source.txt","surprise":true}`,
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, ".agw", "artifacts")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if testCase.prepare != nil {
				testCase.prepare(t, workspace)
			}
			if err := os.WriteFile(filepath.Join(directory, "bad.json"), []byte(testCase.descriptor), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := collectAuthoredArtifacts(workspace); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe descriptor error=%v", err)
			}
		})
	}
}

func TestPromptDocumentsOptionalArtifactContractWithoutGrantingNetwork(t *testing.T) {
	value := prompt("build a report")
	for _, required := range []string{".agw/artifacts", "agents-gateway.artifact.v1", "sandboxed_scripts", "Network, external calls, host access, and credentials are unavailable"} {
		if !strings.Contains(value, required) {
			t.Fatalf("prompt missing %q", required)
		}
	}
}
