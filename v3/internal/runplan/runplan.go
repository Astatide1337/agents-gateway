// Package runplan composes the controller-owned inputs for a work Sandbox.
//
// The package is intentionally independent of internal/controller. It exposes
// the same PlanWork method shape, but keeps the security-sensitive composition
// in one small seam:
//
//   - one named, immutable GitHub App Secret is read from the trusted system
//     namespace;
//   - a repository-scoped, contents-read installation token is minted only for
//     the clone initContainer;
//   - runsecret creates one immutable, owner-referenced projection Secret;
//   - workload builds the complete work Sandbox with explicit image digests.
//
// The GitHub App private key and publish-capable credential never enter the
// returned Sandbox or the per-run Secret.
package runplan

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runsecret"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultSystemNamespace = "agw-system"

	GitHubAppIDKey          = "app-id"
	GitHubInstallationIDKey = "installation-id"
	GitHubPrivateKeyKey     = "private-key.pem"

	// These bounds are deliberately independent of Kubernetes' general Secret
	// limits. A GitHub App Secret is expected to contain three small fields; a
	// large object is an operator configuration error and must not become a
	// credential parsing or logging DoS.
	MaxAppIDBytes          = 32
	MaxInstallationIDBytes = 32
	MaxPrivateKeyBytes     = 1 << 20
	MaxAppSecretBytes      = 1 << 20
	MaxCredentialRefLength = 253
	MaxRepositoryLength    = 512
	MaxImageLength         = 512
	MaxTimeoutLength       = 32
)

var (
	ErrInvalidConfig     = errors.New("runplan: invalid configuration")
	ErrInvalidInput      = errors.New("runplan: invalid run input")
	ErrInvalidIdentity   = errors.New("runplan: run identity does not match resolved snapshot")
	ErrInvalidSpecDigest = errors.New("runplan: spec digest does not match resolved snapshot")
	ErrInvalidRepository = errors.New("runplan: invalid GitHub repository")
	ErrInvalidTimeout    = errors.New("runplan: invalid or excessive run timeout")
	ErrInvalidImage      = errors.New("runplan: image configuration is not digest-pinned")
	ErrPublishCredential = errors.New("runplan: publish credential configuration is invalid")
	ErrPublishOverlap    = errors.New("runplan: publish credential cannot be projected into a work run")
	ErrAppSecretMissing  = errors.New("runplan: GitHub App Secret is missing")
	ErrAppSecretRead     = errors.New("runplan: GitHub App Secret could not be read")
	ErrAppSecretInvalid  = errors.New("runplan: GitHub App Secret is invalid")
	ErrMinter            = errors.New("runplan: GitHub App token minter could not be configured")
	ErrRunSecret         = errors.New("runplan: per-run credential Secret could not be materialized")
	ErrVerifyCredential  = errors.New("runplan: verify-phase credential Secret could not be materialized")
	ErrVerifyLeaseTTL    = errors.New("runplan: Gate verify timeout exceeds verify credential lease TTL")
	ErrWorkload          = errors.New("runplan: work Sandbox could not be built")
)

// SourceResolutionClassOf maps both GitHub transport outcomes and runplan's
// local source/configuration failures onto the same bounded retry contract.
// Unknown errors remain unclassified so a caller cannot accidentally make a
// terminal decision from an error this package does not understand.
func SourceResolutionClassOf(err error) (githubapp.ResolutionClass, bool) {
	if err == nil {
		return "", false
	}
	if class, ok := githubapp.ClassOf(err); ok {
		return class, true
	}
	switch {
	case errors.Is(err, ErrInvalidInput),
		errors.Is(err, ErrInvalidIdentity),
		errors.Is(err, ErrInvalidSpecDigest),
		errors.Is(err, ErrInvalidRepository),
		errors.Is(err, ErrPublishCredential),
		errors.Is(err, ErrAppSecretMissing),
		errors.Is(err, ErrAppSecretInvalid),
		errors.Is(err, ErrMinter),
		errors.Is(err, ErrInvalidConfig):
		return githubapp.ClassConfiguration, true
	case errors.Is(err, ErrAppSecretRead):
		return githubapp.ClassTransport, true
	case errors.Is(err, context.DeadlineExceeded):
		return githubapp.ClassTimeout, true
	case errors.Is(err, context.Canceled):
		return githubapp.ClassCanceled, true
	default:
		return "", false
	}
}

