// Package githubapp mints short-lived, repository-scoped GitHub App
// installation tokens.
//
// The package intentionally has no GitHub SDK dependency. It owns the narrow
// authentication boundary needed by the operator: an App JWT is made from a
// configured RSA private key, and that JWT is exchanged for an installation
// token with one of two closed permission profiles. The returned token is an
// opaque value whose formatting is always redacted; callers must explicitly
// ask for Value when they need to put it in an Authorization header.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultAPIBaseURL is the public GitHub API origin used by New when no
	// custom origin is configured.
	DefaultAPIBaseURL = "https://api.github.com"

	// DefaultRequestTimeout bounds a token exchange when the caller has not
	// supplied a timeout.
	DefaultRequestTimeout = 15 * time.Second
	MaxRequestTimeout     = 2 * time.Minute

	// DefaultJWTClockSkew follows GitHub's recommendation to put iat slightly
	// in the past, protecting against small clock differences.
	DefaultJWTClockSkew = time.Minute
	MaxJWTClockSkew     = 5 * time.Minute

	// GitHub permits App JWTs for at most ten minutes. Keeping our own hard
	// ceiling below that limit leaves room for clock skew and operational
	// mistakes.
	DefaultJWTLifetime = 9 * time.Minute
	MaxJWTLifetime     = 9 * time.Minute

	DefaultMaxResponseBytes = 64 << 10
	MaxResponseBytes        = 1 << 20

	// The GitHub installation-token response should be small. This is a
	// separate bound from the complete response bound so a pathological token
	// field cannot consume the whole JSON budget.
	MaxInstallationTokenBytes = 16 << 10

	// GitHub installation tokens normally live for one hour. The larger bound
	// accommodates compatible GitHub Enterprise implementations without
	// accepting effectively unbounded credentials.
	MaxInstallationTokenLifetime = 24 * time.Hour

	maxPrivateKeyPEMBytes = 1 << 20
	minRSAKeyBits         = 2048
	maxRSAKeyBits         = 8192
	maxRepositoryPart     = 100

	userAgent = "agents-gateway-v3-githubapp"
)

var (
	// The exported sentinel errors deliberately contain no dynamic input. In
	// particular, none of them can include a private key, JWT, installation
	// token, or response body.
	ErrInvalidConfig        = errors.New("githubapp: invalid configuration")
	ErrInvalidPrivateKey    = errors.New("githubapp: invalid RSA private key")
	ErrInvalidAPIOrigin     = errors.New("githubapp: invalid API origin")
	ErrInvalidRepository    = errors.New("githubapp: invalid repository")
	ErrInvalidPermission    = errors.New("githubapp: invalid permission profile")
	ErrInvalidContext       = errors.New("githubapp: invalid context")
	ErrClock                = errors.New("githubapp: invalid clock")
	ErrJWT                  = errors.New("githubapp: could not create App JWT")
	ErrRequest              = errors.New("githubapp: GitHub token request failed")
	ErrUnexpectedStatus     = errors.New("githubapp: unexpected GitHub token response status")
	ErrResponseTooLarge     = errors.New("githubapp: GitHub token response is too large")
	ErrInvalidResponse      = errors.New("githubapp: invalid GitHub token response")
	ErrTokenExpired         = errors.New("githubapp: GitHub installation token is expired")
	ErrTokenPermission      = errors.New("githubapp: GitHub installation token permissions are insufficient")
	ErrRedirect             = errors.New("githubapp: GitHub token endpoint redirected")
	ErrPrivateTestOnly      = errors.New("githubapp: private API origins are test-only")
	ErrInvalidHTTPClient    = errors.New("githubapp: invalid HTTP client")
	ErrPrivateKeyTooLarge   = errors.New("githubapp: private key is too large")
	ErrInvalidReference     = errors.New("githubapp: invalid Git reference")
	ErrCommitResolution     = errors.New("githubapp: Git commit resolution failed")
	ErrSourceNotFound       = errors.New("githubapp: source was not found")
	ErrSourceUnauthorized   = errors.New("githubapp: source authorization failed")
	ErrSourceConfiguration  = errors.New("githubapp: source configuration is invalid")
	ErrSourceRateLimited    = errors.New("githubapp: source request was rate limited")
	ErrSourceUnavailable    = errors.New("githubapp: source service is unavailable")
	ErrSourceTimeout        = errors.New("githubapp: source request timed out")
	ErrSourceTransport      = errors.New("githubapp: source transport failed")
	ErrSourceCanceled       = errors.New("githubapp: source request was canceled")
	errInvalidBaseURLPath   = errors.New("githubapp: invalid API origin path")
	errInvalidTokenContents = errors.New("githubapp: invalid installation token")
)

