// Package artifactauth defines the credential lease boundary for artifacts.
//
// The package intentionally does not know about Kubernetes, an S3 SDK, or a
// particular STS implementation. A provider adapter implements Minter and
// returns an already-scoped, short-lived lease. Issuer validates that result
// and exposes only bounded, non-secret Metadata for status/audit. Credential
// bytes remain in an in-memory Result until an integration explicitly copies
// them into the owner-referenced per-run Secret.
//
// The static path is a compatibility escape hatch for installations that do
// not have an S3-compatible lease service yet. It requires two independent
// opt-ins (operator configuration and the run request), is allowed only for a
// bounded projection lifetime, and is marked StaticCopy=true and Revocable=false
// in Metadata. It is not equivalent to a revocable temporary credential.
package artifactauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ContractVersion = 1

	// These are the only Secret data keys this package can produce. They are
	// deliberately stable so runsecret/workload integrations can project them
	// without accepting arbitrary provider-controlled names.
	AccessKeyIDKey     = "artifact-access-key-id"
	SecretAccessKeyKey = "artifact-secret-access-key"
	SessionTokenKey    = "artifact-session-token"

	ModeShortLived Mode = "short-lived"
	ModeStaticCopy Mode = "static-copy"

	DefaultLeaseLifetime       = 15 * time.Minute
	MaxLeaseLifetime           = time.Hour
	MinLeaseLifetime           = 30 * time.Second
	DefaultStaticLeaseLifetime = 15 * time.Minute
	MaxStaticLeaseLifetime     = 30 * time.Minute

	MaxEndpointBytes        = 512
	MaxRegionBytes          = 64
	MaxBucketBytes          = 63
	MaxPrefixBytes          = 512
	MaxPurposeBytes         = 64
	MaxSourceRefBytes       = 253
	MaxLeaseIDBytes         = 256
	MaxAccessKeyIDBytes     = 256
	MaxSecretAccessKeyBytes = 512
	MaxSessionTokenBytes    = 4096
	MaxRunUIDBytes          = 128
)

var (
	ErrInvalidConfig          = errors.New("artifactauth: invalid configuration")
	ErrInvalidRequest         = errors.New("artifactauth: invalid lease request")
	ErrInvalidScope           = errors.New("artifactauth: invalid object-store scope")
	ErrInvalidCredentials     = errors.New("artifactauth: invalid object-store credentials")
	ErrInvalidExpiry          = errors.New("artifactauth: invalid credential expiry")
	ErrProviderUnavailable    = errors.New("artifactauth: credential provider unavailable")
	ErrMintFailed             = errors.New("artifactauth: credential mint failed")
	ErrStaticFallbackDisabled = errors.New("artifactauth: static credential fallback is disabled")
	ErrStaticSourceFailed     = errors.New("artifactauth: static credential source failed")
	ErrProjection             = errors.New("artifactauth: invalid Secret projection")
	ErrProjectionConflict     = errors.New("artifactauth: Secret projection conflicts with existing data")
	ErrMetadata               = errors.New("artifactauth: invalid lease metadata")
)

// Mode identifies how a lease was obtained. It is safe to persist in status.
type Mode string

// Permissions is intentionally limited to read and write. Delete/list/admin
// permissions cannot be expressed by this contract and therefore cannot be
// accidentally requested through it.
type Permissions struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
}

// Scope is the complete object-store boundary granted to one run. Prefix is
// an exact relative object-key prefix; wildcard, traversal, root-bucket, and
// cross-run scopes are rejected. A request scope must end in
// /runs/<RunIdentity.UID> (or be exactly runs/<RunIdentity.UID>). Endpoint may
// be empty for the provider's default S3 endpoint, but a custom endpoint must
// be HTTPS and credential-free.
type Scope struct {
	Endpoint       string      `json:"endpoint,omitempty"`
	Region         string      `json:"region,omitempty"`
	Bucket         string      `json:"bucket"`
	Prefix         string      `json:"prefix"`
	Permissions    Permissions `json:"permissions"`
	ForcePathStyle bool        `json:"forcePathStyle,omitempty"`
}

// KeyPrefix is the effective directory boundary an S3 policy adapter should
// use. Prefix intentionally omits the separator so the scope cannot be
// confused with a key that merely shares the same leading bytes (for example,
// runs/job versus runs/job-evil).
func (s Scope) KeyPrefix() string {
	if s.Prefix == "" {
		return ""
	}
	return s.Prefix + "/"
}

