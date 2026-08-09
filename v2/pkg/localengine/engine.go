package localengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

// Engine is safe to run as one worker or as several owner-operated workers.
// The database lease is the concurrency boundary; RunnerActivities must be
// idempotent for the stable task keys supplied below.
type Engine struct {
	DB         *sql.DB
	Activities workflow.RunnerActivities
	Compiler   ManifestCompiler
	Options    Options
}

func New(db *sql.DB, activities workflow.RunnerActivities) *Engine {
	return &Engine{DB: db, Activities: activities, Options: Options{}.normalized()}
}

func (e *Engine) options() Options { return e.Options.normalized() }

// StartRun implements httpapi.RunStarter. Compilation is injected so the
// existing orchestration.Compiler can be reused without coupling this package
// to Temporal or changing the compiler package.
func (e *Engine) StartRun(ctx context.Context, request httpapi.RunStartRequest) error {
	if e.Compiler == nil {
		return errors.New("local manifest compiler is required")
	}
	manifest, err := e.Compiler.CompileRun(ctx, request)
	if err != nil {
		return fmt.Errorf("compile local workflow: %w", err)
	}
	return e.Enqueue(ctx, EnqueueInput{Run: request.Run, Manifest: manifest, InputRef: request.InputRef})
}

// SignalRun implements httpapi.RunSignaler by durably enqueueing a command.
// The HTTP handler normally supplies a SHA-256 idempotency-key digest. Direct
// callers may supply the raw key; it is hashed before it is persisted.
func (e *Engine) SignalRun(ctx context.Context, scope store.Scope, runID string, signal httpapi.RunSignal) error {
	command := Command{
		Kind: signal.Kind, Target: signal.Target, IdempotencyKey: signal.IdempotencyKey,
		Reason: signal.Reason, ApprovalID: signal.ApprovalID, Decision: signal.Decision,
		StepID: signal.StepID, TaskID: signal.TaskID, ReplyRef: signal.ReplyRef,
	}
	return e.EnqueueCommand(ctx, scope, runID, command)
}

