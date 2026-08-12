package criticworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/airlock"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifactauth"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
)

const (
	Role = "critic"

	RunUIDAnnotationKey                = "agents.astatide.com/run-uid"
	RunGenerationAnnotationKey         = "agents.astatide.com/run-generation"
	SpecDigestAnnotationKey            = "agents.astatide.com/spec-digest"
	PatchDigestAnnotationKey           = "agents.astatide.com/patch-digest"
	ContextDigestAnnotationKey         = "agents.astatide.com/context-digest"
	GatewayConfigDigestAnnotationKey   = "agents.astatide.com/agentgateway-config-digest"
	GatewayRouteRefAnnotationKey       = "agents.astatide.com/agentgateway-route-ref"
	GatewayServiceAccountAnnotationKey = "agents.astatide.com/agentgateway-service-account"
	GatewayJWTPolicyAnnotationKey      = "agents.astatide.com/agentgateway-jwt-policy"
	GatewayJWTEvidenceAnnotationKey    = "agents.astatide.com/agentgateway-jwt-evidence"
	GatewayEndpointAnnotationKey       = "agents.astatide.com/agentgateway-endpoint"
	ContractDigestAnnotationKey        = "agents.astatide.com/critic-contract-digest"
	BindingDigestAnnotationKey         = "agents.astatide.com/critic-binding-digest"
	OutputKeyPrefixAnnotationKey       = "agents.astatide.com/critic-output-key-prefix"
	CriticRouteNameAnnotationKey       = "agents.astatide.com/critic-route-name"
	CriticRouteUIDAnnotationKey        = "agents.astatide.com/critic-route-uid"
	CriticRouteGenerationAnnotation    = "agents.astatide.com/critic-route-generation"
	CriticRouteFamilyAnnotationKey     = "agents.astatide.com/critic-route-family"
	WorkerRouteFamilyAnnotationKey     = "agents.astatide.com/worker-route-families"
	JobSpecFingerprintAnnotationKey    = "agents.astatide.com/critic-job-spec-fingerprint"

	InputEnv               = "AGW_CRITIC_INPUT_JSON"
	OutputProtocolEnv      = "AGW_CRITIC_OUTPUT_PROTOCOL"
	MaxOutputBytesEnv      = "AGW_CRITIC_MAX_OUTPUT_BYTES"
	RunUIDEnv              = "AGW_CRITIC_RUN_UID"
	SpecDigestEnv          = "AGW_CRITIC_SPEC_DIGEST"
	PatchDigestEnv         = "AGW_CRITIC_PATCH_DIGEST"
	ContextDigestEnv       = "AGW_CRITIC_CONTEXT_DIGEST"
	CriticRouteFamilyEnv   = "AGW_CRITIC_MODEL_FAMILY"
	CriticRouteProviderEnv = "AGW_CRITIC_MODEL_PROVIDER"
	CriticRouteModelEnv    = "AGW_CRITIC_MODEL"
	CriticRouteKindEnv     = "AGW_CRITIC_MODEL_KIND"
	WorkerRouteFamilyEnv   = "AGW_WORKER_MODEL_FAMILY"

	Entrypoint         = "/agw/critic"
	LockdownEntrypoint = "sh"

	InputVolumeName           = "critic-input"
	GatewayConfigVolumeName   = "agentgateway-config"
	ObjectStoreVolumeName     = "object-store-credentials"
	GatewayTmpVolumeName      = "agentgateway-tmp"
	GatewayIdentityVolumeName = "agentgateway-identity"
	NetworkPolicyNameSuffix   = "-egress"
	GatewayNamespace          = "agw-system"

	InputMountPath           = "/workspace/input"
	GatewayConfigMountPath   = "/run/agw/agentgateway"
	GatewayIdentityMountPath = "/var/run/secrets/agw-gateway"
	GatewayTmpMountPath      = "/tmp"
	ObjectStoreMountPath     = "/run/agw/object-store"

	NodeLabelKey   = "agw.astatide.com/agents"
	NodeLabelValue = "true"

	defaultMaxOutputBytes = int64(MaxOutputBytes)
	minTimeout            = 10 * time.Second
	maxTimeout            = 2 * time.Hour
)