// IsPermanentSourceResolutionError reports only source failures that cannot
// be repaired by retrying the same run. It is intentionally fail-closed for
// unknown errors; those remain eligible for the controller's normal retry
// path.
func IsPermanentSourceResolutionError(err error) bool {
	class, ok := SourceResolutionClassOf(err)
	return ok && githubapp.IsPermanentClass(class)
}

// SecretReader is deliberately smaller than client.Reader. PlanWork uses a
// named GET for the GitHub App Secret; runsecret additionally needs Create for
// the deterministic per-run Secret. Implementations must not need List or
// Watch permissions for this package.
type SecretReader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
	Create(context.Context, client.Object, ...client.CreateOption) error
}

// MinterFactory is a narrow constructor seam. Production leaves it nil, which
// selects githubapp.New and therefore rejects private/loopback API origins.
// Tests may provide a constructor backed by githubapp.NewForTest so an
// httptest TLS server can exercise the real JWT and installation-token path.
type MinterFactory func(githubapp.Config) (*githubapp.Minter, error)

// Config contains operator-owned, non-run configuration. Image references are
// required even when a particular run has no skills so a deployment cannot
// silently fall back to a mutable tag later.
type Config struct {
	Client          SecretReader
	SystemNamespace string
	// GitHubAppSecret is the single operator-approved GitHub App identity.
	// It supplies clone tokens even when publish.mode is none and prevents a
	// run from selecting an arbitrary Secret by name.
	GitHubAppSecret string
	// CredentialSecretNames is the operator-owned allowlist of source Secrets
	// that a run may project into its broker or skills initContainer.
	CredentialSecretNames []string
	// Placement allowlists are operator-owned exact names. Empty means the
	// corresponding caller-controlled snapshot field must remain empty.
	AllowedRuntimeClasses []string
	AllowedStorageClasses []string
	SkillsToken           *runsecret.SecretKeyRef
	SkillsEndpoint        string

	CloneImage    string
	SkillsImage   string
	ContextImage  string
	LockdownImage string
	BrokerImage   string
	// AgentGateway is the explicit, fail-closed seam for a future per-run
	// agentgateway sidecar. The workload package accepts only its zero value
	// today; a fully specified enablement is still rejected until the
	// guard-to-agentgateway adapter has been proven.
	AgentGateway workload.AgentGatewaySidecarOptions

	MaxShutdownDuration time.Duration
	Clock               func() time.Time

	GitHubAPIBaseURL string
	GitHubHTTPClient *http.Client
	MinterFactory    MinterFactory

	RunSecretLimits runsecret.Limits

	ArtifactIssuer              runsecret.ArtifactIssuer
	ArtifactStoreEndpoint       string
	ArtifactStoreRegion         string
	ArtifactStoreBucket         string
	ArtifactStorePrefix         string
	ArtifactStoreForcePathStyle bool
	ArtifactStoreMaxObjectBytes int64
	ArtifactCredentialTTL       time.Duration
}

// Factory implements the controller.WorkPlanFactory method without importing
// controller. It is safe to share between reconciles after construction.
type Factory struct {
	client                SecretReader
	systemNamespace       string
	githubAppSecret       string
	credentialSecretNames []string
	allowedRuntimeClasses []string
	allowedStorageClasses []string
	skillsToken           *runsecret.SecretKeyRef
	skillsEndpoint        string
	images                imageConfig
	agentGateway          workload.AgentGatewaySidecarOptions
	maxShutdown           time.Duration
	clock                 func() time.Time
	github                githubConfig
	secretLimits          runsecret.Limits
	artifact              artifactConfig
}

