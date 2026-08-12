package criticworkload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
)

var (
	ErrRunnerUnavailable = errors.New("critic runner is temporarily unavailable")
	ErrJobNotReady       = errors.New("critic Job is not ready")
	ErrOutputNotReady    = errors.New("critic output is not ready")
	ErrJobFailed         = errors.New("critic Job failed")
	ErrPodIdentity       = errors.New("critic output Pod identity is invalid")
)

// LogReader is intentionally narrow. A production implementation must read
// logs through an authenticated Kubernetes client and enforce maxBytes before
// returning data to the parser.
type LogReader interface {
	Read(context.Context, string, string, string, int64) ([]byte, error)
}

// KubernetesLogReader reads one bounded critic container log stream.
type KubernetesLogReader struct {
	Client kubernetes.Interface
}

// Read implements LogReader. It does not decode or interpret the frame.
func (r KubernetesLogReader) Read(ctx context.Context, namespace, pod, container string, maxBytes int64) ([]byte, error) {
	if r.Client == nil || ctx == nil || namespace == "" || pod == "" || container == "" || maxBytes <= 0 || maxBytes > MaxFrameBytes {
		return nil, ErrRunnerUnavailable
	}
	stream, err := r.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: false, Timestamps: false}).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrOutputNotReady
		}
		return nil, fmt.Errorf("%w: read critic logs: %v", ErrRunnerUnavailable, err)
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read critic logs: %v", ErrRunnerUnavailable, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrOutputOversized
	}
	return body, nil
}

// RunnerStatus is the bounded status returned by Ensure. InputRef is present
// only after the authenticated Job/Pod output has been persisted.
type RunnerStatus struct {
	ID       string
	Ready    bool
	Finished bool
	Failed   bool
	Job      JobIdentity
	Pod      *PodIdentity
	InputRef *v1alpha1.ArtifactRef
}

// RunnerOptions configures the controller-owned Job runner. The object store
// is used by this trusted runner after Kubernetes producer authentication. The
// critic Job's input-materializer receives only the scoped artifact Secret; the
// critic and agentgateway containers do not.
type RunnerOptions struct {
	Client client.Client
	Logs   LogReader
	Store  artifacts.Store
	Auth   RecordAuthenticator
	Build  BuildOptions
	// BuildPlan is an internal pure-builder seam used by contract tests. When it
	// is nil, the production two-container Build path is used; it is never
	// silently replaced with agw-broker or a metadata-only Job.
	BuildPlan func(Input, BuildOptions) (Plan, error)
	Clock     func() time.Time
}

// Runner creates/observes one immutable Job and publishes its raw canonical
// input as a content-addressed object plus an authenticated output record.
type Runner struct {
	client client.Client
	logs   LogReader
	store  artifacts.Store
	auth   RecordAuthenticator
	build  BuildOptions
	plan   func(Input, BuildOptions) (Plan, error)
	clock  func() time.Time
}

