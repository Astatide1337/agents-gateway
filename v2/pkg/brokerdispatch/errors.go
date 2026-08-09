// Package brokerdispatch wraps workflow runner activities with the lifecycle
// of one private, short-lived broker session per idempotent task.
package brokerdispatch

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfig    = errors.New("brokerdispatch: invalid configuration")
	ErrClosed           = errors.New("brokerdispatch: dispatcher is closed")
	ErrIdentityMismatch = errors.New("brokerdispatch: idempotency identity mismatch")
	ErrSessionClosed    = errors.New("brokerdispatch: broker session is closed")
	ErrUnsafePath       = errors.New("brokerdispatch: unsafe private path")
)

// HandlerSetupStage is a closed, non-secret diagnostic code emitted by the
// production handler factory. The dispatcher accepts only these constants so
// an arbitrary HandlerFactory error can never smuggle a credential into a
// durable workflow error.
type HandlerSetupStage string

const (
	HandlerSetupFactory       HandlerSetupStage = "factory"
	HandlerSetupModelRoute    HandlerSetupStage = "model-route"
	HandlerSetupModelBoundary HandlerSetupStage = "model-boundary"
	HandlerSetupToolSet       HandlerSetupStage = "tool-set"
	HandlerSetupMCPBoundary   HandlerSetupStage = "mcp-boundary"
	HandlerSetupArtifact      HandlerSetupStage = "artifact-boundary"
	HandlerSetupSkills        HandlerSetupStage = "skills-boundary"
)

type handlerSetupError struct{ stage HandlerSetupStage }

func (e *handlerSetupError) Error() string {
	return fmt.Sprintf("broker handler setup failed at %s", e.stage)
}

// NewHandlerSetupError returns a secret-safe error only for a known stage.
func NewHandlerSetupError(stage HandlerSetupStage) error {
	switch stage {
	case HandlerSetupFactory, HandlerSetupModelRoute, HandlerSetupModelBoundary,
		HandlerSetupToolSet, HandlerSetupMCPBoundary, HandlerSetupArtifact,
		HandlerSetupSkills:
		return &handlerSetupError{stage: stage}
	default:
		return ErrInvalidConfig
	}
}