// RunIdentity binds a lease to one AgentRun without carrying any credential
// material. UID is included in idempotency and metadata so a deleted/recreated
// object with the same name cannot reuse a lease contract.
type RunIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Request is the controller-owned input to Issuer.Issue. AllowStaticFallback
// is deliberately per-run in addition to the operator-level opt-in; a static
// credential can never be selected accidentally by a normal run.
type Request struct {
	Run                 RunIdentity   `json:"run"`
	Scope               Scope         `json:"scope"`
	Purpose             string        `json:"purpose"`
	TTL                 time.Duration `json:"-"`
	AllowStaticFallback bool          `json:"allowStaticFallback"`
}

// MintRequest is the provider-facing request. IdempotencyKey is a digest and
// contains no raw run identity or credentials. Providers should use it when
// their lease API supports idempotent issuance.
type MintRequest struct {
	Run            RunIdentity   `json:"run"`
	Scope          Scope         `json:"scope"`
	Purpose        string        `json:"purpose"`
	TTL            time.Duration `json:"-"`
	IdempotencyKey string        `json:"idempotencyKey"`
}

// CredentialSet is intentionally not serializable as part of Metadata. It is
// held only in memory and copied into a Secret by an explicit integration.
// SessionToken is optional for S3-compatible providers that issue expiring
// access/secret pairs rather than AWS-style session credentials.
type CredentialSet struct {
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
	SessionToken    string `json:"-"`
}

// MintedLease is the untrusted provider result. Issuer requires an exact
// scope match and rejects credentials or expiry outside the requested bounds.
type MintedLease struct {
	Credentials CredentialSet
	Scope       Scope
	ExpiresAt   time.Time
	LeaseID     string `json:"-"`
}

// Minter is implemented by an adapter around STS, Vault, an S3-compatible
// lease service, or another provider. It must return credentials scoped to the
// exact Scope in MintRequest and an expiry no later than MintRequest.TTL.
// Provider errors are never returned verbatim by Issuer because they may
// contain credentials, URLs, or response bodies.
type Minter interface {
	Mint(context.Context, MintRequest) (MintedLease, error)
}

// MinterFunc adapts a function to Minter for provider adapters and tests.
type MinterFunc func(context.Context, MintRequest) (MintedLease, error)

func (f MinterFunc) Mint(ctx context.Context, request MintRequest) (MintedLease, error) {
	return f(ctx, request)
}

// StaticRequest is passed only to the explicitly configured static source.
// SourceRef is operator configuration, not AgentRun input.
type StaticRequest struct {
	Run       RunIdentity
	Scope     Scope
	Purpose   string
	SourceRef string `json:"-"`
}

// StaticSource reads a trusted operator-owned credential. Implementations
// should perform a named read only; Issuer does not retain the source name in
// Metadata and never includes source errors in returned error text.
type StaticSource interface {
	Load(context.Context, StaticRequest) (CredentialSet, error)
}

// StaticSourceFunc adapts a function to StaticSource for operator integrations
// and tests.
type StaticSourceFunc func(context.Context, StaticRequest) (CredentialSet, error)

func (f StaticSourceFunc) Load(ctx context.Context, request StaticRequest) (CredentialSet, error) {
	return f(ctx, request)
}

// StaticFallbackConfig enables the compatibility path. Enabled, Source, and
// SourceRef must all be present. MaxLifetime bounds the per-run Secret
// projection, although static credentials themselves are not cryptographically
// revoked at that time.
type StaticFallbackConfig struct {
	Enabled     bool
	SourceRef   string
	Source      StaticSource
	MaxLifetime time.Duration
}

// Config supplies provider seams. Minter may be nil only when the static path
// is explicitly configured; a request must still opt into that path.
type Config struct {
	Minter Minter
	Static StaticFallbackConfig
	Clock  func() time.Time
}