var (
	ErrInvalidOptions       = errors.New("invalid critic workload options")
	ErrJobConflict          = errors.New("critic Job conflicts with immutable plan")
	ErrJobIdentity          = errors.New("critic Job identity is not authenticated")
	ErrUnsupportedModel     = errors.New("critic model route is unsupported by the agentgateway adapter")
	ErrCredentialBinding    = errors.New("critic provider credential binding is invalid")
	ErrGatewayBinding       = errors.New("critic central agentgateway binding is invalid")
	ErrArtifactStoreBinding = errors.New("critic artifact store binding is invalid")

	criticResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
	initResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	gatewayResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
)

// ArtifactStoreOptions describes the only credential-bearing Secret projected
// into the input materializer. Provider credentials are never projected into a
// critic run; they remain owned by the central agw-system gateway.
type ArtifactStoreOptions struct {
	Endpoint       string
	Region         string
	Bucket         string
	ForcePathStyle bool

	AccessKeyIDSecretKey  string
	SecretAccessKeyKey    string
	SessionTokenSecretKey string
	// EgressCIDRs are the exact resolved network destinations for the object
	// store. Kubernetes NetworkPolicy has no hostname rule, so admission fails
	// closed when the operator has not supplied these routes.
	EgressCIDRs []string
}

// GatewayClientOptions describes the scoped identity used by the run-local
// agentgateway forwarder to call the long-lived agw-system data plane. It has
// no gateway-client Secret selector: productionPod projects a bounded,
// audience-specific ServiceAccount JWT only into the agentgateway container.
// The central gateway owns provider auth and injects it from its agw-system
// Backend/Secret configuration.
type GatewayClientOptions struct {
	Endpoint               string
	RouteRef               string
	ServiceAccountName     string
	TokenExpirationSeconds int64
	JWTCapability          GatewayJWTCapability
	// PodSelector identifies only the long-lived gateway data-plane Pods in
	// agw-system. A Service name cannot be selected directly by NetworkPolicy.
	PodSelector map[string]string
}

// BuildOptions are trusted operator inputs. Every image is immutable. Provider
// and gateway-client Secret values/selectors are deliberately absent: the
// resulting Pod contains one explicit projected ServiceAccount token for the
// local gateway, while the only Secret reference is the scoped object-store
// credential used by the trusted input-materializer init container.
type BuildOptions struct {
	CriticImage            string
	AgentGatewayImage      string
	LockdownImage          string
	AgentGatewayCapability AgentGatewayCapability
	Gateway                GatewayClientOptions
	ArtifactStore          ArtifactStoreOptions
	MaxOutputBytes         int64
	Timeout                time.Duration
}

// Plan is the immutable Job plan and its content-addressed identity.
type Plan struct {
	Job             *batchv1.Job
	NetworkPolicy   *networkingv1.NetworkPolicy
	Input           Input
	InputBytes      []byte
	InputDigest     string
	BindingDigest   string
	JobName         string
	OutputKeyPrefix string
	SpecFingerprint string
	// ArtifactStoreSecretName is derived from Input.Run.UID by the canonical
	// verify-phase naming function. It is part of the plan so a read-back Job
	// cannot silently bind a different run's object-store credentials.
	ArtifactStoreSecretName string
}

