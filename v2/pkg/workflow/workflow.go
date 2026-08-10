package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// Workflow executes the compiled manifest as a deterministic dependency DAG.
// It never asks a model to choose the next step: readiness is derived solely
// from the manifest and completed step states.
func Workflow(ctx temporalworkflow.Context, input WorkflowInput) (result WorkflowResult, returnedErr error) {
	plan, err := CompileManifest(input.Manifest)
	if err != nil {
		return WorkflowResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidManifest", err)
	}
	if err := validateWorkflowInput(input); err != nil {
		return WorkflowResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidWorkflowInput", err)
	}
	if err := setRunState(ctx, input, "Running", ""); err != nil {
		return WorkflowResult{}, err
	}
	defer func() {
		status, condition := "Succeeded", ""
		if returnedErr != nil {
			status, condition = "Failed", "workflow_failed"
			if temporal.IsCanceledError(returnedErr) || errors.Is(returnedErr, temporalworkflow.ErrCanceled) {
				status, condition = "Cancelled", "workflow_cancelled"
			}
		}
		finalCtx, _ := temporalworkflow.NewDisconnectedContext(ctx)
		if stateErr := setRunState(finalCtx, input, status, condition); stateErr != nil && returnedErr == nil {
			result, returnedErr = WorkflowResult{}, stateErr
		}
	}()
	if err := upsertWorkflowSearchAttributes(ctx, input, plan.Name, "Pending", ""); err != nil {
		return WorkflowResult{}, err
	}

	phases := make(map[string]stepPhase, len(plan.Steps))
	outputs := make(map[string]ArtifactRef, len(plan.Steps))
	for _, step := range plan.Steps {
		phases[step.ID] = phasePending
	}

	for completed := 0; completed < len(plan.Steps); {
		ready := readySteps(plan, phases)
		if len(ready) == 0 {
			return WorkflowResult{}, temporal.NewNonRetryableApplicationError("no runnable steps remain", "UnresolvedWorkflow", nil)
		}
		// readySteps is already ordered by the canonical plan. This explicit sort
		// keeps the invariant visible if the plan implementation changes later.
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		for _, step := range ready {
			phases[step.ID] = phaseRunning
			if err := upsertWorkflowSearchAttributes(ctx, input, plan.Name, "Running", step.ID); err != nil {
				return WorkflowResult{}, err
			}

			var output ArtifactRef
			if step.Approval != nil {
				if err := setRunState(ctx, input, "WaitingApproval", "workflow_approval"); err != nil {
					return WorkflowResult{}, err
				}
				output, err = awaitApproval(ctx, input, step)
				if err == nil {
					err = setRunState(ctx, input, "Running", "")
				}
			} else {
				dependencies := dependencyOutputs(step, outputs)
				output, err = executeAgentStep(ctx, input, plan, step, dependencies)
			}
			if err != nil {
				if errors.Is(err, temporalworkflow.ErrCanceled) || temporal.IsCanceledError(err) {
					return WorkflowResult{}, err
				}
				return WorkflowResult{}, err
			}
			if !isZeroArtifact(output) {
				if err := output.Validate(); err != nil {
					return WorkflowResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidOutputReference", err)
				}
				outputs[step.ID] = output
			}
			phases[step.ID] = phaseSucceeded
			completed++
		}
	}

	if err := upsertWorkflowSearchAttributes(ctx, input, plan.Name, "Succeeded", ""); err != nil {
		return WorkflowResult{}, err
	}
	return WorkflowResult{RunID: input.RunID, Outputs: outputs}, nil
}