// ResolutionClass is the bounded classification of a source-pinning result.
// The class is deliberately independent of HTTP response text or request
// details so it can safely cross the controller retry boundary.
type ResolutionClass string

const (
	ClassInvalidReference ResolutionClass = "invalid-reference"
	ClassNotFound         ResolutionClass = "not-found"
	ClassUnauthorized     ResolutionClass = "unauthorized"
	ClassConfiguration    ResolutionClass = "configuration"
	ClassRateLimited      ResolutionClass = "rate-limited"
	ClassServer           ResolutionClass = "server"
	ClassTimeout          ResolutionClass = "timeout"
	ClassTransport        ResolutionClass = "transport"
	ClassCanceled         ResolutionClass = "canceled"
)

// ClassifiedError carries only a bounded class and a safe sentinel cause.
// It intentionally does not retain or render response bodies, URLs, tokens,
// private keys, or arbitrary transport error strings.
type ClassifiedError struct {
	class ResolutionClass
	cause error
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "githubapp: source resolution " + string(e.class)
}

func (e *ClassifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ClassifiedError) Is(target error) bool {
	return e != nil && target == sentinelForClass(e.class)
}

// ClassOf returns the bounded source-resolution class carried by err.
func ClassOf(err error) (ResolutionClass, bool) {
	if err == nil {
		return "", false
	}
	var classified *ClassifiedError
	if errors.As(err, &classified) && classified != nil {
		return classified.class, true
	}
	switch {
	case errors.Is(err, ErrInvalidReference):
		return ClassInvalidReference, true
	case errors.Is(err, ErrSourceNotFound):
		return ClassNotFound, true
	case errors.Is(err, ErrSourceUnauthorized), errors.Is(err, ErrTokenPermission):
		return ClassUnauthorized, true
	case errors.Is(err, ErrSourceConfiguration), errors.Is(err, ErrInvalidConfig), errors.Is(err, ErrInvalidPrivateKey), errors.Is(err, ErrPrivateKeyTooLarge), errors.Is(err, ErrInvalidAPIOrigin), errors.Is(err, ErrInvalidRepository), errors.Is(err, ErrInvalidPermission), errors.Is(err, ErrInvalidContext), errors.Is(err, ErrClock), errors.Is(err, ErrJWT), errors.Is(err, ErrInvalidHTTPClient), errors.Is(err, ErrInvalidResponse), errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrUnexpectedStatus), errors.Is(err, ErrRedirect), errors.Is(err, ErrTokenExpired), errors.Is(err, errInvalidTokenContents):
		return ClassConfiguration, true
	case errors.Is(err, ErrSourceRateLimited):
		return ClassRateLimited, true
	case errors.Is(err, ErrSourceUnavailable):
		return ClassServer, true
	case errors.Is(err, ErrSourceTimeout), errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout, true
	case errors.Is(err, ErrSourceCanceled), errors.Is(err, context.Canceled):
		return ClassCanceled, true
	case errors.Is(err, ErrSourceTransport), errors.Is(err, ErrRequest), errors.Is(err, ErrCommitResolution):
		return ClassTransport, true
	default:
		return "", false
	}
}

// IsPermanentClass reports whether retrying the same source request cannot
// change the outcome without an input or authorization/configuration change.
func IsPermanentClass(class ResolutionClass) bool {
	switch class {
	case ClassInvalidReference, ClassNotFound, ClassUnauthorized, ClassConfiguration:
		return true
	default:
		return false
	}
}

// IsRetryableClass reports whether a classified outcome is a bounded
// transient condition.
func IsRetryableClass(class ResolutionClass) bool {
	switch class {
	case ClassRateLimited, ClassServer, ClassTimeout, ClassTransport:
		return true
	default:
		return false
	}
}

