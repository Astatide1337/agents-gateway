// Package runsecret materializes the one credential Secret used by a work
// Sandbox.
//
// Source credentials are deliberately not modeled as arbitrary Secret
// selectors. A logical ToolSet or ModelRoute credentialRef is the name of a
// Secret in the operator-configured source namespace, and that Secret must expose its credential under
// exactly SourceTokenKey ("token"). The materializer copies only that key.
//
// Skills are the one explicit-key exception: Config.SkillsToken may identify a
// Secret name and key, still in that trusted namespace. This keeps skills providers
// that use a non-standard key possible without making logical credentials
// arbitrary key selectors.
//
// The publish credential is intentionally not an input to this package. A
// publish credentialRef that is also selected by a ToolSet or ModelRoute is a
// hard error, because silently projecting the same Secret would violate the
// rule that controller-owned write credentials never enter a work pod.
package runsecret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SourceNamespace is the default trusted operator namespace. Config may
	// override it at operator startup; AgentRun input can never select it.
	SourceNamespace = "agw-system"

	// SourceTokenKey is the only key read for logical ToolSet and ModelRoute
	// credential references. Extra source Secret keys are ignored.
	SourceTokenKey = "token"

	// The output keys are stable and intentionally do not contain source Secret
	// names. Broker keys are content-addressed from the logical reference.
	CloneSecretKey  = "clone-token"
	SkillsSecretKey = "skills-token"

	managedByLabelKey       = "agents.astatide.com/managed-by"
	managedByLabelValue     = "agw-run-secret"
	phaseLabelKey           = "agents.astatide.com/credential-phase"
	runUIDLabelKey          = "agents.astatide.com/run-uid"
	specDigestAnnotationKey = "agents.astatide.com/spec-digest"
	contractAnnotationKey   = "agents.astatide.com/secret-contract"
	contractVersion         = "v1"

	// These are defaults as well as hard ceilings for caller-supplied limits.
	// They keep a malformed or accidentally unbounded AgentRun from turning
	// one Secret into an in-memory credential dump.
	DefaultMaxSourceSecrets = 64
	DefaultMaxProjectedKeys = 128
	DefaultMaxValueBytes    = 64 << 10
	DefaultMaxTotalBytes    = 512 << 10
	MaxMaxSourceSecrets     = 256
	MaxMaxProjectedKeys     = 256
	MaxMaxValueBytes        = 256 << 10
	MaxMaxTotalBytes        = 1 << 20

	// GitHub installation tokens are normally valid for one hour. The bound
	// allows compatible GitHub Enterprise implementations while still
	// requiring the injected minter to return a short-lived token.
	MaxCloneTokenLifetime = 24 * time.Hour

	maxSecretNameLength = 253
	maxRunUIDLength     = 128
)

var (
	ErrInvalidConfig            = errors.New("runsecret: invalid configuration")
	ErrInvalidInput             = errors.New("runsecret: invalid input")
	ErrBounds                   = errors.New("runsecret: credential material exceeds configured bounds")
	ErrCloneToken               = errors.New("runsecret: clone token could not be minted safely")
	ErrSourceSecretMissing      = errors.New("runsecret: credential source is missing")
	ErrSourceSecretRead         = errors.New("runsecret: credential source could not be read")
	ErrSourceSecretNotAllowed   = errors.New("runsecret: credential source is not on the operator allowlist")
	ErrSourceDataInvalid        = errors.New("runsecret: credential source data is invalid")
	ErrDestinationRead          = errors.New("runsecret: destination Secret could not be read")
	ErrDestinationCreate        = errors.New("runsecret: destination Secret could not be created")
	ErrDestinationRace          = errors.New("runsecret: destination Secret creation became ambiguous")
	ErrOwnershipConflict        = errors.New("runsecret: destination Secret has a foreign owner")
	ErrSpecDigestConflict       = errors.New("runsecret: destination Secret has a conflicting spec digest")
	ErrSecretContractConflict   = errors.New("runsecret: destination Secret has a conflicting contract")
	ErrPublishCredentialOverlap = errors.New("runsecret: publish credential cannot be projected into a work run")
	ErrArtifactLease            = errors.New("runsecret: scoped artifact credential could not be issued")
)

// Phase selects the least-privileged credential contract for a Sandbox. A
// phase is part of the deterministic Secret name and contract, so a verify
// reconcile cannot accidentally reuse a work projection.
type Phase string

