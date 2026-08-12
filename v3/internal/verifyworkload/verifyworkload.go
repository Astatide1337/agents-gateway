// Package verifyworkload builds the independent verification Sandbox.
//
// The builder is deliberately pure. It accepts an immutable resolved snapshot,
// immutable patch metadata, and explicit operator-owned image, Secret, and
// lifecycle inputs. It never reads Kubernetes, the filesystem, environment
// variables, or the clock. The returned plan can be passed directly to the
// agents-sandbox backend.
package verifyworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const (
	// The public-egress NetworkPolicy is intentionally setup-only. It is a
	// pod-level CNI permission needed while fetch runs; lockdown removes the
	// permission from the network namespace before the verify container starts.
	// A dedicated value prevents a verify pod from being mistaken for a work
	// pod's broker egress identity.
	SetupEgressLabelKey   = "agents.astatide.com/egress"
	SetupEgressLabelValue = "verify-fetch-public"

	VerifyInputDigestAnnotationKey = "agents.astatide.com/verify-input-digest"

	WorkspaceVolumeName   = "verify-workspace"
	FetchSecretVolumeName = "fetch-secret"

	WorkspaceMountPath          = "/verify/workspace"
	RepoPath                    = WorkspaceMountPath + "/repo"
	BasePath                    = WorkspaceMountPath + "/base"
	PatchPath                   = WorkspaceMountPath + "/patch.diff"
	FetchSecretPath             = "/run/agw/fetch"
	CloneTokenFile              = FetchSecretPath + "/clone-token"
	ArtifactAccessKeyIDFile     = FetchSecretPath + "/artifact-access-key-id"
	ArtifactSecretAccessKeyFile = FetchSecretPath + "/artifact-secret-access-key"
	ArtifactSessionTokenFile    = FetchSecretPath + "/artifact-session-token"

	// Artifact credential keys are intentionally fixed. The controller may
	// choose the Secret name, but it cannot make this builder project an
	// arbitrary key or a single opaque token.
	ArtifactAccessKeyIDSecretKey     = artifactauth.AccessKeyIDKey
	ArtifactSecretAccessKeySecretKey = artifactauth.SecretAccessKeyKey
	ArtifactSessionTokenSecretKey    = artifactauth.SessionTokenKey

	fetchEntrypoint   = "/agw/fetch"
	applyEntrypoint   = "/agw/apply"
	lockdownShell     = "sh"
	lockdownShellArgs = "-ceu"
	verifyEntrypoint  = "/agw/verify"

	defaultMaxCommandBytes = 64 << 10
	hardMaxCommandBytes    = 256 << 10
	defaultMaxOutputBytes  = int64(64 << 10)
	hardMaxOutputBytes     = int64(256 << 10)
	maxPatchURIBytes       = 1024
	maxPatchBytes          = verifyfetch.PatchFetchMaxObjectBytes
	maxRunUIDBytes         = 63
	maxPolicyChecksBytes   = 512 << 10
)

var (
	ErrInvalidInput = errors.New("invalid verify Sandbox input")

	// These resources are fixed so a manifest cannot be made un-schedulable or
	// unexpectedly expensive by repository/task text.
	fetchResources    = resources("10m", "64Mi", "250m", "256Mi")
	applyResources    = resources("10m", "64Mi", "250m", "256Mi")
	lockdownResources = resources("10m", "16Mi", "100m", "64Mi")
	verifyResources   = resources("100m", "256Mi", "1", "1Gi")

	githubRepoPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	imageNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	shaPattern        = regexp.MustCompile(`^[0-9a-f]+$`)
)

// Options are operator-resolved inputs that are intentionally not part of the
// user-authored snapshot. All image values must be immutable OCI references.
// Secret keys are projected into fetch only; apply and verify never receive a
// Secret volume, SecretKeyRef, or service-account token.
type Options struct {
	FetchImage    string
	ApplyImage    string
	LockdownImage string

	// Placement allowlists must match the work planner. The verify Sandbox is
	// independent, so it validates the same operator-owned placement contract
	// before emitting a second pod.
	AllowedRuntimeClasses []string
	AllowedStorageClasses []string

	// SecretName is normally the deterministic per-run Secret. Only the fixed
	// clone and artifact keys below are mounted, and only in fetch.
	SecretName     string
	CloneSecretKey string
	// ArtifactCredentialTTL is the maximum lifetime of the fresh read-only
	// artifact lease materialized for this verify phase. The Gate timeout must
	// fit inside it; zero is rejected when artifact credentials are configured.
	ArtifactCredentialTTL time.Duration

	ArtifactStoreEndpoint       string
	ArtifactStoreRegion         string
	ArtifactStoreBucket         string
	ArtifactStoreForcePathStyle bool
	MaxPatchBytes               int64
	MaxOutputBytes              int64

	Now                 time.Time
	ShutdownTime        time.Time
	MaxShutdownDuration time.Duration

	// MaxCommandBytes bounds the JSON contract handed to the trusted verifier.
	// Zero selects defaultMaxCommandBytes; values above hardMaxCommandBytes are
	// rejected instead of silently clamped.
	MaxCommandBytes int
	// PolicyChecks is the descriptor-only projection compiled from the same
	// immutable Policy contract used by ContextPack and the broker. It is
	// copied into the verify container as a bounded canonical JSON array.
	PolicyChecks []policycontract.GateCheckDescriptor
}