func newClassifiedError(class ResolutionClass, cause error) error {
	if cause == nil {
		cause = sentinelForClass(class)
	}
	return &ClassifiedError{class: class, cause: cause}
}

func sentinelForClass(class ResolutionClass) error {
	switch class {
	case ClassInvalidReference:
		return ErrInvalidReference
	case ClassNotFound:
		return ErrSourceNotFound
	case ClassUnauthorized:
		return ErrSourceUnauthorized
	case ClassConfiguration:
		return ErrSourceConfiguration
	case ClassRateLimited:
		return ErrSourceRateLimited
	case ClassServer:
		return ErrSourceUnavailable
	case ClassTimeout:
		return ErrSourceTimeout
	case ClassTransport:
		return ErrSourceTransport
	case ClassCanceled:
		return ErrSourceCanceled
	default:
		return nil
	}
}

func classifyHTTPStatus(status int, cause error) error {
	switch {
	case status == http.StatusNotFound:
		return newClassifiedError(ClassNotFound, cause)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return newClassifiedError(ClassUnauthorized, cause)
	case status == http.StatusRequestTimeout:
		return newClassifiedError(ClassTimeout, cause)
	case status == http.StatusTooManyRequests:
		return newClassifiedError(ClassRateLimited, cause)
	case status >= http.StatusInternalServerError && status <= 599:
		return newClassifiedError(ClassServer, cause)
	default:
		return newClassifiedError(ClassConfiguration, cause)
	}
}

func classifySentinel(err error) error {
	if err == nil {
		return nil
	}
	if class, ok := ClassOf(err); ok {
		return newClassifiedError(class, err)
	}
	return newClassifiedError(ClassConfiguration, ErrInvalidResponse)
}

// PermissionProfile is a closed set of permissions the operator may request.
// Keeping this type closed prevents callers from accidentally minting a token
// with a broad or future permission set through this package.
type PermissionProfile string

const (
	// PermissionCloneRead allows the work initContainer to fetch a repository.
	PermissionCloneRead PermissionProfile = "clone-read"
	// PermissionPublish allows the controller to create a branch/commit and
	// open a pull request.
	PermissionPublish PermissionProfile = "publish"
)

// Repository identifies exactly one GitHub owner/repository pair. Owner and
// Name are deliberately separate so a path containing multiple repositories,
// a URL, or traversal syntax cannot be sent to GitHub.
type Repository struct {
	Owner string
	Name  string
}

// FullName returns the canonical owner/name representation. It does not
// validate the repository; Mint validates it before constructing a request.
func (r Repository) FullName() string {
	return r.Owner + "/" + r.Name
}

// Validate checks the restricted owner and repository grammar used by this
// package. It is intentionally narrower than arbitrary URL/path parsing.
func (r Repository) Validate() error {
	if !validRepositoryPart(r.Owner) || !validRepositoryPart(r.Name) {
		return ErrInvalidRepository
	}
	return nil
}

// Config controls a Minter. PrivateKeyPEM is parsed during construction and
// is not retained. BaseURL is empty by default and therefore resolves to the
// official api.github.com origin.
type Config struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte

	// BaseURL may point at a public HTTPS GitHub Enterprise API origin. New
	// rejects private, loopback, link-local, and unspecified destinations. Use
	// NewForTest only with a local httptest TLS origin.
	BaseURL string

	HTTPClient      *http.Client
	Clock           func() time.Time
	RequestTimeout  time.Duration
	JWTClockSkew    time.Duration
	JWTLifetime     time.Duration
	MaxResponseSize int64
}

// Minter is safe for concurrent Mint calls after construction. It contains
// only immutable configuration and a parsed private key; the caller's Clock,
// if injected, must itself be safe for concurrent use.
type Minter struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	baseURL        url.URL
	client         *http.Client
	clock          func() time.Time
	requestTimeout time.Duration
	jwtClockSkew   time.Duration
	jwtLifetime    time.Duration
	maxResponse    int64
}