// Enqueue persists a compiled workflow and its initial machine snapshot. The
// run must already exist (the HTTP control plane creates it first), and replay
// of the same run ID is accepted only when all immutable inputs match.
func (e *Engine) Enqueue(ctx context.Context, input EnqueueInput) error {
	if e.DB == nil {
		return errors.New("local engine database is required")
	}
	if err := input.Run.Scope.Validate(); err != nil {
		return err
	}
	if input.Run.ID == "" || input.Run.DefinitionDigest == "" || input.Run.RequestedBy == "" {
		return ErrInvalidInput
	}
	if err := validateInputRef(input.InputRef); err != nil {
		return err
	}
	plan, err := workflow.CompileManifest(input.Manifest)
	if err != nil {
		return fmt.Errorf("compile manifest: %w", err)
	}
	manifest := input.Manifest
	manifest.Name, manifest.Revision, manifest.Steps = plan.Name, plan.Revision, plan.Steps
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := store.ValidateJSONDocument(manifestJSON); err != nil {
		return fmt.Errorf("validate manifest: %w", err)
	}
	state := newMachineState(manifest, input.InputRef)
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode local state: %w", err)
	}
	if err := store.ValidateJSONDocument(stateJSON); err != nil {
		return fmt.Errorf("validate local state: %w", err)
	}

	return e.tenantTx(ctx, input.Run.Scope, func(tx *sql.Tx) error {
		var existingScope store.Scope
		var existingDigest, existingRequester, existingStatus, existingKind, existingID string
		err := tx.QueryRowContext(ctx, `
			SELECT id,organization_id::text,project_id::text,kind,definition_digest,requested_by,status::text
			FROM runs WHERE id=$1`, input.Run.ID).
			Scan(&existingID, &existingScope.OrganizationID, &existingScope.ProjectID, &existingKind,
				&existingDigest, &existingRequester, &existingStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read run for local enqueue: %w", err)
		}
		if existingScope != input.Run.Scope {
			return store.ErrConflict
		}
		if existingDigest != input.Run.DefinitionDigest || existingRequester != input.Run.RequestedBy || existingKind != input.Run.Kind {
			return fmt.Errorf("%w: run ID refers to different immutable input", store.ErrConflict)
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO local_workflows
			(run_id,organization_id,project_id,manifest,input_ref,state,status)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6::jsonb,$7::run_status)
			ON CONFLICT (run_id) DO NOTHING`, input.Run.ID, input.Run.OrganizationID, input.Run.ProjectID,
			string(manifestJSON), input.InputRef, string(stateJSON), existingStatus)
		if err != nil {
			return fmt.Errorf("enqueue local workflow: %w", err)
		}

		var storedManifest, storedInput, storedState string
		var storedStatus string
		err = tx.QueryRowContext(ctx, `
			SELECT manifest::text,input_ref,state::text,status::text
			FROM local_workflows WHERE run_id=$1 AND organization_id=$2 AND project_id=$3`,
			input.Run.ID, input.Run.OrganizationID, input.Run.ProjectID).
			Scan(&storedManifest, &storedInput, &storedState, &storedStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read local enqueue: %w", err)
		}
		if !store.JSONDocumentsEqual([]byte(storedManifest), manifestJSON) || storedInput != input.InputRef {
			return fmt.Errorf("%w: run ID refers to different local workflow", store.ErrConflict)
		}
		if _, err := decodeMachineState([]byte(storedState)); err != nil {
			return fmt.Errorf("stored local state is invalid: %w", err)
		}
		return nil
	})
}

// EnqueueCommand durably records one command. The unique tenant/run/key
// constraint makes retries idempotent; a reused key with a different command
// is rejected rather than silently retargeted.
func (e *Engine) EnqueueCommand(ctx context.Context, scope store.Scope, runID string, command Command) error {
	if e.DB == nil {
		return errors.New("local engine database is required")
	}
	if command.Target == "" {
		command.Target = "workflow"
	}
	if err := validateCommand(runID, command); err != nil {
		return err
	}
	command.IdempotencyKey = normalizeKey(command.IdempotencyKey)
	payload, err := json.Marshal(command)
	if err != nil {
		return err
	}
	if err := store.ValidateJSONDocument(payload); err != nil {
		return fmt.Errorf("validate local command: %w", err)
	}
	return e.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		var runExists string
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3`,
			scope.OrganizationID, scope.ProjectID, runID).Scan(&runExists); errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("check local signal run: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO local_workflow_commands
			(organization_id,project_id,run_id,idempotency_key_hash,kind,target,payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)
			ON CONFLICT (organization_id,project_id,run_id,idempotency_key_hash) DO NOTHING`,
			scope.OrganizationID, scope.ProjectID, runID, command.IdempotencyKey,
			command.Kind, command.Target, string(payload))
		if err != nil {
			return fmt.Errorf("enqueue local command: %w", err)
		}
		var stored string
		if err := tx.QueryRowContext(ctx, `
			SELECT payload::text FROM local_workflow_commands
			WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND idempotency_key_hash=$4`,
			scope.OrganizationID, scope.ProjectID, runID, command.IdempotencyKey).Scan(&stored); err != nil {
			return fmt.Errorf("read local command: %w", err)
		}
		if !store.JSONDocumentsEqual([]byte(stored), payload) {
			return fmt.Errorf("%w: command idempotency key was reused", store.ErrConflict)
		}
		return nil
	})
}

// RunOnce claims and advances one workflow. It returns false when no due work
// exists. A worker may call it concurrently from multiple processes.
func (e *Engine) RunOnce(ctx context.Context, workerID string) (bool, error) {
	if e.DB == nil || e.Activities == nil {
		return false, errors.New("local engine database and activities are required")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return false, ErrInvalidInput
	}
	scopes, err := e.authorizedScopes(ctx)
	if err != nil {
		return false, err
	}
	for _, scope := range scopes {
		claimed, claimErr := e.claim(ctx, scope, workerID)
		if errors.Is(claimErr, ErrNoWork) {
			continue
		}
		if claimErr != nil {
			return false, claimErr
		}
		return e.advanceClaimed(ctx, claimed)
	}
	return false, nil
}

func (e *Engine) advanceClaimed(ctx context.Context, claimed claimedWorkflow) (bool, error) {
	if err := validateMachineState(claimed.State); err != nil {
		_ = e.failClaim(ctx, claimed, "invalid_state", err.Error())
		return true, err
	}
	command, err := e.nextCommand(ctx, claimed)
	if err != nil {
		_ = e.release(ctx, claimed, e.options().PollInterval)
		return true, err
	}
	if command != nil {
		if err := e.processCommand(ctx, claimed, *command); err != nil {
			_ = e.release(ctx, claimed, e.options().PollInterval)
			return true, err
		}
		return true, nil
	}
	if claimed.State.CurrentStep != "" {
		if err := e.advanceCurrent(ctx, claimed); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := e.startNext(ctx, claimed); err != nil {
		return true, err
	}
	return true, nil
}

// Run continuously drains due workflows until the context is canceled.
func (e *Engine) Run(ctx context.Context, workerID string) error {
	options := e.options()
	for {
		worked, err := e.RunOnce(ctx, workerID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ctx.Err()
			}
			return err
		}
		if worked {
			continue
		}
		timer := time.NewTimer(options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (e *Engine) claim(ctx context.Context, scope store.Scope, workerID string) (claimedWorkflow, error) {
	options := e.options()
	token := randomToken()
	var result claimedWorkflow
	err := e.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		var manifestJSON, stateJSON string
		var expires time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT run_id,organization_id::text,project_id::text,manifest::text,input_ref,
			       state::text,status::text,runner_event_task_id,runner_event_cursor
			FROM local_workflows
			WHERE organization_id=$1 AND project_id=$2
			  AND status NOT IN ('Succeeded','Failed','Cancelled','Lost')
			  AND (wake_at IS NULL OR wake_at <= now())
			  AND (lease_expires_at IS NULL OR lease_expires_at <= now())
			ORDER BY created_at,run_id
			FOR UPDATE SKIP LOCKED LIMIT 1`,
			scope.OrganizationID, scope.ProjectID,
		).Scan(&result.RunID, &result.Scope.OrganizationID, &result.Scope.ProjectID,
			&manifestJSON, &result.InputRef, &stateJSON, &result.Status,
			&result.RunnerEventTaskID, &result.RunnerEventCursor)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("claim local workflow: %w", err)
		}
		if err := store.ValidateJSONDocument([]byte(manifestJSON)); err != nil {
			return fmt.Errorf("validate claimed manifest: %w", err)
		}
		if err := json.Unmarshal([]byte(manifestJSON), &result.Manifest); err != nil {
			return fmt.Errorf("decode claimed manifest: %w", err)
		}
		result.State, err = decodeMachineState([]byte(stateJSON))
		if err != nil {
			return fmt.Errorf("decode claimed state: %w", err)
		}
		expires = options.Now().Add(options.LeaseDuration)
		updated, err := tx.ExecContext(ctx, `
			UPDATE local_workflows SET lease_owner=$2,lease_token=$3,lease_expires_at=$4,updated_at=now()
			WHERE run_id=$1`, result.RunID, workerID, token, expires)
		if err != nil {
			return fmt.Errorf("lease local workflow: %w", err)
		}
		if count, _ := updated.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		result.LeaseOwner, result.LeaseToken = workerID, token
		return nil
	})
	if err != nil {
		return claimedWorkflow{}, err
	}
	return result, nil
}