type artifactConfig struct {
	issuer         runsecret.ArtifactIssuer
	endpoint       string
	region         string
	bucket         string
	prefix         string
	forcePathStyle bool
	maxObjectBytes int64
	ttl            time.Duration
}

type imageConfig struct {
	clone    string
	skills   string
	context  string
	lockdown string
	broker   string
}

type githubConfig struct {
	baseURL    string
	httpClient *http.Client
	newMinter  MinterFactory
}

// New validates operator configuration without contacting Kubernetes or
// GitHub. It does not retain any credential bytes.
func New(config Config) (*Factory, error) {
	if config.Client == nil {
		return nil, ErrInvalidConfig
	}

	systemNamespace := config.SystemNamespace
	if systemNamespace == "" {
		systemNamespace = DefaultSystemNamespace
	}
	if len(validation.IsDNS1123Label(systemNamespace)) != 0 {
		return nil, ErrInvalidConfig
	}
	if !validCredentialRef(config.GitHubAppSecret) {
		return nil, ErrInvalidConfig
	}
	credentialSecretNames, err := normalizeCredentialSecretNames(config.CredentialSecretNames)
	if err != nil {
		return nil, err
	}
	allowedRuntimeClasses, err := workload.NormalizeRuntimeClassAllowlist(config.AllowedRuntimeClasses)
	if err != nil {
		return nil, fmt.Errorf("%w: RuntimeClass allowlist: %v", ErrInvalidConfig, err)
	}
	allowedStorageClasses, err := workload.NormalizeStorageClassAllowlist(config.AllowedStorageClasses)
	if err != nil {
		return nil, fmt.Errorf("%w: StorageClass allowlist: %v", ErrInvalidConfig, err)
	}
	if config.SkillsToken != nil {
		if _, ok := credentialSecretNames[config.SkillsToken.SecretName]; !ok {
			return nil, ErrInvalidConfig
		}
	}

	maxShutdown := config.MaxShutdownDuration
	if maxShutdown == 0 {
		maxShutdown = sandbox.DefaultMaxShutdownDuration
	}
	if maxShutdown <= 0 || maxShutdown > sandbox.DefaultMaxShutdownDuration {
		return nil, ErrInvalidConfig
	}

	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}

	images := imageConfig{
		clone:    config.CloneImage,
		skills:   config.SkillsImage,
		context:  config.ContextImage,
		lockdown: config.LockdownImage,
		broker:   config.BrokerImage,
	}
	if err := validateImages(images); err != nil {
		return nil, err
	}
	if err := workload.ValidateAgentGatewaySidecar(config.AgentGateway); err != nil {
		return nil, fmt.Errorf("%w: agentgateway configuration: %v", ErrInvalidConfig, err)
	}

	if config.SkillsToken != nil {
		copyRef := *config.SkillsToken
		config.SkillsToken = &copyRef
	}
	if config.SkillsEndpoint != "" && !validSkillsEndpoint(config.SkillsEndpoint) {
		return nil, ErrInvalidConfig
	}
	if _, err := runsecret.New(runsecret.Config{
		Client: config.Client, CloneTokenMinter: noopCloneMinter{},
		SourceNamespace: systemNamespace, SkillsToken: config.SkillsToken,
		AllowedSourceSecrets: mapKeys(credentialSecretNames),
		Limits:               config.RunSecretLimits, Clock: clock,
	}); err != nil {
		// runsecret.New validates only the materializer configuration. The
		// temporary minter is never used; PlanWork supplies the real adapter.
		return nil, ErrInvalidConfig
	}
	artifact := artifactConfig{
		issuer: config.ArtifactIssuer, endpoint: config.ArtifactStoreEndpoint,
		region: config.ArtifactStoreRegion, bucket: config.ArtifactStoreBucket,
		prefix: config.ArtifactStorePrefix, forcePathStyle: config.ArtifactStoreForcePathStyle,
		maxObjectBytes: config.ArtifactStoreMaxObjectBytes, ttl: config.ArtifactCredentialTTL,
	}
	if artifact.issuer == nil {
		if artifact.endpoint != "" || artifact.region != "" || artifact.bucket != "" || artifact.prefix != "" || artifact.maxObjectBytes != 0 || artifact.ttl != 0 {
			return nil, ErrInvalidConfig
		}
	} else if artifact.region == "" || artifact.bucket == "" || artifact.prefix == "" || artifact.maxObjectBytes <= 0 || artifact.ttl <= 0 {
		return nil, ErrInvalidConfig
	}

	newMinter := config.MinterFactory
	if newMinter == nil {
		newMinter = githubapp.New
	}

	return &Factory{
		client:                config.Client,
		systemNamespace:       systemNamespace,
		githubAppSecret:       config.GitHubAppSecret,
		credentialSecretNames: mapKeys(credentialSecretNames),
		allowedRuntimeClasses: allowedRuntimeClasses,
		allowedStorageClasses: allowedStorageClasses,
		skillsToken:           config.SkillsToken,
		skillsEndpoint:        config.SkillsEndpoint,
		images:                images,
		agentGateway:          config.AgentGateway,
		maxShutdown:           maxShutdown,
		clock:                 clock,
		github: githubConfig{
			baseURL:    config.GitHubAPIBaseURL,
			httpClient: config.GitHubHTTPClient,
			newMinter:  newMinter,
		},
		secretLimits: config.RunSecretLimits,
		artifact:     artifact,
	}, nil
}

