// Package workflow contains the Temporal execution layer for Agents Gateway
// v2. It deliberately carries references, not prompts, secrets, or payloads.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

const (
	WorkflowName     = "agents-gateway.v2.WorkflowRun"
	AgentRunName     = "agents-gateway.v2.AgentRun"
	ActivitySchedule = "agents-gateway.v2.ScheduleRunnerTask"
	ActivityCancel   = "agents-gateway.v2.CancelRunnerTask"
	ActivityResume   = "agents-gateway.v2.ResumeRunnerTask"
	ActivityStatus   = "agents-gateway.v2.StatusRunnerTask"

	SignalApproval    = "agents-gateway.v2.approval"
	SignalUserReply   = "agents-gateway.v2.user-reply"
	SignalRunnerEvent = "agents-gateway.v2.runner-event"
	SignalCancel      = "agents-gateway.v2.cancel"

	SearchAttributeOrganization = "AGWOrganizationID"
	SearchAttributeProject      = "AGWProjectID"
	SearchAttributeRunID        = "AGWRunID"
	SearchAttributeWorkflow     = "AGWWorkflowName"
	SearchAttributeStep         = "AGWStepID"
	SearchAttributeAgent        = "AGWAgentRef"
	SearchAttributeStatus       = "AGWStatus"

	MaxManifestSteps = 256
	MaxReferenceSize = 4096
)

// Manifest is the immutable, compiled input to Workflow. A manifest contains
// only configuration and references to external immutable objects.
type Manifest struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Steps    []Step `json:"steps"`
}

type Step struct {
	ID        string             `json:"id"`
	AgentRef  string             `json:"agent_ref,omitempty"`
	Needs     []string           `json:"needs,omitempty"`
	InputRef  string             `json:"input_ref,omitempty"`
	Timeout   time.Duration      `json:"timeout,omitempty"`
	Retry     RetryPolicy        `json:"retry,omitempty"`
	Approval  *ApprovalSpec      `json:"approval,omitempty"`
	Execution runner.SandboxSpec `json:"execution,omitempty"`
	Contract  ExecutionContract  `json:"contract,omitempty"`
}

// ExecutionContract pins all non-secret policy/configuration inputs needed by
// an adapter. Values are immutable revision references plus bounded declarative
// instructions; runtime prompts/input payloads, credentials, and artifact bytes
// remain outside Temporal history.
type ExecutionContract struct {
	Agent           RevisionRef            `json:"agent"`
	SandboxProfile  RevisionRef            `json:"sandbox_profile"`
	SkillSet        *RevisionRef           `json:"skill_set,omitempty"`
	ToolSet         *RevisionRef           `json:"tool_set,omitempty"`
	ModelRoute      *RevisionRef           `json:"model_route,omitempty"`
	InlineSkills    []PinnedReference      `json:"inline_skills,omitempty"`
	InstructionsRef string                 `json:"instructions_ref"`
	Instructions    string                 `json:"instructions"`
	VerificationRef string                 `json:"verification_ref,omitempty"`
	Verification    []string               `json:"verification,omitempty"`
	Environment     []EnvironmentReference `json:"environment,omitempty"`
}

type RevisionRef struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type PinnedReference struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type EnvironmentReference struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

func (c ExecutionContract) Validate() error {
	if err := c.Agent.validate("agent"); err != nil {
		return err
	}
	if err := c.SandboxProfile.validate("sandbox profile"); err != nil {
		return err
	}
	for name, ref := range map[string]*RevisionRef{"skill set": c.SkillSet, "tool set": c.ToolSet, "model route": c.ModelRoute} {
		if ref != nil {
			if err := ref.validate(name); err != nil {
				return err
			}
		}
	}
	if err := validateReference(c.InstructionsRef, "instructions reference"); err != nil {
		return err
	}
	if strings.TrimSpace(c.Instructions) == "" || len(c.Instructions) > 64<<10 {
		return errors.New("execution instructions must be non-empty and at most 64 KiB")
	}
	if err := validateOptionalReference(c.VerificationRef, "verification reference"); err != nil {
		return err
	}
	if len(c.InlineSkills) > 256 || len(c.Environment) > 256 {
		return errors.New("execution contract contains too many references")
	}
	if len(c.Verification) > 64 {
		return errors.New("execution contract contains too many verification commands")
	}
	for _, command := range c.Verification {
		if strings.TrimSpace(command) == "" || len(command) > MaxReferenceSize || strings.ContainsRune(command, '\x00') {
			return errors.New("verification command must be bounded and non-empty")
		}
	}
	for _, skill := range c.InlineSkills {
		if err := validateReference(skill.Ref, "skill reference"); err != nil {
			return err
		}
		if !validDigest(skill.Digest) {
			return errors.New("skill reference digest must be sha256")
		}
	}
	for _, variable := range c.Environment {
		if err := validateReference(variable.Name, "environment name"); err != nil {
			return err
		}
		if err := validateReference(variable.Ref, "environment value reference"); err != nil {
			return err
		}
	}
	return nil
}

