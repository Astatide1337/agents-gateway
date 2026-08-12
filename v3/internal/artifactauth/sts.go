package artifactauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// AssumeRole accepts a duration between 900 seconds and the role's configured
	// maximum. The contract's lower bound is intentionally smaller because other
	// providers may support shorter leases; this adapter cannot issue one safely.
	MinSTSLeaseLifetime = 15 * time.Minute

	MaxSTSRoleARNBytes         = 2048
	MaxSTSRoleResourceBytes    = 512
	MaxSTSRoleSessionNameBytes = 64
	MaxSTSExternalIDBytes      = 1224
	MaxSTSSessionPrefixBytes   = 15
	MaxSTSSessionPolicyBytes   = 2048

	defaultSTSSessionPrefix = "agw"
)

// STSConfig configures the AWS STS AssumeRole adapter. The role's identity
// policy must already be limited to the artifacts role; the adapter adds a
// per-request session policy that can only reduce that role's permissions.
//
// Endpoint is optional and, when set, must be an HTTPS endpoint without user
// information, query parameters, or a path. It is intended for an explicitly
// configured regional/proxy STS endpoint, not an untrusted request value.
type STSConfig struct {
	RoleARN             string
	Region              string
	ExternalID          string
	Endpoint            string
	SessionNamePrefix   string
	CredentialsProvider aws.CredentialsProvider
	HTTPClient          aws.HTTPClient
	Clock               func() time.Time
}

// STSAssumeRoleClient is the small AWS SDK v2 seam used by STSMinter. *sts.Client
// implements it; keeping the seam here lets policy and response handling be
// tested without making a real IAM call.
type STSAssumeRoleClient interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// STSMinter implements Minter with AWS SDK v2 STS AssumeRole. It never returns
// provider errors verbatim and never includes provider response data in an
// error. The caller still owns the explicit copy of the returned credentials
// into a per-run Secret.
type STSMinter struct {
	client            STSAssumeRoleClient
	roleARN           string
	externalID        string
	sessionNamePrefix string
	clock             func() time.Time
}

var _ Minter = (*STSMinter)(nil)

// NewSTSMinter loads the AWS SDK default configuration and constructs an STS
// client. A configured HTTPClient and credentials provider are passed through
// to the SDK, which is useful for a controlled endpoint or workload identity.
// SDK retries are disabled for AssumeRole because the operation issues new
// credentials and has no provider-side idempotency token; retrying a response
// that was lost could mint an additional lease.
func NewSTSMinter(config STSConfig) (*STSMinter, error) {
	normalized, err := normalizeSTSConfig(config)
	if err != nil {
		return nil, err
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(normalized.Region),
	}
	if normalized.CredentialsProvider != nil {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(normalized.CredentialsProvider))
	}
	if normalized.HTTPClient != nil {
		loadOptions = append(loadOptions, awsconfig.WithHTTPClient(normalized.HTTPClient))
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(context.Background(), loadOptions...)
	if err != nil {
		// Configuration errors may contain local paths or provider details. The
		// adapter's public error surface is deliberately coarse.
		return nil, ErrInvalidConfig
	}
	awsConfig.Region = normalized.Region
	// Credentials have already been resolved above. Remove endpoint resolvers
	// loaded from ambient environment/shared configuration so an unvalidated
	// AWS_ENDPOINT_URL_STS cannot replace the endpoint selected by this adapter.
	awsConfig.ConfigSources = nil
	awsConfig.EndpointResolver = nil
	awsConfig.EndpointResolverWithOptions = nil
	if normalized.Endpoint == "" {
		// Do not allow an ambient endpoint override to silently select an
		// insecure/custom STS service. The default SDK endpoint is used here.
		awsConfig.BaseEndpoint = nil
	} else {
		awsConfig.BaseEndpoint = aws.String(normalized.Endpoint)
	}

	client := sts.NewFromConfig(awsConfig, func(options *sts.Options) {
		options.RetryMaxAttempts = 1
	})
	return newSTSMinter(normalized, client), nil
}

// NewSTSMinterWithClient constructs the adapter around an already configured
// AWS SDK v2-compatible client. It is also the supported dependency-injection
// seam for tests and for callers that own SDK configuration centrally.
func NewSTSMinterWithClient(config STSConfig, client STSAssumeRoleClient) (*STSMinter, error) {
	normalized, err := normalizeSTSConfig(config)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, ErrInvalidConfig
	}
	return newSTSMinter(normalized, client), nil
}