const (
	PhaseWork   Phase = "work"
	PhaseVerify Phase = "verify"
)

// KubeClient is intentionally smaller than client.Client. The materializer
// can only perform named GETs and creates; in particular it has no List or
// Watch capability. The same client may serve both the agw-system source
// namespace and the AgentRun destination namespace.
type KubeClient interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
	Create(context.Context, client.Object, ...client.CreateOption) error
}

// CloneToken is the security evidence returned by a CloneTokenMinter. The
// materializer refuses a token unless all metadata says it is an unexpired,
// repository-scoped, read-only contents token. Value is consumed only while
// constructing the Secret and is never placed in an error.
type CloneToken struct {
	Value         string
	Repository    string
	ContentsRead  bool
	ContentsWrite bool
	ExpiresAt     time.Time
}

// CloneTokenMinter is implemented by an adapter around the GitHub App client.
// This package deliberately does not depend on that implementation. The
// adapter must request a short-lived installation token with contents:read and
// no contents:write permission, scoped to exactly repository.
type CloneTokenMinter interface {
	MintReadOnlyContentsToken(context.Context, string) (CloneToken, error)
}

type ArtifactIssuer interface {
	Issue(context.Context, artifactauth.Request) (artifactauth.Result, error)
}

// SecretKeyRef is the explicit source selector used only for the optional
// skills token. Namespace is intentionally absent; only operator
// configuration selects the source namespace.
type SecretKeyRef struct {
	SecretName string
	Key        string
}

// Limits bounds source reads and output Secret material. Zero values use the
// package defaults. Values above the package hard ceilings are rejected by
// New instead of silently accepting an unsafe configuration.
type Limits struct {
	MaxSourceSecrets int
	MaxProjectedKeys int
	MaxValueBytes    int
	MaxTotalBytes    int
}

// Config supplies the two external seams and the optional skills source.
type Config struct {
	Client           KubeClient
	CloneTokenMinter CloneTokenMinter
	Phase            Phase
	SourceNamespace  string
	// AllowedSourceSecrets is an operator-owned allowlist for logical
	// ToolSet/ModelRoute credential references and the optional skills Secret.
	// A nil/empty value keeps this low-level package usable in isolated tests;
	// the production runplan requires a non-empty allowlist before startup.
	AllowedSourceSecrets []string
	SkillsToken          *SecretKeyRef
	Limits               Limits
	Clock                func() time.Time
	ArtifactIssuer       ArtifactIssuer
	ArtifactScope        artifactauth.Scope
	ArtifactTTL          time.Duration
}

// Materializer creates and validates a single immutable run Secret.
type Materializer struct {
	client               KubeClient
	cloneTokenMinter     CloneTokenMinter
	phase                Phase
	sourceNamespace      string
	allowedSourceSecrets map[string]struct{}
	skillsToken          *SecretKeyRef
	limits               Limits
	clock                func() time.Time
	artifactIssuer       ArtifactIssuer
	artifactScope        artifactauth.Scope
	artifactTTL          time.Duration
}

// Result contains only non-sensitive projection metadata. It is suitable for
// passing to workload.Build and to the broker configuration. It never
// contains a token value.
type Result struct {
	SecretName string
	Namespace  string
	Phase      Phase

	CloneSecretKey                   string
	SkillsSecretKey                  string
	BrokerSecretKeys                 []string
	ArtifactAccessKeyIDSecretKey     string
	ArtifactSecretAccessKeySecretKey string
	ArtifactSessionTokenSecretKey    string

	// LogicalRefToProjectedKey maps each ToolSet.credentialsRef or
	// ModelRoute.credentialRef to the data key projected into the broker's
	// explicitly selected Secret volume.
	LogicalRefToProjectedKey map[string]string
}

type layout struct {
	result         Result
	keys           map[string]struct{}
	logicalRefs    []string
	skillsToken    *SecretKeyRef
	contractDigest string
}