// AgentRunWorkflow is a child workflow for one agent step. Scheduling and
// cancellation are activities so the runner boundary remains outside the
// deterministic workflow sandbox. Runner results and human input arrive as
// durable signals and contain references only.
func AgentRunWorkflow(ctx temporalworkflow.Context, input AgentRunInput) (AgentRunResult, error) {
	if err := validateAgentRunInput(input); err != nil {
		return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidAgentRunInput", err)
	}

	activityCtx := temporalworkflow.WithActivityOptions(ctx, runnerActivityOptions(input.Timeout))
	scheduled, err := scheduleUntilAccepted(activityCtx, input)
	if err != nil {
		return AgentRunResult{}, err
	}
	taskID := scheduled.TaskID
	if scheduled.Status == "succeeded" {
		if err := scheduled.Output.Validate(); err != nil {
			return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidOutputReference", err)
		}
		return AgentRunResult{TaskID: taskID, Output: scheduled.Output}, nil
	}

	approvalCh := temporalworkflow.GetSignalChannel(ctx, SignalApproval)
	replyCh := temporalworkflow.GetSignalChannel(ctx, SignalUserReply)
	runnerCh := temporalworkflow.GetSignalChannel(ctx, SignalRunnerEvent)
	cancelCh := temporalworkflow.GetSignalChannel(ctx, SignalCancel)
	pendingApprovals := make(map[string]ApprovalSignal)
	var expectedApproval string
	var lastReply UserReplySignal
	handledReplies := make(map[string]struct{})

	for {
		var event RunnerEventSignal
		var approval ApprovalSignal
		var reply UserReplySignal
		var cancelSignal CancelSignal
		var canceled bool
		var received string
		poll := temporalworkflow.NewTimer(ctx, 10*time.Second)

		selector := temporalworkflow.NewSelector(ctx)
		selector.AddReceive(runnerCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &event)
				received = "runner"
			}
		})
		selector.AddReceive(approvalCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &approval)
				received = "approval"
			}
		})
		selector.AddReceive(replyCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &reply)
				received = "reply"
			}
		})
		selector.AddReceive(cancelCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &cancelSignal)
				canceled = true
			}
		})
		selector.AddReceive(ctx.Done(), func(temporalworkflow.ReceiveChannel, bool) { canceled = true })
		selector.AddFuture(poll, func(temporalworkflow.Future) { received = "poll" })
		selector.Select(ctx)

		if canceled {
			return cancelRunnerAndReturn(ctx, input, taskID, cancelSignal.Reason)
		}
		if received == "poll" {
			var status StatusRunnerTaskResult
			if err := temporalworkflow.ExecuteActivity(activityCtx, ActivityStatus, StatusRunnerTaskInput{
				OrganizationID: input.OrganizationID, ProjectID: input.ProjectID,
				RunID: input.RunID, TaskID: taskID,
			}).Get(ctx, &status); err != nil {
				return AgentRunResult{}, err
			}
			event = RunnerEventSignal{TaskID: taskID, Status: status.Status, ApprovalID: status.ApprovalID, Output: status.Output, Error: status.Error}
			received = "runner"
		}
		switch received {
		case "approval":
			if err := validateOptionalReference(approval.ApprovalID, "approval id"); err != nil {
				return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidApproval", err)
			}
			if err := validateOptionalReference(approval.StepID, "approval step id"); err != nil {
				return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidApproval", err)
			}
			if approval.ApprovalID != "" {
				pendingApprovals[approval.ApprovalID] = approval
			}
		case "reply":
			if err := validateReference(reply.ReplyRef, "user reply reference"); err != nil {
				return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidUserReply", err)
			}
			if reply.ReplyRef != "" {
				lastReply = reply
			}
		case "runner":
			if event.TaskID != "" && event.TaskID != taskID {
				continue
			}
			switch event.Status {
			case "waiting_approval":
				if event.ApprovalID == "" {
					return AgentRunResult{}, temporal.NewNonRetryableApplicationError("runner approval event has no approval id", "InvalidRunnerEvent", nil)
				}
				expectedApproval = event.ApprovalID
				if err := setAgentRunState(ctx, input, "WaitingApproval", "runner_approval"); err != nil {
					return AgentRunResult{}, err
				}
			case "scheduled", "running":
				// The durable runner ledger still owns the task. The next timer or
				// runner signal will observe a terminal or approval state.
			case "succeeded":
				if err := event.Output.Validate(); err != nil {
					return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidOutputReference", err)
				}
				return AgentRunResult{TaskID: taskID, Output: event.Output}, nil
			case "failed":
				if err := validateOptionalReference(event.Error, "runner error"); err != nil {
					return AgentRunResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidRunnerEvent", err)
				}
				message := event.Error
				if message == "" {
					message = "runner reported agent failure"
				}
				return AgentRunResult{}, temporal.NewApplicationError(message, "RunnerFailed")
			case "cancelled":
				return AgentRunResult{}, temporal.NewCanceledError("runner cancelled task")
			case "lost":
				return AgentRunResult{}, temporal.NewNonRetryableApplicationError("runner lost task state", "RunnerLost", nil)
			default:
				return AgentRunResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("unknown runner event status %q", event.Status), "InvalidRunnerEvent", nil)
			}
		}

		if expectedApproval != "" {
			if signal, ok := pendingApprovals[expectedApproval]; ok {
				delete(pendingApprovals, expectedApproval)
				if signal.Decision == "denied" {
					return cancelRunnerAndReturn(ctx, input, taskID, "approval denied")
				}
				if signal.Decision != "approved" {
					return AgentRunResult{}, temporal.NewNonRetryableApplicationError("approval decision must be approved or denied", "InvalidApproval", nil)
				}
				if err := resumeRunner(activityCtx, input, taskID, ResumeRunnerTaskInput{Action: "approval", ApprovalID: signal.ApprovalID, Decision: signal.Decision}); err != nil {
					return AgentRunResult{}, err
				}
				if err := setAgentRunState(ctx, input, "Running", ""); err != nil {
					return AgentRunResult{}, err
				}
				expectedApproval = ""
			}
		}
		if lastReply.ReplyRef != "" && (lastReply.TaskID == "" || lastReply.TaskID == taskID) {
			if _, handled := handledReplies[lastReply.ReplyRef]; !handled {
				handledReplies[lastReply.ReplyRef] = struct{}{}
				if err := resumeRunner(activityCtx, input, taskID, ResumeRunnerTaskInput{Action: "user-reply", Reference: lastReply.ReplyRef}); err != nil {
					return AgentRunResult{}, err
				}
				lastReply = UserReplySignal{}
			}
		}
	}
}