func newSTSMinter(config STSConfig, client STSAssumeRoleClient) *STSMinter {
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	return &STSMinter{
		client:            client,
		roleARN:           config.RoleARN,
		externalID:        config.ExternalID,
		sessionNamePrefix: config.SessionNamePrefix,
		clock:             clock,
	}
}

func (m *STSMinter) Mint(ctx context.Context, request MintRequest) (MintedLease, error) {
	if m == nil || m.client == nil {
		return MintedLease{}, ErrInvalidConfig
	}
	if ctx == nil {
		return MintedLease{}, ErrInvalidRequest
	}
	if err := validateSTSMintRequest(request); err != nil {
		return MintedLease{}, err
	}

	policy, err := marshalSTSSessionPolicy(m.roleARN, request.Scope)
	if err != nil {
		return MintedLease{}, err
	}
	sessionName := m.sessionName(request.IdempotencyKey)
	durationSeconds := int32(request.TTL / time.Second)
	input := &sts.AssumeRoleInput{
		RoleArn:         aws.String(m.roleARN),
		RoleSessionName: aws.String(sessionName),
		DurationSeconds: aws.Int32(durationSeconds),
		Policy:          aws.String(policy),
	}
	if m.externalID != "" {
		input.ExternalId = aws.String(m.externalID)
	}

	output, err := m.client.AssumeRole(ctx, input)
	if err != nil {
		// Context errors are safe and useful to the controller. Every other
		// provider error is collapsed so response bodies and credential-like
		// strings cannot cross this package boundary.
		if contextErr := ctx.Err(); contextErr != nil {
			return MintedLease{}, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return MintedLease{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return MintedLease{}, context.DeadlineExceeded
		}
		return MintedLease{}, ErrMintFailed
	}
	if output == nil || output.Credentials == nil {
		return MintedLease{}, ErrInvalidCredentials
	}
	credentials := output.Credentials
	if credentials.AccessKeyId == nil || credentials.SecretAccessKey == nil || credentials.SessionToken == nil {
		return MintedLease{}, ErrInvalidCredentials
	}
	credentialSet := CredentialSet{
		AccessKeyID:     aws.ToString(credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(credentials.SecretAccessKey),
		SessionToken:    aws.ToString(credentials.SessionToken),
	}
	if err := validateCredentials(credentialSet); err != nil || credentialSet.SessionToken == "" {
		return MintedLease{}, ErrInvalidCredentials
	}

	now := m.clock()
	expiresAt := credentials.Expiration
	if expiresAt == nil {
		return MintedLease{}, ErrInvalidExpiry
	}
	expiry := expiresAt.UTC()
	if expiry.Before(now.Add(MinLeaseLifetime)) || expiry.After(now.Add(request.TTL)) {
		return MintedLease{}, ErrInvalidExpiry
	}

	return MintedLease{
		Credentials: credentialSet,
		Scope:       request.Scope,
		ExpiresAt:   expiry,
		LeaseID:     m.leaseID(request.IdempotencyKey, credentialSet.AccessKeyID),
	}, nil
}

func (m *STSMinter) sessionName(idempotencyKey string) string {
	hash := sha256.Sum256([]byte("agw-sts-session-v1\x00" + m.roleARN + "\x00" + idempotencyKey))
	return m.sessionNamePrefix + "-" + hex.EncodeToString(hash[:])[:48]
}

func (m *STSMinter) leaseID(idempotencyKey, accessKeyID string) string {
	// STS has no idempotency token. Keep the session name deterministic for
	// safe correlation, but identify the actual returned credential lease with
	// its access-key ID so a lost response followed by a caller retry cannot
	// make two distinct credential sets look like one lease.
	return digestString("agw-sts-lease-v1\x00" + m.roleARN + "\x00" + idempotencyKey + "\x00" + accessKeyID)
}

func normalizeSTSConfig(config STSConfig) (STSConfig, error) {
	if !validSTSRoleARN(config.RoleARN) || config.Region == "" || len(config.Region) > MaxRegionBytes || !validSimple(config.Region, false) {
		return STSConfig{}, ErrInvalidConfig
	}
	if config.ExternalID != "" && !validSTSExternalID(config.ExternalID) {
		return STSConfig{}, ErrInvalidConfig
	}
	if config.Endpoint != "" {
		if err := validateEndpoint(config.Endpoint); err != nil {
			return STSConfig{}, ErrInvalidConfig
		}
		config.Endpoint = strings.TrimSuffix(config.Endpoint, "/")
	}
	if config.SessionNamePrefix == "" {
		config.SessionNamePrefix = defaultSTSSessionPrefix
	}
	if !validSTSSessionNamePart(config.SessionNamePrefix) || len(config.SessionNamePrefix)+1+48 > MaxSTSRoleSessionNameBytes {
		return STSConfig{}, ErrInvalidConfig
	}
	return config, nil
}

func validateSTSMintRequest(request MintRequest) error {
	if err := validateRun(request.Run); err != nil || !validPurpose(request.Purpose) || !validDigest(request.IdempotencyKey) {
		return ErrInvalidRequest
	}
	if request.TTL < MinSTSLeaseLifetime || request.TTL > MaxLeaseLifetime {
		return ErrInvalidRequest
	}
	normalizedScope, err := normalizeScope(request.Scope)
	if err != nil {
		return err
	}
	if normalizedScope != request.Scope {
		return ErrInvalidScope
	}
	if err := validateRunScope(request.Run, normalizedScope); err != nil {
		return ErrInvalidScope
	}
	return nil
}

func validSTSRoleARN(value string) bool {
	if value == "" || len(value) > MaxSTSRoleARNBytes || strings.TrimSpace(value) != value || hasControl(value) || strings.ContainsAny(value, "*?\\") {
		return false
	}
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || !validSTSPartition(parts[1]) || parts[2] != "iam" || parts[3] != "" || len(parts[4]) != 12 || !allDigits(parts[4]) || !strings.HasPrefix(parts[5], "role/") {
		return false
	}
	resource := strings.TrimPrefix(parts[5], "role/")
	if resource == "" || len(resource) > MaxSTSRoleResourceBytes || strings.HasPrefix(resource, "/") || strings.HasSuffix(resource, "/") || strings.Contains(resource, "//") {
		return false
	}
	for _, character := range resource {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("+=,.@_-", character) || character == '/') {
			return false
		}
	}
	return true
}

func validSTSPartition(value string) bool {
	if value == "" || len(value) > 32 || (value != "aws" && (len(value) <= len("aws-") || !strings.HasPrefix(value, "aws-"))) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validSTSExternalID(value string) bool {
	if value == "" || len(value) > MaxSTSExternalIDBytes || strings.TrimSpace(value) != value || hasControl(value) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("_+=,.@:/-", character)) {
			return false
		}
	}
	return true
}

