// Package workload builds the Phase 1 work Sandbox.
//
// The builder is deliberately pure: it accepts an immutable resolved snapshot
// and explicit, already-resolved image/secret/lifecycle inputs and returns a
// new upstream agents.x-k8s.io/v1beta1 Sandbox. It does not read the
// filesystem, Kubernetes, environment variables, or the clock.
package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/airlock"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const (
	workRole = "work"

	NodeLabelKey   = "agw.astatide.com/agents"
	NodeLabelValue = "true"

	WorkspaceVolumeName      = "workspace"
	SkillsVolumeName         = "skills"
	ContextVolumeName        = "context-pack"
	CloneSecretVolumeName    = "clone-secret"
	SkillsSecretVolumeName   = "skills-secret"
	BrokerSecretVolumeName   = "broker-secret"
	ArtifactSecretVolumeName = "artifact-secret"

	// TrustedPhaseSupervisorAnnotationKey records the deployment contract for
	// the work Sandbox. It is intentionally set to disabled until a separate
	// supervisor can both observe process state and present the broker's
	// independently configured phase capability. This marker prevents a
	// rendered workload from being mistaken for a phase-supervised one.
	TrustedPhaseSupervisorAnnotationKey = "agents.astatide.com/trusted-phase-supervisor"
	TrustedPhaseSupervisorDisabled      = "disabled"

	CloneSecretMountPath    = "/run/agw/clone"
	SkillsSecretMountPath   = "/run/agw/skills"
	BrokerSecretMountPath   = "/run/agw/credentials"
	ArtifactSecretMountPath = "/run/agw/object-store"
	SkillsMountPath         = "/opt/agw/skills"
	ContextMountPath        = "/opt/agw/context"
	ContextBrokerMountPath  = "/opt/agw/context-pack"
	WorkspaceMountPath      = "/workspace"
	WorkspaceRepoSubPath    = "repo"
	WorkspaceBaseSubPath    = "base"

	CloneTokenFile              = CloneSecretMountPath + "/token"
	SkillsTokenFile             = SkillsSecretMountPath + "/token"
	ArtifactAccessKeyIDFile     = ArtifactSecretMountPath + "/access-key-id"
	ArtifactSecretAccessKeyFile = ArtifactSecretMountPath + "/secret-access-key"
	ArtifactSessionTokenFile    = ArtifactSecretMountPath + "/session-token"

	cloneEntrypoint   = "/agw/clone"
	skillsEntrypoint  = "/agw/skills"
	agentEntrypoint   = "/agw/agent"
	brokerEntrypoint  = "/agw/broker"
	lockdownShell     = "sh"
	lockdownShellArgs = "-ceu"

	// The lockdown image contract is intentionally constant. User-controlled
	// source/task/skill values are passed as environment data to trusted
	// binaries; they are never interpolated into this script or a shell argv.
	// The direct policy permits only the agent's TCP hop to 127.0.0.1/::1:8081.
	// UID 1337 intentionally retains broad provider egress in this mode.
	lockdownScript = airlock.DirectLockdownScript

	maxSecretKeyLength = 253
	maxRunUIDLength    = 63
)

var (
	ErrInvalidInput = errors.New("invalid work Sandbox input")

	// Resource profiles are fixed in Phase 1 so identical inputs produce an
	// identical manifest and scheduling cannot be influenced by run text.
	cloneResources    = resources("10m", "32Mi", "100m", "128Mi")
	skillsResources   = resources("10m", "32Mi", "100m", "128Mi")
	lockdownResources = resources("10m", "16Mi", "100m", "64Mi")
	contextResources  = resources("25m", "64Mi", "250m", "256Mi")
	agentResources    = resources("100m", "256Mi", "1", "1Gi")
	brokerResources   = resources("50m", "128Mi", "500m", "512Mi")
)

// Options contains the controller-resolved inputs that are not part of the
// AgentRun snapshot. Every credential projection is explicit and narrow.
//
// The four image fields are trusted image contracts and must be immutable OCI
// references. SecretName must equal WorkSecretName(snapshot.Run.UID); callers
// cannot select an arbitrary Secret by accident. Now and ShutdownTime are
// explicit to keep Build deterministic and to make the lifecycle bound
// testable without a clock.
type Options struct {
	CloneImage     string
	SkillsImage    string
	ContextImage   string
	SkillsEndpoint string
	LockdownImage  string
	BrokerImage    string

	// AgentGateway is deliberately disabled by its zero value. Build validates
	// the complete future sidecar envelope but currently rejects enablement
	// until the work broker has a proven guard-to-agentgateway adapter.
	AgentGateway AgentGatewaySidecarOptions

	SecretName       string
	CloneSecretKey   string
	SkillsSecretKey  string
	BrokerSecretKeys []string

	ArtifactStoreEndpoint            string
	ArtifactStoreRegion              string
	ArtifactStoreBucket              string
	ArtifactStorePrefix              string
	ArtifactStoreForcePathStyle      bool
	ArtifactStoreMaxObjectBytes      int64
	ArtifactAccessKeyIDSecretKey     string
	ArtifactSecretAccessKeySecretKey string
	ArtifactSessionTokenSecretKey    string

	// Placement allowlists are operator-owned. An empty allowlist means the
	// corresponding user-controlled field must remain empty; callers cannot
	// select an arbitrary RuntimeClass or StorageClass by name.
	AllowedRuntimeClasses []string
	AllowedStorageClasses []string

	Now                 time.Time
	ShutdownTime        time.Time
	MaxShutdownDuration time.Duration
}