func executeAgentStep(ctx temporalworkflow.Context, input WorkflowInput, plan Plan, step Step, dependencies []ArtifactRef) (ArtifactRef, error) {
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	options := temporalworkflow.ChildWorkflowOptions{
		WorkflowID:               childWorkflowID(input.RunID, step.ID),
		WorkflowExecutionTimeout: timeout,
		RetryPolicy:              toTemporalRetryPolicy(step.Retry),
	}
	childCtx, cancelChild := temporalworkflow.WithCancel(ctx)
	defer cancelChild()
	childCtx = temporalworkflow.WithChildOptions(childCtx, options)
	future := temporalworkflow.ExecuteChildWorkflow(childCtx, AgentRunWorkflow, AgentRunInput{
		OrganizationID:   input.OrganizationID,
		ProjectID:        input.ProjectID,
		RunID:            input.RunID,
		WorkflowName:     plan.Name,
		StepID:           step.ID,
		AgentRef:         step.AgentRef,
		InputRef:         step.InputRef,
		DependencyOutput: dependencies,
		Timeout:          timeout,
		Retry:            step.Retry,
		Execution:        step.Execution,
		Contract:         step.Contract,
	})

	var result AgentRunResult
	var childErr error
	cancelCh := temporalworkflow.GetSignalChannel(ctx, SignalCancel)
	selector := temporalworkflow.NewSelector(ctx)
	selector.AddFuture(future, func(f temporalworkflow.Future) {
		childErr = f.Get(ctx, &result)
	})
	selector.AddReceive(cancelCh, func(c temporalworkflow.ReceiveChannel, more bool) {
		if more {
			var ignored CancelSignal
			c.Receive(ctx, &ignored)
			cancelChild()
			childErr = temporal.NewCanceledError(ignored.Reason)
		}
	})
	selector.AddReceive(ctx.Done(), func(temporalworkflow.ReceiveChannel, bool) {
		cancelChild()
		childErr = temporal.NewCanceledError("workflow context canceled")
	})
	selector.Select(ctx)
	if childErr != nil {
		if temporal.IsCanceledError(childErr) {
			// Give the child a deterministic opportunity to run its disconnected
			// cancellation activity before the parent closes. This is important
			// for runner cleanup when the parent is externally canceled.
			disconnected, _ := temporalworkflow.NewDisconnectedContext(ctx)
			_ = future.Get(disconnected, &result)
		}
		return ArtifactRef{}, childErr
	}
	if err := result.Output.Validate(); err != nil {
		return ArtifactRef{}, err
	}
	return result.Output, nil
}