// PlanWork resolves controller-owned clone credentials, materializes the
// deterministic run Secret, and constructs the exact work Sandbox plan.
//
// The method is intentionally idempotent at the effect boundaries: an
// existing per-run Secret is validated and reused by runsecret, and a second
// call therefore never mints another installation token or creates another
// Secret. The GitHub App Secret is still read by named GET on each call so a
// reconciler never relies on an unvalidated cached private key.
func (f *Factory) PlanWork(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (sandbox.SandboxPlan, error) {
	if ctx == nil || f == nil || f.client == nil || f.clock == nil {
		return sandbox.SandboxPlan{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return sandbox.SandboxPlan{}, err
	}

	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		return sandbox.SandboxPlan{}, ErrInvalidSpecDigest
	}
	if err := validateRunIdentity(run, snapshot, digest); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if err := validateRepository(snapshot.Spec.Source.Repo); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if err := validatePublishRefs(snapshot, f.githubAppSecret); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	timeout, err := boundedTimeout(snapshot.Spec.Limits.Timeout, f.maxShutdown)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if f.artifact.issuer != nil && timeout > f.artifact.ttl {
		return sandbox.SandboxPlan{}, ErrInvalidTimeout
	}
	now := f.clock()
	if now.IsZero() {
		return sandbox.SandboxPlan{}, ErrInvalidConfig
	}
	now = now.UTC()
	shutdownTime := now.Add(timeout)

	minter, err := f.newGitHubMinter(ctx)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}

	materializerConfig := runsecret.Config{
		Client:               f.client,
		CloneTokenMinter:     cloneTokenAdapter{minter: minter},
		Phase:                runsecret.PhaseWork,
		SourceNamespace:      f.systemNamespace,
		AllowedSourceSecrets: f.credentialSecretNames,
		SkillsToken:          f.skillsToken,
		Limits:               f.secretLimits,
		Clock:                f.clock,
	}
	if f.artifact.issuer != nil {
		materializerConfig.ArtifactIssuer = f.artifact.issuer
		materializerConfig.ArtifactScope = artifactauth.Scope{
			Endpoint: f.artifact.endpoint, Region: f.artifact.region, Bucket: f.artifact.bucket,
			Prefix:      strings.TrimSuffix(f.artifact.prefix, "/") + "/runs/" + snapshot.Run.UID,
			Permissions: artifactauth.Permissions{Read: true, Write: true}, ForcePathStyle: f.artifact.forcePathStyle,
		}
		materializerConfig.ArtifactTTL = f.artifact.ttl
	}
	materializer, err := runsecret.New(materializerConfig)
	if err != nil {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: %w", ErrRunSecret, err)
	}
	projection, err := materializer.Materialize(ctx, run, snapshot, digest)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sandbox.SandboxPlan{}, ctxErr
		}
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: %w", ErrRunSecret, err)
	}

	manifest, err := workload.Build(snapshot, workload.Options{
		CloneImage:                       f.images.clone,
		SkillsImage:                      f.images.skills,
		ContextImage:                     f.images.context,
		SkillsEndpoint:                   f.skillsEndpoint,
		LockdownImage:                    f.images.lockdown,
		BrokerImage:                      f.images.broker,
		AgentGateway:                     f.agentGateway,
		AllowedRuntimeClasses:            append([]string(nil), f.allowedRuntimeClasses...),
		AllowedStorageClasses:            append([]string(nil), f.allowedStorageClasses...),
		SecretName:                       projection.SecretName,
		CloneSecretKey:                   projection.CloneSecretKey,
		SkillsSecretKey:                  projection.SkillsSecretKey,
		BrokerSecretKeys:                 projection.BrokerSecretKeys,
		ArtifactStoreEndpoint:            f.artifact.endpoint,
		ArtifactStoreRegion:              f.artifact.region,
		ArtifactStoreBucket:              f.artifact.bucket,
		ArtifactStorePrefix:              f.artifact.prefix,
		ArtifactStoreForcePathStyle:      f.artifact.forcePathStyle,
		ArtifactStoreMaxObjectBytes:      f.artifact.maxObjectBytes,
		ArtifactAccessKeyIDSecretKey:     projection.ArtifactAccessKeyIDSecretKey,
		ArtifactSecretAccessKeySecretKey: projection.ArtifactSecretAccessKeySecretKey,
		ArtifactSessionTokenSecretKey:    projection.ArtifactSessionTokenSecretKey,
		Now:                              now,
		ShutdownTime:                     shutdownTime,
		MaxShutdownDuration:              f.maxShutdown,
	})
	if err != nil {
		// Preserve the stable workload error class while retaining the bounded
		// builder diagnostic. Without this context every invalid snapshot or
		// projection failure is reported as the same opaque reconciliation error.
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: %w", ErrWorkload, err)
	}

	return sandbox.SandboxPlan{
		Owner:      run,
		Role:       sandbox.RoleWork,
		SpecDigest: digest,
		Sandbox:    manifest,
	}, nil
}

