package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
)

func TestCurrentRepositoryManifestsHaveGoldenTranslations(t *testing.T) {
	root := repositoryRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, "agents", "*", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("no current repository manifests found")
	}

	for _, path := range paths {
		id := filepath.Base(filepath.Dir(path))
		translation, err := TranslateFile(path)
		if err != nil {
			t.Fatalf("TranslateFile(%s): %v", path, err)
		}
		got := marshalGolden(t, translation)
		goldenPath := filepath.Join(root, "v2", "pkg", "compat", "testdata", "golden", id+".json")
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read golden for %s: %v\nGenerated output:\n%s", id, err, got)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("golden mismatch for %s\n--- want ---\n%s--- got ---\n%s", id, want, got)
		}
	}
}

func TestTranslateOutputIsDeterministic(t *testing.T) {
	input := []byte(strings.Join([]string{
		"id: example", "name: Example", "description: Test agent", "version: 1.2.3",
		"runtime:", "  type: process", "  command: python3 run.py", "skills:", "  - z-skill", "  - a-skill",
		"tools:", "  - write_file", "  - read_file", "permissions:", "  zeta: true", "  alpha: false",
		"risk_level: medium", "tags:", "  - z", "  - a", "author: test", "",
	}, "\n"))

	first, err := TranslateYAML(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TranslateYAML(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := marshalTranslation(t, first), marshalTranslation(t, second); !bytes.Equal(got, want) {
		t.Fatalf("translation is not deterministic\nfirst:\n%s\nsecond:\n%s", got, want)
	}
}

func TestDecodeLegacyRejectsUnknownFields(t *testing.T) {
	_, err := DecodeLegacy([]byte(strings.Join([]string{
		"id: example", "name: Example", "runtime:", "  type: process", "unsupported: true", "",
	}, "\n")))
	if err == nil || !strings.Contains(err.Error(), "unsupported_legacy_field") {
		t.Fatalf("expected structured unsupported-field error, got %v", err)
	}
}

func TestTranslateRejectsDangerousOpaquePermissionFields(t *testing.T) {
	_, err := TranslateYAML([]byte(strings.Join([]string{
		"id: example", "name: Example", "runtime:", "  type: process", "permissions:", "  docker_socket: true", "",
	}, "\n")))
	if err == nil || !strings.Contains(err.Error(), "unsupported_dangerous_field") {
		t.Fatalf("expected dangerous permission rejection, got %v", err)
	}
}

func TestTranslateRejectsNestedDangerousPermissionFields(t *testing.T) {
	_, err := TranslateYAML([]byte(strings.Join([]string{
		"id: example", "name: Example", "runtime:", "  type: process", "permissions:", "  container:", "    privileged: true", "",
	}, "\n")))
	if err == nil || !strings.Contains(err.Error(), "permissions.container.privileged") {
		t.Fatalf("expected nested dangerous permission rejection, got %v", err)
	}
}

func TestTranslateToolAndSkillMappingsCannotGrantAccess(t *testing.T) {
	translation, err := TranslateYAML([]byte(strings.Join([]string{
		"id: example", "name: Example", "runtime:", "  type: process", "skills:", "  - test-skill", "tools:", "  - delete_resource", "",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}

	var toolSet *spec.ToolSet
	var skillSet *spec.SkillSet
	for _, resource := range translation.Resources {
		switch typed := resource.(type) {
		case *spec.ToolSet:
			toolSet = typed
		case *spec.SkillSet:
			skillSet = typed
		}
	}
	if toolSet == nil || len(toolSet.Spec.Servers) != 1 || len(toolSet.Spec.Servers[0].Tools) != 1 {
		t.Fatalf("unexpected ToolSet: %#v", toolSet)
	}
	grant := toolSet.Spec.Servers[0].Tools[0]
	if grant.Effect != "write" || grant.Approval != "deny" {
		t.Fatalf("legacy tool was not conservatively denied: %#v", grant)
	}
	if skillSet == nil || skillSet.Spec.Skills[0].Digest != "" {
		t.Fatalf("legacy skill unexpectedly received a synthetic digest: %#v", skillSet)
	}
	foundBoundaryWarning := false
	for _, warning := range translation.Warnings {
		if warning.Code == "missing_tool_enforcement" {
			foundBoundaryWarning = strings.Contains(warning.Message, "TOOLS.md is not enforcement")
		}
	}
	if !foundBoundaryWarning {
		t.Fatal("tool warning did not state that TOOLS.md is not enforcement")
	}
}

func TestTranslateRejectsUnknownRuntime(t *testing.T) {
	_, err := TranslateYAML([]byte(strings.Join([]string{
		"id: example", "name: Example", "runtime:", "  type: host-shell", "",
	}, "\n")))
	if err == nil || !strings.Contains(err.Error(), "unsupported_runtime") {
		t.Fatalf("expected unsupported runtime error, got %v", err)
	}
}

func TestTranslateDockerRuntimeUsesSafeProfileAndWarnsOnUnpinnedImage(t *testing.T) {
	translation, err := TranslateYAML([]byte(strings.Join([]string{
		"id: docker-agent", "name: Docker Agent", "runtime:", "  type: docker", "  docker_image: ghcr.io/example/agent:latest", "",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	agent := translation.Resources[0].(*spec.Agent)
	sandbox := translation.Resources[1].(*spec.SandboxProfile)
	if agent.Spec.Runtime.Harness != "legacy-docker" || sandbox.Spec.Backend != "docker" {
		t.Fatalf("unexpected docker mapping: agent=%#v sandbox=%#v", agent.Spec.Runtime, sandbox.Spec.Backend)
	}
	if sandbox.Spec.Network.Mode != "none" || sandbox.Spec.Network.DirectInternet {
		t.Fatalf("docker mapping is not isolated: %#v", sandbox.Spec.Network)
	}
	if !hasWarning(translation.Warnings, "unpinned_image") {
		t.Fatal("expected unpinned image warning")
	}
}

func TestTranslateLocalStubDoesNotRemainAHostRuntime(t *testing.T) {
	translation, err := TranslateYAML([]byte(strings.Join([]string{
		"id: stub-agent", "name: Stub Agent", "runtime:", "  type: local-stub", "",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	agent := translation.Resources[0].(*spec.Agent)
	sandbox := translation.Resources[1].(*spec.SandboxProfile)
	if agent.Spec.Runtime.Harness != "legacy-local-stub" || sandbox.Spec.Backend != "podman" {
		t.Fatalf("local-stub was not converted to an isolated compatibility profile")
	}
	if hasWarning(translation.Warnings, "unsupported_runtime") || !hasWarning(translation.Warnings, "unsafe_legacy_runtime") {
		t.Fatalf("unexpected local-stub warnings: %#v", translation.Warnings)
	}
}

func TestTranslateOpaquePermissionsWarnWithoutGrantingThem(t *testing.T) {
	translation, err := TranslateYAML([]byte(strings.Join([]string{
		"id: permissions-agent", "name: Permissions Agent", "runtime:", "  type: process", "permissions:", "  custom_legacy_flag: true", "",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(translation.Warnings, "opaque_permissions") {
		t.Fatal("expected opaque permissions warning")
	}
	if len(translation.Resources[0].(*spec.Agent).Spec.Environment) != 0 {
		t.Fatal("opaque permissions were unexpectedly converted into environment access")
	}
}

func hasWarning(warnings []Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func marshalTranslation(t *testing.T, translation Translation) []byte {
	t.Helper()
	data, err := json.MarshalIndent(translation, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

type goldenResource struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

type goldenTranslation struct {
	Resources []goldenResource `json:"resources"`
	Warnings  []Warning        `json:"warnings"`
}

func marshalGolden(t *testing.T, translation Translation) []byte {
	t.Helper()
	golden := goldenTranslation{Warnings: translation.Warnings}
	for _, resource := range translation.Resources {
		digest, err := spec.RevisionDigest(resource)
		if err != nil {
			t.Fatal(err)
		}
		golden.Resources = append(golden.Resources, goldenResource{
			Kind:     spec.ResourceKind(resource),
			Name:     resource.Meta().Metadata.Name,
			Revision: digest,
		})
	}
	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
}

func TestTranslationErrorCanBeInspected(t *testing.T) {
	_, err := Translate(LegacyManifest{ID: "bad/id", Name: "Bad", Runtime: LegacyRuntime{Type: "process"}})
	var translated TranslationErrors
	if err == nil || !strings.Contains(err.Error(), "invalid_identity") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.As(err, &translated) || len(translated) == 0 {
		t.Fatalf("error is not inspectable as TranslationErrors: %T %v", err, err)
	}
}