// New constructs a production-safe minter. It accepts the official GitHub
// API by default and public HTTPS custom origins for GitHub Enterprise. A
// private or loopback custom origin is rejected here even if it is HTTPS.
func New(cfg Config) (*Minter, error) {
	return newMinter(cfg, false)
}

// NewForTest is the explicit local-test seam. It permits a private or
// loopback HTTPS origin so an httptest.NewTLSServer can stand in for GitHub.
// Production code must use New; this constructor exists solely to make the
// transport boundary testable without DNS, proxies, or a live GitHub App.
func NewForTest(cfg Config) (*Minter, error) {
	return newMinter(cfg, true)
}

func newMinter(cfg Config, allowPrivateOrigin bool) (*Minter, error) {
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 {
		return nil, ErrInvalidConfig
	}
	if len(cfg.PrivateKeyPEM) == 0 {
		return nil, ErrInvalidPrivateKey
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}

	base, err := parseBaseURL(cfg.BaseURL, allowPrivateOrigin)
	if err != nil {
		return nil, err
	}

	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = DefaultRequestTimeout
	}
	if requestTimeout <= 0 || requestTimeout > MaxRequestTimeout {
		return nil, ErrInvalidConfig
	}

	clockSkew := cfg.JWTClockSkew
	if clockSkew == 0 {
		clockSkew = DefaultJWTClockSkew
	}
	if clockSkew < 0 || clockSkew > MaxJWTClockSkew {
		return nil, ErrInvalidConfig
	}

	lifetime := cfg.JWTLifetime
	if lifetime == 0 {
		lifetime = DefaultJWTLifetime
	}
	if lifetime < time.Second || lifetime > MaxJWTLifetime {
		return nil, ErrInvalidConfig
	}

	maxResponse := cfg.MaxResponseSize
	if maxResponse == 0 {
		maxResponse = DefaultMaxResponseBytes
	}
	if maxResponse <= 0 || maxResponse > MaxResponseBytes {
		return nil, ErrInvalidConfig
	}

	client, err := cloneHTTPClient(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Minter{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		privateKey:     key,
		baseURL:        base,
		client:         client,
		clock:          clock,
		requestTimeout: requestTimeout,
		jwtClockSkew:   clockSkew,
		jwtLifetime:    lifetime,
		maxResponse:    maxResponse,
	}, nil
}

// InstallationToken is an opaque, short-lived installation credential. Its
// String and fmt.Formatter implementations always redact the secret. The
// only way to obtain the token bytes is an explicit Value call at the point
// where an HTTP Authorization header is assembled.
type InstallationToken struct {
	value      string
	expiresAt  time.Time
	repository Repository
	profile    PermissionProfile
}

// Value returns the GitHub installation token for deliberate use by a caller.
// Callers should keep the returned string within the shortest possible scope.
func (t InstallationToken) Value() string {
	return t.value
}

// ExpiresAt returns the server-provided expiry timestamp.
func (t InstallationToken) ExpiresAt() time.Time {
	return t.expiresAt
}

// Repository returns the one repository to which this value was scoped.
func (t InstallationToken) Repository() Repository {
	return t.repository
}

// PermissionProfile returns the closed profile used for the exchange.
func (t InstallationToken) PermissionProfile() PermissionProfile {
	return t.profile
}

// IsZero reports whether no token was returned.
func (t InstallationToken) IsZero() bool {
	return t.value == ""
}

// String prevents accidental secret exposure in logs and errors.
func (t InstallationToken) String() string {
	return "[REDACTED github installation token]"
}

// Format prevents fmt's alternate verbs, including %+v and %#v, from
// bypassing String and printing the unexported token field.
func (t InstallationToken) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, t.String())
}