// Build constructs one deterministic work Sandbox from a resolved snapshot.
// The returned object is safe to pass to sandbox.AgentSandboxBackend.Ensure.
func Build(snapshot resolved.Snapshot, options Options) (*sandboxv1beta1.Sandbox, error) {
	if err := validateSnapshot(snapshot, options); err != nil {
		return nil, err
	}
	if err := validateOptions(snapshot, options); err != nil {
		return nil, err
	}

	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve spec digest: %v", ErrInvalidInput, err)
	}
	contextContract, err := contextmaterializer.FromSnapshot(snapshot, specDigest)
	if err != nil {
		return nil, fmt.Errorf("%w: build context contract: %v", ErrInvalidInput, err)
	}
	contextInputJSON, err := contextmaterializer.Encode(contextContract)
	if err != nil {
		return nil, fmt.Errorf("%w: encode context contract: %v", ErrInvalidInput, err)
	}
	childName, err := sandbox.ChildName(types.UID(snapshot.Run.UID), sandbox.RoleWork)
	if err != nil {
		return nil, fmt.Errorf("%w: child name: %v", ErrInvalidInput, err)
	}
	depth := snapshot.Spec.Source.Depth
	if depth == 0 {
		depth = 1
	}

	skillsJSON, err := json.Marshal(snapshot.Agent.Skills)
	if err != nil {
		return nil, fmt.Errorf("%w: encode skills: %v", ErrInvalidInput, err)
	}
	scopeJSON, err := json.Marshal(snapshot.Spec.Scope)
	if err != nil {
		return nil, fmt.Errorf("%w: encode scope: %v", ErrInvalidInput, err)
	}
	toolSetJSON, err := json.Marshal(snapshot.ToolSet)
	if err != nil {
		return nil, fmt.Errorf("%w: encode ToolSet: %v", ErrInvalidInput, err)
	}
	modelRouteJSON, err := json.Marshal(snapshot.ModelRoute)
	if err != nil {
		return nil, fmt.Errorf("%w: encode ModelRoute: %v", ErrInvalidInput, err)
	}
	pricingJSON, err := modelPricingJSON(snapshot.ModelRoute)
	if err != nil {
		return nil, fmt.Errorf("%w: encode model pricing: %v", ErrInvalidInput, err)
	}
	primary, err := primaryProvider(snapshot.ModelRoute)
	if err != nil {
		return nil, err
	}
	model := primary.Model
	brokerSecretFiles, err := brokerSecretFileMap(options.BrokerSecretKeys)
	if err != nil {
		return nil, err
	}

	workspaceSize, err := resource.ParseQuantity(snapshot.Spec.Workspace.Size)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace size: %v", ErrInvalidInput, err)
	}
	workspaceStorageClass := optionalString(snapshot.Spec.Workspace.StorageClassName)

	labels := map[string]string{
		sandbox.RunUIDLabelKey:       string(snapshot.Run.UID),
		sandbox.RoleLabelKey:         workRole,
		"agents.astatide.com/egress": "broker-public",
		// A Kubernetes label value is limited to 63 characters. Keep a
		// bounded digest label and the complete digest in an annotation.
		"agents.astatide.com/spec-digest-short": digestShort(specDigest),
	}
	annotations := map[string]string{
		sandbox.SpecDigestAnnotationKey:     specDigest,
		TrustedPhaseSupervisorAnnotationKey: TrustedPhaseSupervisorDisabled,
	}
	owner := metav1.OwnerReference{
		APIVersion:         "agents.astatide.com/v1alpha1",
		Kind:               "AgentRun",
		Name:               snapshot.Run.Name,
		UID:                types.UID(snapshot.Run.UID),
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}

	secretVolumes := []corev1.Volume{
		secretVolume(CloneSecretVolumeName, options.SecretName, options.CloneSecretKey, "token"),
	}
	if options.SkillsSecretKey != "" {
		secretVolumes = append(secretVolumes, secretVolume(SkillsSecretVolumeName, options.SecretName, options.SkillsSecretKey, "token"))
	}
	if len(options.BrokerSecretKeys) > 0 {
		secretVolumes = append(secretVolumes, brokerSecretVolume(options.SecretName, options.BrokerSecretKeys))
	}
	if options.ArtifactAccessKeyIDSecretKey != "" {
		secretVolumes = append(secretVolumes, artifactSecretVolume(options))
	}

	clone := corev1.Container{
		Name:            "clone",
		Image:           options.CloneImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{cloneEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_REPO", Value: snapshot.Spec.Source.Repo},
			{Name: "AGW_BASE_REF", Value: snapshot.Spec.Source.BaseRef},
			{Name: "AGW_BASE_SHA", Value: snapshot.BaseSHA},
			{Name: "AGW_DEPTH", Value: strconv.FormatInt(int64(depth), 10)},
			{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
			{Name: "AGW_BASE_PATH", Value: WorkspaceMountPath + "/base"},
			{Name: "AGW_GIT_TOKEN_FILE", Value: CloneTokenFile},
			{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
		},
		WorkingDir: WorkspaceMountPath,
		Resources:  copyResources(cloneResources),
		// Clone runs as namespace-root so it can leave /workspace/base owned by
		// UID 0 and mode 0555 while handing /workspace/repo to UID 1000. With
		// hostUsers:false this UID has no host-root authority.
		SecurityContext: cloneSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			workspaceMount(false),
			// Secret directory projections are Kubernetes atomic-writer symlinks.
			// The clone helper rejects symlink credentials, so bind the selected
			// immutable item directly over the regular-file image placeholder.
			directSecretItemMount(CloneSecretVolumeName, CloneTokenFile, "token"),
		},
	}

	skills := corev1.Container{
		Name:            "skills",
		Image:           options.SkillsImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{skillsEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_SKILLS_JSON", Value: string(skillsJSON)},
			{Name: "AGW_SKILLS_ENDPOINT", Value: options.SkillsEndpoint},
			{Name: "AGW_SKILLS_DIR", Value: SkillsMountPath},
			{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
		},
		Resources:       copyResources(skillsResources),
		SecurityContext: regularSecurityContext(1000),
		VolumeMounts:    []corev1.VolumeMount{volumeMount(SkillsVolumeName, SkillsMountPath, false)},
	}
	if options.SkillsSecretKey != "" {
		skills.Env = append(skills.Env, corev1.EnvVar{Name: "AGW_SKILLS_TOKEN_FILE", Value: SkillsTokenFile})
		skills.VolumeMounts = append(skills.VolumeMounts, directSecretItemMount(SkillsSecretVolumeName, SkillsTokenFile, "token"))
	}

	contextInit := corev1.Container{
		Name:            "context",
		Image:           options.ContextImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/agw/context"},
		Env: []corev1.EnvVar{
			{Name: "AGW_CONTEXT_INPUT_JSON", Value: string(contextInputJSON)},
			{Name: "AGW_CONTEXT_EXPECTED_SPEC_DIGEST", Value: specDigest},
			{Name: "AGW_CONTEXT_EXPECTED_BASE_SHA", Value: snapshot.BaseSHA},
			{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
			{Name: "AGW_CONTEXT_BASE_DIR", Value: WorkspaceMountPath + "/base"},
			{Name: "AGW_CONTEXT_SKILLS_DIR", Value: SkillsMountPath},
			{Name: "AGW_CONTEXT_OUTPUT_DIR", Value: ContextMountPath},
		},
		WorkingDir:      ContextMountPath,
		Resources:       copyResources(contextResources),
		SecurityContext: namespaceRootSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			workspaceSubPathMount(true, WorkspaceBaseSubPath),
			volumeMount(SkillsVolumeName, SkillsMountPath, true),
			volumeMount(ContextVolumeName, ContextMountPath, false),
		},
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

	agentEnv := []corev1.EnvVar{
		podNameEnv(),
		{Name: "AGW_HARNESS", Value: string(snapshot.Agent.Runtime.Harness)},
		{Name: "AGW_AGENT_REF", Value: snapshot.Spec.AgentRef},
		{Name: "AGW_TASK", Value: snapshot.Task},
		{Name: "AGW_INSTRUCTIONS", Value: snapshot.Instructions},
		{Name: "AGW_REPO", Value: snapshot.Spec.Source.Repo},
		{Name: "AGW_BASE_REF", Value: snapshot.Spec.Source.BaseRef},
		{Name: "AGW_BASE_SHA", Value: snapshot.BaseSHA},
		{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
		{Name: "AGW_BASE_PATH", Value: WorkspaceMountPath + "/base"},
		{Name: "AGW_SKILLS_DIR", Value: SkillsMountPath},
		{Name: "AGW_CONTEXT_DIR", Value: WorkspaceMountPath + "/.agw/context"},
		{Name: "AGW_CONTEXT_PACK_MANIFEST_FILE", Value: WorkspaceMountPath + "/.agw/context/manifest.json"},
		{Name: "AGW_CONTEXT_PACK_REF_FILE", Value: WorkspaceMountPath + "/" + contextmaterializer.RefFileName},
		{Name: "AGW_SCOPE_JSON", Value: string(scopeJSON)},
		{Name: "AGW_BROKER", Value: "http://127.0.0.1:8081"},
		{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
		{Name: "AGW_SPEC_DIGEST", Value: specDigest},
	}
	agentEnv = append(agentEnv, harnessRuntimeEnv(snapshot.Agent.Runtime.Harness, primary, model, snapshot.Spec.Limits.Timeout)...)

	agent := corev1.Container{
		Name:            "agent",
		Image:           snapshot.Agent.Runtime.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{agentEntrypoint},
		Env:             agentEnv,
		WorkingDir:      WorkspaceMountPath + "/repo",
		Resources:       copyResources(agentResources),
		SecurityContext: regularSecurityContext(1000),
		VolumeMounts: []corev1.VolumeMount{
			// Keep the agent's writable worktree and the pristine comparison
			// tree as separate mounts. The pod fsGroup makes the worktree
			// writable by UID 1000, while the base mount remains read-only even
			// if a caller later changes the parent workspace permissions.
			workspaceSubPathMount(false, WorkspaceRepoSubPath),
			workspaceSubPathMount(true, WorkspaceBaseSubPath),
			volumeMount(SkillsVolumeName, SkillsMountPath, true),
			contextSubPathMount("AGENTS.md", WorkspaceMountPath+"/AGENTS.md"),
			contextSubPathMount(".agents", WorkspaceMountPath+"/.agents"),
			// Mount only the immutable context subtree. The harness still owns
			// writable siblings such as .agw/artifacts in the workspace PVC.
			contextSubPathMount(".agw/context", WorkspaceMountPath+"/.agw/context"),
			contextSubPathMount(contextmaterializer.RefFileName, WorkspaceMountPath+"/"+contextmaterializer.RefFileName),
		},
	}

	broker := corev1.Container{
		Name:            "broker",
		Image:           options.BrokerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{brokerEntrypoint},
		Env: []corev1.EnvVar{
			podNameEnv(),
			{Name: "AGW_LISTEN_ADDRESS", Value: "127.0.0.1:8081"},
			{Name: "AGW_CREDENTIALS_DIR", Value: BrokerSecretMountPath},
			{Name: "AGW_BROKER_SECRET_FILES", Value: string(brokerSecretFiles)},
			{Name: "AGW_TOOLSET_JSON", Value: string(toolSetJSON)},
			{Name: "AGW_MODEL_ROUTE_JSON", Value: string(modelRouteJSON)},
			{Name: "AGW_PRICING_JSON", Value: pricingJSON},
			{Name: "AGW_WORKSPACE", Value: WorkspaceMountPath},
			{Name: "AGW_CONTEXT_PACK_DIR", Value: ContextBrokerMountPath},
			{Name: "AGW_RUN_UID", Value: snapshot.Run.UID},
			{Name: "AGW_SPEC_DIGEST", Value: specDigest},
			{Name: "AGW_BASE_SHA", Value: snapshot.BaseSHA},
			{Name: "AGW_EFFECTS_PREFIX", Value: "runs/" + snapshot.Run.UID + "/effects"},
			{Name: "AGW_OBJECT_STORE_BUCKET", Value: options.ArtifactStoreBucket},
			{Name: "AGW_OBJECT_STORE_REGION", Value: options.ArtifactStoreRegion},
			{Name: "AGW_OBJECT_STORE_PREFIX", Value: options.ArtifactStorePrefix},
			{Name: "AGW_OBJECT_STORE_ENDPOINT", Value: options.ArtifactStoreEndpoint},
			{Name: "AGW_OBJECT_STORE_FORCE_PATH_STYLE", Value: strconv.FormatBool(options.ArtifactStoreForcePathStyle)},
			{Name: "AGW_OBJECT_STORE_MAX_BYTES", Value: strconv.FormatInt(options.ArtifactStoreMaxObjectBytes, 10)},
			{Name: "AGW_OBJECT_STORE_ACCESS_KEY_FILE", Value: ArtifactAccessKeyIDFile},
			{Name: "AGW_OBJECT_STORE_SECRET_KEY_FILE", Value: ArtifactSecretAccessKeyFile},
			{Name: "AGW_MAX_TOOL_CALLS", Value: strconv.FormatInt(int64(snapshot.Spec.Limits.MaxToolCalls), 10)},
			{Name: "AGW_MAX_COST_USD", Value: snapshot.Spec.Limits.MaxCostUSD},
			{Name: "AGW_MAX_MODEL_TOKENS", Value: strconv.FormatInt(snapshot.Spec.Limits.MaxModelTokens, 10)},
		},
		WorkingDir:      WorkspaceMountPath,
		Resources:       copyResources(brokerResources),
		SecurityContext: regularSecurityContext(1337),
		VolumeMounts: []corev1.VolumeMount{
			workspaceMount(true),
			// The broker reads the dedicated ContextPack volume directly. It never
			// reads the agent's workspace copy, which may contain agent-authored
			// files and is therefore not an evidence source.
			volumeMount(ContextVolumeName, ContextBrokerMountPath, true),
		},
	}
	if len(options.BrokerSecretKeys) > 0 {
		// Kubernetes Secret directory mounts use atomic-writer symlinks. The
		// broker deliberately opens credentials with O_NOFOLLOW, so bind each
		// immutable projected item directly at its fixed file path instead of
		// weakening the broker's file-integrity check.
		for index := range options.BrokerSecretKeys {
			item := "item-" + strconv.Itoa(index)
			broker.VolumeMounts = append(broker.VolumeMounts, corev1.VolumeMount{
				Name: BrokerSecretVolumeName, MountPath: BrokerSecretMountPath + "/" + item,
				SubPath: item, ReadOnly: true,
			})
		}
	}
	if options.ArtifactAccessKeyIDSecretKey != "" {
		broker.VolumeMounts = append(broker.VolumeMounts,
			directSecretItemMount(ArtifactSecretVolumeName, ArtifactAccessKeyIDFile, "access-key-id"),
			directSecretItemMount(ArtifactSecretVolumeName, ArtifactSecretAccessKeyFile, "secret-access-key"),
		)
		if options.ArtifactSessionTokenSecretKey != "" {
			broker.Env = append(broker.Env, corev1.EnvVar{Name: "AGW_OBJECT_STORE_SESSION_TOKEN_FILE", Value: ArtifactSessionTokenFile})
			broker.VolumeMounts = append(broker.VolumeMounts, directSecretItemMount(ArtifactSecretVolumeName, ArtifactSessionTokenFile, "session-token"))
		}
	}

	initContainers := []corev1.Container{clone}
	if len(snapshot.Agent.Skills) > 0 {
		initContainers = append(initContainers, skills)
	}
	initContainers = append(initContainers, contextInit)
	initContainers = append(initContainers, lockdown)

	shutdownTime := metav1.NewTime(options.ShutdownTime.UTC())
	shutdownPolicy := sandboxv1beta1.ShutdownPolicyDelete
	volumeMode := corev1.PersistentVolumeFilesystem
	return &sandboxv1beta1.Sandbox{
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
						// Keep the agent in a private PID namespace. A future
						// supervisor must not be enabled by merely flipping this
						// field: sharing PIDs would expose companion-process state
						// without supplying a trusted phase-authority contract.
						ShareProcessNamespace: boolPtr(false),
						HostNetwork:           false,
						HostPID:               false,
						HostIPC:               false,
						NodeSelector:          map[string]string{NodeLabelKey: NodeLabelValue},
						Tolerations: []corev1.Toleration{{
							Key: NodeLabelKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
						}},
						RuntimeClassName:              optionalString(snapshot.Agent.Runtime.RuntimeClassName),
						RestartPolicy:                 corev1.RestartPolicyNever,
						TerminationGracePeriodSeconds: int64Ptr(30),
						EnableServiceLinks:            boolPtr(false),
						SecurityContext: &corev1.PodSecurityContext{
							FSGroup:             int64Ptr(1000),
							FSGroupChangePolicy: fsGroupChangePolicyPtr(corev1.FSGroupChangeOnRootMismatch),
							SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						InitContainers: initContainers,
						Containers:     []corev1.Container{agent, broker},
						Volumes: append([]corev1.Volume{
							{Name: WorkspaceVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: WorkspaceVolumeName}}},
							{Name: SkillsVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
							{Name: ContextVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						}, secretVolumes...),
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{
						Name: WorkspaceVolumeName,
						Labels: map[string]string{
							sandbox.RunUIDLabelKey: string(snapshot.Run.UID),
							sandbox.RoleLabelKey:   workRole,
						},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: workspaceStorageClass,
						VolumeMode:       &volumeMode,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: workspaceSize},
							Limits:   corev1.ResourceList{corev1.ResourceStorage: workspaceSize},
						},
					},
				}},
				Service: boolPtr(false),
			},
			Lifecycle: sandboxv1beta1.Lifecycle{ShutdownTime: &shutdownTime, ShutdownPolicy: &shutdownPolicy},
		},
	}, nil
}