// New validates the trust-boundary dependencies and returns a materializer.
// It does not contact Kubernetes or mint credentials.
func New(config Config) (*Materializer, error) {
	if config.Client == nil || config.CloneTokenMinter == nil {
		return nil, ErrInvalidConfig
	}
	phase := config.Phase
	if phase == "" {
		phase = PhaseWork
	}
	if phase != PhaseWork && phase != PhaseVerify {
		return nil, ErrInvalidConfig
	}
	if config.ArtifactIssuer == nil {
		if config.ArtifactScope != (artifactauth.Scope{}) || config.ArtifactTTL != 0 {
			return nil, ErrInvalidConfig
		}
	} else if config.ArtifactScope == (artifactauth.Scope{}) {
		return nil, ErrInvalidConfig
	}
	if phase == PhaseVerify {
		if config.SkillsToken != nil || config.ArtifactIssuer == nil || config.ArtifactScope.Permissions != (artifactauth.Permissions{Read: true}) {
			return nil, ErrInvalidConfig
		}
	} else if config.ArtifactIssuer != nil && !config.ArtifactScope.Permissions.Write {
		return nil, ErrInvalidConfig
	}
	if config.ArtifactIssuer != nil && (config.ArtifactTTL < artifactauth.MinLeaseLifetime || config.ArtifactTTL > artifactauth.MaxLeaseLifetime) {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	if config.SkillsToken != nil {
		if err := validateSecretKeyRef(*config.SkillsToken); err != nil {
			return nil, err
		}
		config.SkillsToken = &SecretKeyRef{SecretName: config.SkillsToken.SecretName, Key: config.SkillsToken.Key}
	}
	allowedSourceSecrets, err := normalizeAllowedSourceSecrets(config.AllowedSourceSecrets)
	if err != nil {
		return nil, err
	}
	sourceNamespace := config.SourceNamespace
	if sourceNamespace == "" {
		sourceNamespace = SourceNamespace
	}
	if len(validation.IsDNS1123Label(sourceNamespace)) != 0 {
		return nil, ErrInvalidConfig
	}
	return &Materializer{
		client:               config.Client,
		cloneTokenMinter:     config.CloneTokenMinter,
		phase:                phase,
		sourceNamespace:      sourceNamespace,
		allowedSourceSecrets: allowedSourceSecrets,
		skillsToken:          config.SkillsToken,
		limits:               limits,
		clock:                clock,
		artifactIssuer:       config.ArtifactIssuer,
		artifactScope:        config.ArtifactScope,
		artifactTTL:          config.ArtifactTTL,
	}, nil
}

// Materialize creates the deterministic Secret for run and returns its
// projection metadata. specDigest must be the digest recorded in
// AgentRun.status. It is independently recomputed from snapshot so a caller
// cannot bind a Secret to a digest for a different execution contract.
//
// The method first performs a named GET of the destination. An existing Secret
// with the same owner, digest, immutable flag, contract, and exact key set is
// returned without re-reading source credentials or minting a second token.
// A missing destination is populated and created once. A create race is
// accepted only after a fresh named GET and the same validation.
func (m *Materializer) Materialize(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot, specDigest string) (Result, error) {
	if ctx == nil {
		return Result{}, ErrInvalidInput
	}
	if m == nil || m.client == nil || m.cloneTokenMinter == nil {
		return Result{}, ErrInvalidConfig
	}
	if err := validateRunIdentity(run, snapshot); err != nil {
		return Result{}, err
	}
	if !canonical.ValidDigest(specDigest) {
		return Result{}, ErrInvalidInput
	}
	if err := validateAllowedSourceSecrets(snapshot, m.phase, m.skillsToken, m.allowedSourceSecrets); err != nil {
		return Result{}, err
	}
	computedDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil || computedDigest != specDigest {
		return Result{}, ErrSpecDigestConflict
	}
	if m.artifactIssuer != nil && m.artifactScope.Prefix != artifactRunPrefix(m.artifactScope.Prefix, string(run.UID)) {
		return Result{}, ErrArtifactLease
	}
	layout, err := makeLayout(snapshot, m.phase, m.skillsToken, m.limits, m.artifactIssuer != nil)
	if err != nil {
		return Result{}, err
	}

	secretName := layout.result.SecretName
	key := client.ObjectKey{Namespace: run.Namespace, Name: secretName}
	existing := &corev1.Secret{}
	getErr := m.client.Get(ctx, key, existing)
	if getErr == nil {
		if err := validateExisting(existing, run, specDigest, layout, m.limits); err != nil {
			return Result{}, err
		}
		return copyResult(layout.result), nil
	}
	if !apierrors.IsNotFound(getErr) {
		return Result{}, ErrDestinationRead
	}

	data, err := m.buildData(ctx, snapshot, layout)
	if err != nil {
		return Result{}, err
	}
	secret := newSecret(run, secretName, specDigest, layout.contractDigest, data, m.phase)
	if err := m.client.Create(ctx, secret); err == nil {
		return copyResult(layout.result), nil
	} else if !apierrors.IsAlreadyExists(err) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		return Result{}, ErrDestinationCreate
	}

	// Another reconciler won the create race. Do not trust AlreadyExists by
	// itself; the object must pass the same owner/digest/contract checks.
	traced := &corev1.Secret{}
	if err := m.client.Get(ctx, key, traced); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{}, ErrDestinationRace
		}
		return Result{}, ErrDestinationRead
	}
	if err := validateExisting(traced, run, specDigest, layout, m.limits); err != nil {
		return Result{}, err
	}
	return copyResult(layout.result), nil
}