// NewRunner validates dependencies without contacting Kubernetes or storage.
func NewRunner(options RunnerOptions) (*Runner, error) {
	if options.Client == nil || options.Logs == nil || options.Store == nil || options.Auth == nil {
		return nil, fmt.Errorf("%w: client, logs, store, and record authenticator are required", ErrInvalidOptions)
	}
	planBuilder := options.BuildPlan
	if planBuilder == nil {
		planBuilder = Build
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	build, err := validateBuildOptions(options.Build)
	if err != nil {
		return nil, err
	}
	return &Runner{client: options.Client, logs: options.Logs, store: options.Store, auth: options.Auth, build: build, plan: planBuilder, clock: options.Clock}, nil
}

// Ensure is create-once/read-back and idempotent for one immutable contract.
// A completed Job is not accepted as critic evidence until its Pod identity,
// bounded output frame, immutable object, and output record all validate.
func (r *Runner) Ensure(ctx context.Context, input Input) (RunnerStatus, error) {
	if r == nil || r.client == nil || r.logs == nil || r.store == nil || r.auth == nil || r.plan == nil || ctx == nil {
		return RunnerStatus{}, ErrRunnerUnavailable
	}
	plan, err := r.plan(input, r.build)
	if err != nil {
		return RunnerStatus{}, err
	}
	if err := r.ensureNetworkPolicy(ctx, plan); err != nil {
		return RunnerStatus{}, err
	}
	job, err := r.ensureJob(ctx, plan)
	if err != nil {
		return RunnerStatus{}, err
	}
	status := RunnerStatus{ID: job.Name, Ready: true, Job: JobIdentity{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)}}
	if job.Status.Failed > 0 {
		status.Finished, status.Failed = true, true
		return status, nil
	}
	if job.Status.Succeeded <= 0 {
		return status, nil
	}
	status.Finished = true
	if job.UID == "" {
		return RunnerStatus{}, fmt.Errorf("%w: completed Job has no UID", ErrJobIdentity)
	}
	pod, err := r.outputPod(ctx, job, plan)
	if err != nil {
		return status, err
	}
	podIdentity := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}
	status.Pod = &podIdentity
	maxFrame, err := MaxEncodedFrameBytes(r.maxOutputBytes())
	if err != nil {
		return RunnerStatus{}, err
	}
	frame, err := r.logs.Read(ctx, pod.Namespace, pod.Name, "critic", maxFrame)
	if err != nil {
		return status, err
	}
	rawInput, err := DecodeOutputFrame(frame, r.maxOutputBytes())
	if err != nil {
		return RunnerStatus{}, err
	}
	ref, err := r.persistOutput(ctx, plan, job, pod, rawInput)
	if err != nil {
		return RunnerStatus{}, err
	}
	status.InputRef = &ref
	return status, nil
}

func (r *Runner) ensureNetworkPolicy(ctx context.Context, plan Plan) error {
	if plan.NetworkPolicy == nil {
		return nil
	}
	key := client.ObjectKey{Namespace: plan.NetworkPolicy.Namespace, Name: plan.NetworkPolicy.Name}
	var current networkingv1.NetworkPolicy
	err := r.client.Get(ctx, key, &current)
	if err == nil {
		return validateNetworkPolicyForPlan(&current, plan.NetworkPolicy)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: get critic NetworkPolicy: %v", ErrRunnerUnavailable, err)
	}
	if err := r.client.Create(ctx, plan.NetworkPolicy.DeepCopy()); err == nil {
		if err := r.client.Get(ctx, key, &current); err != nil {
			return fmt.Errorf("%w: read critic NetworkPolicy after create: %v", ErrRunnerUnavailable, err)
		}
		return validateNetworkPolicyForPlan(&current, plan.NetworkPolicy)
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%w: create critic NetworkPolicy: %v", ErrRunnerUnavailable, err)
	}
	if err := r.client.Get(ctx, key, &current); err != nil {
		return fmt.Errorf("%w: read critic NetworkPolicy after create race: %v", ErrRunnerUnavailable, err)
	}
	return validateNetworkPolicyForPlan(&current, plan.NetworkPolicy)
}

func validateNetworkPolicyForPlan(current, expected *networkingv1.NetworkPolicy) error {
	if current == nil || expected == nil || current.Namespace != expected.Namespace || current.Name != expected.Name {
		return fmt.Errorf("%w: NetworkPolicy identity", ErrJobConflict)
	}
	if !reflect.DeepEqual(current.Labels, expected.Labels) || !reflect.DeepEqual(current.Annotations, expected.Annotations) || !reflect.DeepEqual(current.OwnerReferences, expected.OwnerReferences) {
		return fmt.Errorf("%w: NetworkPolicy metadata", ErrJobConflict)
	}
	if !apiequality.Semantic.DeepEqual(current.Spec, expected.Spec) {
		return fmt.Errorf("%w: NetworkPolicy egress rules", ErrJobConflict)
	}
	return nil
}

func (r *Runner) ensureJob(ctx context.Context, plan Plan) (*batchv1.Job, error) {
	key := client.ObjectKey{Namespace: plan.Job.Namespace, Name: plan.Job.Name}
	var current batchv1.Job
	err := r.client.Get(ctx, key, &current)
	if err == nil {
		if err := validateJobForPlan(&current, plan); err != nil {
			return nil, err
		}
		return &current, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: get Job: %v", ErrRunnerUnavailable, err)
	}
	if err := r.client.Create(ctx, plan.Job.DeepCopy()); err == nil {
		if err := r.client.Get(ctx, key, &current); err != nil {
			return nil, fmt.Errorf("%w: read Job after create: %v", ErrRunnerUnavailable, err)
		}
		if err := validateJobForPlan(&current, plan); err != nil {
			return nil, err
		}
		return &current, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("%w: create Job: %v", ErrRunnerUnavailable, err)
	}
	if err := r.client.Get(ctx, key, &current); err != nil {
		return nil, fmt.Errorf("%w: read Job after create race: %v", ErrRunnerUnavailable, err)
	}
	if err := validateJobForPlan(&current, plan); err != nil {
		return nil, err
	}
	return &current, nil
}