// WorkSecretName returns the deterministic immutable credential projection for
// the work Sandbox. The phase discriminator is part of the hash so a verify
// reconcile can never accidentally address the work Secret.
func WorkSecretName(runUID string) string {
	hash := sha256.Sum256([]byte("agw-work-secret\x00" + runUID))
	return "agw-work-secret-" + hex.EncodeToString(hash[:10])
}

// VerifySecretName returns the deterministic immutable credential projection
// for the independent verification Sandbox. It is deliberately distinct from
// WorkSecretName even though both are owned by the same AgentRun.
func VerifySecretName(runUID string) string {
	hash := sha256.Sum256([]byte("agw-verify-secret\x00" + runUID))
	return "agw-verify-secret-" + hex.EncodeToString(hash[:10])
}

// WorkspaceClaimName encapsulates agent-sandbox v0.5.x's documented PVC
// naming contract (<template>-<sandbox>). Keeping this in one function makes
// an upstream API change local to the workload backend integration.
func WorkspaceClaimName(workSandboxName string) (string, error) {
	if workSandboxName == "" || len(validation.IsDNS1123Subdomain(workSandboxName)) != 0 {
		return "", fmt.Errorf("%w: invalid work Sandbox name", ErrInvalidInput)
	}
	name := WorkspaceVolumeName + "-" + workSandboxName
	if len(name) > 253 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return "", fmt.Errorf("%w: derived workspace claim name is invalid", ErrInvalidInput)
	}
	return name, nil
}