// MaterializeVerifyCredentials creates the independent credential contract
// consumed only by the verify-fetch initContainer. It intentionally does not
// build a Sandbox and never calls the work materializer: the deterministic
// verify Secret name, read-only object-store scope, and lease purpose are all
// distinct. Reconciliation of the same run reuses the immutable Secret after
// validating its owner, digest, phase, and exact key set.
func (f *Factory) MaterializeVerifyCredentials(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (runsecret.Result, error) {
	if ctx == nil || f == nil || f.client == nil || f.clock == nil {
		return runsecret.Result{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return runsecret.Result{}, err
	}
	digest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		return runsecret.Result{}, ErrInvalidSpecDigest
	}
	if err := validateRunIdentity(run, snapshot, digest); err != nil {
		return runsecret.Result{}, err
	}
	if err := validateRepository(snapshot.Spec.Source.Repo); err != nil {
		return runsecret.Result{}, err
	}
	verifyTimeout, err := boundedTimeout(snapshot.Gate.Verify.Timeout, f.maxShutdown)
	if err != nil {
		return runsecret.Result{}, err
	}
	if f.artifact.issuer == nil || f.artifact.ttl <= 0 {
		return runsecret.Result{}, ErrVerifyCredential
	}
	if verifyTimeout > f.artifact.ttl {
		return runsecret.Result{}, ErrVerifyLeaseTTL
	}
	minter, err := f.newGitHubMinter(ctx)
	if err != nil {
		return runsecret.Result{}, err
	}
	materializer, err := runsecret.New(runsecret.Config{
		Client:               f.client,
		CloneTokenMinter:     cloneTokenAdapter{minter: minter},
		Phase:                runsecret.PhaseVerify,
		SourceNamespace:      f.systemNamespace,
		AllowedSourceSecrets: f.credentialSecretNames,
		Limits:               f.secretLimits,
		Clock:                f.clock,
		ArtifactIssuer:       f.artifact.issuer,
		ArtifactScope:        f.artifactScope(string(run.UID), artifactauth.Permissions{Read: true}),
		ArtifactTTL:          f.artifact.ttl,
	})
	if err != nil {
		return runsecret.Result{}, ErrVerifyCredential
	}
	projection, err := materializer.Materialize(ctx, run, snapshot, digest)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return runsecret.Result{}, ctxErr
		}
		return runsecret.Result{}, ErrVerifyCredential
	}
	return projection, nil
}