func awaitApproval(ctx temporalworkflow.Context, input WorkflowInput, step Step) (ArtifactRef, error) {
	approvalCh := temporalworkflow.GetSignalChannel(ctx, SignalApproval)
	replyCh := temporalworkflow.GetSignalChannel(ctx, SignalUserReply)
	cancelCh := temporalworkflow.GetSignalChannel(ctx, SignalCancel)
	var lastReply string
	var deadline temporalworkflow.Future
	if step.Timeout > 0 {
		deadline = temporalworkflow.NewTimer(ctx, step.Timeout)
	}
	for {
		var approval ApprovalSignal
		var reply UserReplySignal
		var canceled bool
		var timedOut bool
		selector := temporalworkflow.NewSelector(ctx)
		selector.AddReceive(approvalCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &approval)
			}
		})
		selector.AddReceive(replyCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				c.Receive(ctx, &reply)
			}
		})
		selector.AddReceive(cancelCh, func(c temporalworkflow.ReceiveChannel, more bool) {
			if more {
				var ignored CancelSignal
				c.Receive(ctx, &ignored)
				canceled = true
			}
		})
		selector.AddReceive(ctx.Done(), func(temporalworkflow.ReceiveChannel, bool) { canceled = true })
		if deadline != nil {
			selector.AddFuture(deadline, func(temporalworkflow.Future) { timedOut = true })
		}
		selector.Select(ctx)
		if canceled {
			return ArtifactRef{}, temporal.NewCanceledError("workflow canceled while awaiting approval")
		}
		if timedOut {
			return ArtifactRef{}, temporal.NewNonRetryableApplicationError("approval timed out", "ApprovalTimeout", nil)
		}
		if reply.ReplyRef != "" && (reply.StepID == "" || reply.StepID == step.ID) {
			if err := validateReference(reply.ReplyRef, "user reply reference"); err != nil {
				return ArtifactRef{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidUserReply", err)
			}
			lastReply = reply.ReplyRef
			continue
		}
		if approval.StepID != "" && approval.StepID != step.ID {
			continue
		}
		if approval.ApprovalID != step.Approval.ID {
			continue
		}
		if approval.Decision == "denied" {
			return ArtifactRef{}, temporal.NewNonRetryableApplicationError("approval denied", "ApprovalDenied", nil)
		}
		if approval.Decision != "approved" {
			return ArtifactRef{}, temporal.NewNonRetryableApplicationError("approval decision must be approved or denied", "InvalidApproval", nil)
		}
		if !isZeroArtifact(approval.Output) {
			if err := approval.Output.Validate(); err != nil {
				return ArtifactRef{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidApprovalOutput", err)
			}
			return approval.Output, nil
		}
		// A user reply is already an immutable external reference. Preserve it
		// as the approval step's output without copying its content into history.
		if lastReply != "" {
			return referenceOnly(lastReply), nil
		}
		return ArtifactRef{}, nil
	}
}

func scheduleUntilAccepted(ctx temporalworkflow.Context, input AgentRunInput) (ScheduleRunnerTaskResult, error) {
	for {
		var result ScheduleRunnerTaskResult
		in := ScheduleRunnerTaskInput{
			OrganizationID:   input.OrganizationID,
			ProjectID:        input.ProjectID,
			RunID:            input.RunID,
			WorkflowName:     input.WorkflowName,
			StepID:           input.StepID,
			AgentRef:         input.AgentRef,
			InputRef:         input.InputRef,
			DependencyOutput: input.DependencyOutput,
			IdempotencyKey:   stepDispatchKey(input.RunID, input.WorkflowName, input.StepID),
			Execution:        input.Execution,
			Contract:         input.Contract,
		}
		if err := temporalworkflow.ExecuteActivity(ctx, ActivitySchedule, in).Get(ctx, &result); err != nil {
			return ScheduleRunnerTaskResult{}, err
		}
		switch result.Status {
		case "succeeded":
			if result.TaskID == "" {
				result.TaskID = in.IdempotencyKey
			}
			if err := result.Output.Validate(); err != nil {
				return ScheduleRunnerTaskResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidScheduleOutput", err)
			}
			return result, nil
		case "scheduled":
			if result.TaskID == "" {
				return ScheduleRunnerTaskResult{}, temporal.NewNonRetryableApplicationError("scheduled runner task has no task id", "InvalidScheduleResult", nil)
			}
			return result, nil
		case "cooldown":
			if result.CooldownUntil.IsZero() || !result.CooldownUntil.After(temporalworkflow.Now(ctx)) {
				return ScheduleRunnerTaskResult{}, temporal.NewNonRetryableApplicationError("cooldown result must be in the future", "InvalidCapacityResult", nil)
			}
			if err := setAgentRunState(ctx, input, "WaitingCapacity", "runner_capacity"); err != nil {
				return ScheduleRunnerTaskResult{}, err
			}
			if err := temporalworkflow.NewTimer(ctx, result.CooldownUntil.Sub(temporalworkflow.Now(ctx))).Get(ctx, nil); err != nil {
				return ScheduleRunnerTaskResult{}, err
			}
			if err := setAgentRunState(ctx, input, "Running", ""); err != nil {
				return ScheduleRunnerTaskResult{}, err
			}
		default:
			return ScheduleRunnerTaskResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("unknown runner schedule status %q", result.Status), "InvalidScheduleResult", nil)
		}
	}
}