const maxPlacementAllowlistEntries = 32

// NormalizeRuntimeClassAllowlist validates and canonicalizes the operator's
// exact RuntimeClass allowlist. RuntimeClass names are intentionally not
// wildcarded: a caller must select one of the names the deployment reviewed.
func NormalizeRuntimeClassAllowlist(values []string) ([]string, error) {
	return normalizePlacementAllowlist(values, "RuntimeClass", validation.IsDNS1123Subdomain)
}

// NormalizeStorageClassAllowlist validates and canonicalizes the operator's
// exact StorageClass allowlist. An empty list is meaningful: it means no
// caller-controlled StorageClass selection is permitted.
func NormalizeStorageClassAllowlist(values []string) ([]string, error) {
	return normalizePlacementAllowlist(values, "StorageClass", validation.IsDNS1123Subdomain)
}

func normalizePlacementAllowlist(values []string, kind string, validate func(string) []string) ([]string, error) {
	if len(values) > maxPlacementAllowlistEntries {
		return nil, fmt.Errorf("%s allowlist contains more than %d entries", kind, maxPlacementAllowlistEntries)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || len(validate(value)) != 0 {
			return nil, fmt.Errorf("invalid %s name %q", kind, value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate %s name %q", kind, value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

// ValidateRuntimeClassName applies the operator allowlist to the resolved
// user input. The empty value remains the safe default selected by the pod
// runtime; every non-empty value must be explicitly allowlisted.
func ValidateRuntimeClassName(value string, allowed []string) error {
	return validatePlacementName(value, allowed, NormalizeRuntimeClassAllowlist, "RuntimeClass")
}

// ValidateStorageClassName applies the operator allowlist to the resolved
// workspace request. The empty value lets the cluster's reviewed default
// StorageClass decide; named classes require an exact operator allowlist hit.
func ValidateStorageClassName(value string, allowed []string) error {
	return validatePlacementName(value, allowed, NormalizeStorageClassAllowlist, "StorageClass")
}

func validatePlacementName(value string, allowed []string, normalize func([]string) ([]string, error), kind string) error {
	allowedNames, err := normalize(allowed)
	if err != nil {
		return fmt.Errorf("%w: %s allowlist: %v", ErrInvalidInput, kind, err)
	}
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) || len(validation.IsDNS1123Subdomain(value)) != 0 {
		return fmt.Errorf("%w: invalid %s name %q", ErrInvalidInput, kind, value)
	}
	for _, allowedName := range allowedNames {
		if value == allowedName {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %q is not operator-allowed", ErrInvalidInput, kind, value)
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
	if snapshot.Run.UID == "" || len(snapshot.Run.UID) > maxRunUIDLength || len(validation.IsValidLabelValue(snapshot.Run.UID)) != 0 {
		return fmt.Errorf("%w: run UID is required and bounded", ErrInvalidInput)
	}
	if !validPinnedImage(snapshot.Agent.Runtime.Image) {
		return fmt.Errorf("%w: Agent runtime image must be digest-pinned", ErrInvalidInput)
	}
	if snapshot.Agent.Runtime.Harness != v1alpha1.HarnessCodex && snapshot.Agent.Runtime.Harness != v1alpha1.HarnessClaudeCode {
		return fmt.Errorf("%w: unsupported Agent harness %q", ErrInvalidInput, snapshot.Agent.Runtime.Harness)
	}
	if err := ValidateRuntimeClassName(snapshot.Agent.Runtime.RuntimeClassName, options.AllowedRuntimeClasses); err != nil {
		return err
	}
	if strings.TrimSpace(snapshot.Task) == "" || strings.TrimSpace(snapshot.Instructions) == "" {
		return fmt.Errorf("%w: resolved task and instructions are required", ErrInvalidInput)
	}
	if !sourceRepoPattern.MatchString(snapshot.Spec.Source.Repo) || snapshot.Spec.Source.BaseRef == "" || !resolved.ValidBaseSHA(snapshot.BaseSHA) {
		return fmt.Errorf("%w: source repo and baseRef are required", ErrInvalidInput)
	}
	if strings.ContainsAny(snapshot.Spec.Source.BaseRef, "\x00\r\n") {
		return fmt.Errorf("%w: source baseRef contains forbidden control characters", ErrInvalidInput)
	}
	if snapshot.Spec.Source.Depth < 0 || snapshot.Spec.Source.Depth > 100 {
		return fmt.Errorf("%w: source depth is outside 0..100", ErrInvalidInput)
	}
	if snapshot.Spec.Workspace.Size == "" {
		return fmt.Errorf("%w: workspace size is required", ErrInvalidInput)
	}
	workspaceSize, err := resource.ParseQuantity(snapshot.Spec.Workspace.Size)
	if err != nil || workspaceSize.Sign() <= 0 {
		return fmt.Errorf("%w: workspace size must be a positive Kubernetes quantity", ErrInvalidInput)
	}
	if snapshot.Spec.Workspace.StorageClassName != "" && len(validation.IsDNS1123Subdomain(snapshot.Spec.Workspace.StorageClassName)) != 0 {
		return fmt.Errorf("%w: invalid workspace storage class", ErrInvalidInput)
	}
	if err := ValidateStorageClassName(snapshot.Spec.Workspace.StorageClassName, options.AllowedStorageClasses); err != nil {
		return err
	}
	for index, skill := range snapshot.Agent.Skills {
		if strings.TrimSpace(skill.Name) == "" || strings.TrimSpace(skill.Ref) == "" || !canonical.ValidDigest(skill.Digest) {
			return fmt.Errorf("%w: skill %d must have a name, ref, and valid digest", ErrInvalidInput, index)
		}
	}
	primary, err := primaryProvider(snapshot.ModelRoute)
	if err != nil {
		return err
	}
	if err := validateRuntimeModelPairing(snapshot.Agent.Runtime.Harness, snapshot.ModelRoute); err != nil {
		return err
	}
	if err := validateRuntimeImagePairing(snapshot.Agent.Runtime.Harness, snapshot.Agent.Runtime.Image); err != nil {
		return err
	}
	_ = primary
	return nil
}

// primaryModel selects the first provider exactly as the broker does: lowest
// numeric priority first, with provider name as a stable tie-breaker. The
// runtime must never infer a model or depend on the storage order of a
// listType=map CRD field.
func primaryModel(route v1alpha1.ModelRouteSpec) (string, error) {
	provider, err := primaryProvider(route)
	if err != nil {
		return "", err
	}
	return provider.Model, nil
}

func primaryProvider(route v1alpha1.ModelRouteSpec) (v1alpha1.ModelProvider, error) {
	if len(route.Providers) == 0 {
		return v1alpha1.ModelProvider{}, fmt.Errorf("%w: ModelRoute must contain at least one provider", ErrInvalidInput)
	}
	providers := append([]v1alpha1.ModelProvider(nil), route.Providers...)
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].Priority != providers[j].Priority {
			return providers[i].Priority < providers[j].Priority
		}
		return providers[i].Name < providers[j].Name
	})
	selected := providers[0]
	if selected.Priority < 1 || strings.TrimSpace(selected.Name) == "" || strings.TrimSpace(selected.Model) == "" {
		return v1alpha1.ModelProvider{}, fmt.Errorf("%w: primary ModelRoute provider is invalid", ErrInvalidInput)
	}
	return selected, nil
}

func modelPricingJSON(route v1alpha1.ModelRouteSpec) (string, error) {
	type pricing struct {
		InputMicrosPerToken  int64 `json:"inputMicrosPerToken"`
		OutputMicrosPerToken int64 `json:"outputMicrosPerToken"`
	}
	table := make(map[string]pricing)
	for _, provider := range route.Providers {
		if provider.Pricing == nil {
			continue
		}
		table[provider.Name] = pricing{
			InputMicrosPerToken:  provider.Pricing.InputMicrosPerToken,
			OutputMicrosPerToken: provider.Pricing.OutputMicrosPerToken,
		}
	}
	if len(table) == 0 {
		return "", nil
	}
	body, err := json.Marshal(table)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func validateRuntimeModelPairing(harness v1alpha1.Harness, route v1alpha1.ModelRouteSpec) error {
	for _, provider := range route.Providers {
		isClaudeProvider := provider.Kind == "anthropic-messages" || provider.Kind == "openrouter-anthropic-messages"
		isCodexProvider := provider.Kind == "openai-responses" || provider.Kind == "openrouter-responses"
		switch harness {
		case v1alpha1.HarnessCodex:
			if !isCodexProvider {
				return fmt.Errorf("%w: Codex runtime requires a Responses ModelRoute provider", ErrInvalidInput)
			}
		case v1alpha1.HarnessClaudeCode:
			if !isClaudeProvider {
				return fmt.Errorf("%w: Claude Code runtime requires an Anthropic Messages ModelRoute provider", ErrInvalidInput)
			}
		default:
			return fmt.Errorf("%w: unsupported runtime harness %q", ErrInvalidInput, harness)
		}
	}
	return nil
}

// Image references are immutable, but their contents are not inspectable by
// this pure builder. Enforce the published image naming contract whenever the
// reference uses an AGW runtime name; arbitrary custom images remain allowed
// for development, while a known Codex/Claude image mismatch fails closed.
func validateRuntimeImagePairing(harness v1alpha1.Harness, image string) error {
	base := strings.ToLower(strings.SplitN(image, "@", 2)[0])
	name := base[strings.LastIndex(base, "/")+1:]
	if strings.Contains(name, "agw-runtime-codex") && harness != v1alpha1.HarnessCodex {
		return fmt.Errorf("%w: runtime image is a Codex image but harness is %q", ErrInvalidInput, harness)
	}
	if strings.Contains(name, "agw-runtime-claude") && harness != v1alpha1.HarnessClaudeCode {
		return fmt.Errorf("%w: runtime image is a Claude Code image but harness is %q", ErrInvalidInput, harness)
	}
	return nil
}

func harnessRuntimeEnv(harness v1alpha1.Harness, provider v1alpha1.ModelProvider, model, timeout string) []corev1.EnvVar {
	switch harness {
	case v1alpha1.HarnessCodex:
		return []corev1.EnvVar{
			{Name: "AGW_CODEX_WORKSPACE", Value: WorkspaceMountPath + "/repo"},
			{Name: "AGW_CODEX_MODEL", Value: model},
			// The Kubernetes work Sandbox is the authoritative outer boundary.
			// Codex's nested Linux sandbox cannot create user namespaces under the
			// pod security profile, so the runtime delegates command isolation to
			// the pod's UID/network/filesystem controls.
			{Name: "AGW_CODEX_SANDBOX", Value: "danger-full-access"},
			{Name: "AGW_CODEX_MAX_RUNTIME", Value: timeout},
			{Name: "AGW_CODEX_REQUIRE_ARTIFACT", Value: "true"},
		}
	case v1alpha1.HarnessClaudeCode:
		base := "http://127.0.0.1:8081"
		if provider.Kind == "openrouter-anthropic-messages" {
			base += "/api"
		}
		return []corev1.EnvVar{
			{Name: "AGW_CLAUDE_WORKSPACE", Value: WorkspaceMountPath + "/repo"},
			{Name: "AGW_CLAUDE_MODEL", Value: model},
			{Name: "AGW_CLAUDE_MAX_RUNTIME", Value: timeout},
			{Name: "AGW_CLAUDE_REQUIRE_ARTIFACT", Value: "true"},
			{Name: "AGW_CLAUDE_ANTHROPIC_BASE_URL", Value: base},
			{Name: "AGW_CLAUDE_MCP_URL", Value: "http://127.0.0.1:8081/mcp"},
		}
	default:
		return nil
	}
}

func validateOptions(snapshot resolved.Snapshot, options Options) error {
	if err := validateAgentGatewaySidecar(options.AgentGateway); err != nil {
		return err
	}
	images := []struct {
		name  string
		value string
	}{
		{name: "clone", value: options.CloneImage},
		{name: "context", value: options.ContextImage},
		{name: "lockdown", value: options.LockdownImage},
		{name: "broker", value: options.BrokerImage},
	}
	if len(snapshot.Agent.Skills) > 0 {
		images = append(images, struct {
			name  string
			value string
		}{name: "skills", value: options.SkillsImage})
		if !validSkillsEndpoint(options.SkillsEndpoint) {
			return fmt.Errorf("%w: skills endpoint must be a credential-free HTTPS /mcp URL", ErrInvalidInput)
		}
	} else if options.SkillsEndpoint != "" {
		return fmt.Errorf("%w: skills endpoint is not allowed without configured skills", ErrInvalidInput)
	}
	for _, image := range images {
		if !validPinnedImage(image.value) {
			return fmt.Errorf("%w: %s image must be digest-pinned", ErrInvalidInput, image.name)
		}
	}
	if options.SecretName == "" || len(validation.IsDNS1123Subdomain(options.SecretName)) != 0 {
		return fmt.Errorf("%w: SecretName must be a valid DNS name", ErrInvalidInput)
	}
	if options.SecretName != WorkSecretName(snapshot.Run.UID) {
		return fmt.Errorf("%w: SecretName must equal deterministic work name %q", ErrInvalidInput, WorkSecretName(snapshot.Run.UID))
	}
	if err := validateSecretKey(options.CloneSecretKey, "clone"); err != nil {
		return err
	}
	if options.SkillsSecretKey != "" {
		if err := validateSecretKey(options.SkillsSecretKey, "skills"); err != nil {
			return err
		}
	}
	if len(snapshot.Agent.Skills) == 0 && options.SkillsSecretKey != "" {
		return fmt.Errorf("%w: skills Secret key is not allowed without configured skills", ErrInvalidInput)
	}
	seen := map[string]string{}
	for _, entry := range []struct{ scope, key string }{{"clone", options.CloneSecretKey}, {"skills", options.SkillsSecretKey}} {
		if entry.key == "" {
			continue
		}
		seen[entry.key] = entry.scope
	}
	for index, key := range options.BrokerSecretKeys {
		if err := validateSecretKey(key, fmt.Sprintf("broker[%d]", index)); err != nil {
			return err
		}
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("%w: broker key %q duplicates %s key", ErrInvalidInput, key, previous)
		}
		seen[key] = "broker"
	}
	if err := validateArtifactStoreOptions(options); err != nil {
		return err
	}
	if options.Now.IsZero() || options.ShutdownTime.IsZero() {
		return fmt.Errorf("%w: Now and ShutdownTime are required", ErrInvalidInput)
	}
	if options.MaxShutdownDuration <= 0 || options.MaxShutdownDuration > sandbox.DefaultMaxShutdownDuration {
		return fmt.Errorf("%w: MaxShutdownDuration must be in (0,%s]", ErrInvalidInput, sandbox.DefaultMaxShutdownDuration)
	}
	if !options.ShutdownTime.After(options.Now) || options.ShutdownTime.After(options.Now.Add(options.MaxShutdownDuration)) {
		return fmt.Errorf("%w: ShutdownTime must be future and within the configured bound", ErrInvalidInput)
	}
	return nil
}

func validateSecretKey(key, scope string) error {
	if key == "" || len(key) > maxSecretKeyLength || len(validation.IsConfigMapKey(key)) != 0 {
		return fmt.Errorf("%w: %s Secret key is invalid", ErrInvalidInput, scope)
	}
	return nil
}

func brokerSecretFileMap(keys []string) ([]byte, error) {
	type file struct {
		Key  string `json:"key"`
		Path string `json:"path"`
	}
	files := make([]file, len(keys))
	for index, key := range keys {
		files[index] = file{Key: key, Path: "item-" + strconv.Itoa(index)}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return nil, fmt.Errorf("%w: encode broker Secret files: %v", ErrInvalidInput, err)
	}
	return encoded, nil
}

func secretVolume(name, secretName, key, path string) corev1.Volume {
	// The mount itself is the credential boundary: each projection is mounted
	// only into its intended container. World-readable *inside that one mount*
	// permits its non-root UID to read the file without granting pod-wide
	// supplemental groups; the agent has no mount for any Secret volume.
	mode := int32(0444)
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secretName,
			Items:       []corev1.KeyToPath{{Key: key, Path: path, Mode: &mode}},
			DefaultMode: &mode,
		}},
	}
}

