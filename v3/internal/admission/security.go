package admission

import (
	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/toolsecurity"
)

// ValidateToolSetSecurity adapts the shared ToolSet boundary checks to the
// bounded diagnostic type used by the admission webhook. Resolution invokes
// the same shared implementation again because referenced ToolSets are
// mutable until a run's immutable snapshot is persisted.
func ValidateToolSetSecurity(toolSet *v1alpha1.ToolSet) ValidationErrors {
	shared := toolsecurity.ValidateToolSetSecurity(toolSet)
	violations := make(ValidationErrors, 0, len(shared))
	for _, violation := range shared {
		add(&violations, Code(violation.Code), violation.Field, violation.Message)
	}
	return violations
}

// validateMCPEndpoint is retained as a small testable adapter for the webhook
// package's endpoint diagnostics.
func validateMCPEndpoint(raw string) (Code, string) {
	code, message := toolsecurity.ValidateMCPEndpoint(raw)
	return Code(code), message
}

func validateExactArguments(field string, arguments map[string]apix.JSON) ValidationErrors {
	shared := toolsecurity.ValidateExactArguments(field, arguments)
	violations := make(ValidationErrors, 0, len(shared))
	for _, violation := range shared {
		add(&violations, Code(violation.Code), violation.Field, violation.Message)
	}
	return violations
}