// ProjectionKeys describes the fixed names written to the existing per-run
// Secret. It contains names only, never values.
type ProjectionKeys struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// Metadata is the immutable, non-secret lease result. It is safe to copy into
// AgentRun status or an audit artifact. ContractDigest covers every field
// except itself and binds the projection key set, exact scope, run identity,
// mode, lease ID digest, and expiry.
type Metadata struct {
	Version               int            `json:"version"`
	Run                   RunIdentity    `json:"run"`
	Purpose               string         `json:"purpose"`
	Mode                  Mode           `json:"mode"`
	StaticCopy            bool           `json:"staticCopy"`
	Revocable             bool           `json:"revocable"`
	CredentialExpiryKnown bool           `json:"credentialExpiryKnown"`
	LeaseIDDigest         string         `json:"leaseIdDigest"`
	ScopeDigest           string         `json:"scopeDigest"`
	IssuedAt              time.Time      `json:"issuedAt"`
	ExpiresAt             time.Time      `json:"expiresAt"`
	Scope                 Scope          `json:"scope"`
	Projection            ProjectionKeys `json:"projection"`
	ContractDigest        string         `json:"contractDigest"`
}

// Result carries safe Metadata plus sensitive bytes kept out of its JSON
// representation. Use ProjectSecretData to add the bytes to a freshly created
// immutable run Secret. Result values should be Wipe'd after projection.
type Result struct {
	Metadata Metadata `json:"metadata"`

	data map[string][]byte
}

// Issuer validates and packages leases. It is stateless after construction and
// safe for concurrent use.
type Issuer struct {
	minter Minter
	static StaticFallbackConfig
	clock  func() time.Time
}

// New validates configuration without contacting a provider or source.
func New(config Config) (*Issuer, error) {
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	static := config.Static
	if !static.Enabled {
		if static.Source != nil || static.SourceRef != "" || static.MaxLifetime != 0 {
			return nil, ErrInvalidConfig
		}
		if config.Minter == nil {
			return nil, ErrInvalidConfig
		}
	} else {
		if static.Source == nil || !validSourceRef(static.SourceRef) {
			return nil, ErrInvalidConfig
		}
		if static.MaxLifetime == 0 {
			static.MaxLifetime = DefaultStaticLeaseLifetime
		}
		if static.MaxLifetime < MinLeaseLifetime || static.MaxLifetime > MaxStaticLeaseLifetime {
			return nil, ErrInvalidConfig
		}
	}
	return &Issuer{minter: config.Minter, static: static, clock: clock}, nil
}

// Issue obtains a lease, validates its provider-declared scope and expiry, and
// returns non-secret metadata plus an in-memory Secret projection. It never
// returns provider/source error text.
func (i *Issuer) Issue(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, ErrInvalidRequest
	}
	if i == nil || i.clock == nil {
		return Result{}, ErrInvalidConfig
	}
	normalized, err := normalizeRequest(request)
	if err != nil {
		return Result{}, err
	}
	now := i.clock().UTC()
	if now.IsZero() {
		return Result{}, ErrInvalidConfig
	}
	requestForProvider := MintRequest{
		Run:            normalized.Run,
		Scope:          normalized.Scope,
		Purpose:        normalized.Purpose,
		TTL:            normalized.TTL,
		IdempotencyKey: idempotencyKey(normalized),
	}

	if i.minter != nil {
		minted, mintErr := i.minter.Mint(ctx, requestForProvider)
		if mintErr == nil {
			return i.resultFromMint(now, normalized, minted)
		}
		if contextError(mintErr) != nil {
			return Result{}, contextError(mintErr)
		}
		if !errors.Is(mintErr, ErrProviderUnavailable) {
			return Result{}, ErrMintFailed
		}
		if !normalized.AllowStaticFallback {
			return Result{}, ErrProviderUnavailable
		}
	} else if !normalized.AllowStaticFallback {
		return Result{}, ErrProviderUnavailable
	}

	if !i.static.Enabled || i.static.Source == nil || !normalized.AllowStaticFallback {
		return Result{}, ErrStaticFallbackDisabled
	}
	if normalized.TTL > i.static.MaxLifetime {
		return Result{}, ErrStaticFallbackDisabled
	}
	credentials, sourceErr := i.static.Source.Load(ctx, StaticRequest{
		Run: normalized.Run, Scope: normalized.Scope, Purpose: normalized.Purpose, SourceRef: i.static.SourceRef,
	})
	if sourceErr != nil {
		if contextErr := contextError(sourceErr); contextErr != nil {
			return Result{}, contextErr
		}
		return Result{}, ErrStaticSourceFailed
	}
	if err := validateCredentials(credentials); err != nil {
		return Result{}, err
	}
	return makeResult(now, now.Add(normalized.TTL), normalized, credentials, ModeStaticCopy, false, false, staticLeaseID(normalized))
}