func (e *Engine) authorizedScopes(ctx context.Context) ([]store.Scope, error) {
	var scopes []store.Scope
	if e.Options.ScopeProvider != nil {
		provided, err := e.Options.ScopeProvider(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve local worker scopes: %w", err)
		}
		scopes = append(scopes, provided...)
	} else {
		scopes = append(scopes, e.Options.Scopes...)
	}
	if len(scopes) == 0 {
		return nil, errors.New("local worker requires an authorized tenant scope or scope provider")
	}
	seen := make(map[store.Scope]struct{}, len(scopes))
	result := make([]store.Scope, 0, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	return result, nil
}

func (e *Engine) startNext(ctx context.Context, claimed claimedWorkflow) error {
	ready := readyStep(claimed.Manifest, claimed.State)
	if len(ready) == 0 {
		if allSucceeded(claimed.Manifest, claimed.State) {
			claimed.State.Condition = ""
			return e.persist(ctx, claimed, claimed.State, "Succeeded", time.Time{}, 0)
		}
		return e.failClaim(ctx, claimed, "unresolved_workflow", "no runnable steps remain")
	}
	step := ready[0]
	claimed.State.CurrentStep = step.ID
	stepState := claimed.State.Steps[step.ID]
	if step.Approval != nil {
		stepState.Phase = StepWaitingApproval
		stepState.ApprovalID = step.Approval.ID
		claimed.State.Steps[step.ID] = stepState
		// Keep the same lifecycle invariant as the Temporal workflow: a run
		// enters Running before it can wait for an approval signal.
		return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), 0)
	}
	stepState.Phase = StepPending
	stepState.Attempts++
	stepState.StartedAt = e.options().Now()
	claimed.State.Steps[step.ID] = stepState
	if err := e.persistHold(ctx, claimed, claimed.State, "Starting", e.wakeSoon()); err != nil {
		return err
	}
	return e.schedule(ctx, claimed, step)
}

func (e *Engine) schedule(ctx context.Context, claimed claimedWorkflow, step workflow.Step) error {
	stepState := claimed.State.Steps[step.ID]
	input := workflow.ScheduleRunnerTaskInput{
		OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
		RunID: claimed.RunID, WorkflowName: claimed.Manifest.Name, StepID: step.ID,
		AgentRef: step.AgentRef, InputRef: step.InputRef, DependencyOutput: dependencies(step, claimed.State),
		IdempotencyKey: dispatchKey(claimed.RunID, claimed.Manifest.Name, step.ID),
		Execution:      step.Execution, Contract: step.Contract,
	}
	callCtx, cancel := contextForStep(ctx, step)
	defer cancel()
	result, err := claimedActivities(e).ScheduleRunnerTask(callCtx, input)
	if err != nil {
		return e.handleActivityError(ctx, claimed, step, err)
	}
	switch result.Status {
	case "cooldown":
		if result.CooldownUntil.IsZero() || !result.CooldownUntil.After(e.options().Now()) {
			return e.failClaim(ctx, claimed, "invalid_runner_result", "cooldown must be in the future")
		}
		stepState.Phase = StepWaitingCapacity
		claimed.State.Steps[step.ID] = stepState
		return e.persist(ctx, claimed, claimed.State, "WaitingCapacity", result.CooldownUntil, 0)
	case "scheduled", "running":
		if strings.TrimSpace(result.TaskID) == "" {
			return e.failClaim(ctx, claimed, "invalid_runner_result", "scheduled task has no task ID")
		}
		stepState.Phase, stepState.TaskID = StepRunning, result.TaskID
		// Runtime sequences are scoped to a runner task. Reset the cursor when a
		// workflow advances to a new task, including after a retryable crash
		// before the schedule result was persisted.
		claimed.RunnerEventTaskID, claimed.RunnerEventCursor = result.TaskID, 0
		claimed.State.Steps[step.ID] = stepState
		return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), 0)
	case "succeeded":
		return e.completeStep(ctx, claimed, step, result.Output)
	default:
		return e.failClaim(ctx, claimed, "invalid_runner_result", "unknown schedule status "+result.Status)
	}
}

func (e *Engine) advanceCurrent(ctx context.Context, claimed claimedWorkflow) error {
	step, ok := findStep(claimed.Manifest, claimed.State.CurrentStep)
	if !ok {
		return e.failClaim(ctx, claimed, "invalid_state", "current step is not in the manifest")
	}
	stepState := claimed.State.Steps[step.ID]
	if stepState.Phase == StepWaitingCapacity {
		if err := e.persistHold(ctx, claimed, claimed.State, "Starting", e.wakeSoon()); err != nil {
			return err
		}
		stepState.Attempts++
		stepState.Phase = StepPending
		claimed.State.Steps[step.ID] = stepState
		return e.schedule(ctx, claimed, step)
	}
	if step.Approval != nil && stepState.Phase == StepWaitingApproval {
		return e.persist(ctx, claimed, claimed.State, "WaitingApproval", e.wakeSoon(), 0)
	}
	if stepState.Phase != StepRunning {
		return e.schedule(ctx, claimed, step)
	}
	callCtx, cancel := contextForStep(ctx, step)
	defer cancel()
	status, err := claimedActivities(e).StatusRunnerTask(callCtx, workflow.StatusRunnerTaskInput{
		OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
		RunID: claimed.RunID, TaskID: stepState.TaskID, AfterSequence: claimed.RunnerEventCursor,
	})
	if err != nil {
		return e.handleActivityError(ctx, claimed, step, err)
	}
	runnerEventCursor, err := e.persistRunnerRuntimeEvents(ctx, claimed, stepState.TaskID, status)
	if err != nil {
		if errors.Is(err, ErrRunnerEventHistoryGap) {
			return e.failClaim(ctx, claimed, "runner_event_history_gap", err.Error())
		}
		return err
	}
	claimed.RunnerEventTaskID, claimed.RunnerEventCursor = stepState.TaskID, runnerEventCursor
	switch status.Status {
	case "scheduled", "running":
		return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), 0)
	case "waiting_approval":
		if status.ApprovalID == "" {
			return e.failClaim(ctx, claimed, "invalid_runner_result", "runner approval has no approval ID")
		}
		stepState.Phase, stepState.ApprovalID = StepWaitingApproval, status.ApprovalID
		claimed.State.Steps[step.ID] = stepState
		return e.persist(ctx, claimed, claimed.State, "WaitingApproval", e.wakeSoon(), 0)
	case "succeeded":
		return e.completeStep(ctx, claimed, step, status.Output)
	case "failed":
		message := status.Error
		if message == "" {
			message = "runner reported agent failure"
		}
		return e.handleStepFailure(ctx, claimed, step, message, "runner_failed")
	case "cancelled":
		return e.finishCancelled(ctx, claimed, step.ID, "runner_cancelled", 0)
	case "lost":
		return e.finishTerminal(ctx, claimed, "Lost", "runner_lost", 0)
	default:
		return e.failClaim(ctx, claimed, "invalid_runner_result", "unknown status "+status.Status)
	}
}

