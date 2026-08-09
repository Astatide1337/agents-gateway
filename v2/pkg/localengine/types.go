// Package localengine provides the PostgreSQL-backed, Temporal-free execution
// engine. It is deliberately an adapter: API admission remains in httpapi,
// manifest compilation may be supplied by orchestration.Compiler, and side
// effects cross only the workflow.RunnerActivities boundary.
package localengine

import (
	"context"
	"errors"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

const (
	DefaultLeaseDuration = 30 * time.Second
	DefaultPollInterval  = 2 * time.Second
	DefaultCommandLimit  = 128

	StepPending         = "pending"
	StepRunning         = "running"
	StepWaitingApproval = "waiting_approval"
	StepWaitingCapacity = "waiting_capacity"
	StepSucceeded       = "succeeded"
	StepFailed          = "failed"
	StepCancelled       = "cancelled"
)

var (
	ErrInvalidInput          = errors.New("invalid local orchestration input")
	ErrLeaseLost             = errors.New("local workflow lease was lost")
	ErrNoWork                = errors.New("no local workflow is ready")
	ErrRunnerEventHistoryGap = errors.New("runner runtime event history gap")
)

// ManifestCompiler is implemented by orchestration.Compiler. Keeping it as a
// local interface avoids coupling this package to the Temporal composition.
type ManifestCompiler interface {
	CompileRun(context.Context, httpapi.RunStartRequest) (workflow.Manifest, error)
}

// EnqueueInput is the durable handoff after a manifest has been compiled.
// Manifest and InputRef are immutable execution inputs and are persisted
// before Enqueue returns.
type EnqueueInput struct {
	Run      store.Run
	Manifest workflow.Manifest
	InputRef string
}

// Command is a durable public control command. References are retained, but
// prompts, credentials, and reply bytes never cross this boundary.
type Command struct {
	Kind           string `json:"kind"`
	Target         string `json:"target"`
	IdempotencyKey string `json:"idempotency_key"`
	Reason         string `json:"reason,omitempty"`
	ApprovalID     string `json:"approval_id,omitempty"`
	Decision       string `json:"decision,omitempty"`
	StepID         string `json:"step_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	ReplyRef       string `json:"reply_ref,omitempty"`
}

// StepState is the durable state for one manifest step.
type StepState struct {
	Phase      string               `json:"phase"`
	TaskID     string               `json:"task_id,omitempty"`
	ApprovalID string               `json:"approval_id,omitempty"`
	Attempts   int32                `json:"attempts,omitempty"`
	Output     workflow.ArtifactRef `json:"output,omitempty"`
	Error      string               `json:"error,omitempty"`
	StartedAt  time.Time            `json:"started_at,omitempty"`
}

// MachineState is the restartable local workflow state machine. The manifest
// itself is also stored in local_workflows.manifest; keeping it here makes a
// state snapshot self-describing and lets the worker validate consistency.
type MachineState struct {
	Version      int                             `json:"version"`
	Manifest     workflow.Manifest               `json:"manifest"`
	InputRef     string                          `json:"input_ref,omitempty"`
	Steps        map[string]StepState            `json:"steps"`
	Outputs      map[string]workflow.ArtifactRef `json:"outputs,omitempty"`
	CurrentStep  string                          `json:"current_step,omitempty"`
	PendingReply string                          `json:"pending_reply,omitempty"`
	CancelReason string                          `json:"cancel_reason,omitempty"`
	Condition    string                          `json:"condition,omitempty"`
	LastError    string                          `json:"last_error,omitempty"`
}

type claimedWorkflow struct {
	Scope             store.Scope
	RunID             string
	Manifest          workflow.Manifest
	InputRef          string
	State             MachineState
	Status            string
	LeaseOwner        string
	LeaseToken        string
	RunnerEventTaskID string
	RunnerEventCursor uint64
}

// Options controls worker timing. Zero values use conservative defaults.
type Options struct {
	LeaseDuration time.Duration
	PollInterval  time.Duration
	CommandLimit  int
	Now           func() time.Time
	// Scopes is the explicitly authorized tenant set this worker may poll.
	// Standalone installs normally provide one scope. Shared deployments should
	// provide ScopeProvider from their already-authorized tenancy path.
	Scopes        []store.Scope
	ScopeProvider func(context.Context) ([]store.Scope, error)
}

func (o Options) normalized() Options {
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = DefaultLeaseDuration
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.CommandLimit <= 0 || o.CommandLimit > 1024 {
		o.CommandLimit = DefaultCommandLimit
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}
