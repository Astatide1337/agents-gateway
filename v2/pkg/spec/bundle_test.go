package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bundleImage = "ghcr.io/astatide/agents-gateway-runtime-codex@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validBundleYAML(prompt string) string {
	return `apiVersion: agents.astatide.com/v1alpha1
kind: AgentBundle
metadata:
  name: review-agent
spec:
  prompt: ` + prompt + `
  runtime:
    harness: codex
    image: ` + bundleImage + `
  model:
    provider: openrouter
    model: cohere/north-mini-code:free
    credentialRef: openrouter-api
  sandbox:
    backend: podman
    resources:
      cpu: "1"
      memory: 1Gi
      disk: 2Gi
      pids: 128
    network:
      mode: brokered
      directInternet: false
  skills:
    - ref: agent-manager/example/reviewer
      digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
  mcp:
    servers:
      - name: github
        ref: https://mcp.example.test/mcp
        credentialsRef: github-api
        tools:
          - name: get_me
            effect: read
            approval: allow
  verification:
    commands:
      - go test ./...
  artifacts:
    enabled: true
    required: true
    primaryKind: document
    primaryMediaType: text/markdown
`
}

func TestAgentBundleCompilesDeterministicallyWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(validBundleYAML("|\n    Review the repository.")), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, baseDir, err := LoadAgentBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: baseDir})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: baseDir})
	if err != nil {
		t.Fatal(err)
	}
	if first.AgentDigest != second.AgentDigest {
		t.Fatalf("bundle digest changed: %s != %s", first.AgentDigest, second.AgentDigest)
	}
	if first.BundleDigest == "" || first.BundleDigest != second.BundleDigest || first.BundleDigest == first.AgentDigest {
		t.Fatalf("complete bundle digest is not deterministic or distinct: %q %q", first.BundleDigest, second.BundleDigest)
	}
	if len(first.Resources) != 5 {
		t.Fatalf("expected Agent plus four generated resources, got %d", len(first.Resources))
	}
	for _, resource := range first.Resources {
		encoded, encodeErr := AsJSON(resource)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if strings.Contains(string(encoded), "secret-value") {
			t.Fatal("compiled resource contained a secret value")
		}
	}
	if first.Agent.Spec.Artifacts == nil || first.Agent.Spec.Artifacts.PrimaryMediaType != "text/markdown" {
		t.Fatalf("artifact preferences were not preserved: %#v", first.Agent.Spec.Artifacts)
	}
	if strings.TrimSpace(first.Agent.Spec.Instructions.Inline) != "Review the repository." {
		t.Fatalf("prompt was not compiled: %q", first.Agent.Spec.Instructions.Inline)
	}
	withInput, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: baseDir, Input: "issue-42"})
	if err != nil {
		t.Fatal(err)
	}
	if withInput.AgentDigest == first.AgentDigest || !strings.Contains(withInput.Agent.Spec.Instructions.Inline, "issue-42") {
		t.Fatal("input did not become part of the immutable agent definition")
	}
}

func TestAgentBundlePromptFileIsContainedAndInlined(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("prompt from file"), 0600); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Replace(validBundleYAML("{file: prompt.md}"), "skills:\n", "skills:\n", 1))
	bundle, baseDir, err := func() (*AgentBundle, string, error) {
		path := filepath.Join(dir, "agent.yaml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, "", err
		}
		return LoadAgentBundle(path)
	}()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: baseDir})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Agent.Spec.Instructions.Inline != "prompt from file" || compiled.Agent.Spec.Instructions.File != "" {
		t.Fatalf("prompt file was not inlined: %#v", compiled.Agent.Spec.Instructions)
	}

	escape := filepath.Join(dir, "escape.yaml")
	escapeData := []byte(strings.Replace(validBundleYAML("{file: ../outside.md}"), "metadata:\n  name: review-agent", "metadata:\n  name: escape-agent", 1))
	if err := os.WriteFile(escape, escapeData, 0600); err != nil {
		t.Fatal(err)
	}
	escapeBundle, escapeBase, err := LoadAgentBundle(escape)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileAgentBundle(escapeBundle, BundleCompileOptions{BaseDir: escapeBase}); err == nil || !strings.Contains(err.Error(), "prompt.file") {
		t.Fatalf("prompt path escape was accepted: %v", err)
	}
}

func TestAgentBundleRejectsNullAndUnpinnedInputs(t *testing.T) {
	for _, source := range []string{
		`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"AgentBundle","metadata":{"name":"null-agent"},"spec":{"prompt":null}}`,
		"apiVersion: agents.astatide.com/v1alpha1\nkind: AgentBundle\nmetadata:\n  name: null-agent\nspec:\n  prompt: null\n",
	} {
		if _, err := DecodeAgentBundle([]byte(source)); err == nil || !strings.Contains(err.Error(), "null") {
			t.Fatalf("explicit null was accepted: %v", err)
		}
	}
	data := strings.Replace(validBundleYAML("|\n    Review."), bundleImage, "ghcr.io/example/runtime:latest", 1)
	bundle, err := DecodeAgentBundle([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: "."}); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("unpinned runtime image was accepted: %v", err)
	}
}

func TestAgentBundleJSONAndArrayMCPFormsDecode(t *testing.T) {
	source := `{"apiVersion":"agents.astatide.com/v1alpha1","kind":"AgentBundle","metadata":{"name":"json-agent"},"spec":{"prompt":"review","runtime":{"harness":"codex","image":"` + bundleImage + `"},"model":{"provider":"openrouter","model":"cohere/north-mini-code:free","credentialRef":"openrouter-api"},"sandbox":{"backend":"podman"},"mcp":[{"name":"github","ref":"https://mcp.example.test/mcp","credentialsRef":"github-api","tools":[{"name":"get_me","effect":"read","approval":"allow"}]}]}}`
	bundle, err := DecodeAgentBundle([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Spec.MCP.Servers) != 1 || bundle.Spec.MCP.Servers[0].Tools[0].Name != "get_me" {
		t.Fatalf("JSON MCP array was not decoded: %#v", bundle.Spec.MCP)
	}
	if _, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: "."}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentBundleReusableReferencesCompileWithoutInlineComponents(t *testing.T) {
	source := `apiVersion: agents.astatide.com/v1alpha1
kind: AgentBundle
metadata:
  name: referenced-agent
spec:
  prompt: review the repository
  runtime:
    harness: codex
    image: ` + bundleImage + `
  toolSetRef: shared-tools
  skillSetRef: shared-skills
  modelRouteRef: shared-model
  sandboxProfileRef: shared-sandbox
`
	bundle, err := DecodeAgentBundle([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentBundle(bundle, BundleCompileOptions{BaseDir: "."})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Resources) != 1 || compiled.Agent.Spec.ToolSetRef != "shared-tools" || compiled.Agent.Spec.SandboxProfileRef != "shared-sandbox" {
		t.Fatalf("references were not preserved: %#v", compiled.Resources)
	}
}
