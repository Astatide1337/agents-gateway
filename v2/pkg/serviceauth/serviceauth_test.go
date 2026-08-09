package serviceauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/identity"
)

type fakeAccounts struct {
	account identity.ServiceAccount
	err     error
}

func (f fakeAccounts) Authenticate(context.Context, string, string) (identity.ServiceAccount, error) {
	return f.account, f.err
}

type rejectingHuman struct{}

func (rejectingHuman) Authenticate(*http.Request) (authz.Principal, error) {
	return authz.Principal{}, errors.New("not a human token")
}

func TestCredentialExchangeAndCombinedAuthentication(t *testing.T) {
	_, private, err := identity.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := identity.NewIssuer("https://gateway.example", "agents-gateway", "key-1", private)
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler{
		Accounts: fakeAccounts{account: identity.ServiceAccount{ID: "account-1", ProjectRoles: map[string]authz.Role{"org/project": authz.RoleProjectEditor}}},
		Issuer:   issuer,
		TTL:      5 * time.Minute,
	}
	secret := "agw_sa_long-lived-secret"
	request := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(`{"accountId":"account-1","secret":"`+secret+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	marker := `"accessToken":"`
	start := strings.Index(response.Body.String(), marker)
	if start < 0 {
		t.Fatal("access token missing")
	}
	start += len(marker)
	end := strings.Index(response.Body.String()[start:], `"`)
	token := response.Body.String()[start : start+end]
	authenticator := CombinedAuthenticator{Humans: rejectingHuman{}, Issuer: issuer}
	authRequest := httptest.NewRequest(http.MethodGet, "/api", nil)
	authRequest.Header.Set("Authorization", "Bearer "+token)
	principal, err := authenticator.Authenticate(authRequest)
	if err != nil || principal.ID != "account-1" || principal.ProjectRoles["org/project"] != authz.RoleProjectEditor {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
}

func TestExchangeFailsClosedWithoutCredentialLeak(t *testing.T) {
	handler := Handler{Accounts: fakeAccounts{err: errors.New("database detail: secret")}}
	request := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(`{"accountId":"account-1","secret":"secret"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "database detail") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