func (i *Issuer) resultFromMint(now time.Time, request Request, minted MintedLease) (Result, error) {
	if err := validateCredentials(minted.Credentials); err != nil {
		return Result{}, err
	}
	providerScope, err := normalizeScope(minted.Scope)
	if err != nil || providerScope != request.Scope {
		return Result{}, ErrInvalidScope
	}
	if !validLeaseID(minted.LeaseID) {
		return Result{}, ErrInvalidCredentials
	}
	expiresAt := minted.ExpiresAt.UTC()
	if expiresAt.IsZero() || expiresAt.Before(now.Add(MinLeaseLifetime)) || expiresAt.After(now.Add(request.TTL)) {
		return Result{}, ErrInvalidExpiry
	}
	return makeResult(now, expiresAt, request, minted.Credentials, ModeShortLived, true, true, minted.LeaseID)
}

func makeResult(now, expiresAt time.Time, request Request, credentials CredentialSet, mode Mode, revocable, expiryKnown bool, leaseID string) (Result, error) {
	if now.IsZero() || expiresAt.IsZero() || !expiresAt.After(now) {
		return Result{}, ErrInvalidExpiry
	}
	scopeDigest, err := digestScope(request.Scope)
	if err != nil {
		return Result{}, ErrInvalidScope
	}
	keys := ProjectionKeys{AccessKeyID: AccessKeyIDKey, SecretAccessKey: SecretAccessKeyKey}
	data := map[string][]byte{
		AccessKeyIDKey:     []byte(credentials.AccessKeyID),
		SecretAccessKeyKey: []byte(credentials.SecretAccessKey),
	}
	if credentials.SessionToken != "" {
		keys.SessionToken = SessionTokenKey
		data[SessionTokenKey] = []byte(credentials.SessionToken)
	}
	metadata := Metadata{
		Version:               ContractVersion,
		Run:                   request.Run,
		Purpose:               request.Purpose,
		Mode:                  mode,
		StaticCopy:            mode == ModeStaticCopy,
		Revocable:             revocable,
		CredentialExpiryKnown: expiryKnown,
		LeaseIDDigest:         digestString("agw-artifact-lease-id\x00" + leaseID),
		ScopeDigest:           scopeDigest,
		IssuedAt:              now.UTC(),
		ExpiresAt:             expiresAt.UTC(),
		Scope:                 request.Scope,
		Projection:            keys,
	}
	metadata.ContractDigest, err = metadataDigest(metadata)
	if err != nil {
		return Result{}, ErrMetadata
	}
	return Result{Metadata: metadata, data: data}, nil
}

// CopySecretData returns a deep copy of the sensitive projection. The result
// is suitable for the Data field of a Kubernetes Secret, but callers must not
// serialize it into status, pod specs, logs, or errors.
func (r Result) CopySecretData() (map[string][]byte, error) {
	if err := r.verify(); err != nil {
		return nil, err
	}
	return cloneData(r.data), nil
}

// ProjectSecretData adds this lease's fixed keys to dst atomically. Existing
// equal values are accepted for idempotent reconciliation; a different value
// or an invalid result fails closed. Unknown keys already in dst are untouched.
func (r Result) ProjectSecretData(dst map[string][]byte) error {
	if dst == nil {
		return ErrProjection
	}
	if err := r.verify(); err != nil {
		return err
	}
	for key, value := range r.data {
		if existing, ok := dst[key]; ok && !bytes.Equal(existing, value) {
			return ErrProjectionConflict
		}
	}
	for key, value := range r.data {
		if _, ok := dst[key]; !ok {
			dst[key] = append([]byte(nil), value...)
		}
	}
	return nil
}

// Wipe clears the in-memory credential byte slices. It does not and cannot
// erase copies already made by a caller or by the Kubernetes API client.
func (r *Result) Wipe() {
	if r == nil {
		return
	}
	for key, value := range r.data {
		for index := range value {
			value[index] = 0
		}
		delete(r.data, key)
	}
	r.data = nil
}

