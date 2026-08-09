package runbroker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	randomBytes   = 32
	sessionPrefix = "ags_"
	tokenPrefix   = "agt_"
)

type sessionRecord struct {
	public    Session
	tokenHash [sha256.Size]byte
	revoked   bool
}

// Manager holds short-lived run sessions. It is safe for concurrent Create,
// Authorize, and Revoke calls. Raw bearer tokens are never retained.
type Manager struct {
	mu       sync.RWMutex
	sessions map[SessionID]*sessionRecord
	clock    func() time.Time
	maxTTL   time.Duration
}

// NewManager constructs an empty session manager.
func NewManager(config ManagerConfig) (*Manager, error) {
	maxTTL := config.MaxTTL
	if maxTTL <= 0 {
		maxTTL = defaultMaxTTL
	}
	if maxTTL <= 0 || maxTTL > 365*24*time.Hour {
		return nil, fmt.Errorf("%w: max TTL is outside the supported range", ErrInvalidInput)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Manager{
		sessions: make(map[SessionID]*sessionRecord),
		clock:    clock,
		maxTTL:   maxTTL,
	}, nil
}

// Create makes one random session ID and one random bearer token. The token
// in the result is the only raw token value ever available from this Manager.
func (m *Manager) Create(ctx context.Context, request CreateRequest) (CreateResult, error) {
	if err := contextErr(ctx); err != nil {
		return CreateResult{}, err
	}
	binding, models, policy, err := validateCreateRequest(request)
	if err != nil {
		return CreateResult{}, err
	}
	if request.TTL <= 0 || request.TTL > m.maxTTL {
		return CreateResult{}, fmt.Errorf("%w: TTL must be positive and no greater than %s", ErrInvalidInput, m.maxTTL)
	}

	identifier, err := randomOpaque(sessionPrefix)
	if err != nil {
		return CreateResult{}, fmt.Errorf("generate session ID: %w", err)
	}
	token, err := randomOpaque(tokenPrefix)
	if err != nil {
		return CreateResult{}, fmt.Errorf("generate bearer token: %w", err)
	}
	now := m.clock().UTC()
	if now.IsZero() {
		return CreateResult{}, fmt.Errorf("%w: manager clock returned zero time", ErrInvalidInput)
	}
	public := Session{
		ID:            SessionID(identifier),
		Binding:       binding,
		AllowedModels: models,
		PolicyDigest:  policy,
		ExpiresAt:     now.Add(request.TTL),
	}
	record := &sessionRecord{public: public, tokenHash: sha256.Sum256([]byte(token))}

	if err := contextErr(ctx); err != nil {
		return CreateResult{}, err
	}
	m.mu.Lock()
	// A 256-bit random collision is not a normal operational condition, but
	// treating it as an error prevents silently replacing an active run.
	if _, exists := m.sessions[public.ID]; exists {
		m.mu.Unlock()
		return CreateResult{}, errors.New("runbroker: random session ID collision")
	}
	m.sessions[public.ID] = record
	m.mu.Unlock()
	return CreateResult{Session: cloneSession(public), Token: BearerToken(token)}, nil
}

// Authorize validates the complete run scope. The token comparison is always
// performed against a fixed-size SHA-256 digest using ConstantTimeCompare,
// including for malformed token-shaped input after lookup.
func (m *Manager) Authorize(ctx context.Context, request AuthorizeRequest) (Session, error) {
	if err := contextErr(ctx); err != nil {
		return Session{}, err
	}
	if err := validateSessionID(request.SessionID); err != nil {
		return Session{}, err
	}
	if err := validateBinding(request.Binding); err != nil {
		return Session{}, err
	}
	if err := validateModel(request.Model); err != nil {
		return Session{}, err
	}
	if err := validatePolicy(request.PolicyDigest); err != nil {
		return Session{}, err
	}

	m.mu.RLock()
	record := m.sessions[request.SessionID]
	if record == nil {
		m.mu.RUnlock()
		return Session{}, ErrUnauthorized
	}
	// Hash even malformed input and compare fixed-size values. Shape validation
	// is reported only after the constant-time comparison has completed.
	candidate := sha256.Sum256([]byte(request.Token))
	equal := subtle.ConstantTimeCompare(candidate[:], record.tokenHash[:]) == 1
	public := record.public
	revoked := record.revoked
	if !equal {
		m.mu.RUnlock()
		if !validOpaque(string(request.Token), tokenPrefix, randomBytes) {
			return Session{}, ErrMalformedToken
		}
		return Session{}, ErrUnauthorized
	}
	if !validOpaque(string(request.Token), tokenPrefix, randomBytes) {
		m.mu.RUnlock()
		return Session{}, ErrMalformedToken
	}
	now := m.clock().UTC()
	if revoked {
		m.mu.RUnlock()
		return Session{}, ErrRevoked
	}
	if !now.Before(public.ExpiresAt) {
		m.mu.RUnlock()
		return Session{}, ErrExpired
	}
	if request.Binding.RunID != public.Binding.RunID {
		m.mu.RUnlock()
		return Session{}, ErrCrossRun
	}
	if request.Binding.OrgID != public.Binding.OrgID || request.Binding.ProjectID != public.Binding.ProjectID || request.Binding.UserID != public.Binding.UserID {
		m.mu.RUnlock()
		return Session{}, ErrWrongBinding
	}
	if request.PolicyDigest != public.PolicyDigest {
		m.mu.RUnlock()
		return Session{}, ErrWrongPolicy
	}
	if !containsExact(public.AllowedModels, request.Model) {
		m.mu.RUnlock()
		return Session{}, ErrWrongModel
	}
	m.mu.RUnlock()
	if err := contextErr(ctx); err != nil {
		return Session{}, err
	}
	return cloneSession(public), nil
}

// Revoke marks a session revoked. It is idempotent: a missing, already
// revoked, or repeatedly revoked session returns nil. Invalid IDs remain an
// input error so callers do not accidentally revoke a malformed identifier.
func (m *Manager) Revoke(ctx context.Context, id SessionID) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	m.mu.Lock()
	if record := m.sessions[id]; record != nil {
		record.revoked = true
	}
	m.mu.Unlock()
	return contextErr(ctx)
}