func validateJobForPlan(job *batchv1.Job, plan Plan) error {
	if job == nil || job.Namespace != plan.Job.Namespace || job.Name != plan.Job.Name || job.Annotations == nil || job.Annotations[JobSpecFingerprintAnnotationKey] != plan.SpecFingerprint {
		return fmt.Errorf("%w: top-level identity or fingerprint", ErrJobConflict)
	}
	if !reflect.DeepEqual(job.Annotations, plan.Job.Annotations) {
		return fmt.Errorf("%w: annotations", ErrJobConflict)
	}
	if !reflect.DeepEqual(job.Labels, plan.Job.Labels) || len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != types.UID(plan.Input.Run.UID) || job.OwnerReferences[0].Name != plan.Input.Run.Name || job.OwnerReferences[0].Kind != "AgentRun" || job.OwnerReferences[0].Controller == nil || !*job.OwnerReferences[0].Controller {
		return fmt.Errorf("%w: labels or owner reference", ErrJobIdentity)
	}
	if job.Spec.Completions == nil || *job.Spec.Completions != 1 || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || plan.Job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != *plan.Job.Spec.ActiveDeadlineSeconds || job.Spec.ManualSelector == nil || !*job.Spec.ManualSelector || job.Spec.Selector == nil || !reflect.DeepEqual(job.Spec.Selector.MatchLabels, plan.Job.Spec.Selector.MatchLabels) || job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.NonIndexedCompletion {
		return fmt.Errorf("%w: lifecycle or selector", ErrJobConflict)
	}
	if err := validateAuthenticatedJobShape(job); err != nil {
		return err
	}
	if plan.ArtifactStoreSecretName != "" {
		expectedSecretName, err := verifyArtifactStoreSecretName(plan.Input)
		if err != nil || plan.ArtifactStoreSecretName != expectedSecretName {
			return fmt.Errorf("%w: immutable verify Secret derivation", ErrJobConflict)
		}
		if err := validateArtifactStoreSecretBinding(job.Spec.Template.Spec, expectedSecretName); err != nil {
			return err
		}
	}
	if err := validatePodTemplate(&job.Spec.Template, plan); err != nil {
		return err
	}
	return nil
}

func validateArtifactStoreSecretBinding(pod corev1.PodSpec, expectedName string) error {
	if expectedName == "" {
		return fmt.Errorf("%w: verify Secret name is empty", ErrJobConflict)
	}
	volumeMatches := 0
	for _, volume := range pod.Volumes {
		if volume.Name != ObjectStoreVolumeName {
			continue
		}
		volumeMatches++
		if volume.Secret == nil || volume.Secret.SecretName != expectedName {
			return fmt.Errorf("%w: object-store Secret volume is not the immutable verify Secret", ErrJobConflict)
		}
	}
	if volumeMatches != 1 {
		return fmt.Errorf("%w: expected exactly one object-store Secret volume", ErrJobConflict)
	}
	sessionMatches := 0
	for _, container := range pod.InitContainers {
		for _, env := range container.Env {
			if env.Name != CriticObjectStoreSessionTokenEnv {
				continue
			}
			sessionMatches++
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != expectedName {
				return fmt.Errorf("%w: object-store session credential is not the immutable verify Secret", ErrJobConflict)
			}
		}
	}
	if sessionMatches != 1 {
		return fmt.Errorf("%w: expected exactly one object-store session credential reference", ErrJobConflict)
	}
	return nil
}

func validateAuthenticatedJobShape(job *batchv1.Job) error {
	if job == nil {
		return ErrJobIdentity
	}
	return validatePodRuntimeShape(job.Spec.Template.Spec)
}

