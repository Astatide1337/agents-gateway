// Package cedarpolicy wraps Cedar with fail-closed defaults and the normalized
// principal/action/resource/context shape used by Agents Gateway.
package cedarpolicy

import (
	"errors"
	"fmt"

	cedar "github.com/cedar-policy/cedar-go"
)

type Engine struct{ policies *cedar.PolicySet }

type Request struct {
	PrincipalType, PrincipalID string
	Action                     string
	ResourceType, ResourceID   string
	PrincipalAttributes        map[string]any
	ResourceAttributes         map[string]any
	Context                    map[string]any
}

type Decision struct {
	Allowed          bool
	Reasons          []string
	EvaluationErrors []string
}

func New(name string, document []byte) (*Engine, error) {
	if len(document) == 0 {
		return nil, errors.New("Cedar policy document is required")
	}
	policies, err := cedar.NewPolicySetFromBytes(name, document)
	if err != nil {
		return nil, fmt.Errorf("parse Cedar policy: %w", err)
	}
	return &Engine{policies: policies}, nil
}

func (e *Engine) Authorize(request Request) Decision {
	if e == nil || e.policies == nil || request.PrincipalType == "" || request.PrincipalID == "" || request.Action == "" || request.ResourceType == "" || request.ResourceID == "" {
		return Decision{EvaluationErrors: []string{"incomplete authorization request"}}
	}
	principal := cedar.NewEntityUID(cedar.EntityType(request.PrincipalType), cedar.String(request.PrincipalID))
	resource := cedar.NewEntityUID(cedar.EntityType(request.ResourceType), cedar.String(request.ResourceID))
	action := cedar.NewEntityUID("Action", cedar.String(request.Action))
	principalAttrs, err := record(request.PrincipalAttributes)
	if err != nil {
		return Decision{EvaluationErrors: []string{err.Error()}}
	}
	resourceAttrs, err := record(request.ResourceAttributes)
	if err != nil {
		return Decision{EvaluationErrors: []string{err.Error()}}
	}
	context, err := record(request.Context)
	if err != nil {
		return Decision{EvaluationErrors: []string{err.Error()}}
	}
	entities := cedar.EntityMap{principal: {UID: principal, Attributes: principalAttrs}, resource: {UID: resource, Attributes: resourceAttrs}}
	decision, diagnostic := cedar.Authorize(e.policies, entities, cedar.Request{Principal: principal, Action: action, Resource: resource, Context: context})
	result := Decision{Allowed: decision == cedar.Allow && len(diagnostic.Errors) == 0}
	for _, reason := range diagnostic.Reasons {
		result.Reasons = append(result.Reasons, string(reason.PolicyID))
	}
	for _, evaluationErr := range diagnostic.Errors {
		result.EvaluationErrors = append(result.EvaluationErrors, evaluationErr.String())
	}
	return result
}

func record(values map[string]any) (cedar.Record, error) {
	mapped := cedar.RecordMap{}
	for key, value := range values {
		switch typed := value.(type) {
		case string:
			mapped[cedar.String(key)] = cedar.String(typed)
		case bool:
			mapped[cedar.String(key)] = cedar.Boolean(typed)
		case int:
			mapped[cedar.String(key)] = cedar.Long(typed)
		case int64:
			mapped[cedar.String(key)] = cedar.Long(typed)
		default:
			return cedar.Record{}, fmt.Errorf("unsupported Cedar context value %T for %q", value, key)
		}
	}
	return cedar.NewRecord(mapped), nil
}