func (m *Materializer) buildData(ctx context.Context, snapshot resolved.Snapshot, layout layout) (map[string][]byte, error) {
	data := make(map[string][]byte, len(layout.keys))
	totalBytes := 0
	add := func(key, value string) error {
		if _, exists := data[key]; exists || !validOutputKey(key) {
			return ErrSecretContractConflict
		}
		if err := validateValue(value, m.limits.MaxValueBytes); err != nil {
			return err
		}
		totalBytes += len(key) + len(value)
		if totalBytes > m.limits.MaxTotalBytes {
			return ErrBounds
		}
		data[key] = []byte(value)
		return nil
	}

	token, err := m.cloneTokenMinter.MintReadOnlyContentsToken(ctx, snapshot.Spec.Source.Repo)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrCloneToken
	}
	now := m.clock().UTC()
	if !validCloneToken(token, snapshot.Spec.Source.Repo, now, m.limits.MaxValueBytes) {
		return nil, ErrCloneToken
	}
	if err := add(CloneSecretKey, token.Value); err != nil {
		return nil, err
	}

	if m.phase == PhaseWork {
		cache := make(map[string]*corev1.Secret, len(layout.logicalRefs)+1)
		getSource := func(name string) (*corev1.Secret, error) {
			if source, ok := cache[name]; ok {
				return source, nil
			}
			if len(cache) >= m.limits.MaxSourceSecrets {
				return nil, ErrBounds
			}
			source := &corev1.Secret{}
			if err := m.client.Get(ctx, client.ObjectKey{Namespace: m.sourceNamespace, Name: name}, source); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, ErrSourceSecretMissing
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				return nil, ErrSourceSecretRead
			}
			cache[name] = source
			return source, nil
		}

		if layout.skillsToken != nil {
			source, err := getSource(layout.skillsToken.SecretName)
			if err != nil {
				return nil, err
			}
			value, ok := source.Data[layout.skillsToken.Key]
			if !ok {
				return nil, ErrSourceDataInvalid
			}
			if err := add(SkillsSecretKey, string(value)); err != nil {
				return nil, err
			}
		}

		for _, logicalRef := range layout.logicalRefs {
			source, err := getSource(logicalRef)
			if err != nil {
				return nil, err
			}
			value, ok := source.Data[SourceTokenKey]
			if !ok {
				return nil, ErrSourceDataInvalid
			}
			if err := add(layout.result.LogicalRefToProjectedKey[logicalRef], string(value)); err != nil {
				return nil, err
			}
		}
	}
	if m.artifactIssuer != nil {
		lease, err := m.artifactIssuer.Issue(ctx, artifactauth.Request{
			Run:   artifactauth.RunIdentity{Namespace: snapshot.Run.Namespace, Name: snapshot.Run.Name, UID: snapshot.Run.UID},
			Scope: m.artifactScope, Purpose: phasePurpose(m.phase), TTL: m.artifactTTL,
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, ErrArtifactLease
		}
		defer lease.Wipe()
		credentialData, err := lease.CopySecretData()
		if err != nil {
			return nil, ErrArtifactLease
		}
		defer wipeCredentialData(credentialData)
		for _, key := range []string{artifactauth.AccessKeyIDKey, artifactauth.SecretAccessKeyKey, artifactauth.SessionTokenKey} {
			value, ok := credentialData[key]
			if !ok {
				return nil, ErrArtifactLease
			}
			if err := add(key, string(value)); err != nil {
				return nil, err
			}
			for index := range value {
				value[index] = 0
			}
		}
	}
	if len(data) != len(layout.keys) {
		return nil, ErrSecretContractConflict
	}
	return data, nil
}