func validatePodRuntimeShape(pod corev1.PodSpec) error {
	if pod.HostUsers == nil || *pod.HostUsers || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.ShareProcessNamespace != nil && *pod.ShareProcessNamespace || pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.RestartPolicy != corev1.RestartPolicyNever || pod.DNSConfig != nil || len(pod.HostAliases) != 0 || len(pod.EphemeralContainers) != 0 || pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || len(pod.NodeSelector) != 1 || pod.NodeSelector[NodeLabelKey] != NodeLabelValue || !hasRequiredToleration(pod.Tolerations) {
		return ErrJobIdentity
	}
	if len(pod.InitContainers) == 0 && len(pod.Containers) == 1 && len(pod.Volumes) == 0 && pod.DNSPolicy == corev1.DNSNone {
		return validateLegacyPodRuntimeShape(pod)
	}
	if pod.DNSPolicy != corev1.DNSClusterFirst || len(pod.InitContainers) != 3 || len(pod.Containers) != 2 || len(pod.Volumes) != 5 {
		return ErrJobIdentity
	}
	return validateProductionPodRuntimeShape(pod)
}

func validateLegacyPodRuntimeShape(pod corev1.PodSpec) error {
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 1000 || pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup != 1000 {
		return ErrJobIdentity
	}
	container := pod.Containers[0]
	if !validRegularContainer(container, "critic", 1000, Entrypoint) || len(container.VolumeMounts) != 0 || hasSecretEnv(container) {
		return ErrJobIdentity
	}
	return nil
}

func validateProductionPodRuntimeShape(pod corev1.PodSpec) error {
	if pod.SecurityContext.RunAsNonRoot != nil || pod.SecurityContext.RunAsUser != nil || pod.SecurityContext.RunAsGroup != nil || !validPathSegment(pod.ServiceAccountName) {
		return ErrJobIdentity
	}
	if pod.InitContainers[0].Name != "input-materializer" || pod.InitContainers[1].Name != "gateway-config" || pod.InitContainers[2].Name != "airlock" || pod.Containers[0].Name != Role || pod.Containers[1].Name != "agentgateway" {
		return ErrJobIdentity
	}
	if !validRootInitContainer(pod.InitContainers[0], Entrypoint) || !validRootInitContainer(pod.InitContainers[1], Entrypoint) || !validAirlockContainer(pod.InitContainers[2]) {
		return ErrJobIdentity
	}
	if !validRegularContainer(pod.Containers[0], Role, 1000, Entrypoint) || !validRegularContainer(pod.Containers[1], "agentgateway", 1338, "") {
		return ErrJobIdentity
	}
	critic := pod.Containers[0]
	if len(critic.VolumeMounts) != 1 || critic.VolumeMounts[0].Name != InputVolumeName || !critic.VolumeMounts[0].ReadOnly || hasSecretEnv(critic) {
		return ErrJobIdentity
	}
	gateway := pod.Containers[1]
	if len(gateway.VolumeMounts) != 3 || gateway.VolumeMounts[0].Name != GatewayConfigVolumeName || !gateway.VolumeMounts[0].ReadOnly || gateway.VolumeMounts[1].Name != GatewayIdentityVolumeName || !gateway.VolumeMounts[1].ReadOnly || gateway.VolumeMounts[2].Name != GatewayTmpVolumeName || countSecretEnvs(gateway) != 0 {
		return ErrJobIdentity
	}
	if len(pod.InitContainers[0].VolumeMounts) != 3 || pod.InitContainers[0].VolumeMounts[0].Name != InputVolumeName || pod.InitContainers[0].VolumeMounts[0].ReadOnly || pod.InitContainers[0].VolumeMounts[1].Name != ObjectStoreVolumeName || pod.InitContainers[0].VolumeMounts[2].Name != ObjectStoreVolumeName || len(pod.InitContainers[1].VolumeMounts) != 1 || pod.InitContainers[1].VolumeMounts[0].Name != GatewayConfigVolumeName || len(pod.InitContainers[2].VolumeMounts) != 0 {
		return ErrJobIdentity
	}
	if !validGatewayIdentityVolume(pod.Volumes) {
		return ErrJobIdentity
	}
	return nil
}