// Build constructs a complete RoleVerify SandboxPlan. Plan.Sandbox is also
// the deterministic upstream agents.x-k8s.io/v1beta1 Sandbox object.
func Build(snapshot resolved.Snapshot, patch v1alpha1.ArtifactRef, baseSHA string, options Options) (sandbox.SandboxPlan, error) {
	if err := validateSnapshot(snapshot, options); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if err := validatePatch(patch); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if err := validateBaseSHA(baseSHA); err != nil {
		return sandbox.SandboxPlan{}, err
	}
	if snapshot.BaseSHA != baseSHA {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: baseSHA does not match immutable resolved snapshot", ErrInvalidInput)
	}
	if err := normalizeOptions(&options, snapshot, patch); err != nil {
		return sandbox.SandboxPlan{}, err
	}

	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil || !canonical.ValidDigest(specDigest) {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: compute resolved spec digest: %v", ErrInvalidInput, err)
	}
	childName, err := sandbox.ChildName(types.UID(snapshot.Run.UID), sandbox.RoleVerify)
	if err != nil {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: child name: %v", ErrInvalidInput, err)
	}

	verifyCommands, err := encodeVerifyCommands(snapshot.Gate.Verify.Commands, snapshot.Gate.Require, options.MaxCommandBytes)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	policyChecks, err := encodePolicyChecks(options.PolicyChecks)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	fetchSpec, err := encodeFetchSpec(snapshot, patch, baseSHA, options)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	applySpec, err := encodeApplySpecFor(patch, baseSHA, options.MaxPatchBytes)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	verifyInputDigest, err := verifyInputDigest(specDigest, patch, baseSHA, options)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}

	labels := map[string]string{
		sandbox.RunUIDLabelKey:                  snapshot.Run.UID,
		sandbox.RoleLabelKey:                    string(sandbox.RoleVerify),
		sandbox.SpecDigestLabelKey:              digestShort(specDigest),
		"agents.astatide.com/spec-digest-short": digestShort(specDigest),
		SetupEgressLabelKey:                     SetupEgressLabelValue,
	}
	annotations := map[string]string{
		sandbox.SpecDigestAnnotationKey: specDigest,
		VerifyInputDigestAnnotationKey:  verifyInputDigest,
	}
	owner := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupName + "/" + v1alpha1.Version,
		Kind:               "AgentRun",
		Name:               snapshot.Run.Name,
		UID:                types.UID(snapshot.Run.UID),
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}

	secretVolumes, err := fetchSecretVolumes(options)
	if err != nil {
		return sandbox.SandboxPlan{}, err
	}
	shutdown := metav1.NewTime(options.ShutdownTime.UTC())
	shutdownPolicy := sandboxv1beta1.ShutdownPolicyDelete

	fetch := corev1.Container{
		Name:            "fetch",
		Image:           options.FetchImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{fetchEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_FETCH_SPEC_JSON", Value: string(fetchSpec)},
			{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
			{Name: "AGW_REPO_PATH", Value: RepoPath},
			{Name: "AGW_BASE_PATH", Value: BasePath},
			{Name: "AGW_PATCH_PATH", Value: PatchPath},
			{Name: "AGW_GIT_TOKEN_FILE", Value: CloneTokenFile},
		},
		WorkingDir:      WorkspaceMountPath,
		Resources:       copyResources(fetchResources),
		SecurityContext: fetchSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			volumeMount(WorkspaceVolumeName, WorkspaceMountPath, false),
			// Projected Secret directories are Kubernetes atomic-writer
			// symlinks. The fetch helper deliberately rejects symlink
			// credentials, so mount each selected item directly over a regular
			// file baked into the image.
			directSecretItemMount(FetchSecretVolumeName, CloneTokenFile, "clone-token"),
			directSecretItemMount(FetchSecretVolumeName, ArtifactAccessKeyIDFile, "artifact-access-key-id"),
			directSecretItemMount(FetchSecretVolumeName, ArtifactSecretAccessKeyFile, "artifact-secret-access-key"),
			directSecretItemMount(FetchSecretVolumeName, ArtifactSessionTokenFile, "artifact-session-token"),
		},
	}
	apply := corev1.Container{
		Name:            "apply",
		Image:           options.ApplyImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{applyEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_APPLY_SPEC_JSON", Value: string(applySpec)},
			{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
			{Name: "AGW_REPO_PATH", Value: RepoPath},
			{Name: "AGW_BASE_PATH", Value: BasePath},
			{Name: "AGW_PATCH_PATH", Value: PatchPath},
		},
		WorkingDir:      RepoPath,
		Resources:       copyResources(applyResources),
		SecurityContext: regularSecurityContext(1000),
		VolumeMounts:    []corev1.VolumeMount{volumeMount(WorkspaceVolumeName, WorkspaceMountPath, false)},
	}

	lockdown := corev1.Container{
		Name:            "lockdown",
		Image:           options.LockdownImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{lockdownShell},
		Args:            []string{lockdownShellArgs, lockdownScript},
		Resources:       copyResources(lockdownResources),
		SecurityContext: lockdownSecurityContext(),
	}

	verify := corev1.Container{
		Name:            "verify",
		Image:           snapshot.Gate.Verify.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{verifyEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_VERIFY_COMMANDS_JSON", Value: string(verifyCommands)},
			{Name: "AGW_VERIFY_TIMEOUT", Value: snapshot.Gate.Verify.Timeout},
			{Name: "AGW_VERIFY_MAX_OUTPUT_BYTES", Value: strconv.FormatInt(options.MaxOutputBytes, 10)},
			{Name: "AGW_VERIFY_NETWORK", Value: "disabled"},
			{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
			{Name: "AGW_REPO_PATH", Value: RepoPath},
			{Name: "AGW_BASE_PATH", Value: BasePath},
			{Name: "AGW_PATCH_PATH", Value: PatchPath},
			{Name: "AGW_BASE_SHA", Value: baseSHA},
			{Name: "AGW_PATCH_DIGEST", Value: patch.Digest},
			{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
			{Name: "AGW_SPEC_DIGEST", Value: specDigest},
			{Name: "AGW_VERIFY_INPUT_DIGEST", Value: verifyInputDigest},
			{Name: "AGW_VERIFY_POLICY_CHECKS_JSON", Value: string(policyChecks)},
		},
		WorkingDir:      RepoPath,
		Resources:       copyResources(verifyResources),
		SecurityContext: regularSecurityContext(1000),
		VolumeMounts:    []corev1.VolumeMount{volumeMount(WorkspaceVolumeName, WorkspaceMountPath, false)},
	}

	manifest := &sandboxv1beta1.Sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: sandboxv1beta1.SandboxKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:            childName,
			Namespace:       snapshot.Run.Namespace,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: copyStringMap(labels), Annotations: copyStringMap(annotations)},
					Spec: corev1.PodSpec{
						HostUsers:                    boolPtr(false),
						AutomountServiceAccountToken: boolPtr(false),
						HostNetwork:                  false,
						HostPID:                      false,
						HostIPC:                      false,
						NodeSelector:                 map[string]string{workload.NodeLabelKey: workload.NodeLabelValue},
						Tolerations: []corev1.Toleration{{
							Key: workload.NodeLabelKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
						}},
						RuntimeClassName:              optionalString(snapshot.Agent.Runtime.RuntimeClassName),
						RestartPolicy:                 corev1.RestartPolicyNever,
						TerminationGracePeriodSeconds: int64Ptr(30),
						EnableServiceLinks:            boolPtr(false),
						SecurityContext: &corev1.PodSecurityContext{
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						InitContainers: []corev1.Container{fetch, apply, lockdown},
						Containers:     []corev1.Container{verify},
						Volumes: append([]corev1.Volume{{
							Name:         WorkspaceVolumeName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						}}, secretVolumes...),
					},
				},
				VolumeClaimTemplates: nil,
				Service:              boolPtr(false),
			},
			Lifecycle: sandboxv1beta1.Lifecycle{ShutdownTime: &shutdown, ShutdownPolicy: &shutdownPolicy},
		},
	}

	return sandbox.SandboxPlan{Owner: ownerObject(snapshot), Role: sandbox.RoleVerify, SpecDigest: specDigest, Sandbox: manifest}, nil
}