func (f *Factory) artifactScope(runUID string, permissions artifactauth.Permissions) artifactauth.Scope {
	return artifactauth.Scope{
		Endpoint:       f.artifact.endpoint,
		Region:         f.artifact.region,
		Bucket:         f.artifact.bucket,
		Prefix:         strings.TrimSuffix(f.artifact.prefix, "/") + "/runs/" + runUID,
		Permissions:    permissions,
		ForcePathStyle: f.artifact.forcePathStyle,
	}
}

func validSkillsEndpoint(value string) bool {
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "/mcp" && parsed.RawQuery == "" && parsed.Fragment == ""
}

// PinSource resolves baseRef through the same operator-approved GitHub App
// used for clone. It returns a new canonical result whose specDigest includes
// the exact commit SHA. Callers must invoke this before persisting admission
// evidence or creating any Sandbox.
func (f *Factory) PinSource(ctx context.Context, input resolved.Result) (resolved.Result, error) {
	if ctx == nil || f == nil || f.client == nil || input.Snapshot.Spec.Source.Repo == "" {
		return resolved.Result{}, ErrInvalidInput
	}
	if err := validateRepository(input.Snapshot.Spec.Source.Repo); err != nil {
		return resolved.Result{}, err
	}
	minter, err := f.newGitHubMinter(ctx)
	if err != nil {
		return resolved.Result{}, err
	}
	parts := strings.Split(input.Snapshot.Spec.Source.Repo, "/")
	repository := githubapp.Repository{Owner: parts[1], Name: parts[2]}
	token, err := minter.Mint(ctx, repository, githubapp.PermissionCloneRead)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return resolved.Result{}, ctxErr
		}
		return resolved.Result{}, fmt.Errorf("%w: %w", ErrMinter, err)
	}
	baseSHA, err := minter.ResolveCommit(ctx, token, repository, input.Snapshot.Spec.Source.BaseRef)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return resolved.Result{}, ctxErr
		}
		return resolved.Result{}, fmt.Errorf("%w: %w", ErrMinter, err)
	}
	result, err := resolved.WithBaseSHA(input, baseSHA)
	if err != nil {
		return resolved.Result{}, ErrInvalidInput
	}
	return result, nil
}

func (f *Factory) newGitHubMinter(ctx context.Context) (*githubapp.Minter, error) {
	credentials, err := f.readAppCredentials(ctx, f.githubAppSecret)
	if err != nil {
		return nil, err
	}
	minterConfig := githubapp.Config{
		AppID: credentials.appID, InstallationID: credentials.installationID,
		PrivateKeyPEM: credentials.privateKey, BaseURL: f.github.baseURL,
		HTTPClient: f.github.httpClient, Clock: f.clock,
	}
	minter, err := f.github.newMinter(minterConfig)
	wipe(credentials.privateKey)
	if err != nil || minter == nil {
		if errors.Is(err, githubapp.ErrInvalidPrivateKey) || errors.Is(err, githubapp.ErrPrivateKeyTooLarge) {
			return nil, ErrAppSecretInvalid
		}
		return nil, ErrMinter
	}
	return minter, nil
}

