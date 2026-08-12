package bridge

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestManifestsRejectDuplicateMappingKeys protects security-sensitive pod
// fields from silent last-key-wins behavior. This caught a duplicate
// securityContext in the first version of the Phase 0 WorkflowTemplate.
func TestManifestsRejectDuplicateMappingKeys(t *testing.T) {
	for _, name := range []string{
		"kustomization.yaml",
		"namespace.yaml",
		"rbac.yaml",
		"workflow-template.yaml",
		"workflow.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", name)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			decoder := yaml.NewDecoder(file)
			for {
				var document any
				err := decoder.Decode(&document)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("strict YAML decode: %v", err)
				}
			}
		})
	}
}