func (r RevisionRef) validate(name string) error {
	if err := validateReference(r.Kind, name+" kind"); err != nil {
		return err
	}
	if err := validateReference(r.Name, name+" name"); err != nil {
		return err
	}
	if !validDigest(r.Digest) {
		return fmt.Errorf("%s digest must be sha256", name)
	}
	return nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, char := range strings.TrimPrefix(value, "sha256:") {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

type ApprovalSpec struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Role   string `json:"role,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts        int32         `json:"max_attempts,omitempty"`
	InitialInterval    time.Duration `json:"initial_interval,omitempty"`
	BackoffCoefficient float64       `json:"backoff_coefficient,omitempty"`
	MaximumInterval    time.Duration `json:"maximum_interval,omitempty"`
}

type WorkflowInput struct {
	OrganizationID string   `json:"organization_id"`
	ProjectID      string   `json:"project_id"`
	RunID          string   `json:"run_id"`
	Manifest       Manifest `json:"manifest"`
	InputRef       string   `json:"input_ref,omitempty"`
}

type WorkflowResult struct {
	RunID   string                 `json:"run_id"`
	Outputs map[string]ArtifactRef `json:"outputs,omitempty"`
}

type AgentRunInput struct {
	OrganizationID   string             `json:"organization_id"`
	ProjectID        string             `json:"project_id"`
	RunID            string             `json:"run_id"`
	WorkflowName     string             `json:"workflow_name"`
	StepID           string             `json:"step_id"`
	AgentRef         string             `json:"agent_ref"`
	InputRef         string             `json:"input_ref,omitempty"`
	DependencyOutput []ArtifactRef      `json:"dependency_output,omitempty"`
	Timeout          time.Duration      `json:"timeout,omitempty"`
	Retry            RetryPolicy        `json:"retry,omitempty"`
	Execution        runner.SandboxSpec `json:"execution,omitempty"`
	Contract         ExecutionContract  `json:"contract"`
}

type AgentRunResult struct {
	TaskID string      `json:"task_id"`
	Output ArtifactRef `json:"output"`
}

// ArtifactRef is an immutable reference to data held outside Temporal
// history. The referenced object is addressed by both a stable ID/URI and a
// content digest; the workflow never carries the object bytes.
type ArtifactRef struct {
	ID        string `json:"id"`
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type,omitempty"`
}

func (r ArtifactRef) Validate() error {
	if err := validateReference(r.ID, "artifact id"); err != nil {
		return err
	}
	if err := validateReference(r.URI, "artifact uri"); err != nil {
		return err
	}
	if !strings.HasPrefix(r.Digest, "sha256:") || len(r.Digest) != len("sha256:")+64 {
		return errors.New("artifact digest must be a sha256 digest")
	}
	if r.SizeBytes < 0 {
		return errors.New("artifact size must not be negative")
	}
	return nil
}

type ScheduleRunnerTaskInput struct {
	OrganizationID   string             `json:"organization_id"`
	ProjectID        string             `json:"project_id"`
	RunID            string             `json:"run_id"`
	WorkflowName     string             `json:"workflow_name"`
	StepID           string             `json:"step_id"`
	AgentRef         string             `json:"agent_ref"`
	InputRef         string             `json:"input_ref,omitempty"`
	DependencyOutput []ArtifactRef      `json:"dependency_output,omitempty"`
	IdempotencyKey   string             `json:"idempotency_key"`
	Execution        runner.SandboxSpec `json:"execution"`
	Contract         ExecutionContract  `json:"contract"`
}

type ScheduleRunnerTaskResult struct {
	Status        string      `json:"status"` // scheduled, succeeded, or cooldown
	TaskID        string      `json:"task_id,omitempty"`
	CooldownUntil time.Time   `json:"cooldown_until,omitempty"`
	Output        ArtifactRef `json:"output,omitempty"`
}

type CancelRunnerTaskInput struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ResumeRunnerTaskInput struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	Action         string `json:"action"` // approval or user-reply
	ApprovalID     string `json:"approval_id,omitempty"`
	Decision       string `json:"decision,omitempty"`
	Reference      string `json:"reference,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

type StatusRunnerTaskInput struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	AfterSequence  uint64 `json:"after_sequence,omitempty"`
}

// RunnerRuntimeEvent is the bounded, privacy-safe projection of one validated
// runtime-protocol event. Sequence is the runner's source sequence, while
// Payload deliberately keeps the runtime event data shape unchanged for the
// control-plane/API event consumers.
type RunnerRuntimeEvent struct {
	Sequence uint64          `json:"sequence"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Terminal bool            `json:"terminal,omitempty"`
}