// Mint exchanges an App JWT for an installation token scoped to exactly repo
// and one of the two explicit permission profiles.
func (m *Minter) Mint(ctx context.Context, repo Repository, profile PermissionProfile) (InstallationToken, error) {
	if m == nil || m.privateKey == nil || m.client == nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, ErrInvalidConfig)
	}
	if ctx == nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, ErrInvalidContext)
	}
	if err := repo.Validate(); err != nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, err)
	}
	permissions, err := permissionsFor(profile)
	if err != nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, err)
	}
	if err := ctx.Err(); err != nil {
		return InstallationToken{}, err
	}

	now := m.clock()
	if now.IsZero() || now.Unix() <= 0 {
		return InstallationToken{}, ErrClock
	}
	now = now.UTC()
	jwt, err := m.appJWT(now)
	if err != nil {
		return InstallationToken{}, err
	}

	body, err := json.Marshal(tokenRequest{
		Repositories: []string{repo.FullName()},
		Permissions:  permissions,
	})
	if err != nil {
		return InstallationToken{}, ErrRequest
	}
	endpoint := m.installationEndpoint()
	requestCtx, cancel := context.WithTimeout(ctx, m.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return InstallationToken{}, ErrRequest
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	response, err := m.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return InstallationToken{}, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return InstallationToken{}, newClassifiedError(ClassTimeout, context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) {
			return InstallationToken{}, newClassifiedError(ClassCanceled, context.Canceled)
		}
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			return InstallationToken{}, newClassifiedError(ClassTimeout, ErrRequest)
		}
		return InstallationToken{}, newClassifiedError(ClassTransport, ErrRequest)
	}
	if response == nil || response.Body == nil {
		return InstallationToken{}, newClassifiedError(ClassTransport, ErrRequest)
	}
	defer response.Body.Close()

	responseBody, err := readBounded(response.Body, m.maxResponse)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return InstallationToken{}, ctxErr
		}
		if errors.Is(err, ErrResponseTooLarge) {
			return InstallationToken{}, newClassifiedError(ClassConfiguration, err)
		}
		return InstallationToken{}, newClassifiedError(ClassTransport, ErrRequest)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, ErrRedirect)
	}
	if response.StatusCode != http.StatusCreated {
		return InstallationToken{}, classifyHTTPStatus(response.StatusCode, ErrUnexpectedStatus)
	}

	decoded, err := decodeTokenResponse(responseBody)
	if err != nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, err)
	}
	if err := validateTokenResponse(decoded, repo, profile, now); err != nil {
		return InstallationToken{}, classifySentinel(err)
	}
	expiresAt, err := parseTokenExpiry(decoded.ExpiresAt)
	if err != nil {
		return InstallationToken{}, newClassifiedError(ClassConfiguration, ErrInvalidResponse)
	}

	return InstallationToken{
		value:      decoded.Token,
		expiresAt:  expiresAt,
		repository: repo,
		profile:    profile,
	}, nil
}

// ResolveCommit resolves ref to an exact Git object ID using a previously
// minted repository-scoped installation token. The response body and token
// are never included in returned errors. This is called by the controller
// before it persists the immutable resolved run and before a Sandbox exists.
func (m *Minter) ResolveCommit(ctx context.Context, token InstallationToken, repo Repository, ref string) (string, error) {
	if m == nil || m.client == nil || ctx == nil {
		return "", newClassifiedError(ClassConfiguration, ErrInvalidConfig)
	}
	if err := repo.Validate(); err != nil {
		return "", newClassifiedError(ClassConfiguration, err)
	}
	if !validReference(ref) {
		return "", newClassifiedError(ClassInvalidReference, ErrInvalidReference)
	}
	now := m.clock().UTC()
	if token.IsZero() || token.Repository() != repo || token.ExpiresAt().IsZero() || !token.ExpiresAt().After(now) ||
		(token.PermissionProfile() != PermissionCloneRead && token.PermissionProfile() != PermissionPublish) {
		return "", newClassifiedError(ClassUnauthorized, ErrTokenPermission)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(m.baseURL.String(), "/") + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/commits/" + url.PathEscape(ref)
	requestCtx, cancel := context.WithTimeout(ctx, m.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", newClassifiedError(ClassConfiguration, ErrCommitResolution)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token.Value())
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := m.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", newClassifiedError(ClassTimeout, context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) {
			return "", newClassifiedError(ClassCanceled, context.Canceled)
		}
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			return "", newClassifiedError(ClassTimeout, ErrCommitResolution)
		}
		return "", newClassifiedError(ClassTransport, ErrCommitResolution)
	}
	if response == nil || response.Body == nil {
		return "", newClassifiedError(ClassTransport, ErrCommitResolution)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, m.maxResponse)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return "", newClassifiedError(ClassConfiguration, err)
		}
		return "", newClassifiedError(ClassTransport, ErrCommitResolution)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		return "", newClassifiedError(ClassConfiguration, ErrRedirect)
	}
	if response.StatusCode != http.StatusOK {
		return "", classifyHTTPStatus(response.StatusCode, ErrUnexpectedStatus)
	}
	var decoded struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || !validCommitSHA(decoded.SHA) {
		return "", newClassifiedError(ClassConfiguration, ErrInvalidResponse)
	}
	return decoded.SHA, nil
}