func (e *Engine) processCommand(ctx context.Context, claimed claimedWorkflow, command localCommand) error {
	if command.Kind == "cancel" {
		if command.Target != "workflow" {
			return e.consumeOnly(ctx, claimed, command.ID)
		}
		if claimed.State.CurrentStep != "" {
			stepState := claimed.State.Steps[claimed.State.CurrentStep]
			if stepState.TaskID != "" && stepState.Phase != StepSucceeded && stepState.Phase != StepCancelled {
				callCtx, cancel := contextForStep(ctx, workflow.Step{})
				err := claimedActivities(e).CancelRunnerTask(callCtx, workflow.CancelRunnerTaskInput{
					OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
					RunID: claimed.RunID, TaskID: stepState.TaskID,
					IdempotencyKey: "cancel/" + stepState.TaskID,
				})
				cancel()
				if err != nil {
					return err
				}
			}
			stepState.Phase = StepCancelled
			claimed.State.Steps[claimed.State.CurrentStep] = stepState
		}
		claimed.State.CancelReason = command.Reason
		return e.persist(ctx, claimed, claimed.State, "Cancelled", time.Time{}, command.ID)
	}

	if claimed.State.CurrentStep == "" {
		return nil // retain a future-targeted command until its step is current
	}
	step, ok := findStep(claimed.Manifest, claimed.State.CurrentStep)
	if !ok {
		return e.failClaim(ctx, claimed, "invalid_state", "command targets an unknown current step")
	}
	stepState := claimed.State.Steps[step.ID]
	switch command.Kind {
	case "approval":
		if command.StepID != "" && command.StepID != step.ID {
			return nil
		}
		if command.ApprovalID == "" || command.ApprovalID != stepState.ApprovalID {
			return nil
		}
		if command.Decision != "approved" && command.Decision != "denied" {
			return e.consumeOnly(ctx, claimed, command.ID)
		}
		if command.Decision == "denied" {
			if step.AgentRef != "" && stepState.TaskID != "" {
				if err := claimedActivities(e).CancelRunnerTask(ctx, workflow.CancelRunnerTaskInput{
					OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
					RunID: claimed.RunID, TaskID: stepState.TaskID, IdempotencyKey: "cancel/" + stepState.TaskID,
				}); err != nil {
					return err
				}
				return e.finishCancelled(ctx, claimed, step.ID, "approval_denied", command.ID)
			}
			return e.finishTerminal(ctx, claimed, "Failed", "approval_denied", command.ID)
		}
		if step.AgentRef != "" {
			if stepState.TaskID == "" {
				return e.failClaim(ctx, claimed, "invalid_state", "approved agent has no task ID")
			}
			if err := claimedActivities(e).ResumeRunnerTask(ctx, workflow.ResumeRunnerTaskInput{
				OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
				RunID: claimed.RunID, TaskID: stepState.TaskID, Action: "approval",
				ApprovalID: command.ApprovalID, Decision: command.Decision,
				IdempotencyKey: "resume/" + stepState.TaskID + "/approval/" + command.ApprovalID,
			}); err != nil {
				return err
			}
			stepState.Phase = StepRunning
			stepState.ApprovalID = ""
			claimed.State.Steps[step.ID] = stepState
			return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), command.ID)
		}
		output := stepState.Output
		if output.ID == "" && claimed.State.PendingReply != "" {
			output = referenceOnly(claimed.State.PendingReply)
		}
		return e.completeStepWithCommand(ctx, claimed, step, output, command.ID)
	case "reply":
		if command.StepID != "" && command.StepID != step.ID {
			return nil
		}
		if command.ReplyRef == "" || (command.TaskID != "" && command.TaskID != stepState.TaskID) {
			return e.consumeOnly(ctx, claimed, command.ID)
		}
		if step.AgentRef == "" {
			claimed.State.PendingReply = command.ReplyRef
			return e.persist(ctx, claimed, claimed.State, "WaitingApproval", e.wakeSoon(), command.ID)
		}
		if stepState.TaskID == "" {
			return nil
		}
		if err := claimedActivities(e).ResumeRunnerTask(ctx, workflow.ResumeRunnerTaskInput{
			OrganizationID: claimed.Scope.OrganizationID, ProjectID: claimed.Scope.ProjectID,
			RunID: claimed.RunID, TaskID: stepState.TaskID, Action: "user-reply",
			Reference: command.ReplyRef, IdempotencyKey: "resume/" + stepState.TaskID + "/reply/" + command.ReplyRef,
		}); err != nil {
			return err
		}
		stepState.Phase = StepRunning
		stepState.ApprovalID = ""
		claimed.State.Steps[step.ID] = stepState
		return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), command.ID)
	}
	return e.consumeOnly(ctx, claimed, command.ID)
}