// Build creates the production two-container critic workload. The capability
// result is mandatory: it proves the exact pinned agentgateway image supports
// native Anthropic Messages and the one-attempt route policy before a Job can
// be admitted. No broker or guessed listener is substituted.
func Build(input Input, options BuildOptions) (Plan, error) {
	if err := ValidateInput(input); err != nil {
		return Plan{}, err
	}
	var err error
	options, err = validateBuildOptions(options)
	if err != nil {
		return Plan{}, err
	}
	if err := validateProductionBinding(input, options); err != nil {
		return Plan{}, err
	}
	inputBytes, err := CanonicalInputBytes(input)
	if err != nil {
		return Plan{}, err
	}
	inputDigest, err := InputDigest(input)
	if err != nil {
		return Plan{}, err
	}
	bindingDigest, err := BindingDigest(input)
	if err != nil {
		return Plan{}, err
	}
	config, err := RenderAgentGatewayConfig(input.CriticRoute.Selected.Model, AgentGatewayClientBinding{Endpoint: options.Gateway.Endpoint, RouteRef: options.Gateway.RouteRef})
	if err != nil {
		return Plan{}, fmt.Errorf("%w: render agentgateway config: %v", ErrInvalidOptions, err)
	}
	configDigest := digestBytes(config)
	jobName := criticJobName(input)
	outputPrefix := "runs/" + input.Run.UID + "/critic/" + jobName
	artifactStoreSecretName, err := verifyArtifactStoreSecretName(input)
	if err != nil {
		return Plan{}, err
	}
	specFingerprint, err := jobFingerprint(inputDigest, bindingDigest, options)
	if err != nil {
		return Plan{}, err
	}
	activeDeadline := int64(math.Ceil(options.Timeout.Seconds()))
	if activeDeadline <= 0 {
		return Plan{}, fmt.Errorf("%w: timeout rounds to zero", ErrInvalidOptions)
	}
	labels, annotations, owner, selector := jobMetadata(input, bindingDigest, inputDigest, outputPrefix, specFingerprint)
	annotations[GatewayConfigDigestAnnotationKey] = configDigest
	annotations[GatewayRouteRefAnnotationKey] = options.Gateway.RouteRef
	annotations[GatewayServiceAccountAnnotationKey] = options.Gateway.ServiceAccountName
	annotations[GatewayJWTPolicyAnnotationKey] = options.Gateway.JWTCapability.PolicyRef
	annotations[GatewayJWTEvidenceAnnotationKey] = options.Gateway.JWTCapability.EvidenceDigest
	annotations[GatewayEndpointAnnotationKey] = options.Gateway.Endpoint
	pod, err := productionPod(input, options, inputBytes, configDigest, artifactStoreSecretName)
	if err != nil {
		return Plan{}, err
	}
	job := newJob(jobName, input.Run.Namespace, labels, annotations, owner, selector, activeDeadline, pod)
	networkPolicy, err := newEgressNetworkPolicy(jobName, input.Run.Namespace, labels, annotations, owner, *selector, options)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Job: job, NetworkPolicy: networkPolicy, Input: input, InputBytes: inputBytes, InputDigest: inputDigest, BindingDigest: bindingDigest, JobName: jobName, OutputKeyPrefix: outputPrefix, SpecFingerprint: specFingerprint, ArtifactStoreSecretName: artifactStoreSecretName}, nil
}

// verifyArtifactStoreSecretName is the only critic-side derivation of the
// object-store credential Secret. The verify phase owns this Secret and uses
// the same workload package helper; keeping the derivation here as a call to
// that helper prevents a second hash/prefix convention from being introduced.
func verifyArtifactStoreSecretName(input Input) (string, error) {
	if input.Run.UID == "" {
		return "", fmt.Errorf("%w: run UID is required before deriving verify Secret", ErrArtifactStoreBinding)
	}
	name := workload.VerifySecretName(input.Run.UID)
	if !validPathSegment(name) {
		return "", fmt.Errorf("%w: derived verify Secret name is invalid", ErrArtifactStoreBinding)
	}
	return name, nil
}

func validateProductionBinding(input Input, options BuildOptions) error {
	if len(input.CriticRoute.Providers) != 1 {
		return fmt.Errorf("%w: critic route must contain exactly one provider when retries are disabled", ErrUnsupportedModel)
	}
	primary := input.CriticRoute.Selected
	if primary.Kind != "anthropic-messages" {
		return fmt.Errorf("%w: kind %q is not supported", ErrUnsupportedModel, primary.Kind)
	}
	if err := validateProductionGatewayBinding(options.Gateway); err != nil {
		return err
	}
	for _, artifact := range []v1alpha1.ArtifactRef{input.Patch, input.Context} {
		location, err := verifyfetch.ParseS3URI(artifact.URI)
		if err != nil || location.Bucket != options.ArtifactStore.Bucket {
			return fmt.Errorf("%w: %s must resolve inside the configured bucket", ErrArtifactStoreBinding, artifact.Kind)
		}
	}
	if input.Patch.SizeBytes+input.Context.SizeBytes > MaxPromptBytes-PromptEnvelopeBytes {
		return fmt.Errorf("%w: verified patch and ContextPack exceed the critic prompt bound", ErrUnsupportedModel)
	}
	return nil
}