type StatusRunnerTaskResult struct {
	Status                string               `json:"status"` // scheduled, running, waiting_approval, succeeded, failed, cancelled, lost
	ApprovalID            string               `json:"approval_id,omitempty"`
	Output                ArtifactRef          `json:"output,omitempty"`
	Error                 string               `json:"error,omitempty"`
	Events                []RunnerRuntimeEvent `json:"events,omitempty"`
	EventCursor           uint64               `json:"event_cursor,omitempty"`
	EventHistoryStart     uint64               `json:"event_history_start,omitempty"`
	EventHistoryTruncated bool                 `json:"event_history_truncated,omitempty"`
}

// RunnerActivities is the narrow side-effect boundary implemented by the
// host runner adapter. ScheduleRunnerTask must use IdempotencyKey to dedupe a
// dispatch whose outcome is ambiguous. Cancel and Resume are also idempotent.
type RunnerActivities interface {
	ScheduleRunnerTask(context.Context, ScheduleRunnerTaskInput) (ScheduleRunnerTaskResult, error)
	CancelRunnerTask(context.Context, CancelRunnerTaskInput) error
	ResumeRunnerTask(context.Context, ResumeRunnerTaskInput) error
	StatusRunnerTask(context.Context, StatusRunnerTaskInput) (StatusRunnerTaskResult, error)
}

type ActivityHandlers struct{ Impl RunnerActivities }

func (h ActivityHandlers) ScheduleRunnerTask(ctx context.Context, in ScheduleRunnerTaskInput) (ScheduleRunnerTaskResult, error) {
	if h.Impl == nil {
		return ScheduleRunnerTaskResult{}, errors.New("runner activity implementation is nil")
	}
	return h.Impl.ScheduleRunnerTask(ctx, in)
}

func (h ActivityHandlers) CancelRunnerTask(ctx context.Context, in CancelRunnerTaskInput) error {
	if h.Impl == nil {
		return errors.New("runner activity implementation is nil")
	}
	return h.Impl.CancelRunnerTask(ctx, in)
}

func (h ActivityHandlers) ResumeRunnerTask(ctx context.Context, in ResumeRunnerTaskInput) error {
	if h.Impl == nil {
		return errors.New("runner activity implementation is nil")
	}
	return h.Impl.ResumeRunnerTask(ctx, in)
}

func (h ActivityHandlers) StatusRunnerTask(ctx context.Context, in StatusRunnerTaskInput) (StatusRunnerTaskResult, error) {
	if h.Impl == nil {
		return StatusRunnerTaskResult{}, errors.New("runner activity implementation is nil")
	}
	return h.Impl.StatusRunnerTask(ctx, in)
}

type ApprovalSignal struct {
	ApprovalID string      `json:"approval_id"`
	StepID     string      `json:"step_id"`
	Decision   string      `json:"decision"` // approved or denied
	Output     ArtifactRef `json:"output,omitempty"`
}

type UserReplySignal struct {
	RunID    string `json:"run_id"`
	StepID   string `json:"step_id"`
	TaskID   string `json:"task_id,omitempty"`
	ReplyRef string `json:"reply_ref"`
}

type RunnerEventSignal struct {
	TaskID     string      `json:"task_id"`
	Status     string      `json:"status"` // succeeded, failed, waiting_approval
	ApprovalID string      `json:"approval_id,omitempty"`
	Output     ArtifactRef `json:"output,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type CancelSignal struct {
	Reason string `json:"reason,omitempty"`
}

func validateReference(value, field string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > MaxReferenceSize || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s is not a bounded reference", field)
	}
	return nil
}

func validateInputReference(value, field string) error {
	if value == "" {
		return nil
	}
	return validateReference(value, field)
}

func validateOptionalReference(value, field string) error {
	if value == "" {
		return nil
	}
	return validateReference(value, field)
}
