// Package runstate adapts the durable control-plane store to Temporal run
// lifecycle activities.
package runstate

import (
	"context"
	"errors"

	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

type StoreActivity struct{ Store store.Store }

func (a StoreActivity) SetRunState(ctx context.Context, input agentworkflow.SetRunStateInput) error {
	if a.Store == nil {
		return errors.New("run state store is required")
	}
	_, err := a.Store.SetRunStatus(ctx, store.Scope{
		OrganizationID: input.OrganizationID,
		ProjectID:      input.ProjectID,
	}, input.RunID, input.Status, input.Condition)
	return err
}

var _ agentworkflow.RunStateActivities = StoreActivity{}
