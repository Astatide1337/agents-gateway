package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
)

func TestLocalAuthenticationIsProductionSafeOwnerMode(t *testing.T) {
	t.Setenv("AGW_ENVIRONMENT", "production")
	t.Setenv("AGW_AUTH_MODE", "local")
	t.Setenv("AGW_AUTH_TOKEN", "test-owner-token-test-owner-token-12")

	authenticator, database, _, err := configureAuthenticator(context.Background(), nil)
	if err != nil {
		t.Fatalf("configure local authentication: %v", err)
	}
	if database != nil {
		t.Fatal("local authentication must not require an authorization database")
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer test-owner-token-test-owner-token-12")
	principal, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatalf("authenticate local owner: %v", err)
	}
	if principal.ID != "local-owner" || principal.Type != authz.PrincipalHuman || principal.InstanceRole != authz.RoleInstanceAdmin {
		t.Fatalf("unexpected local owner principal: %#v", principal)
	}
}

func TestLocalAuthenticationRejectsWeakToken(t *testing.T) {
	t.Setenv("AGW_AUTH_MODE", "local")
	t.Setenv("AGW_AUTH_TOKEN", "too-short")
	if _, _, _, err := configureAuthenticator(context.Background(), nil); err == nil {
		t.Fatal("expected weak local token to fail closed")
	}
}

func TestRunEngineModeFailsClosed(t *testing.T) {
	t.Setenv("AGW_ORCHESTRATION_MODE", "unknown")
	if _, err := configureRunEngine(nil, nil, nil); err == nil {
		t.Fatal("unknown orchestration mode was accepted")
	}
	t.Setenv("AGW_ORCHESTRATION_MODE", "local")
	if _, err := configureRunEngine(nil, nil, nil); err == nil {
		t.Fatal("local orchestration without PostgreSQL was accepted")
	}
}