// GitHubMinter returns the operator-owned GitHub App boundary for controller
// publication. It never exposes a token or private-key bytes; the returned
// minter creates short-lived, repository-scoped tokens per operation.
func (f *Factory) GitHubMinter(ctx context.Context) (*githubapp.Minter, error) {
	return f.newGitHubMinter(ctx)
}

type appCredentials struct {
	appID          int64
	installationID int64
	privateKey     []byte
}

func (f *Factory) readAppCredentials(ctx context.Context, credentialRef string) (appCredentials, error) {
	if !validCredentialRef(credentialRef) {
		return appCredentials{}, ErrPublishCredential
	}
	secret := &corev1.Secret{}
	err := f.client.Get(ctx, client.ObjectKey{Namespace: f.systemNamespace, Name: credentialRef}, secret)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return appCredentials{}, ctxErr
		}
		if apierrors.IsNotFound(err) {
			return appCredentials{}, ErrAppSecretMissing
		}
		return appCredentials{}, ErrAppSecretRead
	}
	if err := validateAppSecret(secret); err != nil {
		return appCredentials{}, err
	}
	appID, ok := parsePositiveID(secret.Data[GitHubAppIDKey], MaxAppIDBytes)
	if !ok {
		return appCredentials{}, ErrAppSecretInvalid
	}
	installationID, ok := parsePositiveID(secret.Data[GitHubInstallationIDKey], MaxInstallationIDBytes)
	if !ok {
		return appCredentials{}, ErrAppSecretInvalid
	}
	privateKey := append([]byte(nil), secret.Data[GitHubPrivateKeyKey]...)
	return appCredentials{appID: appID, installationID: installationID, privateKey: privateKey}, nil
}

func validateAppSecret(secret *corev1.Secret) error {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.StringData) != 0 || len(secret.Data) != 3 {
		return ErrAppSecretInvalid
	}
	var total int
	for key, value := range secret.Data {
		allowed := key == GitHubAppIDKey || key == GitHubInstallationIDKey || key == GitHubPrivateKeyKey
		if !allowed || len(value) == 0 {
			return ErrAppSecretInvalid
		}
		total += len(key) + len(value)
		if total > MaxAppSecretBytes {
			return ErrAppSecretInvalid
		}
	}
	if len(secret.Data[GitHubAppIDKey]) > MaxAppIDBytes || len(secret.Data[GitHubInstallationIDKey]) > MaxInstallationIDBytes || len(secret.Data[GitHubPrivateKeyKey]) > MaxPrivateKeyBytes {
		return ErrAppSecretInvalid
	}
	return nil
}

func parsePositiveID(value []byte, maxBytes int) (int64, bool) {
	if len(value) == 0 || len(value) > maxBytes {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(string(value), 10, 64)
	return parsed, err == nil && parsed > 0
}

func validateRunIdentity(run *v1alpha1.AgentRun, snapshot resolved.Snapshot, digest string) error {
	if run == nil || run.Namespace == "" || run.Name == "" || run.UID == "" || len(validation.IsValidLabelValue(string(run.UID))) != 0 || run.Generation != snapshot.Run.Generation || run.Namespace != snapshot.Run.Namespace || run.Name != snapshot.Run.Name || string(run.UID) != snapshot.Run.UID {
		return ErrInvalidIdentity
	}
	if !canonical.ValidDigest(run.Status.SpecDigest) || run.Status.SpecDigest != digest {
		return ErrInvalidSpecDigest
	}
	return nil
}

func validateRepository(value string) error {
	if len(value) == 0 || len(value) > MaxRepositoryLength || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\\?#@:") {
		return ErrInvalidRepository
	}
	parts := strings.Split(value, "/")
	if len(parts) != 3 || parts[0] != "github.com" || parts[1] == "" || parts[2] == "" {
		return ErrInvalidRepository
	}
	repository := githubapp.Repository{Owner: parts[1], Name: parts[2]}
	if repository.Validate() != nil || repository.FullName() != parts[1]+"/"+parts[2] {
		return ErrInvalidRepository
	}
	return nil
}