// The verifier is intentionally networkless after the fetch/apply init
// containers finish. It has no sidecar or local service to reach, so even
// loopback is denied; this prevents a future helper/listener from becoming an
// accidental egress path without changing the verifier contract.
const lockdownScript = `set -eu
iptables-restore --wait --noflush <<'AGW_VERIFY_IPV4'
*filter
-P OUTPUT DROP
-F OUTPUT
COMMIT
AGW_VERIFY_IPV4
ip6tables-restore --wait --noflush <<'AGW_VERIFY_IPV6'
*filter
-P OUTPUT DROP
-F OUTPUT
COMMIT
AGW_VERIFY_IPV6`

type fetchSpec struct {
	Version                     int                     `json:"version"`
	Repository                  string                  `json:"repository"`
	BaseSHA                     string                  `json:"baseSHA"`
	Depth                       int                     `json:"depth"`
	RepoPath                    string                  `json:"repoPath"`
	BasePath                    string                  `json:"basePath"`
	PatchPath                   string                  `json:"patchPath"`
	PatchBucket                 string                  `json:"patchBucket"`
	PatchKey                    string                  `json:"patchKey"`
	PatchDigest                 string                  `json:"patchDigest"`
	PatchSizeBytes              int64                   `json:"patchSizeBytes"`
	MaxPatchBytes               int64                   `json:"maxPatchBytes"`
	CloneTokenFile              string                  `json:"cloneTokenFile"`
	ArtifactAccessKeyIDFile     string                  `json:"artifactAccessKeyIDFile"`
	ArtifactSecretAccessKeyFile string                  `json:"artifactSecretAccessKeyFile"`
	ArtifactSessionTokenFile    string                  `json:"artifactSessionTokenFile,omitempty"`
	ArtifactStore               verifyfetch.StoreConfig `json:"artifactStore"`
	DisableHooks                bool                    `json:"disableHooks"`
	RequireExactBase            bool                    `json:"requireExactBase"`
}