func makeLayout(snapshot resolved.Snapshot, phase Phase, skillsToken *SecretKeyRef, limits Limits, artifactCredentials bool) (layout, error) {
	if phase != PhaseWork && phase != PhaseVerify {
		return layout{}, ErrInvalidInput
	}
	if phase == PhaseVerify && skillsToken != nil {
		return layout{}, ErrInvalidInput
	}
	if skillsToken != nil && len(snapshot.Agent.Skills) == 0 {
		return layout{}, ErrInvalidInput
	}
	refs := make(map[string]struct{})
	if phase == PhaseWork {
		for _, server := range snapshot.ToolSet.Servers {
			if server.CredentialsRef == "" {
				continue
			}
			if err := validateSecretName(server.CredentialsRef); err != nil {
				return layout{}, ErrInvalidInput
			}
			refs[server.CredentialsRef] = struct{}{}
		}
		for _, provider := range snapshot.ModelRoute.Providers {
			if provider.CredentialRef == "" {
				continue
			}
			if err := validateSecretName(provider.CredentialRef); err != nil {
				return layout{}, ErrInvalidInput
			}
			refs[provider.CredentialRef] = struct{}{}
		}
		publishRef := snapshot.Spec.Publish.CredentialRef
		if publishRef != "" {
			if err := validateSecretName(publishRef); err != nil {
				return layout{}, ErrInvalidInput
			}
			if _, projected := refs[publishRef]; projected {
				return layout{}, ErrPublishCredentialOverlap
			}
		}
	}

	logicalRefs := make([]string, 0, len(refs))
	for ref := range refs {
		logicalRefs = append(logicalRefs, ref)
	}
	sort.Strings(logicalRefs)
	if len(logicalRefs) > limits.MaxSourceSecrets {
		return layout{}, ErrBounds
	}

	result := Result{
		SecretName:               secretNameForPhase(phase, snapshot.Run.UID),
		Namespace:                snapshot.Run.Namespace,
		Phase:                    phase,
		CloneSecretKey:           CloneSecretKey,
		LogicalRefToProjectedKey: make(map[string]string, len(logicalRefs)),
	}
	keys := map[string]struct{}{CloneSecretKey: {}}
	brokerKeys := make([]string, 0, len(logicalRefs))
	for _, ref := range logicalRefs {
		projectedKey := brokerSecretKey(ref)
		if _, collision := keys[projectedKey]; collision {
			return layout{}, ErrSecretContractConflict
		}
		keys[projectedKey] = struct{}{}
		result.LogicalRefToProjectedKey[ref] = projectedKey
		brokerKeys = append(brokerKeys, projectedKey)
	}
	result.BrokerSecretKeys = brokerKeys

	var normalizedSkills *SecretKeyRef
	if skillsToken != nil {
		copyRef := *skillsToken
		normalizedSkills = &copyRef
		if _, collision := keys[SkillsSecretKey]; collision {
			return layout{}, ErrSecretContractConflict
		}
		keys[SkillsSecretKey] = struct{}{}
		result.SkillsSecretKey = SkillsSecretKey
	}
	if artifactCredentials {
		for _, key := range []string{artifactauth.AccessKeyIDKey, artifactauth.SecretAccessKeyKey, artifactauth.SessionTokenKey} {
			if _, collision := keys[key]; collision {
				return layout{}, ErrSecretContractConflict
			}
			keys[key] = struct{}{}
		}
		result.ArtifactAccessKeyIDSecretKey = artifactauth.AccessKeyIDKey
		result.ArtifactSecretAccessKeySecretKey = artifactauth.SecretAccessKeyKey
		result.ArtifactSessionTokenSecretKey = artifactauth.SessionTokenKey
	}
	if len(keys) > limits.MaxProjectedKeys {
		return layout{}, ErrBounds
	}

	return layout{
		result:         result,
		keys:           keys,
		logicalRefs:    logicalRefs,
		skillsToken:    normalizedSkills,
		contractDigest: contractDigest(result, keys),
	}, nil
}

func secretNameForPhase(phase Phase, runUID string) string {
	if phase == PhaseVerify {
		return workload.VerifySecretName(runUID)
	}
	return workload.WorkSecretName(runUID)
}

func phasePurpose(phase Phase) string {
	if phase == PhaseVerify {
		return "verify-fetch"
	}
	return "work-broker"
}