func validateProductionGatewayBinding(binding GatewayClientOptions) error {
	if _, err := gatewayHostOverride(binding.Endpoint, true); err != nil {
		return fmt.Errorf("%w: %v", ErrGatewayBinding, err)
	}
	if _, err := httpEndpointPort(binding.Endpoint); err != nil {
		return fmt.Errorf("%w: %v", ErrGatewayBinding, err)
	}
	if !validPathSegment(binding.ServiceAccountName) {
		return fmt.Errorf("%w: projected-token ServiceAccount is invalid", ErrGatewayBinding)
	}
	if binding.TokenExpirationSeconds < 600 || binding.TokenExpirationSeconds > 3600 {
		return fmt.Errorf("%w: projected-token expiration must be between 600 and 3600 seconds", ErrGatewayBinding)
	}
	if !binding.JWTCapability.Verified || binding.JWTCapability.Issuer == "" || binding.JWTCapability.Audience == "" || binding.JWTCapability.PolicyRef == "" || !canonical.ValidDigest(binding.JWTCapability.EvidenceDigest) {
		return fmt.Errorf("%w: central Gateway JWT capability probe is required", ErrGatewayBinding)
	}
	if !validModelText(binding.JWTCapability.Issuer) || !validModelText(binding.JWTCapability.Audience) || !validPathSegment(binding.JWTCapability.PolicyRef) {
		return fmt.Errorf("%w: central Gateway JWT capability identity is invalid", ErrGatewayBinding)
	}
	if len(binding.PodSelector) == 0 || len(binding.PodSelector) > 16 {
		return fmt.Errorf("%w: gateway Pod selector is required", ErrGatewayBinding)
	}
	selector := &metav1.LabelSelector{MatchLabels: binding.PodSelector}
	if _, err := metav1.LabelSelectorAsSelector(selector); err != nil {
		return fmt.Errorf("%w: gateway Pod selector is invalid", ErrGatewayBinding)
	}
	evidenceDigest, err := GatewayJWTCapabilityEvidenceDigest(binding)
	if err != nil || binding.JWTCapability.EvidenceDigest != evidenceDigest {
		return fmt.Errorf("%w: JWT capability evidence is not bound to the configured gateway identity", ErrGatewayBinding)
	}
	return nil
}

func jobMetadata(input Input, bindingDigest, inputDigest, outputPrefix, specFingerprint string) (map[string]string, map[string]string, metav1.OwnerReference, *metav1.LabelSelector) {
	primary := input.CriticRoute.Selected
	labels := map[string]string{
		"agents.astatide.com/role":                 Role,
		"agents.astatide.com/run-uid-short":        shortIdentity(input.Run.UID),
		"agents.astatide.com/spec-digest-short":    shortIdentity(strings.TrimPrefix(input.SpecDigest, canonical.DigestPrefix)),
		"agents.astatide.com/binding-digest-short": shortIdentity(strings.TrimPrefix(bindingDigest, canonical.DigestPrefix)),
	}
	annotations := map[string]string{
		RunUIDAnnotationKey:             input.Run.UID,
		RunGenerationAnnotationKey:      strconv.FormatInt(input.Run.Generation, 10),
		SpecDigestAnnotationKey:         input.SpecDigest,
		PatchDigestAnnotationKey:        input.Patch.Digest,
		ContextDigestAnnotationKey:      input.Context.Digest,
		ContractDigestAnnotationKey:     inputDigest,
		BindingDigestAnnotationKey:      bindingDigest,
		OutputKeyPrefixAnnotationKey:    outputPrefix,
		CriticRouteNameAnnotationKey:    input.CriticRoute.Name,
		CriticRouteUIDAnnotationKey:     input.CriticRoute.UID,
		CriticRouteGenerationAnnotation: strconv.FormatInt(input.CriticRoute.Generation, 10),
		CriticRouteFamilyAnnotationKey:  primary.Family,
		WorkerRouteFamilyAnnotationKey:  routeFamilies(input.WorkerRoute),
		JobSpecFingerprintAnnotationKey: specFingerprint,
	}
	owner := metav1.OwnerReference{
		APIVersion: "agents.astatide.com/v1alpha1", Kind: "AgentRun", Name: input.Run.Name,
		UID: typesUID(input.Run.UID), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
	}
	return labels, annotations, owner, &metav1.LabelSelector{MatchLabels: copyStringMap(labels)}
}

