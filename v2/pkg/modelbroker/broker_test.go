package modelbroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/modelroute"
)

type testSource struct {
	route        modelroute.Route
	entitlements []modelroute.Entitlement
}

func (s testSource) Route(context.Context, string, string, string) (modelroute.Route, error) {
	return s.route, nil
}
func (s testSource) Entitlements(context.Context, string, string) ([]modelroute.Entitlement, error) {
	return s.entitlements, nil
}

type testCredentials struct{ secret string }

func (c testCredentials) ResolveCredential(context.Context, string, string) ([]byte, error) {
	return []byte(c.secret), nil
}

func TestBrokerKeepsCredentialAtProviderBoundary(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource{route: modelroute.Route{Entries: []modelroute.Entry{{Provider: "openai", Mode: modelroute.ModeAPI, Model: "model"}}}, entitlements: []modelroute.Entitlement{{ID: "api", Provider: "openai", Mode: modelroute.ModeAPI, OwnerType: modelroute.OwnerOrganization, OwnerID: "org", Models: []string{"model"}, Enabled: true, CredentialRef: "credential"}}}
	broker, err := New(map[string]Provider{"openai": provider}, testCredentials{secret: "super-secret"}, source)
	if err != nil {
		t.Fatal(err)
	}
	response, err := broker.Invoke(context.Background(), Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Model: "model", Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "ok" || authorization != "Bearer super-secret" {
		t.Fatalf("response=%#v auth=%q", response, authorization)
	}
}

func TestBrokerErrorDoesNotLeakCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "super-secret", http.StatusUnauthorized) }))
	defer server.Close()
	provider, _ := NewOpenAICompatible(server.URL, nil)
	source := testSource{route: modelroute.Route{Entries: []modelroute.Entry{{Provider: "openai", Mode: modelroute.ModeAPI, Model: "model"}}}, entitlements: []modelroute.Entitlement{{ID: "api", Provider: "openai", Mode: modelroute.ModeAPI, OwnerType: modelroute.OwnerOrganization, OwnerID: "org", Models: []string{"model"}, Enabled: true, CredentialRef: "credential"}}}
	broker, _ := New(map[string]Provider{"openai": provider}, testCredentials{secret: "super-secret"}, source)
	_, err := broker.Invoke(context.Background(), Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Model: "model", Messages: []Message{{Role: "user", Content: "hello"}}})
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestBrokerRejectsOtherUsersSubscription(t *testing.T) {
	source := testSource{route: modelroute.Route{Entries: []modelroute.Entry{{Provider: "openai", Mode: modelroute.ModeSubscription, Model: "model"}}}, entitlements: []modelroute.Entitlement{{ID: "other", Provider: "openai", Mode: modelroute.ModeSubscription, OwnerType: modelroute.OwnerUser, OwnerID: "someone-else", Models: []string{"model"}, Enabled: true, CredentialRef: "credential"}}}
	broker, _ := New(map[string]Provider{"openai": ProviderFunc(func(context.Context, Request, []byte) (Response, error) { return Response{}, nil })}, testCredentials{}, source)
	_, err := broker.Invoke(context.Background(), Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Model: "model", Messages: []Message{{Role: "user", Content: "hello"}}})
	if !errors.Is(err, modelroute.ErrNoEntitlement) {
		t.Fatalf("unexpected error %v", err)
	}
}

type ProviderFunc func(context.Context, Request, []byte) (Response, error)

func (f ProviderFunc) Invoke(ctx context.Context, r Request, c []byte) (Response, error) {
	return f(ctx, r, c)
}