func brokerSecretVolume(secretName string, keys []string) corev1.Volume {
	mode := int32(0444)
	items := make([]corev1.KeyToPath, len(keys))
	for index, key := range keys {
		items[index] = corev1.KeyToPath{Key: key, Path: "item-" + strconv.Itoa(index), Mode: &mode}
	}
	return corev1.Volume{
		Name: BrokerSecretVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secretName,
			Items:       items,
			DefaultMode: &mode,
		}},
	}
}

func artifactSecretVolume(options Options) corev1.Volume {
	mode := int32(0444)
	items := []corev1.KeyToPath{
		{Key: options.ArtifactAccessKeyIDSecretKey, Path: "access-key-id", Mode: &mode},
		{Key: options.ArtifactSecretAccessKeySecretKey, Path: "secret-access-key", Mode: &mode},
	}
	if options.ArtifactSessionTokenSecretKey != "" {
		items = append(items, corev1.KeyToPath{Key: options.ArtifactSessionTokenSecretKey, Path: "session-token", Mode: &mode})
	}
	return corev1.Volume{Name: ArtifactSecretVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: options.SecretName, Items: items, DefaultMode: &mode,
	}}}
}

func directSecretItemMount(name, mountPath, subPath string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: mountPath, SubPath: subPath, ReadOnly: true}
}