func newJob(name, namespace string, labels, annotations map[string]string, owner metav1.OwnerReference, selector *metav1.LabelSelector, activeDeadline int64, pod corev1.PodSpec) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: batchv1.JobSpec{
			Parallelism: int32Ptr(1), Completions: int32Ptr(1), BackoffLimit: int32Ptr(0),
			ActiveDeadlineSeconds: &activeDeadline, ManualSelector: boolPtr(true), Selector: selector,
			CompletionMode: completionModePtr(batchv1.NonIndexedCompletion),
			Template:       corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: copyStringMap(labels), Annotations: copyStringMap(annotations)}, Spec: pod},
		},
	}
}

func newEgressNetworkPolicy(name, namespace string, labelsMap, annotations map[string]string, owner metav1.OwnerReference, selector metav1.LabelSelector, options BuildOptions) (*networkingv1.NetworkPolicy, error) {
	objectPort, err := httpsEndpointPort(options.ArtifactStore.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: object-store port: %v", ErrArtifactStoreBinding, err)
	}
	centralPort, err := httpEndpointPort(options.Gateway.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: gateway port: %v", ErrGatewayBinding, err)
	}
	dnsNamespace := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}
	dnsPods := &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}
	centralNamespace := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": GatewayNamespace}}
	centralPods := &metav1.LabelSelector{MatchLabels: copyStringMap(options.Gateway.PodSelector)}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := func(value int32, protocol corev1.Protocol) networkingv1.NetworkPolicyPort {
		port := intOrString(value)
		return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &port}
	}
	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: dnsNamespace, PodSelector: dnsPods}}, Ports: []networkingv1.NetworkPolicyPort{port(53, udp), port(53, tcp)}},
		{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: centralNamespace, PodSelector: centralPods}}, Ports: []networkingv1.NetworkPolicyPort{port(centralPort, tcp)}},
	}
	for _, cidr := range options.ArtifactStore.EgressCIDRs {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}, Ports: []networkingv1.NetworkPolicyPort{port(objectPort, tcp)}})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: name + NetworkPolicyNameSuffix, Namespace: namespace, Labels: copyStringMap(labelsMap), Annotations: copyStringMap(annotations), OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: selector, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, Ingress: []networkingv1.NetworkPolicyIngressRule{}, Egress: egress},
	}, nil
}

func intOrString(value int32) intstr.IntOrString {
	return intstr.FromInt32(value)
}

func httpEndpointPort(endpoint string) (int32, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Port() == "" {
		return 0, fmt.Errorf("endpoint must include a port")
	}
	value, err := strconv.Atoi(u.Port())
	if err != nil || value < 1 || value > 65535 {
		return 0, fmt.Errorf("endpoint port is invalid")
	}
	return int32(value), nil
}

func httpsEndpointPort(endpoint string) (int32, error) {
	u, err := url.Parse(endpoint)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return 0, fmt.Errorf("object-store endpoint must be HTTPS")
	}
	if u.Port() == "" {
		return 443, nil
	}
	value, err := strconv.Atoi(u.Port())
	if err != nil || value < 1 || value > 65535 {
		return 0, fmt.Errorf("endpoint port is invalid")
	}
	return int32(value), nil
}

