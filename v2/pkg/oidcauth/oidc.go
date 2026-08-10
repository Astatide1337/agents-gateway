// Package oidcauth validates human bearer tokens through standards-compliant
// OIDC discovery, then resolves current tenant roles from the control plane.
package oidcauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/coreos/go-oidc/v3/oidc"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type MembershipResolver interface {
	ResolvePrincipal(context.Context, string) (authz.Principal, error)
}

type Authenticator struct {
	verifier    *oidc.IDTokenVerifier
	memberships MembershipResolver
	tokenHeader string
}

type Options struct {
	// TokenHeader permits a trusted identity-aware reverse proxy (for example
	// Cloudflare Access) to forward its signed JWT. The API must not be directly
	// reachable when this mode is used, because clients can set HTTP headers.
	TokenHeader string
}

func New(ctx context.Context, issuer, audience string, memberships MembershipResolver) (*Authenticator, error) {
	return NewWithOptions(ctx, issuer, audience, memberships, Options{})
}

func NewWithOptions(ctx context.Context, issuer, audience string, memberships MembershipResolver, options Options) (*Authenticator, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" || memberships == nil {
		return nil, errors.New("OIDC issuer, audience, and membership resolver are required")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	header := http.CanonicalHeaderKey(strings.TrimSpace(options.TokenHeader))
	if header == "Authorization" {
		header = ""
	}
	if strings.ContainsAny(header, "\r\n:") {
		return nil, errors.New("OIDC token header is invalid")
	}
	return &Authenticator{verifier: provider.Verifier(&oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA}}), memberships: memberships, tokenHeader: header}, nil
}

func (a *Authenticator) Authenticate(request *http.Request) (authz.Principal, error) {
	if a == nil || a.verifier == nil || a.memberships == nil {
		return authz.Principal{}, ErrUnauthenticated
	}
	rawToken := ""
	if a.tokenHeader != "" {
		rawToken = strings.TrimSpace(request.Header.Get(a.tokenHeader))
	}
	if rawToken == "" {
		header := strings.TrimSpace(request.Header.Get("Authorization"))
		parts := strings.Fields(header)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			rawToken = parts[1]
		}
	}
	if rawToken == "" {
		return authz.Principal{}, ErrUnauthenticated
	}
	token, err := a.verifier.Verify(request.Context(), rawToken)
	if err != nil {
		return authz.Principal{}, ErrUnauthenticated
	}
	if token.Subject == "" {
		return authz.Principal{}, ErrUnauthenticated
	}
	principal, err := a.memberships.ResolvePrincipal(request.Context(), token.Subject)
	if err != nil || principal.ID != token.Subject {
		return authz.Principal{}, ErrUnauthenticated
	}
	principal.Type = authz.PrincipalHuman
	return principal, nil
}