func validGatewayIdentityVolume(volumes []corev1.Volume) bool {
	for _, volume := range volumes {
		if volume.Name != GatewayIdentityVolumeName || volume.Projected == nil || volume.Projected.DefaultMode == nil || *volume.Projected.DefaultMode != 0444 || len(volume.Projected.Sources) != 1 || volume.Projected.Sources[0].ServiceAccountToken == nil {
			continue
		}
		token := volume.Projected.Sources[0].ServiceAccountToken
		return token.Path == "token" && validModelText(token.Audience) && token.ExpirationSeconds != nil && *token.ExpirationSeconds >= 600 && *token.ExpirationSeconds <= 3600
	}
	return false
}

func validRegularContainer(container corev1.Container, name string, uid int64, command string) bool {
	if container.Name != name || container.SecurityContext == nil || container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot || container.SecurityContext.RunAsUser == nil || *container.SecurityContext.RunAsUser != uid || container.SecurityContext.RunAsGroup == nil || *container.SecurityContext.RunAsGroup != uid || container.SecurityContext.SeccompProfile == nil || container.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Add) != 0 || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		return false
	}
	if command != "" && (len(container.Command) != 1 || container.Command[0] != command) {
		return false
	}
	return true
}

func validRootInitContainer(container corev1.Container, command string) bool {
	return len(container.Command) == 1 && container.Command[0] == command && container.SecurityContext != nil && container.SecurityContext.RunAsNonRoot != nil && !*container.SecurityContext.RunAsNonRoot && container.SecurityContext.RunAsUser != nil && *container.SecurityContext.RunAsUser == 0 && container.SecurityContext.RunAsGroup != nil && *container.SecurityContext.RunAsGroup == 0 && container.SecurityContext.AllowPrivilegeEscalation != nil && !*container.SecurityContext.AllowPrivilegeEscalation && container.SecurityContext.ReadOnlyRootFilesystem != nil && *container.SecurityContext.ReadOnlyRootFilesystem && container.SecurityContext.Capabilities != nil && len(container.SecurityContext.Capabilities.Add) == 0 && reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"})
}

func validAirlockContainer(container corev1.Container) bool {
	return len(container.Command) == 2 && container.Command[0] == LockdownEntrypoint && container.Command[1] == "-ceu" && len(container.Args) == 1 && container.Args[0] != "" && container.SecurityContext != nil && container.SecurityContext.RunAsNonRoot != nil && !*container.SecurityContext.RunAsNonRoot && container.SecurityContext.RunAsUser != nil && *container.SecurityContext.RunAsUser == 0 && container.SecurityContext.RunAsGroup != nil && *container.SecurityContext.RunAsGroup == 0 && container.SecurityContext.AllowPrivilegeEscalation != nil && !*container.SecurityContext.AllowPrivilegeEscalation && container.SecurityContext.ReadOnlyRootFilesystem != nil && *container.SecurityContext.ReadOnlyRootFilesystem && container.SecurityContext.Capabilities != nil && reflect.DeepEqual(container.SecurityContext.Capabilities.Add, []corev1.Capability{"NET_ADMIN", "NET_RAW"}) && reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"})
}

func hasSecretEnv(container corev1.Container) bool {
	for _, env := range container.Env {
		if env.ValueFrom != nil {
			return true
		}
	}
	return false
}

func countSecretEnvs(container corev1.Container) int {
	count := 0
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			count++
		}
	}
	return count
}