func productionPod(input Input, options BuildOptions, inputBytes []byte, configDigest, artifactStoreSecretName string) (corev1.PodSpec, error) {
	airlockScript, err := airlock.Render(airlock.Policy{Rules: []airlock.Rule{
		{UID: airlock.UIDAgent, Family: airlock.FamilyIPv4, Protocol: airlock.ProtocolTCP, Destination: "127.0.0.1", Port: airlock.GatewayPort},
		{UID: airlock.UIDAgent, Family: airlock.FamilyIPv6, Protocol: airlock.ProtocolTCP, Destination: "::1", Port: airlock.GatewayPort},
		{UID: airlock.UIDGateway, Family: airlock.FamilyAny, AnyEgress: true},
	}})
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("%w: render UID airlock: %v", ErrInvalidOptions, err)
	}
	primary := input.CriticRoute.Selected
	materializer := corev1.Container{
		Name: "input-materializer", Image: options.CriticImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{Entrypoint}, Resources: copyResources(initResources),
		WorkingDir: "/workspace", SecurityContext: rootInitSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: CriticModeEnv, Value: CriticModeMaterializeInput},
			{Name: InputEnv, Value: string(inputBytes)},
			{Name: CriticPatchPathEnv, Value: InputMountPath + "/patch.diff"},
			{Name: CriticContextPathEnv, Value: InputMountPath + "/context-pack.json"},
			{Name: PatchDigestEnv, Value: input.Patch.Digest},
			{Name: ContextDigestEnv, Value: input.Context.Digest},
			{Name: CriticObjectStoreEndpointEnv, Value: options.ArtifactStore.Endpoint},
			{Name: CriticObjectStoreRegionEnv, Value: options.ArtifactStore.Region},
			{Name: CriticObjectStoreBucketEnv, Value: options.ArtifactStore.Bucket},
			{Name: CriticObjectStoreForcePathStyleEnv, Value: strconv.FormatBool(options.ArtifactStore.ForcePathStyle)},
			{Name: CriticObjectStoreAccessKeyFileEnv, Value: CriticObjectStoreAccessKeyFile},
			{Name: CriticObjectStoreSecretKeyFileEnv, Value: CriticObjectStoreSecretKeyFile},
			{Name: CriticObjectStoreSessionTokenEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: artifactStoreSecretName}, Key: options.ArtifactStore.SessionTokenSecretKey, Optional: boolPtr(true)}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: InputVolumeName, MountPath: InputMountPath},
			{Name: ObjectStoreVolumeName, MountPath: CriticObjectStoreAccessKeyFile, SubPath: "access-key-id", ReadOnly: true},
			{Name: ObjectStoreVolumeName, MountPath: CriticObjectStoreSecretKeyFile, SubPath: "secret-access-key", ReadOnly: true},
		},
	}
	renderer := corev1.Container{
		Name: "gateway-config", Image: options.CriticImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{Entrypoint}, Resources: copyResources(initResources),
		WorkingDir: "/workspace", SecurityContext: rootInitSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: CriticModeEnv, Value: CriticModeRenderGateway},
			{Name: CriticRouteModelEnv, Value: primary.Model},
			{Name: CriticGatewayConfigPathEnv, Value: AgentGatewayConfigPath},
			{Name: CriticGatewayConfigDigestEnv, Value: configDigest},
			{Name: CriticGatewayPortEnv, Value: strconv.Itoa(int(AgentGatewayModelPort))},
			{Name: CriticGatewayEndpointEnv, Value: options.Gateway.Endpoint},
			{Name: CriticGatewayRouteRefEnv, Value: options.Gateway.RouteRef},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: GatewayConfigVolumeName, MountPath: GatewayConfigMountPath}},
	}
	lockdown := corev1.Container{
		Name: "airlock", Image: options.LockdownImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{LockdownEntrypoint, "-ceu"}, Args: []string{airlockScript},
		Resources: copyResources(initResources), SecurityContext: airlockSecurityContext(),
	}
	critic := corev1.Container{
		Name: Role, Image: options.CriticImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{Entrypoint}, WorkingDir: "/workspace", Resources: copyResources(criticResources),
		SecurityContext: regularSecurityContext(airlock.UIDAgent),
		Env: []corev1.EnvVar{
			{Name: OutputProtocolEnv, Value: OutputProtocol},
			{Name: MaxOutputBytesEnv, Value: strconv.FormatInt(options.MaxOutputBytes, 10)},
			{Name: RunUIDEnv, Value: input.Run.UID}, {Name: SpecDigestEnv, Value: input.SpecDigest},
			{Name: PatchDigestEnv, Value: input.Patch.Digest}, {Name: ContextDigestEnv, Value: input.Context.Digest},
			{Name: CriticRouteFamilyEnv, Value: primary.Family}, {Name: CriticRouteProviderEnv, Value: primary.Name},
			{Name: CriticRouteModelEnv, Value: primary.Model}, {Name: CriticRouteKindEnv, Value: primary.Kind},
			{Name: WorkerRouteFamilyEnv, Value: input.WorkerRoute.Selected.Family},
			{Name: "AGW_CRITIC_GATEWAY_URL", Value: "http://127.0.0.1:8082/v1/messages"},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true}},
	}
	agentgateway := corev1.Container{
		Name: "agentgateway", Image: options.AgentGatewayImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{"--file", AgentGatewayConfigPath}, Resources: copyResources(gatewayResources),
		SecurityContext: regularSecurityContext(airlock.UIDGateway),
		VolumeMounts: []corev1.VolumeMount{
			{Name: GatewayConfigVolumeName, MountPath: GatewayConfigMountPath, ReadOnly: true},
			{Name: GatewayIdentityVolumeName, MountPath: GatewayIdentityMountPath, ReadOnly: true},
			{Name: GatewayTmpVolumeName, MountPath: GatewayTmpMountPath},
		},
	}
	mode := int32(0444)
	tokenMode := int32(0444)
	tokenExpiration := options.Gateway.TokenExpirationSeconds
	return corev1.PodSpec{
		HostUsers: boolPtr(false), AutomountServiceAccountToken: boolPtr(false), HostNetwork: false, HostPID: false, HostIPC: false,
		EnableServiceLinks: boolPtr(false), RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: int64Ptr(10),
		ServiceAccountName: options.Gateway.ServiceAccountName,
		DNSPolicy:          corev1.DNSClusterFirst, NodeSelector: map[string]string{NodeLabelKey: NodeLabelValue},
		Tolerations:     []corev1.Toleration{{Key: NodeLabelKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
		SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		InitContainers:  []corev1.Container{materializer, renderer, lockdown},
		Containers:      []corev1.Container{critic, agentgateway},
		Volumes: []corev1.Volume{
			{Name: InputVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: GatewayConfigVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
			{Name: GatewayTmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
			{Name: GatewayIdentityVolumeName, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: &tokenMode, Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: options.Gateway.JWTCapability.Audience, ExpirationSeconds: &tokenExpiration, Path: "token"}}}}}},
			{Name: ObjectStoreVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: artifactStoreSecretName, DefaultMode: &mode, Items: []corev1.KeyToPath{{Key: options.ArtifactStore.AccessKeyIDSecretKey, Path: "access-key-id", Mode: &mode}, {Key: options.ArtifactStore.SecretAccessKeyKey, Path: "secret-access-key", Mode: &mode}}}}},
		},
	}, nil
}

func rootInitSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0), RunAsNonRoot: boolPtr(false), AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
}

func regularSecurityContext(uid uint32) *corev1.SecurityContext {
	value := int64(uid)
	return &corev1.SecurityContext{RunAsUser: &value, RunAsGroup: &value, RunAsNonRoot: boolPtr(true), AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
}

func airlockSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0), RunAsNonRoot: boolPtr(false), AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}, Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
}

func validateBuildOptions(options BuildOptions) (BuildOptions, error) {
	if !resolved.ValidPinnedImage(options.CriticImage) {
		return BuildOptions{}, fmt.Errorf("%w: critic image must be digest-pinned", ErrInvalidOptions)
	}
	if err := options.AgentGatewayCapability.Validate(options.AgentGatewayImage); err != nil {
		return BuildOptions{}, err
	}
	if !resolved.ValidPinnedImage(options.LockdownImage) {
		return BuildOptions{}, fmt.Errorf("%w: lockdown image must be digest-pinned", ErrInvalidOptions)
	}
	if err := validateProductionGatewayBinding(options.Gateway); err != nil {
		return BuildOptions{}, err
	}
	if options.ArtifactStore.AccessKeyIDSecretKey == "" {
		options.ArtifactStore.AccessKeyIDSecretKey = artifactauth.AccessKeyIDKey
	}
	if options.ArtifactStore.SecretAccessKeyKey == "" {
		options.ArtifactStore.SecretAccessKeyKey = artifactauth.SecretAccessKeyKey
	}
	if options.ArtifactStore.SessionTokenSecretKey == "" {
		options.ArtifactStore.SessionTokenSecretKey = artifactauth.SessionTokenKey
	}
	if !validIdentity(options.ArtifactStore.AccessKeyIDSecretKey) || !validIdentity(options.ArtifactStore.SecretAccessKeyKey) || !validIdentity(options.ArtifactStore.SessionTokenSecretKey) {
		return BuildOptions{}, fmt.Errorf("%w: object-store Secret projection is invalid", ErrInvalidOptions)
	}
	if err := verifyfetch.ValidateStoreConfig(verifyfetch.StoreConfig{Endpoint: options.ArtifactStore.Endpoint, Region: options.ArtifactStore.Region, Bucket: options.ArtifactStore.Bucket, ForcePathStyle: options.ArtifactStore.ForcePathStyle, MaxObjectBytes: MaxPatchBytes}); err != nil {
		return BuildOptions{}, fmt.Errorf("%w: object-store config: %v", ErrInvalidOptions, err)
	}
	if options.ArtifactStore.Endpoint == "" || len(options.ArtifactStore.EgressCIDRs) == 0 {
		return BuildOptions{}, fmt.Errorf("%w: HTTPS endpoint and resolved egress CIDRs are required", ErrArtifactStoreBinding)
	}
	if _, err := httpsEndpointPort(options.ArtifactStore.Endpoint); err != nil {
		return BuildOptions{}, fmt.Errorf("%w: %v", ErrArtifactStoreBinding, err)
	}
	for index, rawCIDR := range options.ArtifactStore.EgressCIDRs {
		trimmed := strings.TrimSpace(rawCIDR)
		ip, network, err := net.ParseCIDR(trimmed)
		if err != nil || ip == nil || network.String() != trimmed {
			return BuildOptions{}, fmt.Errorf("%w: egress CIDR %d is not canonical", ErrArtifactStoreBinding, index)
		}
		ones, _ := network.Mask.Size()
		if ones == 0 {
			return BuildOptions{}, fmt.Errorf("%w: egress CIDR %d cannot be default route", ErrArtifactStoreBinding, index)
		}
	}
	options.ArtifactStore.EgressCIDRs = append([]string(nil), options.ArtifactStore.EgressCIDRs...)
	options.Gateway.PodSelector = copyStringMap(options.Gateway.PodSelector)
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = defaultMaxOutputBytes
	}
	if options.MaxOutputBytes <= 0 || options.MaxOutputBytes > MaxOutputBytes {
		return BuildOptions{}, fmt.Errorf("%w: max output bytes must be in 1..%d", ErrInvalidOptions, MaxOutputBytes)
	}
	if options.Timeout == 0 {
		options.Timeout = 20 * time.Minute
	}
	if options.Timeout < minTimeout || options.Timeout > maxTimeout {
		return BuildOptions{}, fmt.Errorf("%w: timeout must be in %s..%s", ErrInvalidOptions, minTimeout, maxTimeout)
	}
	return options, nil
}

