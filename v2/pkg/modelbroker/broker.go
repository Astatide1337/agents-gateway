// Package modelbroker keeps upstream model credentials and routing decisions
// outside agent sandboxes.
package modelbroker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/modelroute"
)

type Message struct{ Role, Content string }

type Request struct {
	OrganizationID string
	ProjectID      string
	UserID         string
	RunID          string
	Model          string
	Messages       []Message
	MaxTokens      int
}

type Response struct {
	Provider     string
	Model        string
	Content      string
	InputTokens  int64
	OutputTokens int64
	FinishReason string
}

type Provider interface {
	Invoke(context.Context, Request, []byte) (Response, error)
}

type CredentialResolver interface {
	ResolveCredential(context.Context, string, string) ([]byte, error)
}

type EntitlementSource interface {
	Entitlements(context.Context, string, string) ([]modelroute.Entitlement, error)
	Route(context.Context, string, string, string) (modelroute.Route, error)
}

type Broker struct {
	providers   map[string]Provider
	credentials CredentialResolver
	source      EntitlementSource
	now         func() time.Time
}

func New(providers map[string]Provider, credentials CredentialResolver, source EntitlementSource) (*Broker, error) {
	if len(providers) == 0 || credentials == nil || source == nil {
		return nil, errors.New("providers, credential resolver, and entitlement source are required")
	}
	return &Broker{providers: providers, credentials: credentials, source: source, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (b *Broker) Invoke(ctx context.Context, request Request) (Response, error) {
	if request.OrganizationID == "" || request.ProjectID == "" || request.UserID == "" || request.RunID == "" || request.Model == "" || len(request.Messages) == 0 {
		return Response{}, errors.New("complete tenant, run, model, and message context is required")
	}
	route, err := b.source.Route(ctx, request.OrganizationID, request.ProjectID, request.Model)
	if err != nil {
		return Response{}, fmt.Errorf("resolve model route: %w", err)
	}
	entitlements, err := b.source.Entitlements(ctx, request.OrganizationID, request.UserID)
	if err != nil {
		return Response{}, fmt.Errorf("list model entitlements: %w", err)
	}
	selection, err := modelroute.Select(route, entitlements, modelroute.Request{UserID: request.UserID, OrganizationID: request.OrganizationID, Model: request.Model, Now: b.now()})
	if err != nil {
		return Response{}, err
	}
	provider, ok := b.providers[selection.Entitlement.Provider]
	if !ok {
		return Response{}, fmt.Errorf("model provider %q is not configured", selection.Entitlement.Provider)
	}
	secret, err := b.credentials.ResolveCredential(ctx, request.OrganizationID, selection.Entitlement.CredentialRef)
	if err != nil {
		return Response{}, fmt.Errorf("resolve provider credential: %w", err)
	}
	defer zero(secret)
	response, err := provider.Invoke(ctx, request, secret)
	if err != nil {
		return Response{}, fmt.Errorf("provider %s request failed", selection.Entitlement.Provider)
	}
	response.Provider = selection.Entitlement.Provider
	if response.Model == "" {
		response.Model = request.Model
	}
	return response, nil
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
