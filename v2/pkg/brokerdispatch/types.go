package brokerdispatch

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

const (
	// ModelLoopbackURL and ToolsLoopbackURL are fixed URLs exposed by the
	// in-sandbox broker bridge. They are references only; this package never
	// listens on a host TCP port.
	ModelLoopbackURL    = "http://127.0.0.1:8787/v1/responses"
	ToolsLoopbackURL    = "http://127.0.0.1:8787/mcp"
	ArtifactLoopbackURL = "http://127.0.0.1:8787/v1/artifacts/output"

	defaultSessionTTL = 15 * time.Minute
	cleanupTimeout    = 5 * time.Second
	clientFileMode    = 0400
)

// HandlerRequest contains only non-secret schedule data needed to construct a
// per-run handler. Provider and MCP credentials must be captured by the
// factory's host-side implementation, never returned by it or serialized to
// the sandbox.
type HandlerRequest struct {
	Input   workflow.ScheduleRunnerTaskInput
	Binding runbroker.SessionBinding
}

// HandlerSpec is the host-side result of resolving a model/tool policy. The
// single allowed model is deliberate: client.json has one model value and the
// sandbox cannot select an unadvertised route.
type HandlerSpec struct {
	Handler      http.Handler
	AllowedModel string
	PolicyDigest string
	TTL          time.Duration
	// PrepareSandbox may place validated, read-only inputs below the newly
	// created per-run directory. It must never write credentials there.
	PrepareSandbox func(context.Context, string) error
}

// HandlerFactory creates the already-authorized host-side handler for one
// task. It must not put provider or MCP credentials in HandlerSpec.
type HandlerFactory interface {
	NewHandler(context.Context, HandlerRequest) (HandlerSpec, error)
}

// HandlerFactoryFunc adapts a function to HandlerFactory.
type HandlerFactoryFunc func(context.Context, HandlerRequest) (HandlerSpec, error)

func (f HandlerFactoryFunc) NewHandler(ctx context.Context, request HandlerRequest) (HandlerSpec, error) {
	if f == nil {
		return HandlerSpec{}, errors.New("brokerdispatch: nil handler factory")
	}
	return f(ctx, request)
}

// SandboxSessionSetter is isolated so integrations can adapt a renamed
// SandboxSpec field without allowing the broker lifecycle to mutate paths,
// tokens, or other execution policy. The current v2 field is
// runner.SandboxSpec.BrokerSessionID.
type SandboxSessionSetter func(*runner.SandboxSpec, string) error

// SetBrokerSession sets only the opaque broker session reference on a copied
// SandboxSpec. It rejects a caller-supplied different session reference.
func SetBrokerSession(spec *runner.SandboxSpec, sessionID string) error {
	if spec == nil || sessionID == "" {
		return ErrInvalidConfig
	}
	if spec.BrokerSessionID != "" && spec.BrokerSessionID != sessionID {
		return ErrIdentityMismatch
	}
	if err := runner.ValidateBrokerSessionID(sessionID); err != nil {
		return ErrInvalidConfig
	}
	spec.BrokerSessionID = sessionID
	return nil
}

// Config controls the wrapper's private filesystem and peer policy.
type Config struct {
	BrokerRoot string
	Manager    *runbroker.Manager
	Factory    HandlerFactory

	// UserID is the host-authenticated principal bound into every session.
	UserID string
	// RunnerUID and RunnerGID are the only Unix peers accepted on a session
	// socket. Both must be non-zero and are normally the sandbox identity.
	RunnerUID uint32
	RunnerGID uint32

	SessionTTL          time.Duration
	SetSandboxSessionID SandboxSessionSetter
}