func criticJobName(input Input) string {
	body, _ := CanonicalInputBytes(input)
	sum := sha256.Sum256(body)
	return "agw-critic-" + hex.EncodeToString(sum[:10])
}

func jobFingerprint(inputDigest, bindingDigest string, options BuildOptions) (string, error) {
	value := struct {
		InputDigest            string                 `json:"inputDigest"`
		BindingDigest          string                 `json:"bindingDigest"`
		CriticImage            string                 `json:"criticImage"`
		AgentGatewayImage      string                 `json:"agentgatewayImage"`
		LockdownImage          string                 `json:"lockdownImage"`
		AgentGatewayCapability AgentGatewayCapability `json:"agentgatewayCapability"`
		Gateway                GatewayClientOptions   `json:"gateway"`
		ArtifactStore          ArtifactStoreOptions   `json:"artifactStore"`
		MaxOutput              int64                  `json:"maxOutputBytes"`
		Timeout                int64                  `json:"timeoutSeconds"`
	}{InputDigest: inputDigest, BindingDigest: bindingDigest, CriticImage: options.CriticImage, AgentGatewayImage: options.AgentGatewayImage, LockdownImage: options.LockdownImage, AgentGatewayCapability: options.AgentGatewayCapability, Gateway: options.Gateway, ArtifactStore: options.ArtifactStore, MaxOutput: options.MaxOutputBytes, Timeout: int64(options.Timeout.Seconds())}
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func routeFamilies(route ModelRouteIdentity) string {
	values := make([]string, 0, len(route.Providers))
	for _, provider := range route.Providers {
		values = append(values, provider.Family)
	}
	return strings.Join(values, ",")
}

func shortIdentity(value string) string {
	if len(value) <= 63 {
		return value
	}
	return value[:63]
}

func typesUID(value string) types.UID { return types.UID(value) }

func boolPtr(value bool) *bool                                               { return &value }
func int32Ptr(value int32) *int32                                            { return &value }
func int64Ptr(value int64) *int64                                            { return &value }
func completionModePtr(value batchv1.CompletionMode) *batchv1.CompletionMode { return &value }

func copyStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func copyResources(input corev1.ResourceRequirements) corev1.ResourceRequirements {
	var output corev1.ResourceRequirements
	input.DeepCopyInto(&output)
	return output
}