func (e *Engine) completeStep(ctx context.Context, claimed claimedWorkflow, step workflow.Step, output workflow.ArtifactRef) error {
	if isZeroArtifact(output) {
		return e.failClaim(ctx, claimed, "invalid_output", "agent succeeded without an output reference")
	}
	return e.completeStepWithCommand(ctx, claimed, step, output, 0)
}

func (e *Engine) completeStepWithCommand(ctx context.Context, claimed claimedWorkflow, step workflow.Step, output workflow.ArtifactRef, commandID int64) error {
	if !isZeroArtifact(output) {
		if err := output.Validate(); err != nil {
			return e.failClaim(ctx, claimed, "invalid_output", err.Error())
		}
	}
	stepState := claimed.State.Steps[step.ID]
	stepState.Phase, stepState.Output, stepState.Error = StepSucceeded, output, ""
	claimed.State.Steps[step.ID] = stepState
	if !isZeroArtifact(output) {
		claimed.State.Outputs[step.ID] = output
	}
	claimed.State.CurrentStep = ""
	claimed.State.PendingReply = ""
	if allSucceeded(claimed.Manifest, claimed.State) {
		return e.persist(ctx, claimed, claimed.State, "Succeeded", time.Time{}, commandID)
	}
	return e.persist(ctx, claimed, claimed.State, "Running", e.wakeSoon(), commandID)
}

func (e *Engine) handleStepFailure(ctx context.Context, claimed claimedWorkflow, step workflow.Step, message, condition string) error {
	state := claimed.State.Steps[step.ID]
	if state.Attempts < maxAttempts(step.Retry) {
		state.Phase, state.Error = StepPending, message
		claimed.State.Steps[step.ID] = state
		claimed.State.CurrentStep = ""
		return e.persist(ctx, claimed, claimed.State, "WaitingCapacity", e.retryWake(step.Retry, state.Attempts), 0)
	}
	state.Phase, state.Error = StepFailed, message
	claimed.State.Steps[step.ID] = state
	claimed.State.LastError = message
	claimed.State.Condition = condition
	claimed.State.CurrentStep = ""
	return e.persist(ctx, claimed, claimed.State, "Failed", time.Time{}, 0)
}

func (e *Engine) handleActivityError(ctx context.Context, claimed claimedWorkflow, step workflow.Step, activityErr error) error {
	return e.handleStepFailure(ctx, claimed, step, activityErr.Error(), "activity_failed")
}

func (e *Engine) finishCancelled(ctx context.Context, claimed claimedWorkflow, stepID, condition string, commandID int64) error {
	if stepID != "" {
		stepState := claimed.State.Steps[stepID]
		stepState.Phase = StepCancelled
		claimed.State.Steps[stepID] = stepState
	}
	claimed.State.CurrentStep = ""
	claimed.State.Condition = condition
	return e.persist(ctx, claimed, claimed.State, "Cancelled", time.Time{}, commandID)
}

func (e *Engine) finishTerminal(ctx context.Context, claimed claimedWorkflow, status, condition string, commandID int64) error {
	claimed.State.CurrentStep = ""
	claimed.State.Condition = condition
	if status == "Failed" {
		claimed.State.LastError = condition
	}
	return e.persist(ctx, claimed, claimed.State, status, time.Time{}, commandID)
}

func (e *Engine) failClaim(ctx context.Context, claimed claimedWorkflow, condition, message string) error {
	claimed.State.LastError = message
	claimed.State.Condition = condition
	claimed.State.CurrentStep = ""
	return e.persist(ctx, claimed, claimed.State, "Failed", time.Time{}, 0)
}

func (e *Engine) consumeOnly(ctx context.Context, claimed claimedWorkflow, commandID int64) error {
	return e.persist(ctx, claimed, claimed.State, claimed.Status, e.wakeSoon(), commandID)
}

func (e *Engine) persist(ctx context.Context, claimed claimedWorkflow, state MachineState, status string, wakeAt time.Time, commandID int64) error {
	return e.persistWithLease(ctx, claimed, state, status, wakeAt, commandID, false)
}

func (e *Engine) persistHold(ctx context.Context, claimed claimedWorkflow, state MachineState, status string, wakeAt time.Time) error {
	return e.persistWithLease(ctx, claimed, state, status, wakeAt, 0, true)
}

func (e *Engine) persistWithLease(ctx context.Context, claimed claimedWorkflow, state MachineState, status string, wakeAt time.Time, commandID int64, holdLease bool) error {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	options := e.options()
	return e.tenantTx(ctx, claimed.Scope, func(tx *sql.Tx) error {
		var currentStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status::text FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`,
			claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID).Scan(&currentStatus); errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lock local run: %w", err)
		}
		if currentStatus != status && !validTransition(currentStatus, status) {
			return fmt.Errorf("%w: invalid local transition %s -> %s", store.ErrConflict, currentStatus, status)
		}
		if wakeAt.IsZero() {
			wakeAt = time.Time{}
		}
		if status == "Succeeded" || status == "Failed" || status == "Cancelled" || status == "Lost" {
			wakeAt = time.Time{}
		}
		leaseExpiry := options.Now().Add(options.LeaseDuration)
		result, err := tx.ExecContext(ctx, `
			UPDATE local_workflows SET state=$4::jsonb,status=$5::run_status,wake_at=$6,
				runner_event_task_id=$7,runner_event_cursor=$8,
				lease_owner=CASE WHEN $9::bigint=1 THEN lease_owner ELSE NULL END,
				lease_token=CASE WHEN $9::bigint=1 THEN lease_token ELSE NULL END,
				lease_expires_at=CASE WHEN $9::bigint=1 THEN $10::timestamptz ELSE NULL END,
				updated_at=now()
			WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND lease_token=$11`,
			claimed.RunID, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, string(stateJSON), status,
			nullTime(wakeAt), claimed.RunnerEventTaskID, int64(claimed.RunnerEventCursor),
			boolInt(holdLease), leaseExpiry, claimed.LeaseToken)
		if err != nil {
			return fmt.Errorf("persist local state: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		if currentStatus != status {
			payload, _ := json.Marshal(map[string]string{"status": status, "condition": state.Condition})
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=$4::run_status,condition=NULLIF($5,''),updated_at=now() WHERE organization_id=$1 AND project_id=$2 AND id=$3`,
				claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID, status, state.Condition); err != nil {
				return fmt.Errorf("persist run status: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_events (organization_id,project_id,run_id,event_type,payload) VALUES ($1,$2,$3,'run.status_changed',$4::jsonb)`,
				claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID, string(payload)); err != nil {
				return fmt.Errorf("persist run status event: %w", err)
			}
		}
		if commandID != 0 {
			result, err := tx.ExecContext(ctx, `UPDATE local_workflow_commands SET state='consumed',consumed_at=now() WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND run_id=$4 AND state='pending'`,
				commandID, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID)
			if err != nil {
				return fmt.Errorf("ack local command: %w", err)
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrLeaseLost
			}
		}
		return nil
	})
}