func artifactRunPrefix(prefix, runUID string) string {
	parts := strings.Split(prefix, "/")
	if len(parts) < 2 || parts[len(parts)-2] != "runs" || parts[len(parts)-1] != runUID {
		return ""
	}
	return prefix
}

func validateRunIdentity(run *v1alpha1.AgentRun, snapshot resolved.Snapshot) error {
	if run == nil || run.Namespace == "" || run.Name == "" || run.UID == "" {
		return ErrInvalidInput
	}
	if len(run.UID) > maxRunUIDLength || len(validation.IsValidLabelValue(string(run.UID))) != 0 || len(validation.IsDNS1123Subdomain(run.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(run.Name)) != 0 {
		return ErrInvalidInput
	}
	if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) {
		return ErrInvalidInput
	}
	return nil
}

func validateSecretKeyRef(ref SecretKeyRef) error {
	if err := validateSecretName(ref.SecretName); err != nil || len(validation.IsConfigMapKey(ref.Key)) != 0 {
		return ErrInvalidConfig
	}
	return nil
}

func validateSecretName(name string) error {
	if name == "" || len(name) > maxSecretNameLength || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return ErrInvalidInput
	}
	return nil
}

func normalizeAllowedSourceSecrets(values []string) (map[string]struct{}, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > MaxMaxSourceSecrets {
		return nil, ErrInvalidConfig
	}
	allowed := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateSecretName(value); err != nil {
			return nil, ErrInvalidConfig
		}
		if _, exists := allowed[value]; exists {
			return nil, ErrInvalidConfig
		}
		allowed[value] = struct{}{}
	}
	return allowed, nil
}

func validateAllowedSourceSecrets(snapshot resolved.Snapshot, phase Phase, skillsToken *SecretKeyRef, allowed map[string]struct{}) error {
	if allowed == nil {
		return nil
	}
	check := func(name string) error {
		if _, ok := allowed[name]; !ok {
			return ErrSourceSecretNotAllowed
		}
		return nil
	}
	if phase == PhaseWork {
		for _, server := range snapshot.ToolSet.Servers {
			if server.CredentialsRef != "" {
				if err := check(server.CredentialsRef); err != nil {
					return err
				}
			}
		}
		for _, provider := range snapshot.ModelRoute.Providers {
			if provider.CredentialRef != "" {
				if err := check(provider.CredentialRef); err != nil {
					return err
				}
			}
		}
	}
	if skillsToken != nil {
		if err := check(skillsToken.SecretName); err != nil {
			return err
		}
	}
	return nil
}

func normalizeLimits(input Limits) (Limits, error) {
	limits := input
	if limits.MaxSourceSecrets == 0 {
		limits.MaxSourceSecrets = DefaultMaxSourceSecrets
	}
	if limits.MaxProjectedKeys == 0 {
		limits.MaxProjectedKeys = DefaultMaxProjectedKeys
	}
	if limits.MaxValueBytes == 0 {
		limits.MaxValueBytes = DefaultMaxValueBytes
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if limits.MaxSourceSecrets < 1 || limits.MaxSourceSecrets > MaxMaxSourceSecrets ||
		limits.MaxProjectedKeys < 1 || limits.MaxProjectedKeys > MaxMaxProjectedKeys ||
		limits.MaxValueBytes < 1 || limits.MaxValueBytes > MaxMaxValueBytes ||
		limits.MaxTotalBytes < 1 || limits.MaxTotalBytes > MaxMaxTotalBytes ||
		limits.MaxTotalBytes < limits.MaxValueBytes {
		return Limits{}, ErrInvalidConfig
	}
	return limits, nil
}

func validCloneToken(token CloneToken, repository string, now time.Time, maxValueBytes int) bool {
	if token.Value == "" || len(token.Value) > maxValueBytes || token.Repository != repository || !token.ContentsRead || token.ContentsWrite || token.ExpiresAt.IsZero() {
		return false
	}
	if strings.TrimSpace(token.Value) != token.Value || strings.ContainsAny(token.Value, "\x00\r\n") {
		return false
	}
	if now.IsZero() || !token.ExpiresAt.After(now) || token.ExpiresAt.After(now.Add(MaxCloneTokenLifetime)) {
		return false
	}
	return true
}

func validateValue(value string, maxValueBytes int) error {
	if value == "" || len(value) > maxValueBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return ErrSourceDataInvalid
	}
	return nil
}

func validOutputKey(key string) bool {
	return key != "" && len(validation.IsConfigMapKey(key)) == 0
}

