package workflow

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

const ActivitySetRunState = "agents-gateway.v2.SetRunState"

type SetRunStateInput struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	RunID          string `json:"run_id"`
	Status         string `json:"status"`
	Condition      string `json:"condition,omitempty"`
}

type RunStateActivities interface {
	SetRunState(context.Context, SetRunStateInput) error
}

type RunStateHandlers struct{ Impl RunStateActivities }

func (h RunStateHandlers) SetRunState(ctx context.Context, input SetRunStateInput) error {
	if h.Impl == nil {
		return errors.New("run state activity implementation is nil")
	}
	return h.Impl.SetRunState(ctx, input)
}

func RegisterRunStateActivities(w worker.Worker, implementation RunStateActivities) {
	handlers := RunStateHandlers{Impl: implementation}
	w.RegisterActivityWithOptions(handlers.SetRunState, activity.RegisterOptions{Name: ActivitySetRunState})
}

func setRunState(ctx temporalworkflow.Context, input WorkflowInput, status, condition string) error {
	return setRunStateFor(ctx, SetRunStateInput{OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, Status: status, Condition: condition})
}

func setAgentRunState(ctx temporalworkflow.Context, input AgentRunInput, status, condition string) error {
	return setRunStateFor(ctx, SetRunStateInput{OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, Status: status, Condition: condition})
}

func setRunStateFor(ctx temporalworkflow.Context, input SetRunStateInput) error {
	activityCtx := temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    10 * time.Second,
			MaximumAttempts:    5,
		},
	})
	return temporalworkflow.ExecuteActivity(activityCtx, ActivitySetRunState, input).Get(activityCtx, nil)
}