func validReference(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.TrimSpace(value) != value || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n?#") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

type tokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  githubPermissions `json:"permissions"`
}

type githubPermissions struct {
	Contents     string `json:"contents"`
	PullRequests string `json:"pull_requests,omitempty"`
}

func permissionsFor(profile PermissionProfile) (githubPermissions, error) {
	switch profile {
	case PermissionCloneRead:
		return githubPermissions{Contents: "read"}, nil
	case PermissionPublish:
		return githubPermissions{Contents: "write", PullRequests: "write"}, nil
	default:
		return githubPermissions{}, ErrInvalidPermission
	}
}

type tokenResponse struct {
	Token               string                    `json:"token"`
	ExpiresAt           string                    `json:"expires_at"`
	Permissions         map[string]string         `json:"permissions"`
	RepositorySelection string                    `json:"repository_selection"`
	Repositories        []tokenResponseRepository `json:"repositories"`
}

type tokenResponseRepository struct {
	FullName string `json:"full_name"`
}

func decodeTokenResponse(body []byte) (tokenResponse, error) {
	if len(body) == 0 {
		return tokenResponse{}, ErrInvalidResponse
	}
	var response tokenResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return tokenResponse{}, ErrInvalidResponse
	}
	return response, nil
}

func validateTokenResponse(response tokenResponse, repo Repository, profile PermissionProfile, now time.Time) error {
	if len(response.Token) == 0 || len(response.Token) > MaxInstallationTokenBytes || !validTokenContents(response.Token) {
		return errInvalidTokenContents
	}
	if response.ExpiresAt == "" {
		return ErrInvalidResponse
	}
	expiresAt, err := parseTokenExpiry(response.ExpiresAt)
	if err != nil {
		return ErrInvalidResponse
	}
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return ErrTokenExpired
	}
	if expiresAt.After(now.Add(MaxInstallationTokenLifetime)) {
		return ErrInvalidResponse
	}

	required, err := permissionsFor(profile)
	if err != nil {
		return err
	}
	if response.Permissions == nil || response.Permissions["contents"] != required.Contents {
		return ErrTokenPermission
	}
	if profile == PermissionPublish && response.Permissions["pull_requests"] != required.PullRequests {
		return ErrTokenPermission
	}
	if profile == PermissionCloneRead {
		if value, ok := response.Permissions["pull_requests"]; ok && value != "" {
			return ErrTokenPermission
		}
	}
	if response.RepositorySelection != "" && response.RepositorySelection != "selected" {
		return ErrInvalidResponse
	}
	if response.Repositories != nil {
		if len(response.Repositories) != 1 || response.Repositories[0].FullName != repo.FullName() {
			return ErrInvalidResponse
		}
	}
	return nil
}

func parseTokenExpiry(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, ErrInvalidResponse
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || expiresAt.IsZero() {
		return time.Time{}, ErrInvalidResponse
	}
	return expiresAt.UTC(), nil
}

func validTokenContents(token string) bool {
	if strings.TrimSpace(token) != token {
		return false
	}
	for _, character := range token {
		if character < 0x21 || character > 0x7e || unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func (m *Minter) installationEndpoint() string {
	endpoint := m.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/app/installations/" + strconv.FormatInt(m.installationID, 10) + "/access_tokens"
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String()
}

func (m *Minter) appJWT(now time.Time) (string, error) {
	now = now.UTC()
	nowUnix := now.Unix()
	iat := now.Add(-m.jwtClockSkew).Unix()
	exp := now.Add(m.jwtLifetime).Unix()
	maxExp := nowUnix + int64(MaxJWTLifetime/time.Second)
	if nowUnix <= 0 || iat <= 0 || exp <= nowUnix || exp <= iat || exp > maxExp {
		return "", ErrJWT
	}

	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", ErrJWT
	}
	claims, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{IssuedAt: iat, ExpiresAt: exp, Issuer: m.appID})
	if err != nil {
		return "", ErrJWT
	}
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, m.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", ErrJWT
	}
	return signingInput + "." + encoding.EncodeToString(signature), nil
}