func brokerSecretKey(logicalRef string) string {
	digest := sha256.Sum256([]byte("agw-broker-credential\x00" + logicalRef))
	return "broker-" + hex.EncodeToString(digest[:])
}

func contractDigest(result Result, keys map[string]struct{}) string {
	orderedKeys := make([]string, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	refs := make([]string, 0, len(result.LogicalRefToProjectedKey))
	for ref := range result.LogicalRefToProjectedKey {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	builder := strings.Builder{}
	builder.WriteString(contractVersion)
	builder.WriteByte('\n')
	builder.WriteString(string(result.Phase))
	builder.WriteByte('\n')
	for _, key := range orderedKeys {
		builder.WriteString(key)
		builder.WriteByte('\n')
	}
	for _, ref := range refs {
		builder.WriteString(ref)
		builder.WriteByte('=')
		builder.WriteString(result.LogicalRefToProjectedKey[ref])
		builder.WriteByte('\n')
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func newSecret(run *v1alpha1.AgentRun, name, specDigest, contract string, data map[string][]byte, phases ...Phase) *corev1.Secret {
	phase := PhaseWork
	if len(phases) > 0 && phases[0] != "" {
		phase = phases[0]
	}
	immutable := true
	controller := true
	blockOwnerDeletion := true
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
				phaseLabelKey:     string(phase),
				runUIDLabelKey:    string(run.UID),
			},
			Annotations: map[string]string{
				specDigestAnnotationKey: specDigest,
				contractAnnotationKey:   contract,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "AgentRun",
				Name:               run.Name,
				UID:                types.UID(run.UID),
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      cloneData(data),
	}
}

func validateExisting(secret *corev1.Secret, run *v1alpha1.AgentRun, specDigest string, layout layout, limits Limits) error {
	if secret == nil || secret.Namespace != run.Namespace || secret.Name != layout.result.SecretName {
		return ErrSecretContractConflict
	}
	if len(secret.OwnerReferences) != 1 || !matchesOwner(secret.OwnerReferences[0], run) {
		return ErrOwnershipConflict
	}
	if secret.Annotations == nil || secret.Annotations[specDigestAnnotationKey] != specDigest {
		return ErrSpecDigestConflict
	}
	if secret.Annotations[contractAnnotationKey] != layout.contractDigest ||
		secret.Labels == nil || secret.Labels[managedByLabelKey] != managedByLabelValue || secret.Labels[phaseLabelKey] != string(layout.result.Phase) || secret.Labels[runUIDLabelKey] != string(run.UID) {
		return ErrSecretContractConflict
	}
	if secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.StringData) != 0 {
		return ErrSecretContractConflict
	}
	if len(secret.Data) != len(layout.keys) || len(secret.Data) > limits.MaxProjectedKeys {
		return ErrSecretContractConflict
	}
	totalBytes := 0
	for key, value := range secret.Data {
		if _, expected := layout.keys[key]; !expected {
			return ErrSecretContractConflict
		}
		if err := validateValue(string(value), limits.MaxValueBytes); err != nil {
			return ErrSecretContractConflict
		}
		totalBytes += len(key) + len(value)
		if totalBytes > limits.MaxTotalBytes {
			return ErrSecretContractConflict
		}
	}
	return nil
}

func matchesOwner(owner metav1.OwnerReference, run *v1alpha1.AgentRun) bool {
	return owner.APIVersion == v1alpha1.GroupVersion.String() && owner.Kind == "AgentRun" && owner.Name == run.Name && owner.UID == types.UID(run.UID) && owner.Controller != nil && *owner.Controller && owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion
}

func cloneData(input map[string][]byte) map[string][]byte {
	output := make(map[string][]byte, len(input))
	for key, value := range input {
		output[key] = append([]byte(nil), value...)
	}
	return output
}

func wipeCredentialData(input map[string][]byte) {
	for key, value := range input {
		for index := range value {
			value[index] = 0
		}
		delete(input, key)
	}
}

func copyResult(input Result) Result {
	output := input
	output.BrokerSecretKeys = append([]string(nil), input.BrokerSecretKeys...)
	output.LogicalRefToProjectedKey = make(map[string]string, len(input.LogicalRefToProjectedKey))
	for key, value := range input.LogicalRefToProjectedKey {
		output.LogicalRefToProjectedKey[key] = value
	}
	return output
}