type applySpec struct {
	Version        int    `json:"version"`
	RepoPath       string `json:"repoPath"`
	BasePath       string `json:"basePath"`
	PatchPath      string `json:"patchPath"`
	BaseSHA        string `json:"baseSHA"`
	PatchDigest    string `json:"patchDigest"`
	PatchSizeBytes int64  `json:"patchSizeBytes"`
	MaxPatchBytes  int64  `json:"maxPatchBytes"`
}

type commandSpec struct {
	Version      int                      `json:"version"`
	Commands     []v1alpha1.VerifyCommand `json:"commands"`
	Requirements *requirementsSpec        `json:"requirements,omitempty"`
}

type requirementsSpec struct {
	Version         int                     `json:"version"`
	Adapter         v1alpha1.GateAdapter    `json:"adapter,omitempty"`
	TestStrength    v1alpha1.TestStrength   `json:"testStrength"`
	BaseTestCommand *v1alpha1.VerifyCommand `json:"baseTestCommand,omitempty"`
	CoverageDelta   string                  `json:"coverageDelta,omitempty"`
}

type verifyDigestInput struct {
	Version                     int                  `json:"version"`
	ResolvedSpecDigest          string               `json:"resolvedSpecDigest"`
	Patch                       v1alpha1.ArtifactRef `json:"patch"`
	BaseSHA                     string               `json:"baseSHA"`
	FetchImage                  string               `json:"fetchImage"`
	ApplyImage                  string               `json:"applyImage"`
	LockdownImage               string               `json:"lockdownImage"`
	SecretName                  string               `json:"secretName"`
	CloneSecretKey              string               `json:"cloneSecretKey"`
	ArtifactStoreEndpoint       string               `json:"artifactStoreEndpoint,omitempty"`
	ArtifactStoreRegion         string               `json:"artifactStoreRegion"`
	ArtifactStoreBucket         string               `json:"artifactStoreBucket"`
	ArtifactStoreForcePathStyle bool                 `json:"artifactStoreForcePathStyle"`
	MaxPatchBytes               int64                `json:"maxPatchBytes"`
	PolicyChecksDigest          string               `json:"policyChecksDigest"`
	ShutdownTime                string               `json:"shutdownTime"`
	MaxShutdown                 string               `json:"maxShutdown"`
}