func resumeRunner(ctx temporalworkflow.Context, input AgentRunInput, taskID string, resume ResumeRunnerTaskInput) error {
	resume.OrganizationID = input.OrganizationID
	resume.ProjectID = input.ProjectID
	resume.RunID = input.RunID
	resume.TaskID = taskID
	if resume.IdempotencyKey == "" {
		resume.IdempotencyKey = fmt.Sprintf("%s/%s/%s", taskID, resume.Action, resume.Reference)
	}
	return temporalworkflow.ExecuteActivity(ctx, ActivityResume, resume).Get(ctx, nil)
}

func cancelRunnerAndReturn(ctx temporalworkflow.Context, input AgentRunInput, taskID, reason string) (AgentRunResult, error) {
	disconnected, _ := temporalworkflow.NewDisconnectedContext(ctx)
	cancelCtx := temporalworkflow.WithActivityOptions(disconnected, runnerActivityOptions(input.Timeout))
	err := temporalworkflow.ExecuteActivity(cancelCtx, ActivityCancel, CancelRunnerTaskInput{
		OrganizationID: input.OrganizationID,
		ProjectID:      input.ProjectID,
		RunID:          input.RunID,
		TaskID:         taskID,
		IdempotencyKey: "cancel/" + taskID,
	}).Get(cancelCtx, nil)
	if err != nil {
		return AgentRunResult{}, err
	}
	return AgentRunResult{}, temporal.NewCanceledError(reason)
}

func runnerActivityOptions(timeout time.Duration) temporalworkflow.ActivityOptions {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return temporalworkflow.ActivityOptions{
		StartToCloseTimeout:    timeout,
		ScheduleToCloseTimeout: timeout,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

func toTemporalRetryPolicy(policy RetryPolicy) *temporal.RetryPolicy {
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	return &temporal.RetryPolicy{
		MaximumAttempts:    maxAttempts,
		InitialInterval:    defaultDuration(policy.InitialInterval, time.Second),
		BackoffCoefficient: defaultBackoff(policy.BackoffCoefficient),
		MaximumInterval:    defaultDuration(policy.MaximumInterval, 30*time.Second),
	}
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func defaultBackoff(value float64) float64 {
	if value >= 1 {
		return value
	}
	return 2
}

func dependencyOutputs(step Step, outputs map[string]ArtifactRef) []ArtifactRef {
	result := make([]ArtifactRef, 0, len(step.Needs))
	for _, need := range step.Needs {
		if output, ok := outputs[need]; ok {
			result = append(result, output)
		}
	}
	return result
}

func validateWorkflowInput(input WorkflowInput) error {
	for _, value := range []struct{ name, value string }{{"organization id", input.OrganizationID}, {"project id", input.ProjectID}, {"run id", input.RunID}} {
		if err := validateReference(value.value, value.name); err != nil {
			return err
		}
	}
	return validateInputReference(input.InputRef, "workflow input reference")
}

func validateAgentRunInput(input AgentRunInput) error {
	for _, value := range []struct{ name, value string }{{"organization id", input.OrganizationID}, {"project id", input.ProjectID}, {"run id", input.RunID}, {"workflow name", input.WorkflowName}, {"step id", input.StepID}, {"agent reference", input.AgentRef}} {
		if err := validateReference(value.value, value.name); err != nil {
			return err
		}
	}
	if err := validateInputReference(input.InputRef, "input reference"); err != nil {
		return err
	}
	for _, ref := range input.DependencyOutput {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func upsertWorkflowSearchAttributes(ctx temporalworkflow.Context, input WorkflowInput, workflowName, status, stepID string) error {
	attributes := map[string]interface{}{
		SearchAttributeOrganization: input.OrganizationID,
		SearchAttributeProject:      input.ProjectID,
		SearchAttributeRunID:        input.RunID,
		SearchAttributeWorkflow:     workflowName,
		SearchAttributeStatus:       status,
	}
	if stepID != "" {
		attributes[SearchAttributeStep] = stepID
	}
	return temporalworkflow.UpsertSearchAttributes(ctx, attributes)
}

func isZeroArtifact(ref ArtifactRef) bool {
	return ref.ID == "" && ref.URI == "" && ref.Digest == "" && ref.SizeBytes == 0 && ref.MediaType == ""
}

func referenceOnly(value string) ArtifactRef {
	return ArtifactRef{ID: "reply-reference", URI: value, Digest: "sha256:" + strings.Repeat("0", 64), MediaType: "application/reference"}
}