func validateArtifactStoreOptions(options Options) error {
	configured := options.ArtifactAccessKeyIDSecretKey != "" || options.ArtifactSecretAccessKeySecretKey != "" || options.ArtifactSessionTokenSecretKey != "" ||
		options.ArtifactStoreBucket != "" || options.ArtifactStorePrefix != "" || options.ArtifactStoreEndpoint != ""
	if !configured {
		return nil
	}
	if options.ArtifactAccessKeyIDSecretKey != artifactauth.AccessKeyIDKey || options.ArtifactSecretAccessKeySecretKey != artifactauth.SecretAccessKeyKey ||
		(options.ArtifactSessionTokenSecretKey != "" && options.ArtifactSessionTokenSecretKey != artifactauth.SessionTokenKey) {
		return fmt.Errorf("%w: artifact credential keys must use the fixed projection contract", ErrInvalidInput)
	}
	// This is the general runtime artifact/effect store limit. The independent
	// verifyfetch path intentionally has separate patch/archive bounds because
	// it reads a different, controller-authored verification input.
	if options.ArtifactStoreBucket == "" || options.ArtifactStoreRegion == "" || options.ArtifactStorePrefix == "" ||
		options.ArtifactStoreMaxObjectBytes < 1 || options.ArtifactStoreMaxObjectBytes > objectstore.GeneralMaxObjectBytes || !safeObjectPrefix(options.ArtifactStorePrefix) {
		return fmt.Errorf("%w: artifact store configuration is incomplete", ErrInvalidInput)
	}
	if options.ArtifactStoreEndpoint != "" {
		parsed, err := url.Parse(options.ArtifactStoreEndpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return fmt.Errorf("%w: artifact store endpoint must be a credential-free HTTPS origin", ErrInvalidInput)
		}
	}
	return nil
}