// Verify checks the non-secret contract digest and fixed projection shape.
// It is useful immediately before a runsecret integration writes the Secret.
func (m Metadata) Verify() error {
	if m.Version != ContractVersion || m.Run.Namespace == "" || m.Run.Name == "" || m.Run.UID == "" || m.Purpose == "" {
		return ErrMetadata
	}
	if m.Mode != ModeShortLived && m.Mode != ModeStaticCopy {
		return ErrMetadata
	}
	if m.StaticCopy != (m.Mode == ModeStaticCopy) || m.ExpiresAt.IsZero() || m.IssuedAt.IsZero() || !m.ExpiresAt.After(m.IssuedAt) || m.ExpiresAt.Before(m.IssuedAt.Add(MinLeaseLifetime)) || m.ExpiresAt.After(m.IssuedAt.Add(MaxLeaseLifetime)) {
		return ErrMetadata
	}
	if m.Mode == ModeShortLived && (!m.Revocable || !m.CredentialExpiryKnown) {
		return ErrMetadata
	}
	if m.Mode == ModeStaticCopy && (m.Revocable || m.CredentialExpiryKnown) {
		return ErrMetadata
	}
	if m.Projection.AccessKeyID != AccessKeyIDKey || m.Projection.SecretAccessKey != SecretAccessKeyKey {
		return ErrMetadata
	}
	if m.Projection.SessionToken != "" && m.Projection.SessionToken != SessionTokenKey {
		return ErrMetadata
	}
	if !validDigest(m.LeaseIDDigest) || !validDigest(m.ScopeDigest) || !validDigest(m.ContractDigest) {
		return ErrMetadata
	}
	if err := validateRun(m.Run); err != nil {
		return ErrMetadata
	}
	normalizedScope, err := normalizeScope(m.Scope)
	if err != nil || normalizedScope != m.Scope {
		return ErrMetadata
	}
	if err := validateRunScope(m.Run, normalizedScope); err != nil {
		return ErrMetadata
	}
	expectedScopeDigest, err := digestScope(normalizedScope)
	if err != nil || expectedScopeDigest != m.ScopeDigest {
		return ErrMetadata
	}
	expected, err := metadataDigest(m)
	if err != nil || expected != m.ContractDigest {
		return ErrMetadata
	}
	return nil
}

func (r Result) verify() error {
	if err := r.Metadata.Verify(); err != nil {
		return err
	}
	expected := map[string]struct{}{
		AccessKeyIDKey: {}, SecretAccessKeyKey: {},
	}
	if r.Metadata.Projection.SessionToken != "" {
		expected[SessionTokenKey] = struct{}{}
	}
	if len(r.data) != len(expected) {
		return ErrProjection
	}
	for key, value := range r.data {
		if _, ok := expected[key]; !ok || len(value) == 0 {
			return ErrProjection
		}
	}
	return nil
}

func normalizeRequest(request Request) (Request, error) {
	if err := validateRun(request.Run); err != nil {
		return Request{}, ErrInvalidRequest
	}
	scope, err := normalizeScope(request.Scope)
	if err != nil {
		return Request{}, err
	}
	if err := validateRunScope(request.Run, scope); err != nil {
		return Request{}, err
	}
	purpose := request.Purpose
	if !validPurpose(purpose) {
		return Request{}, ErrInvalidRequest
	}
	ttl := request.TTL
	if ttl == 0 {
		ttl = DefaultLeaseLifetime
	}
	if ttl < MinLeaseLifetime || ttl > MaxLeaseLifetime {
		return Request{}, ErrInvalidRequest
	}
	request.Scope = scope
	request.Purpose = purpose
	request.TTL = ttl
	return request, nil
}

func normalizeScope(scope Scope) (Scope, error) {
	if scope.Bucket == "" || !validBucket(scope.Bucket) || scope.Prefix == "" || !validObjectPath(scope.Prefix, MaxPrefixBytes) {
		return Scope{}, ErrInvalidScope
	}
	if scope.Region != "" {
		if strings.TrimSpace(scope.Region) != scope.Region || len(scope.Region) > MaxRegionBytes || !validSimple(scope.Region, false) {
			return Scope{}, ErrInvalidScope
		}
	}
	if scope.Endpoint != "" {
		if err := validateEndpoint(scope.Endpoint); err != nil {
			return Scope{}, ErrInvalidScope
		}
		scope.Endpoint = strings.TrimSuffix(scope.Endpoint, "/")
	}
	if !scope.Permissions.Read && !scope.Permissions.Write {
		return Scope{}, ErrInvalidScope
	}
	return scope, nil
}

func validateEndpoint(endpoint string) error {
	if len(endpoint) > MaxEndpointBytes || strings.TrimSpace(endpoint) != endpoint || hasControl(endpoint) {
		return ErrInvalidScope
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return ErrInvalidScope
	}
	if u.Hostname() == "" || strings.HasSuffix(u.Hostname(), ".") || hasControl(u.Hostname()) {
		return ErrInvalidScope
	}
	if port := u.Port(); port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return ErrInvalidScope
		}
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		for index := 0; index < len(host); index++ {
			value := host[index]
			if !((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '.' || value == '-') {
				return ErrInvalidScope
			}
		}
	}
	return nil
}