func validatePublishRefs(snapshot resolved.Snapshot, githubAppSecret string) error {
	publish := snapshot.Spec.Publish.CredentialRef
	if !validCredentialRef(githubAppSecret) {
		return ErrPublishCredential
	}
	if snapshot.Spec.Publish.Mode == v1alpha1.PublishPullRequest && publish != githubAppSecret {
		return ErrPublishCredential
	}
	if snapshot.Spec.Publish.Mode == v1alpha1.PublishNone && publish != "" {
		return ErrPublishCredential
	}
	for _, server := range snapshot.ToolSet.Servers {
		if server.CredentialsRef != "" && server.CredentialsRef == githubAppSecret {
			return ErrPublishOverlap
		}
	}
	for _, provider := range snapshot.ModelRoute.Providers {
		if provider.CredentialRef != "" && provider.CredentialRef == githubAppSecret {
			return ErrPublishOverlap
		}
	}
	return nil
}

func validCredentialRef(value string) bool {
	return value != "" && len(value) <= MaxCredentialRefLength && len(validation.IsDNS1123Subdomain(value)) == 0
}

func normalizeCredentialSecretNames(values []string) (map[string]struct{}, error) {
	if len(values) == 0 || len(values) > runsecret.MaxMaxSourceSecrets {
		return nil, ErrInvalidConfig
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validCredentialRef(value) {
			return nil, ErrInvalidConfig
		}
		if _, exists := result[value]; exists {
			return nil, ErrInvalidConfig
		}
		result[value] = struct{}{}
	}
	return result, nil
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func boundedTimeout(value string, max time.Duration) (time.Duration, error) {
	if value == "" || len(value) > MaxTimeoutLength || strings.TrimSpace(value) != value {
		return 0, ErrInvalidTimeout
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration > max || duration > sandbox.DefaultMaxShutdownDuration {
		return 0, ErrInvalidTimeout
	}
	return duration, nil
}

func validateImages(images imageConfig) error {
	values := []struct {
		name  string
		value string
	}{
		{name: "clone", value: images.clone},
		{name: "skills", value: images.skills},
		{name: "context", value: images.context},
		{name: "lockdown", value: images.lockdown},
		{name: "broker", value: images.broker},
	}
	for _, image := range values {
		if len(image.value) == 0 || len(image.value) > MaxImageLength || !resolved.ValidPinnedImage(image.value) {
			return ErrInvalidImage
		}
	}
	return nil
}

type cloneTokenAdapter struct {
	minter *githubapp.Minter
}

func (m cloneTokenAdapter) MintReadOnlyContentsToken(ctx context.Context, repository string) (runsecret.CloneToken, error) {
	if m.minter == nil {
		return runsecret.CloneToken{}, ErrMinter
	}
	if err := validateRepository(repository); err != nil {
		return runsecret.CloneToken{}, ErrInvalidRepository
	}
	parts := strings.Split(repository, "/")
	token, err := m.minter.Mint(ctx, githubapp.Repository{Owner: parts[1], Name: parts[2]}, githubapp.PermissionCloneRead)
	if err != nil {
		return runsecret.CloneToken{}, ErrMinter
	}
	return runsecret.CloneToken{
		Value:         token.Value(),
		Repository:    repository,
		ContentsRead:  true,
		ContentsWrite: false,
		ExpiresAt:     token.ExpiresAt(),
	}, nil
}

// noopCloneMinter exists only so New can run runsecret's structural config
// validation without minting anything. It is never stored in Factory.
type noopCloneMinter struct{}

func (noopCloneMinter) MintReadOnlyContentsToken(context.Context, string) (runsecret.CloneToken, error) {
	return runsecret.CloneToken{}, ErrMinter
}

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ interface {
	PlanWork(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (sandbox.SandboxPlan, error)
} = (*Factory)(nil)

var _ interface {
	PinSource(context.Context, resolved.Result) (resolved.Result, error)
} = (*Factory)(nil)

var _ runsecret.CloneTokenMinter = cloneTokenAdapter{}
