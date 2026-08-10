package oidcauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
)

type memberships map[string]authz.Principal

func (m memberships) ResolvePrincipal(_ context.Context, subject string) (authz.Principal, error) {
	return m[subject], nil
}

func TestOIDCVerificationAndMembershipResolution(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	authenticator, err := New(context.Background(), issuer, "agents-gateway", memberships{"human-1": {ID: "human-1", OrgRoles: map[string]authz.Role{"org": authz.RoleOrgAdmin}}})
	if err != nil {
		t.Fatal(err)
	}
	token := signedToken(t, private, issuer, "agents-gateway", "human-1", time.Now().Add(5*time.Minute))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != "human-1" || principal.Type != authz.PrincipalHuman {
		t.Fatalf("principal=%#v", principal)
	}
	request.Header.Set("Authorization", "Bearer "+token+"tampered")
	if _, err := authenticator.Authenticate(request); err == nil {
		t.Fatal("tampered token accepted")
	}
	proxyAuthenticator, err := NewWithOptions(context.Background(), issuer, "agents-gateway", memberships{"human-1": {ID: "human-1"}}, Options{TokenHeader: "Cf-Access-Jwt-Assertion"})
	if err != nil {
		t.Fatal(err)
	}
	proxyRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	proxyRequest.Header.Set("Cf-Access-Jwt-Assertion", token)
	if principal, err := proxyAuthenticator.Authenticate(proxyRequest); err != nil || principal.ID != "human-1" {
		t.Fatalf("trusted proxy token principal=%#v err=%v", principal, err)
	}
}

func signedToken(t *testing.T, private *rsa.PrivateKey, issuer, audience, subject string, expires time.Time) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": audience, "sub": subject, "iat": time.Now().Add(-time.Minute).Unix(), "exp": expires.Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := crypto.SHA256.New()
	_, _ = sum.Write([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, sum.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