func validBucket(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > MaxBucketBytes || bucket != strings.ToLower(bucket) || strings.Contains(bucket, "..") || net.ParseIP(bucket) != nil || strings.HasPrefix(bucket, "xn--") {
		return false
	}
	if bucket[0] == '-' || bucket[len(bucket)-1] == '-' || bucket[0] == '.' || bucket[len(bucket)-1] == '.' {
		return false
	}
	for index := 0; index < len(bucket); index++ {
		value := bucket[index]
		if !((value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || value == '.' || value == '-') {
			return false
		}
	}
	for _, suffix := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(bucket, suffix) {
			return false
		}
	}
	return true
}

func validObjectPath(path string, maxBytes int) bool {
	if path == "" || len(path) > maxBytes || !utf8.ValidString(path) || strings.TrimSpace(path) != path || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." || !validSimple(segment, true) {
			return false
		}
	}
	return true
}

func validSimple(value string, allowDot bool) bool {
	if value == "" || hasControl(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || (allowDot && character == '.') {
			continue
		}
		return false
	}
	return true
}

func validateRun(run RunIdentity) error {
	if !validDNS(run.Namespace) || !validDNS(run.Name) || !validRunUID(run.UID) {
		return ErrInvalidRequest
	}
	return nil
}

func validRunUID(value string) bool {
	if value == "" || len(value) > MaxRunUIDBytes || strings.TrimSpace(value) != value || strings.Contains(value, "..") || value == "." || value == ".." {
		return false
	}
	for index, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.') {
			return false
		}
		if (index == 0 || index == len(value)-1) && (character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}

func validateRunScope(run RunIdentity, scope Scope) error {
	if !validRunUID(run.UID) {
		return ErrInvalidRequest
	}
	expectedSuffix := "/runs/" + run.UID
	if scope.Prefix != "runs/"+run.UID && !strings.HasSuffix(scope.Prefix, expectedSuffix) {
		return ErrInvalidScope
	}
	return nil
}

func validDNS(value string) bool {
	if value == "" || len(value) > 253 || strings.TrimSpace(value) != value || hasControl(value) || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return false
			}
		}
	}
	return true
}

func validPurpose(value string) bool {
	if value == "" || len(value) > MaxPurposeBytes || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.') {
			return false
		}
	}
	return true
}

func validSourceRef(value string) bool {
	if value == "" || len(value) > MaxSourceRefBytes || strings.TrimSpace(value) != value || hasControl(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._/-", rune(character))) {
			return false
		}
	}
	return true
}

func validateCredentials(credentials CredentialSet) error {
	if !validCredentialValue(credentials.AccessKeyID, MaxAccessKeyIDBytes) || !validCredentialValue(credentials.SecretAccessKey, MaxSecretAccessKeyBytes) {
		return ErrInvalidCredentials
	}
	if credentials.SessionToken != "" && !validCredentialValue(credentials.SessionToken, MaxSessionTokenBytes) {
		return ErrInvalidCredentials
	}
	return nil
}

func validCredentialValue(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && !hasControl(value)
}

func validLeaseID(value string) bool {
	return validCredentialValue(value, MaxLeaseIDBytes)
}

func metadataDigest(metadata Metadata) (string, error) {
	metadata.ContractDigest = ""
	body, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

func digestScope(scope Scope) (string, error) {
	body, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

func idempotencyKey(request Request) string {
	scopeDigest, _ := digestScope(request.Scope)
	return digestString("agw-artifact-lease-v1\x00" + request.Run.Namespace + "\x00" + request.Run.Name + "\x00" + request.Run.UID + "\x00" + request.Purpose + "\x00" + scopeDigest + "\x00" + request.TTL.String())
}

func staticLeaseID(request Request) string {
	return idempotencyKey(request)
}

func digestString(value string) string {
	return digestBytes([]byte(value))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for index := len("sha256:"); index < len(value); index++ {
		character := value[index]
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func cloneData(input map[string][]byte) map[string][]byte {
	output := make(map[string][]byte, len(input))
	for key, value := range input {
		output[key] = append([]byte(nil), value...)
	}
	return output
}

func contextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func hasControl(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return true
		}
	}
	return false
}