func hasRequiredToleration(tolerations []corev1.Toleration) bool {
	for _, toleration := range tolerations {
		if toleration.Key == NodeLabelKey && toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func validatePodTemplate(template *corev1.PodTemplateSpec, plan Plan) error {
	if template == nil || !reflect.DeepEqual(template.Labels, plan.Job.Spec.Template.Labels) || !reflect.DeepEqual(template.Annotations, plan.Job.Spec.Template.Annotations) {
		return fmt.Errorf("%w: pod template metadata", ErrJobConflict)
	}
	pod := template.Spec
	expected := plan.Job.Spec.Template.Spec
	if err := validatePodRuntimeShape(pod); err != nil || !podSpecCriticalEqual(pod, expected) {
		return fmt.Errorf("%w: pod topology or security", ErrJobConflict)
	}
	return nil
}

func podSpecCriticalEqual(actual, expected corev1.PodSpec) bool {
	actual = normalizePodSpecForAuthentication(actual)
	expected = normalizePodSpecForAuthentication(expected)
	// Semantic.DeepEqual compares the complete typed PodSpec, including every
	// init/regular container field, volume source (including projected JWT
	// details), mount, environment reference, resource, and security field.
	// It is intentionally not a hand-picked security-field comparison.
	return apiequality.Semantic.DeepEqual(actual, expected)
}

// normalizePodSpecForAuthentication removes only values Kubernetes is allowed
// to add/change while creating and scheduling a Pod. It must stay an explicit
// allowlist: anything not listed here is part of the immutable Job template.
func normalizePodSpecForAuthentication(input corev1.PodSpec) corev1.PodSpec {
	pod := *input.DeepCopy()

	// The scheduler writes this after the Job template has been copied.
	pod.NodeName = ""
	// These are API/admission defaults when omitted from a Pod template.
	if pod.SchedulerName == corev1.DefaultSchedulerName {
		pod.SchedulerName = ""
	}
	if pod.Priority != nil && *pod.Priority == 0 {
		pod.Priority = nil
	}
	if pod.PreemptionPolicy != nil && *pod.PreemptionPolicy == corev1.PreemptLowerPriority {
		pod.PreemptionPolicy = nil
	}
	if pod.ShareProcessNamespace != nil && !*pod.ShareProcessNamespace {
		pod.ShareProcessNamespace = nil
	}
	if pod.SetHostnameAsFQDN != nil && !*pod.SetHostnameAsFQDN {
		pod.SetHostnameAsFQDN = nil
	}
	pod.Tolerations = removeDefaultPodTolerations(pod.Tolerations)

	normalizeContainersForAuthentication(pod.InitContainers)
	normalizeContainersForAuthentication(pod.Containers)
	return pod
}

func normalizeContainersForAuthentication(containers []corev1.Container) {
	for index := range containers {
		container := &containers[index]
		if container.TerminationMessagePath == corev1.TerminationMessagePathDefault {
			container.TerminationMessagePath = ""
		}
		if container.TerminationMessagePolicy == corev1.TerminationMessageReadFile {
			container.TerminationMessagePolicy = ""
		}
		for portIndex := range container.Ports {
			if container.Ports[portIndex].Protocol == corev1.ProtocolTCP {
				container.Ports[portIndex].Protocol = ""
			}
		}
		for mountIndex := range container.VolumeMounts {
			mount := &container.VolumeMounts[mountIndex]
			if mount.MountPropagation != nil && *mount.MountPropagation == corev1.MountPropagationNone {
				mount.MountPropagation = nil
			}
			if mount.RecursiveReadOnly != nil && *mount.RecursiveReadOnly == corev1.RecursiveReadOnlyDisabled {
				mount.RecursiveReadOnly = nil
			}
		}
	}
}

func removeDefaultPodTolerations(input []corev1.Toleration) []corev1.Toleration {
	output := make([]corev1.Toleration, 0, len(input))
	for _, toleration := range input {
		if isDefaultPodToleration(toleration) {
			continue
		}
		output = append(output, toleration)
	}
	return output
}

func isDefaultPodToleration(toleration corev1.Toleration) bool {
	if toleration.Operator != corev1.TolerationOpExists || toleration.Effect != corev1.TaintEffectNoExecute || toleration.TolerationSeconds == nil || *toleration.TolerationSeconds != 300 {
		return false
	}
	return toleration.Key == "node.kubernetes.io/not-ready" || toleration.Key == "node.kubernetes.io/unreachable"
}

func (r *Runner) outputPod(ctx context.Context, job *batchv1.Job, plan Plan) (*corev1.Pod, error) {
	if job == nil || job.UID == "" {
		return nil, ErrJobIdentity
	}
	var pods corev1.PodList
	if err := r.client.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{jobControllerUIDLabel: string(job.UID)}); err != nil {
		return nil, fmt.Errorf("%w: list critic output pods: %v", ErrRunnerUnavailable, err)
	}
	if len(pods.Items) == 0 {
		return nil, ErrOutputNotReady
	}
	if len(pods.Items) != 1 {
		return nil, ErrDuplicateOutput
	}
	pod := &pods.Items[0]
	if err := validateOutputPod(pod, job, plan); err != nil {
		return nil, err
	}
	return pod, nil
}

func validateOutputPod(pod *corev1.Pod, job *batchv1.Job, plan Plan) error {
	if pod == nil || job == nil || pod.Namespace != job.Namespace || pod.UID == "" || pod.Status.Phase != corev1.PodSucceeded || pod.Labels["agents.astatide.com/role"] != Role || pod.Annotations[ContractDigestAnnotationKey] != plan.InputDigest || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Job" || pod.OwnerReferences[0].UID != job.UID || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller || validatePodRuntimeShape(pod.Spec) != nil || !podSpecCriticalEqual(pod.Spec, job.Spec.Template.Spec) {
		return ErrPodIdentity
	}
	return nil
}

