package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const workflowTemplateDigestDomain = "agents.astatide.com/argo-workflow-template/v1\x00"

// WorkflowTemplateContentDigest computes the immutable content binding for a
// WorkflowTemplate. Only spec is included: Kubernetes metadata, status, and
// controller-managed fields are not template content. encoding/json sorts map
// keys, so the resulting digest is stable across API-server round trips.
func WorkflowTemplateContentDigest(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("%w: WorkflowTemplate is nil", ErrInvalidInput)
	}
	if template.GetAPIVersion() != "argoproj.io/v1alpha1" || template.GetKind() != "WorkflowTemplate" {
		return "", fmt.Errorf("%w: object is not an Argo WorkflowTemplate", ErrInvalidInput)
	}
	spec, found, err := unstructured.NestedMap(template.Object, "spec")
	if err != nil || !found || len(spec) == 0 {
		return "", fmt.Errorf("%w: WorkflowTemplate spec is missing or malformed", ErrInvalidInput)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("%w: encode WorkflowTemplate spec: %v", ErrInvalidInput, err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(workflowTemplateDigestDomain))
	_, _ = hash.Write(body)
	return canonical.DigestPrefix + hex.EncodeToString(hash.Sum(nil)), nil
}

func workflowTemplateObject() *unstructured.Unstructured {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowTemplate"})
	return template
}
