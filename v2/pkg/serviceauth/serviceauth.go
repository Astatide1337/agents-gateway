// Package serviceauth exchanges rotatable service credentials for short-lived
// identity tokens and combines those tokens with human OIDC authentication.
package serviceauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/identity"
)

const maxExchangeBody int64 = 16 << 10

type AccountRepository interface {
	Authenticate(context.Context, string, string) (identity.ServiceAccount, error)
}

type Handler struct {
	Accounts AccountRepository
	Issuer   *identity.Issuer
	TTL      time.Duration
}

func (h Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeExchangeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if h.Accounts == nil || h.Issuer == nil {
		writeExchangeError(w, http.StatusServiceUnavailable, "service_auth_unavailable")
		return
	}
	if contentType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); contentType != "application/json" {
		writeExchangeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, maxExchangeBody+1))
	if err != nil || int64(len(data)) > maxExchangeBody {
		writeExchangeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	var input struct {
		AccountID string `json:"accountId"`
		Secret    string `json:"secret"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || input.AccountID == "" || input.Secret == "" {
		writeExchangeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	account, err := h.Accounts.Authenticate(request.Context(), input.AccountID, input.Secret)
	if err != nil {
		// Authentication failures are intentionally indistinguishable from a
		// missing or disabled account.
		writeExchangeError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	token, err := h.Issuer.Issue(account, ttl)
	if err != nil {
		writeExchangeError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": token, "tokenType": "Bearer", "expiresIn": int64(ttl.Seconds())})
}

func writeExchangeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "service credential exchange failed"}})
}

// HumanAuthenticator is implemented by the OIDC authenticator.
type HumanAuthenticator interface {
	Authenticate(*http.Request) (authz.Principal, error)
}

type CombinedAuthenticator struct {
	Humans HumanAuthenticator
	Issuer *identity.Issuer
}

func (a CombinedAuthenticator) Authenticate(request *http.Request) (authz.Principal, error) {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") && a.Issuer != nil {
		if principal, err := a.Issuer.Verify(strings.TrimSpace(value[len("Bearer "):])); err == nil {
			return principal, nil
		}
	}
	if a.Humans == nil {
		return authz.Principal{}, httpapi.ErrUnauthenticated
	}
	return a.Humans.Authenticate(request)
}

// EncodedDigest converts the database byte representation into the stable
// identity verifier representation.
func EncodedDigest(digest []byte) (string, error) {
	if len(digest) != 32 {
		return "", errors.New("invalid credential digest")
	}
	return base64.RawURLEncoding.EncodeToString(digest), nil
}