const runnerRuntimeEventSourcePrefix = "runner-runtime:"

// persistRunnerRuntimeEvents commits the runner source events and the source
// cursor in one tenant-scoped, lease-fenced transaction. Replaying a status
// response is harmless: the task-qualified source sequence is unique and the
// existing payload is compared before the cursor is advanced.
func (e *Engine) persistRunnerRuntimeEvents(ctx context.Context, claimed claimedWorkflow, taskID string, status workflow.StatusRunnerTaskResult) (uint64, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || len(taskID) > workflow.MaxReferenceSize || strings.ContainsAny(taskID, "\r\n\x00") {
		return claimed.RunnerEventCursor, errors.New("runner task ID is not bounded")
	}
	if claimed.RunnerEventTaskID != "" && claimed.RunnerEventTaskID != taskID {
		return claimed.RunnerEventCursor, fmt.Errorf("%w: runner task changed while leased", store.ErrConflict)
	}
	runnerEventSource := runnerRuntimeEventSourcePrefix + taskID
	targetCursor := status.EventCursor
	var highestEvent uint64
	for index, event := range status.Events {
		if err := validateRunnerRuntimeEvent(event); err != nil {
			return claimed.RunnerEventCursor, fmt.Errorf("runner runtime event %d: %w", index, err)
		}
		if index > 0 && event.Sequence <= status.Events[index-1].Sequence {
			return claimed.RunnerEventCursor, errors.New("runner runtime events are not ordered")
		}
		if event.Sequence > highestEvent {
			highestEvent = event.Sequence
		}
	}
	if targetCursor != 0 && targetCursor < highestEvent {
		return claimed.RunnerEventCursor, errors.New("runner event cursor precedes returned events")
	}
	if targetCursor == 0 {
		targetCursor = highestEvent
	}
	if targetCursor < claimed.RunnerEventCursor {
		// Older runner versions do not return a cursor. They also cannot return
		// runtime events, so retain the durable cursor instead of regressing it.
		targetCursor = claimed.RunnerEventCursor
	}
	if status.EventHistoryTruncated && status.EventHistoryStart > claimed.RunnerEventCursor && status.EventHistoryStart-claimed.RunnerEventCursor > 1 {
		return claimed.RunnerEventCursor, fmt.Errorf("%w: history starts at source sequence %d after cursor %d", ErrRunnerEventHistoryGap, status.EventHistoryStart, claimed.RunnerEventCursor)
	}
	if targetCursor > uint64(math.MaxInt64) {
		return claimed.RunnerEventCursor, errors.New("runner event cursor exceeds database range")
	}
	for _, event := range status.Events {
		if event.Sequence > uint64(math.MaxInt64) {
			return claimed.RunnerEventCursor, errors.New("runner event sequence exceeds database range")
		}
	}
	for index := range status.Events {
		normalizedPayload, normalizeErr := store.NormalizeJSONDocument(status.Events[index].Payload)
		if normalizeErr != nil {
			return claimed.RunnerEventCursor, fmt.Errorf("normalize runner runtime event %d: %w", status.Events[index].Sequence, normalizeErr)
		}
		status.Events[index].Payload = normalizedPayload
	}

	err := e.tenantTx(ctx, claimed.Scope, func(tx *sql.Tx) error {
		var databaseTaskID string
		var databaseCursor int64
		if err := tx.QueryRowContext(ctx, `
			SELECT runner_event_task_id,runner_event_cursor FROM local_workflows
			WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND lease_token=$4
			FOR UPDATE`, claimed.RunID, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.LeaseToken).Scan(&databaseTaskID, &databaseCursor); errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		} else if err != nil {
			return fmt.Errorf("lock runner event cursor: %w", err)
		}
		if databaseCursor < 0 {
			return errors.New("stored runner event cursor is negative")
		}
		if databaseTaskID != "" && databaseTaskID != taskID {
			return fmt.Errorf("%w: durable runner cursor belongs to task %q", store.ErrConflict, databaseTaskID)
		}
		if databaseTaskID == "" && databaseCursor != 0 {
			return errors.New("stored runner cursor has no task identity")
		}
		if databaseTaskID == "" {
			databaseTaskID = taskID
		}
		if uint64(databaseCursor) > targetCursor {
			targetCursor = uint64(databaseCursor)
		}
		for _, event := range status.Events {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_events
				(organization_id,project_id,run_id,event_type,payload,source,source_sequence)
					VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)
					ON CONFLICT (organization_id,project_id,run_id,source,source_sequence) DO NOTHING`,
				claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID,
				event.Type, string(event.Payload), runnerEventSource, int64(event.Sequence)); err != nil {
				return fmt.Errorf("persist runner event %d: %w", event.Sequence, err)
			}
			var storedType string
			var storedPayload string
			if err := tx.QueryRowContext(ctx, `
					SELECT event_type,payload::text FROM run_events
					WHERE organization_id=$1 AND project_id=$2 AND run_id=$3
					  AND source=$4 AND source_sequence=$5`,
				claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID,
				runnerEventSource, int64(event.Sequence)).Scan(&storedType, &storedPayload); err != nil {
				return fmt.Errorf("verify runner event %d: %w", event.Sequence, err)
			}
			if storedType != event.Type || !store.JSONDocumentsEqual([]byte(storedPayload), event.Payload) {
				return fmt.Errorf("%w: runner source sequence %d was reused with different data", store.ErrConflict, event.Sequence)
			}
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE local_workflows SET runner_event_task_id=$4,runner_event_cursor=$5,updated_at=now()
			WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND lease_token=$6`,
			claimed.RunID, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, databaseTaskID, int64(targetCursor), claimed.LeaseToken)
		if err != nil {
			return fmt.Errorf("advance runner event cursor: %w", err)
		}
		if count, _ := updated.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		return nil
	})
	if err != nil {
		return claimed.RunnerEventCursor, err
	}
	return targetCursor, nil
}