func parsePrivateKey(input []byte) (*rsa.PrivateKey, error) {
	if len(input) > maxPrivateKeyPEMBytes {
		return nil, ErrPrivateKeyTooLarge
	}
	block, rest := pem.Decode(input)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, ErrInvalidPrivateKey
	}
	var (
		key *rsa.PrivateKey
		err error
	)
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			key, ok = parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, ErrInvalidPrivateKey
			}
		}
	default:
		return nil, ErrInvalidPrivateKey
	}
	if err != nil || key == nil || key.N == nil || key.E <= 0 || key.Size() == 0 {
		return nil, ErrInvalidPrivateKey
	}
	if key.N.BitLen() < minRSAKeyBits || key.N.BitLen() > maxRSAKeyBits {
		return nil, ErrInvalidPrivateKey
	}
	if err := key.Validate(); err != nil {
		return nil, ErrInvalidPrivateKey
	}
	key.Precompute()
	return key, nil
}

func parseBaseURL(raw string, allowPrivateOrigin bool) (url.URL, error) {
	if raw == "" {
		raw = DefaultAPIBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return url.URL{}, ErrInvalidAPIOrigin
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return url.URL{}, ErrInvalidAPIOrigin
	}
	if parsed.RawPath != "" || !validBasePath(parsed.Path) {
		return url.URL{}, errInvalidBaseURLPath
	}
	if parsed.Port() != "" {
		if _, err := strconv.ParseUint(parsed.Port(), 10, 16); err != nil {
			return url.URL{}, ErrInvalidAPIOrigin
		}
	}
	if !allowPrivateOrigin && unsafeHost(parsed.Hostname()) {
		return url.URL{}, ErrPrivateTestOnly
	}
	parsed.Scheme = "https"
	return *parsed, nil
}

func validBasePath(value string) bool {
	if value == "" || value == "/" {
		return true
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return path.Clean(value) == value || strings.HasSuffix(value, "/") && path.Clean(value)+"/" == value
}

func unsafeHost(rawHost string) bool {
	host := strings.ToLower(strings.TrimSuffix(rawHost, "."))
	if host == "" || host == "localhost" || host == "local" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return unsafeIP(ip)
	}
	return false
}

func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func cloneHTTPClient(input *http.Client) (*http.Client, error) {
	client := &http.Client{}
	if input != nil {
		*client = *input
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// Installation-token requests must not carry ambient cookies from a
	// caller's browser-oriented client. The JWT is the only credential this
	// exchange should send.
	client.Jar = nil

	transport := client.Transport
	if transport == nil {
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || defaultTransport == nil {
			return nil, ErrInvalidHTTPClient
		}
		transport = defaultTransport.Clone()
	}
	if standardTransport, ok := transport.(*http.Transport); ok {
		standardTransport = standardTransport.Clone()
		// The package never inherits HTTP(S)_PROXY from the process for the
		// default transport or for an injected standard transport.
		standardTransport.Proxy = nil
		client.Transport = standardTransport
	} else {
		// A custom RoundTripper has no proxy setting we can safely inspect or
		// mutate. It is retained as the explicit test seam.
		client.Transport = transport
	}
	return client, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit > MaxResponseBytes {
		return nil, ErrInvalidConfig
	}
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, ErrRequest
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func validRepositoryPart(value string) bool {
	if len(value) == 0 || len(value) > maxRepositoryPart || value == "." || value == ".." || !utf8ASCII(value) {
		return false
	}
	for index, character := range []byte(value) {
		if index == 0 && !isASCIIAlphaNumeric(character) {
			return false
		}
		if !isASCIIAlphaNumeric(character) && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func utf8ASCII(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] >= 0x80 {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