func safeObjectPrefix(value string) bool {
	if value == "" || len(value) > 512 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

func workspaceMount(readOnly bool) corev1.VolumeMount {
	return volumeMount(WorkspaceVolumeName, WorkspaceMountPath, readOnly)
}

func workspaceSubPathMount(readOnly bool, subPath string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      WorkspaceVolumeName,
		MountPath: WorkspaceMountPath + "/" + subPath,
		SubPath:   subPath,
		ReadOnly:  readOnly,
	}
}

func contextSubPathMount(subPath, mountPath string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name: ContextVolumeName, MountPath: mountPath, SubPath: subPath, ReadOnly: true,
	}
}

func volumeMount(name, path string, readOnly bool) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: readOnly}
}

func regularSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                int64Ptr(uid),
		RunAsGroup:               int64Ptr(uid),
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Privileged:               boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func namespaceRootSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                int64Ptr(0),
		RunAsGroup:               int64Ptr(0),
		RunAsNonRoot:             boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Privileged:               boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func cloneSecurityContext() *corev1.SecurityContext {
	security := namespaceRootSecurityContext()
	// The clone helper must hand the working checkout to UID 1000 and seal the
	// pristine checkout as UID 0. CHOWN is the only extra capability required;
	// it is namespaced by hostUsers:false and remains paired with drop-ALL.
	security.Capabilities.Add = []corev1.Capability{"CHOWN"}
	return security
}

func lockdownSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                int64Ptr(0),
		RunAsGroup:               int64Ptr(0),
		RunAsNonRoot:             boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Privileged:               boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}, Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func podNameEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: "AGW_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
}

func resources(requestCPU, requestMemory, limitCPU, limitMemory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(requestCPU), corev1.ResourceMemory: resource.MustParse(requestMemory)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limitCPU), corev1.ResourceMemory: resource.MustParse(limitMemory)},
	}
}

func copyResources(input corev1.ResourceRequirements) corev1.ResourceRequirements {
	return *input.DeepCopy()
}

func validPinnedImage(value string) bool {
	if len(value) == 0 || len(value) > 512 || !resolved.ValidPinnedImage(value) {
		return false
	}
	parts := strings.Split(value, "@sha256:")
	return len(parts) == 2 && imageNamePattern.MatchString(parts[0])
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func digestShort(value string) string {
	value = strings.TrimPrefix(value, canonical.DigestPrefix)
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func copyStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func boolPtr(value bool) *bool { return &value }

func int64Ptr(value int64) *int64 { return &value }

func fsGroupChangePolicyPtr(value corev1.PodFSGroupChangePolicy) *corev1.PodFSGroupChangePolicy {
	return &value
}

var sourceRepoPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

var imageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func validSkillsEndpoint(value string) bool {
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "/mcp" && parsed.RawQuery == "" && parsed.Fragment == ""
}