func validateSnapshot(snapshot resolved.Snapshot, options Options) error {
	if snapshot.SchemaVersion != resolved.SchemaVersion {
		return fmt.Errorf("%w: unsupported resolved snapshot schema version %d", ErrInvalidInput, snapshot.SchemaVersion)
	}
	if snapshot.Run.Namespace == "" || len(validation.IsDNS1123Label(snapshot.Run.Namespace)) != 0 {
		return fmt.Errorf("%w: invalid run namespace", ErrInvalidInput)
	}
	if snapshot.Run.Name == "" || len(validation.IsDNS1123Subdomain(snapshot.Run.Name)) != 0 {
		return fmt.Errorf("%w: invalid run name", ErrInvalidInput)
	}
	if snapshot.Run.UID == "" || len(snapshot.Run.UID) > maxRunUIDBytes || len(validation.IsValidLabelValue(snapshot.Run.UID)) != 0 {
		return fmt.Errorf("%w: run UID is required and bounded", ErrInvalidInput)
	}
	if err := workload.ValidateRuntimeClassName(snapshot.Agent.Runtime.RuntimeClassName, options.AllowedRuntimeClasses); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := workload.ValidateStorageClassName(snapshot.Spec.Workspace.StorageClassName, options.AllowedStorageClasses); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if !githubRepoPattern.MatchString(snapshot.Spec.Source.Repo) || snapshot.Spec.Source.BaseRef == "" || strings.ContainsAny(snapshot.Spec.Source.BaseRef, "\x00\r\n") {
		return fmt.Errorf("%w: source must be a canonical GitHub repo and safe baseRef", ErrInvalidInput)
	}
	if !validPinnedImage(snapshot.Gate.Verify.Image) {
		return fmt.Errorf("%w: verifier image must be a non-placeholder digest-pinned image", ErrInvalidInput)
	}
	if !snapshot.Gate.Verify.FromCleanCheckout {
		return fmt.Errorf("%w: verify Gate must require a clean checkout", ErrInvalidInput)
	}
	verifyTimeout, err := time.ParseDuration(snapshot.Gate.Verify.Timeout)
	if err != nil || verifyTimeout <= 0 {
		return fmt.Errorf("%w: verify timeout must be a positive duration", ErrInvalidInput)
	}
	if options.MaxShutdownDuration > 0 && verifyTimeout > options.MaxShutdownDuration {
		return fmt.Errorf("%w: verify timeout exceeds shutdown bound", ErrInvalidInput)
	}
	if options.ArtifactCredentialTTL <= 0 {
		return fmt.Errorf("%w: verify credential lease TTL is required", ErrInvalidInput)
	}
	if verifyTimeout > options.ArtifactCredentialTTL {
		return fmt.Errorf("%w: verify timeout exceeds credential lease TTL", ErrInvalidInput)
	}
	if len(snapshot.Gate.Verify.Commands) == 0 || len(snapshot.Gate.Verify.Commands) > 32 {
		return fmt.Errorf("%w: verify commands must contain 1..32 commands", ErrInvalidInput)
	}
	if len(snapshot.Policies) > 0 && len(options.PolicyChecks) == 0 {
		return fmt.Errorf("%w: policy contract is required when the resolved run references policies", ErrInvalidInput)
	}
	return nil
}

func validatePatch(patch v1alpha1.ArtifactRef) error {
	if patch.URI == "" || len(patch.URI) > maxPatchURIBytes || !canonical.ValidDigest(patch.Digest) {
		return fmt.Errorf("%w: patch URI and sha256 digest are required", ErrInvalidInput)
	}
	if _, err := verifyfetch.ParseS3URI(patch.URI); err != nil {
		return fmt.Errorf("%w: patch URI must be an exact s3 object location", ErrInvalidInput)
	}
	if patch.SizeBytes < 0 || patch.SizeBytes > maxPatchBytes {
		return fmt.Errorf("%w: patch size exceeds bound", ErrInvalidInput)
	}
	if len(patch.Kind) > 64 || strings.ContainsAny(patch.Kind, "\x00\r\n") || len(patch.Name) > 128 || strings.ContainsAny(patch.Name, "\x00\r\n") || len(patch.MediaType) > 128 || strings.ContainsAny(patch.MediaType, "\x00\r\n") {
		return fmt.Errorf("%w: patch metadata is invalid", ErrInvalidInput)
	}
	return nil
}

func validateBaseSHA(baseSHA string) error {
	if (len(baseSHA) != 40 && len(baseSHA) != 64) || !shaPattern.MatchString(baseSHA) {
		return fmt.Errorf("%w: baseSHA must be a lowercase 40- or 64-character commit digest", ErrInvalidInput)
	}
	return nil
}

func validateOptions(snapshot resolved.Snapshot, options Options) error {
	for name, image := range map[string]string{"fetch": options.FetchImage, "apply": options.ApplyImage, "lockdown": options.LockdownImage} {
		if !validPinnedImage(image) {
			return fmt.Errorf("%w: %s helper image must be digest-pinned and non-placeholder", ErrInvalidInput, name)
		}
	}
	if options.SecretName == "" || len(validation.IsDNS1123Subdomain(options.SecretName)) != 0 || options.SecretName != workload.VerifySecretName(snapshot.Run.UID) {
		return fmt.Errorf("%w: SecretName must equal the deterministic verify Secret name", ErrInvalidInput)
	}
	if err := validateSecretKey(options.CloneSecretKey, "clone"); err != nil {
		return err
	}
	if options.Now.IsZero() || options.ShutdownTime.IsZero() {
		return fmt.Errorf("%w: Now and ShutdownTime are required", ErrInvalidInput)
	}
	if options.MaxShutdownDuration <= 0 || options.MaxShutdownDuration > sandbox.DefaultMaxShutdownDuration {
		return fmt.Errorf("%w: MaxShutdownDuration must be in (0,%s]", ErrInvalidInput, sandbox.DefaultMaxShutdownDuration)
	}
	now := options.Now.UTC()
	shutdown := options.ShutdownTime.UTC()
	if !shutdown.After(now) || shutdown.After(now.Add(options.MaxShutdownDuration)) {
		return fmt.Errorf("%w: ShutdownTime must be future and within the configured bound", ErrInvalidInput)
	}
	return nil
}