func validSTSSessionNamePart(value string) bool {
	if value == "" || len(value) > MaxSTSSessionPrefixBytes || strings.TrimSpace(value) != value || hasControl(value) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("+=,.@-_", character)) {
			return false
		}
	}
	return true
}

type stsSessionPolicy struct {
	Version   string               `json:"Version"`
	Statement []stsPolicyStatement `json:"Statement"`
}

type stsPolicyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

func marshalSTSSessionPolicy(roleARN string, scope Scope) (string, error) {
	normalizedScope, err := normalizeScope(scope)
	if err != nil || normalizedScope != scope {
		return "", ErrInvalidScope
	}
	partition := strings.SplitN(roleARN, ":", 6)[1]
	actions := make([]string, 0, 2)
	if scope.Permissions.Read {
		actions = append(actions, "s3:GetObject")
	}
	if scope.Permissions.Write {
		actions = append(actions, "s3:PutObject")
	}
	if len(actions) == 0 {
		return "", ErrInvalidScope
	}
	resource := "arn:" + partition + ":s3:::" + scope.Bucket + "/" + scope.KeyPrefix() + "*"
	policy := stsSessionPolicy{
		Version: "2012-10-17",
		Statement: []stsPolicyStatement{{
			Effect:   "Allow",
			Action:   actions,
			Resource: []string{resource},
		}},
	}
	encoded, err := json.Marshal(policy)
	if err != nil || len(encoded) > MaxSTSSessionPolicyBytes {
		return "", ErrInvalidScope
	}
	return string(encoded), nil
}
