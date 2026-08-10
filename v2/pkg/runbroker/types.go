// Package runbroker provides short-lived, per-run authorization and a private
// Unix-socket HTTP boundary for agent sandboxes.
//
// The package deliberately has no provider, credential, model-client, or tool
// dependencies. A caller supplies a typed session binding and an http.Handler;
// provider credentials must remain in the caller's host-side implementation.
package runbroker

import (
	"net/http"
	"time"
)

const (
	// HeaderAuthorization carries the bearer token as "Bearer <token>".
	HeaderAuthorization = "Authorization"
	// HeaderSessionID binds a request to the per-run session.
	HeaderSessionID = "X-AGW-Session-ID"
	// HeaderModel identifies the model route being requested.
	HeaderModel = "X-AGW-Model"
	// HeaderPolicyDigest identifies the policy under which the request runs.
	HeaderPolicyDigest = "X-AGW-Policy-Digest"

	defaultMaxTTL         = 1 * time.Hour
	defaultMaxHeaderBytes = 32 << 10
	defaultMaxBodyBytes   = 8 << 20
	defaultReadHeader     = 5 * time.Second
	defaultReadTimeout    = 30 * time.Second
	// Streaming model responses are bounded by the run context and session
	// expiry, not by a fixed HTTP write deadline.
	defaultWriteTimeout    = 0
	defaultIdleTimeout     = 60 * time.Second
	defaultShutdownTimeout = 5 * time.Second

	maxIdentifierBytes = 256
	maxPolicyBytes     = 512
	maxModelBytes      = 256
	maxTokenBytes      = 128
)

// SessionID is an opaque, cryptographically random identifier for one run.
type SessionID string

// BearerToken is returned exactly once by Create. Managers store only a hash
// of its string representation.
type BearerToken string

// String returns the opaque token value for placing it in an Authorization
// header. Callers should avoid logging it.
func (t BearerToken) String() string { return string(t) }

// SessionBinding is the non-secret identity and policy scope of a run.
type SessionBinding struct {
	OrgID     string
	ProjectID string
	UserID    string
	RunID     string
}

// Session is the public, non-secret session view. It never contains a token
// or token hash.
type Session struct {
	ID            SessionID
	Binding       SessionBinding
	AllowedModels []string
	PolicyDigest  string
	ExpiresAt     time.Time
}

// CreateRequest describes a new per-run session. TTL is relative to the
// manager clock and must be positive and no greater than Manager's MaxTTL.
type CreateRequest struct {
	Binding       SessionBinding
	AllowedModels []string
	PolicyDigest  string
	TTL           time.Duration
}

// CreateResult returns the public session and the raw token. The token is not
// recoverable from the Manager after this call.
type CreateResult struct {
	Session Session
	Token   BearerToken
}

// AuthorizeRequest is the complete request scope checked against a session.
// All binding fields are checked, which makes a session ID or token alone
// insufficient for authorization through this API.
type AuthorizeRequest struct {
	SessionID    SessionID
	Token        BearerToken
	Binding      SessionBinding
	Model        string
	PolicyDigest string
}

// PeerPolicy optionally constrains the operating-system identity of a Unix
// socket client. A nil field means that identity is captured but not
// constrained by the server configuration.
type PeerPolicy struct {
	UID *uint32
	GID *uint32
}

// UnixServerConfig configures a private per-run Unix HTTP server. The binding
// is fixed when the server is constructed; clients can select only a model
// already allowed by the session and must echo the policy digest.
type UnixServerConfig struct {
	SocketPath   string
	SessionID    SessionID
	Binding      SessionBinding
	PolicyDigest string
	Sessions     *Manager
	Peer         PeerPolicy
	Handler      http.Handler

	MaxHeaderBytes    int
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// ManagerConfig configures a session Manager.
type ManagerConfig struct {
	// Clock is injectable for deterministic tests. Production callers should
	// leave it nil.
	Clock  func() time.Time
	MaxTTL time.Duration
}

type contextSessionKey struct{}

// ContextPeerCredentialsKey is intentionally private. Use
// PeerCredentialsFromContext to read captured SO_PEERCRED information.
type contextPeerCredentialsKey struct{}

// ContextPeerErrorKey is intentionally private. A missing credential capture
// causes the authorized server to reject the request.
type contextPeerErrorKey struct{}