func normalizeOptions(options *Options, snapshot resolved.Snapshot, patch v1alpha1.ArtifactRef) error {
	if options == nil {
		return fmt.Errorf("%w: options are required", ErrInvalidInput)
	}
	if err := validateOptions(snapshot, *options); err != nil {
		return err
	}
	if err := policycontract.ValidateGateChecks(options.PolicyChecks); err != nil {
		return fmt.Errorf("%w: policy check projection: %v", ErrInvalidInput, err)
	}
	location, err := verifyfetch.ParseS3URI(patch.URI)
	if err != nil {
		return fmt.Errorf("%w: parse patch location", ErrInvalidInput)
	}
	if options.ArtifactStoreBucket == "" {
		options.ArtifactStoreBucket = location.Bucket
	}
	if options.ArtifactStoreBucket != location.Bucket {
		return fmt.Errorf("%w: patch bucket does not match configured artifact bucket", ErrInvalidInput)
	}
	if options.ArtifactStoreRegion == "" {
		options.ArtifactStoreRegion = "us-east-1"
	}
	if options.MaxPatchBytes == 0 {
		options.MaxPatchBytes = objectstore.GeneralMaxObjectBytes
	}
	if options.MaxPatchBytes <= 0 || options.MaxPatchBytes > maxPatchBytes || patch.SizeBytes > options.MaxPatchBytes {
		return fmt.Errorf("%w: patch byte bound is invalid or smaller than the patch", ErrInvalidInput)
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = defaultMaxOutputBytes
	}
	if options.MaxOutputBytes <= 0 || options.MaxOutputBytes > hardMaxOutputBytes {
		return fmt.Errorf("%w: verifier output byte bound is invalid", ErrInvalidInput)
	}
	if err := verifyfetch.ValidateStoreConfig(verifyfetch.StoreConfig{
		Endpoint: options.ArtifactStoreEndpoint, Region: options.ArtifactStoreRegion,
		Bucket: options.ArtifactStoreBucket, ForcePathStyle: options.ArtifactStoreForcePathStyle,
		MaxObjectBytes: options.MaxPatchBytes,
	}); err != nil {
		return fmt.Errorf("%w: artifact store configuration: %v", ErrInvalidInput, err)
	}
	return nil
}

func encodePolicyChecks(checks []policycontract.GateCheckDescriptor) ([]byte, error) {
	if err := policycontract.ValidateGateChecks(checks); err != nil {
		return nil, fmt.Errorf("%w: policy check projection: %v", ErrInvalidInput, err)
	}
	if checks == nil {
		checks = []policycontract.GateCheckDescriptor{}
	}
	body, err := json.Marshal(checks)
	if err != nil {
		return nil, fmt.Errorf("%w: encode policy checks: %v", ErrInvalidInput, err)
	}
	if len(body) > maxPolicyChecksBytes {
		return nil, fmt.Errorf("%w: policy check projection exceeds %d bytes", ErrInvalidInput, maxPolicyChecksBytes)
	}
	return body, nil
}

func validateSecretKey(key, scope string) error {
	if key == "" || len(key) > 253 || len(validation.IsConfigMapKey(key)) != 0 {
		return fmt.Errorf("%w: %s Secret key is invalid", ErrInvalidInput, scope)
	}
	return nil
}

func validPinnedImage(value string) bool {
	if len(value) == 0 || len(value) > 512 || !resolved.ValidPinnedImage(value) {
		return false
	}
	parts := strings.Split(value, "@sha256:")
	return len(parts) == 2 && imageNamePattern.MatchString(parts[0]) && canonical.ValidDigest("sha256:"+parts[1])
}