// Delete permanently removes a session after its listener and sandbox have
// stopped. It is idempotent and makes every previously issued token for the
// session unauthorized. Lifecycle owners should use it after Revoke so a
// long-running gateway does not retain one record for every completed run.
func (m *Manager) Delete(ctx context.Context, id SessionID) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	return contextErr(ctx)
}

// SessionCount is intended for diagnostics and tests. It returns the number
// of records, including expired and revoked records retained for idempotent
// revocation semantics.
func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validateCreateRequest(request CreateRequest) (SessionBinding, []string, string, error) {
	if err := validateBinding(request.Binding); err != nil {
		return SessionBinding{}, nil, "", err
	}
	if err := validatePolicy(request.PolicyDigest); err != nil {
		return SessionBinding{}, nil, "", err
	}
	if len(request.AllowedModels) == 0 {
		return SessionBinding{}, nil, "", fmt.Errorf("%w: at least one allowed model is required", ErrInvalidInput)
	}
	models := make([]string, 0, len(request.AllowedModels))
	seen := make(map[string]struct{}, len(request.AllowedModels))
	for _, model := range request.AllowedModels {
		if err := validateModel(model); err != nil {
			return SessionBinding{}, nil, "", err
		}
		if _, exists := seen[model]; exists {
			return SessionBinding{}, nil, "", fmt.Errorf("%w: duplicate allowed model", ErrInvalidInput)
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return request.Binding, models, request.PolicyDigest, nil
}

func validateBinding(binding SessionBinding) error {
	for name, value := range map[string]string{
		"organization": binding.OrgID,
		"project":      binding.ProjectID,
		"user":         binding.UserID,
		"run":          binding.RunID,
	} {
		if err := validateIdentifier(value, maxIdentifierBytes); err != nil {
			return fmt.Errorf("%w: %s binding: %v", ErrInvalidInput, name, err)
		}
	}
	return nil
}

func validateIdentifier(value string, max int) error {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 {
		return errors.New("identifier is empty, too long, or contains surrounding whitespace/NUL")
	}
	for _, r := range value {
		if r == '\r' || r == '\n' || r == '\t' {
			return errors.New("identifier contains a control character")
		}
	}
	return nil
}

func validatePolicy(policy string) error {
	if err := validateIdentifier(policy, maxPolicyBytes); err != nil {
		return fmt.Errorf("%w: policy digest: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateModel(model string) error {
	if err := validateIdentifier(model, maxModelBytes); err != nil {
		return fmt.Errorf("%w: model: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateSessionID(id SessionID) error {
	if !validOpaque(string(id), sessionPrefix, randomBytes) {
		return ErrMalformedSessionID
	}
	return nil
}

func randomOpaque(prefix string) (string, error) {
	data := make([]byte, randomBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func validOpaque(value, prefix string, bytesExpected int) bool {
	if len(value) <= len(prefix) || !strings.HasPrefix(value, prefix) || len(value) > maxTokenBytes {
		return false
	}
	payload := value[len(prefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(decoded) != bytesExpected {
		return false
	}
	return base64.RawURLEncoding.EncodeToString(decoded) == payload
}

func containsExact(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func cloneSession(session Session) Session {
	session.AllowedModels = append([]string(nil), session.AllowedModels...)
	return session
}