func validateRunnerRuntimeEvent(event workflow.RunnerRuntimeEvent) error {
	if event.Sequence == 0 || strings.TrimSpace(event.Type) == "" {
		return errors.New("event sequence and type are required")
	}
	data := strings.TrimSpace(string(event.Payload))
	if data == "" || data[0] != '{' || store.ValidateJSONDocument([]byte(data)) != nil {
		return errors.New("event payload must be a JSON object")
	}
	if len(data) > 768<<10 {
		return errors.New("event payload exceeds the runtime bound")
	}
	return nil
}

func (e *Engine) release(ctx context.Context, claimed claimedWorkflow, delay time.Duration) error {
	return e.tenantTx(ctx, claimed.Scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE local_workflows SET wake_at=$4,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=now() WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND lease_token=$5`,
			claimed.RunID, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, e.options().Now().Add(delay), claimed.LeaseToken)
		return err
	})
}

func (e *Engine) nextCommand(ctx context.Context, claimed claimedWorkflow) (*localCommand, error) {
	var commands []localCommand
	err := e.tenantTx(ctx, claimed.Scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,kind,target,payload::text FROM local_workflow_commands WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND state='pending' ORDER BY id LIMIT $4`, claimed.Scope.OrganizationID, claimed.Scope.ProjectID, claimed.RunID, e.options().CommandLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var command localCommand
			var payload string
			if err := rows.Scan(&command.ID, &command.Kind, &command.Target, &payload); err != nil {
				return err
			}
			if err := store.ValidateJSONDocument([]byte(payload)); err != nil {
				return fmt.Errorf("validate stored local command: %w", err)
			}
			if err := json.Unmarshal([]byte(payload), &command.Command); err != nil {
				return fmt.Errorf("decode local command: %w", err)
			}
			commands = append(commands, command)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for i := range commands {
		if commandApplies(commands[i].Command, claimed) {
			return &commands[i], nil
		}
	}
	return nil, nil
}

func (e *Engine) tenantTx(ctx context.Context, scope store.Scope, fn func(*sql.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('agw.organization_id',$1,true),set_config('agw.project_id',$2,true)`, scope.OrganizationID, scope.ProjectID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func claimedActivities(e *Engine) workflow.RunnerActivities { return e.Activities }

type localCommand struct {
	ID int64
	Command
}

func newMachineState(manifest workflow.Manifest, inputRef string) MachineState {
	steps := make(map[string]StepState, len(manifest.Steps))
	for _, step := range manifest.Steps {
		steps[step.ID] = StepState{Phase: StepPending}
	}
	return MachineState{Version: 1, Manifest: manifest, InputRef: inputRef, Steps: steps, Outputs: map[string]workflow.ArtifactRef{}}
}

func decodeMachineState(data []byte) (MachineState, error) {
	if err := store.ValidateJSONDocument(data); err != nil {
		return MachineState{}, err
	}
	var state MachineState
	if err := json.Unmarshal(data, &state); err != nil {
		return MachineState{}, err
	}
	if state.Steps == nil {
		state.Steps = map[string]StepState{}
	}
	if state.Outputs == nil {
		state.Outputs = map[string]workflow.ArtifactRef{}
	}
	return state, validateMachineState(state)
}

func validateMachineState(state MachineState) error {
	if state.Version != 1 || state.Manifest.Name == "" || len(state.Manifest.Steps) == 0 || state.Steps == nil {
		return ErrInvalidInput
	}
	if _, err := workflow.CompileManifest(state.Manifest); err != nil {
		return err
	}
	for _, step := range state.Manifest.Steps {
		current, ok := state.Steps[step.ID]
		if !ok {
			return fmt.Errorf("missing state for step %q", step.ID)
		}
		switch current.Phase {
		case StepPending, StepRunning, StepWaitingApproval, StepWaitingCapacity, StepSucceeded, StepFailed, StepCancelled:
		default:
			return fmt.Errorf("unknown step phase %q", current.Phase)
		}
	}
	return nil
}

func readyStep(manifest workflow.Manifest, state MachineState) []workflow.Step {
	for _, step := range manifest.Steps {
		if state.Steps[step.ID].Phase != StepPending {
			continue
		}
		ready := true
		for _, dependency := range step.Needs {
			if state.Steps[dependency].Phase != StepSucceeded {
				ready = false
				break
			}
		}
		if ready {
			return []workflow.Step{step}
		}
	}
	return nil
}

func allSucceeded(manifest workflow.Manifest, state MachineState) bool {
	for _, step := range manifest.Steps {
		if state.Steps[step.ID].Phase != StepSucceeded {
			return false
		}
	}
	return true
}

func dependencies(step workflow.Step, state MachineState) []workflow.ArtifactRef {
	result := make([]workflow.ArtifactRef, 0, len(step.Needs))
	for _, need := range step.Needs {
		if output, ok := state.Outputs[need]; ok {
			result = append(result, output)
		}
	}
	return result
}

func findStep(manifest workflow.Manifest, id string) (workflow.Step, bool) {
	for _, step := range manifest.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return workflow.Step{}, false
}

func commandApplies(command Command, claimed claimedWorkflow) bool {
	if command.Kind == "cancel" {
		return command.Target == "workflow"
	}
	if claimed.State.CurrentStep == "" {
		return false
	}
	step, ok := findStep(claimed.Manifest, claimed.State.CurrentStep)
	if !ok {
		return false
	}
	if command.StepID != "" && command.StepID != step.ID {
		return false
	}
	current := claimed.State.Steps[step.ID]
	switch command.Kind {
	case "approval":
		if step.AgentRef != "" && command.Target != "agent" {
			return false
		}
		if step.Approval != nil && command.Target != "workflow" {
			return false
		}
		return command.ApprovalID != "" && command.ApprovalID == current.ApprovalID
	case "reply":
		if step.AgentRef != "" && command.Target != "agent" {
			return false
		}
		if step.Approval != nil && command.Target != "workflow" {
			return false
		}
		return command.ReplyRef != "" && (command.TaskID == "" || command.TaskID == current.TaskID)
	default:
		return false
	}
}

func validateCommand(runID string, command Command) error {
	if strings.TrimSpace(runID) == "" || command.Kind == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return ErrInvalidInput
	}
	if command.Target == "" {
		command.Target = "workflow"
	}
	if command.Target != "workflow" && command.Target != "agent" {
		return ErrInvalidInput
	}
	if command.Kind != "cancel" && command.Kind != "approval" && command.Kind != "reply" {
		return ErrInvalidInput
	}
	if command.Kind == "cancel" && command.Target != "workflow" {
		return ErrInvalidInput
	}
	for name, value := range map[string]string{"reason": command.Reason, "approval_id": command.ApprovalID, "decision": command.Decision, "step_id": command.StepID, "task_id": command.TaskID, "reply_ref": command.ReplyRef} {
		if len(value) > workflow.MaxReferenceSize || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s is not bounded", name)
		}
	}
	if command.Kind == "approval" && command.Decision != "approved" && command.Decision != "denied" {
		return ErrInvalidInput
	}
	if command.Kind == "reply" && strings.TrimSpace(command.ReplyRef) == "" {
		return ErrInvalidInput
	}
	return nil
}

func normalizeKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 64 {
		if _, err := hex.DecodeString(value); err == nil {
			return strings.ToLower(value)
		}
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func dispatchKey(runID, workflowName, stepID string) string {
	return runID + "/" + workflowName + "/" + stepID
}

func contextForStep(ctx context.Context, step workflow.Step) (context.Context, context.CancelFunc) {
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return context.WithTimeout(ctx, timeout)
}

func maxAttempts(policy workflow.RetryPolicy) int32 {
	if policy.MaxAttempts <= 0 {
		return 1
	}
	return policy.MaxAttempts
}

func (e *Engine) retryWake(policy workflow.RetryPolicy, attempts int32) time.Time {
	interval := policy.InitialInterval
	if interval <= 0 {
		interval = time.Second
	}
	coefficient := policy.BackoffCoefficient
	if coefficient < 1 {
		coefficient = 2
	}
	for i := int32(1); i < attempts; i++ {
		interval = time.Duration(float64(interval) * coefficient)
		if policy.MaximumInterval > 0 && interval > policy.MaximumInterval {
			interval = policy.MaximumInterval
			break
		}
	}
	return e.options().Now().Add(interval)
}

func (e *Engine) wakeSoon() time.Time { return e.options().Now().Add(e.options().PollInterval) }

func validTransition(from, to string) bool {
	if from == to {
		return true
	}
	if from == "Succeeded" || from == "Failed" || from == "Cancelled" || from == "Lost" {
		return false
	}
	switch to {
	case "Failed", "Cancelled", "Lost":
		return true
	case "Scheduled":
		return from == "Pending" || from == "WaitingCapacity"
	case "Starting":
		return from == "Pending" || from == "Scheduled" || from == "Running" || from == "WaitingCapacity"
	case "Running":
		return from == "Pending" || from == "Scheduled" || from == "Starting" || from == "WaitingApproval" || from == "WaitingCapacity"
	case "WaitingApproval":
		return from == "Scheduled" || from == "Starting" || from == "Running"
	case "WaitingCapacity":
		return from == "Pending" || from == "Scheduled" || from == "Starting" || from == "Running"
	case "Succeeded":
		return from == "Scheduled" || from == "Starting" || from == "Running" || from == "WaitingApproval"
	default:
		return false
	}
}

func referenceOnly(value string) workflow.ArtifactRef {
	return workflow.ArtifactRef{ID: "reply-reference", URI: value, Digest: "sha256:" + strings.Repeat("0", 64), MediaType: "application/reference"}
}

func isZeroArtifact(ref workflow.ArtifactRef) bool {
	return ref.ID == "" && ref.URI == "" && ref.Digest == "" && ref.SizeBytes == 0 && ref.MediaType == ""
}

func validateInputRef(value string) error {
	if len(value) > workflow.MaxReferenceSize || strings.ContainsAny(value, "\r\n\x00") {
		return ErrInvalidInput
	}
	return nil
}

func randomToken() string {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes[:])
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