func encodeVerifyCommands(commands []v1alpha1.VerifyCommand, requirements v1alpha1.GateRequirements, maxBytes int) ([]byte, error) {
	limit, err := commandLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	for index, command := range commands {
		if (len(command.Argv) == 0) == (command.Shell == nil || strings.TrimSpace(*command.Shell) == "") {
			return nil, fmt.Errorf("%w: verify command %d must select exactly one execution form", ErrInvalidInput, index)
		}
		if len(command.Argv) > 64 {
			return nil, fmt.Errorf("%w: verify command %d argv is too long", ErrInvalidInput, index)
		}
		for argIndex, arg := range command.Argv {
			if len(arg) > 4096 || strings.ContainsRune(arg, '\x00') || (argIndex == 0 && strings.TrimSpace(arg) == "") {
				return nil, fmt.Errorf("%w: verify command %d argv[%d] is invalid", ErrInvalidInput, index, argIndex)
			}
		}
		if command.Shell != nil && (len(*command.Shell) > 4096 || strings.ContainsRune(*command.Shell, '\x00')) {
			return nil, fmt.Errorf("%w: verify command %d shell is invalid", ErrInvalidInput, index)
		}
	}
	if requirements.Adapter != "" && requirements.Adapter != v1alpha1.GateAdapterGo {
		return nil, fmt.Errorf("%w: unsupported verifier adapter %q", ErrInvalidInput, requirements.Adapter)
	}
	if requirements.TestStrength == v1alpha1.TestStrengthNewTestsFailOnBase {
		if requirements.Adapter != v1alpha1.GateAdapterGo || requirements.BaseTestCommand == nil {
			return nil, fmt.Errorf("%w: newTestsMustFailOnBase requires the go adapter and one explicit baseTestCommand", ErrInvalidInput)
		}
		if err := validateGoTestCommand(*requirements.BaseTestCommand); err != nil {
			return nil, fmt.Errorf("%w: baseTestCommand: %v", ErrInvalidInput, err)
		}
	} else if requirements.BaseTestCommand != nil {
		return nil, fmt.Errorf("%w: baseTestCommand requires newTestsMustFailOnBase", ErrInvalidInput)
	}
	if requirements.CoverageDelta != "" && requirements.Adapter != v1alpha1.GateAdapterGo {
		return nil, fmt.Errorf("%w: coverageDelta requires the go verifier adapter", ErrInvalidInput)
	}

	contract := commandSpec{Version: 1, Commands: commands}
	if requirements.TestStrength != v1alpha1.TestStrengthNone || requirements.CoverageDelta != "" || requirements.Adapter != "" {
		contract.Requirements = &requirementsSpec{
			Version:         2,
			Adapter:         requirements.Adapter,
			TestStrength:    requirements.TestStrength,
			BaseTestCommand: deepCopyVerifyCommand(requirements.BaseTestCommand),
			CoverageDelta:   requirements.CoverageDelta,
		}
	}
	body, err := json.Marshal(contract)
	if err != nil {
		return nil, fmt.Errorf("%w: encode verify commands: %v", ErrInvalidInput, err)
	}
	if len(body) == 0 || len(body) > limit {
		return nil, fmt.Errorf("%w: verify command JSON exceeds %d bytes", ErrInvalidInput, limit)
	}
	return body, nil
}

func validateGoTestCommand(command v1alpha1.VerifyCommand) error {
	if len(command.Argv) < 2 || command.Argv[0] != "go" || command.Argv[1] != "test" || command.Shell != nil {
		return errors.New("must be an argv command beginning with go test")
	}
	return nil
}

func deepCopyVerifyCommand(command *v1alpha1.VerifyCommand) *v1alpha1.VerifyCommand {
	if command == nil {
		return nil
	}
	copy := &v1alpha1.VerifyCommand{Argv: append([]string(nil), command.Argv...)}
	if command.Shell != nil {
		value := *command.Shell
		copy.Shell = &value
	}
	return copy
}

func commandLimit(value int) (int, error) {
	if value == 0 {
		return defaultMaxCommandBytes, nil
	}
	if value < 1 || value > hardMaxCommandBytes {
		return 0, fmt.Errorf("%w: MaxCommandBytes must be in 1..%d or zero", ErrInvalidInput, hardMaxCommandBytes)
	}
	return value, nil
}

func encodeFetchSpec(snapshot resolved.Snapshot, patch v1alpha1.ArtifactRef, baseSHA string, options Options) ([]byte, error) {
	location, err := verifyfetch.ParseS3URI(patch.URI)
	if err != nil || location.Bucket != options.ArtifactStoreBucket {
		return nil, fmt.Errorf("%w: patch location is not bound to the configured artifact bucket", ErrInvalidInput)
	}
	spec := fetchSpec{
		Version: 1, Repository: snapshot.Spec.Source.Repo, BaseSHA: baseSHA, Depth: int(snapshot.Spec.Source.Depth),
		RepoPath: RepoPath, BasePath: BasePath, PatchPath: PatchPath,
		PatchBucket: location.Bucket, PatchKey: location.Key, PatchDigest: patch.Digest, PatchSizeBytes: patch.SizeBytes,
		MaxPatchBytes: options.MaxPatchBytes, CloneTokenFile: CloneTokenFile,
		ArtifactAccessKeyIDFile:     ArtifactAccessKeyIDFile,
		ArtifactSecretAccessKeyFile: ArtifactSecretAccessKeyFile,
		ArtifactSessionTokenFile:    ArtifactSessionTokenFile,
		ArtifactStore: verifyfetch.StoreConfig{
			Endpoint: options.ArtifactStoreEndpoint, Region: options.ArtifactStoreRegion,
			Bucket: options.ArtifactStoreBucket, ForcePathStyle: options.ArtifactStoreForcePathStyle,
			MaxObjectBytes: options.MaxPatchBytes,
		},
		DisableHooks: true, RequireExactBase: true,
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: encode fetch contract: %v", ErrInvalidInput, err)
	}
	if len(body) > 16<<10 {
		return nil, fmt.Errorf("%w: fetch contract exceeds bound", ErrInvalidInput)
	}
	return body, nil
}