func (r *Runner) persistOutput(ctx context.Context, plan Plan, job *batchv1.Job, pod *corev1.Pod, rawInput []byte) (v1alpha1.ArtifactRef, error) {
	key := plan.OutputKeyPrefix + "/" + string(job.UID) + "/input.json"
	ref, err := r.putImmutable(ctx, key, rawInput)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	record := OutputRecord{
		SchemaVersion: RecordSchemaVersion, Input: plan.Input, InputDigest: plan.InputDigest, BindingDigest: plan.BindingDigest,
		Job:    JobIdentity{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)},
		Pod:    PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)},
		Object: ObjectIdentity{Key: key, URI: ref.URI, Digest: ref.Digest, SizeBytes: ref.SizeBytes}, Artifact: ref,
	}
	body, err := CanonicalRecordBytes(record)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	recordKey := key + ".record.json"
	created, _, err := r.store.Put(ctx, recordKey, append([]byte(nil), body...), "application/vnd.agents-gateway.critic-output-record.v1alpha1+json")
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: persist critic output record: %v", ErrRunnerUnavailable, err)
	}
	if !created {
		existing, err := r.store.Get(ctx, recordKey)
		if err != nil || !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrOutputIdentity
		}
	}
	signature, err := r.auth.Sign(body)
	if err != nil || len(signature) == 0 || len(signature) > MaxSignatureBytes {
		return v1alpha1.ArtifactRef{}, ErrAuthentication
	}
	signatureKey := recordKey + ".sig"
	signatureCreated, _, err := r.store.Put(ctx, signatureKey, append([]byte(nil), signature...), "application/vnd.agents-gateway.critic-output-record-signature")
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: persist critic output signature: %v", ErrRunnerUnavailable, err)
	}
	if !signatureCreated {
		existing, err := r.store.Get(ctx, signatureKey)
		if err != nil || !bytes.Equal(existing, signature) {
			return v1alpha1.ArtifactRef{}, ErrOutputIdentity
		}
	}
	return ref, nil
}

func (r *Runner) putImmutable(ctx context.Context, key string, body []byte) (v1alpha1.ArtifactRef, error) {
	if len(body) == 0 || int64(len(body)) > r.maxOutputBytes() || key == "" {
		return v1alpha1.ArtifactRef{}, ErrOutputOversized
	}
	digest := digestBytes(body)
	created, uri, err := r.store.Put(ctx, key, append([]byte(nil), body...), InputMediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("%w: persist critic input: %v", ErrRunnerUnavailable, err)
	}
	if !safeURI(uri) {
		return v1alpha1.ArtifactRef{}, ErrArtifactConflict
	}
	if !created {
		existing, err := r.store.Get(ctx, key)
		if err != nil || !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrOutputIdentity
		}
	}
	return v1alpha1.ArtifactRef{URI: uri, Digest: digest, Kind: InputKind, Name: InputName, MediaType: InputMediaType, SizeBytes: int64(len(body))}, nil
}

func (r *Runner) maxOutputBytes() int64 {
	max := r.build.MaxOutputBytes
	if max == 0 {
		max = MaxOutputBytes
	}
	return max
}

const jobControllerUIDLabel = "batch.kubernetes.io/controller-uid"

// ValidateExistingOutputPod is exported for the authenticated source and
// tests; it performs the same producer identity checks used by Runner.
func ValidateExistingOutputPod(pod *corev1.Pod, job *batchv1.Job, plan Plan) error {
	return validateOutputPod(pod, job, plan)
}