func encodeApplySpecFor(patch v1alpha1.ArtifactRef, baseSHA string, maxPatch int64) ([]byte, error) {
	if err := validateBaseSHA(baseSHA); err != nil || !canonical.ValidDigest(patch.Digest) || patch.SizeBytes < 0 || maxPatch <= 0 || patch.SizeBytes > maxPatch {
		return nil, fmt.Errorf("%w: apply identity is invalid", ErrInvalidInput)
	}
	spec := applySpec{
		Version: 1, RepoPath: RepoPath, BasePath: BasePath, PatchPath: PatchPath,
		BaseSHA: baseSHA, PatchDigest: patch.Digest, PatchSizeBytes: patch.SizeBytes, MaxPatchBytes: maxPatch,
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: encode apply contract: %v", ErrInvalidInput, err)
	}
	return body, nil
}

func verifyInputDigest(specDigest string, patch v1alpha1.ArtifactRef, baseSHA string, options Options) (string, error) {
	policyChecks, err := encodePolicyChecks(options.PolicyChecks)
	if err != nil {
		return "", err
	}
	policySum := sha256.Sum256(policyChecks)
	contract := verifyDigestInput{
		Version: 1, ResolvedSpecDigest: specDigest, Patch: patch, BaseSHA: baseSHA,
		FetchImage: options.FetchImage, ApplyImage: options.ApplyImage, LockdownImage: options.LockdownImage,
		SecretName: options.SecretName, CloneSecretKey: options.CloneSecretKey,
		ArtifactStoreEndpoint: options.ArtifactStoreEndpoint, ArtifactStoreRegion: options.ArtifactStoreRegion,
		ArtifactStoreBucket: options.ArtifactStoreBucket, ArtifactStoreForcePathStyle: options.ArtifactStoreForcePathStyle,
		MaxPatchBytes:      options.MaxPatchBytes,
		PolicyChecksDigest: canonical.DigestPrefix + hex.EncodeToString(policySum[:]),
		ShutdownTime:       options.ShutdownTime.UTC().Format(time.RFC3339Nano), MaxShutdown: options.MaxShutdownDuration.String(),
	}
	body, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("%w: encode verify identity: %v", ErrInvalidInput, err)
	}
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func fetchSecretVolumes(options Options) ([]corev1.Volume, error) {
	mode := int32(0444)
	items := []corev1.KeyToPath{{Key: options.CloneSecretKey, Path: "clone-token", Mode: &mode}}
	artifactItems := []corev1.KeyToPath{
		{Key: ArtifactAccessKeyIDSecretKey, Path: "artifact-access-key-id", Mode: &mode},
		{Key: ArtifactSecretAccessKeySecretKey, Path: "artifact-secret-access-key", Mode: &mode},
	}
	return []corev1.Volume{{
		Name: FetchSecretVolumeName,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: options.SecretName}, Items: items}},
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: options.SecretName}, Items: artifactItems}},
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: options.SecretName}, Items: []corev1.KeyToPath{{Key: ArtifactSessionTokenSecretKey, Path: "artifact-session-token", Mode: &mode}}}},
			},
			DefaultMode: &mode,
		}},
	}}, nil
}

func ownerObject(snapshot resolved.Snapshot) *v1alpha1.AgentRun {
	return &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Namespace: snapshot.Run.Namespace, Name: snapshot.Run.Name, UID: types.UID(snapshot.Run.UID), Generation: snapshot.Run.Generation,
	}}
}

func regularSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: int64Ptr(uid), RunAsGroup: int64Ptr(uid), RunAsNonRoot: boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func fetchSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		// Root is namespaced by PodSpec.hostUsers=false. It is needed only so
		// the trusted fetch step can leave /verify/workspace/base owned by
		// namespace root and mode 0555 before the untrusted verifier starts.
		RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0), RunAsNonRoot: boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
		// The fetch binary keeps the clone writable by UID 1000 through
		// explicit mode bits; it does not need a capability to change owner.
		// The pristine base remains namespace-root-owned and read-only.
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func lockdownSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0), RunAsNonRoot: boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
		Capabilities:   &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}, Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func resources(requestCPU, requestMemory, limitCPU, limitMemory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resourceMustParse(requestCPU), corev1.ResourceMemory: resourceMustParse(requestMemory)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resourceMustParse(limitCPU), corev1.ResourceMemory: resourceMustParse(limitMemory)},
	}
}

func resourceMustParse(value string) resource.Quantity {
	return resource.MustParse(value)
}

func copyResources(input corev1.ResourceRequirements) corev1.ResourceRequirements {
	return *input.DeepCopy()
}

func volumeMount(name, path string, readOnly bool) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: readOnly}
}

func directSecretItemMount(name, mountPath, subPath string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: mountPath, SubPath: subPath, ReadOnly: true}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func copyStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func digestShort(value string) string {
	value = strings.TrimPrefix(value, canonical.DigestPrefix)
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func boolPtr(value bool) *bool { return &value }

func int64Ptr(value int64) *int64 { return &value }
