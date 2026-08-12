// Package capture builds and validates the networkless patch-capture phase.
//
// Capture is deliberately separate from both the agent Sandbox and the Gate
// verifier. It receives only the work PVC, reconstructs the change from the
// recorded base SHA, and emits a bounded stdout frame. The controller reads
// that frame through Kubernetes' pod-log API, validates it, and is the only
// component that calls ArtifactSink. The capture pod never receives object
// store credentials, a Secret volume, or a service-account token.
package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	Role = "capture"

	NodeLabelKey   = workload.NodeLabelKey
	NodeLabelValue = workload.NodeLabelValue

	WorkspaceVolumeName = "workspace"
	WorkspaceMountPath  = "/workspace"
	RepoPath            = WorkspaceMountPath + "/repo"
	OutputDir           = WorkspaceMountPath + "/.agw/capture"
	PatchPath           = OutputDir + "/patch.diff"
	ManifestPath        = OutputDir + "/patch-manifest.json"
	ResultPath          = OutputDir + "/result.json"

	CaptureEntrypoint = "/agw/capture"
	OutputProtocol    = "stdout-frame-v2"

	EgressLabelKey   = "agents.astatide.com/egress"
	EgressLabelValue = "none"

	ResultSchemaVersion = "agents.astatide.com/capture/v1alpha2"
	PatchMediaType      = "text/x-diff"
	ManifestMediaType   = "application/vnd.agents-gateway.patch-manifest.v1+json"

	DefaultTimeout          = 5 * time.Minute
	MaxTimeout              = 30 * time.Minute
	DefaultMaxPatchBytes    = int64(64 << 20)
	HardMaxPatchBytes       = int64(1 << 30)
	DefaultMaxResultBytes   = int64(128 << 10)
	HardMaxResultBytes      = int64(1 << 20)
	DefaultMaxManifestBytes = int64(publish.MaxPatchManifestBytes)
	HardMaxManifestBytes    = int64(publish.MaxPatchManifestBytes)

	defaultCaptureCPURequest    = "10m"
	defaultCaptureMemoryRequest = "64Mi"
	defaultCaptureCPULimit      = "500m"
	defaultCaptureMemoryLimit   = "512Mi"

	maxRunUIDBytes   = 63
	maxImageBytes    = 512
	maxClaimNameSize = 253
	maxBaseSHABytes  = 64
	maxSpecJSONBytes = 128 << 10

	maxArtifactURIBytes = 1024
)

var (
	ErrInvalidInput         = errors.New("capture: invalid input")
	ErrInvalidPlan          = errors.New("capture: invalid plan")
	ErrInvalidResult        = errors.New("capture: invalid result")
	ErrInvalidPatch         = errors.New("capture: invalid patch")
	ErrPathEvidence         = errors.New("capture: invalid path evidence")
	ErrScopeViolation       = errors.New("capture: patch violates scope")
	ErrBinaryPatch          = errors.New("capture: binary patch is not allowed")
	ErrDiffLimit            = errors.New("capture: diff exceeds configured bound")
	ErrDigestMismatch       = errors.New("capture: digest mismatch")
	ErrArtifactConflict     = errors.New("capture: immutable artifact conflicts with existing content")
	ErrInvalidArtifact      = errors.New("capture: invalid artifact")
	ErrInvalidArtifactInput = errors.New("capture: validated output is required")
	ErrOutputUnavailable    = errors.New("capture: output is unavailable")
	ErrInvalidFrame         = errors.New("capture: invalid output frame")
	ErrNonCanonicalResult   = errors.New("capture: result is not canonical JSON")
)

var (
	githubRepoPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern        = regexp.MustCompile(`^[0-9a-f]+$`)
)

// Options are controller-resolved values that do not belong in the user
// authored AgentRun snapshot. The image must be digest pinned and the PVC
// name must be the PVC created for the work Sandbox.
type Options struct {
	Image              string
	WorkspaceClaimName string
	Timeout            time.Duration
	MaxPatchBytes      int64
	MaxResultBytes     int64
	MaxManifestBytes   int64
}

// OutputContract describes the only output channel exposed by the capture
// image. ResultPath, PatchPath, and ManifestPath are staging paths inside the
// one work PVC; the controller does not read them directly. The image verifies
// regular files and emits EncodeOutputFrame(resultJSON, patch, manifest) to
// stdout. Kubernetes pod logs are retrieved without timestamps.
type OutputContract struct {
	Protocol         string
	ResultPath       string
	PatchPath        string
	ManifestPath     string
	MaxFrameBytes    int64
	MaxResultBytes   int64
	MaxPatchBytes    int64
	MaxManifestBytes int64
}

// Plan contains the two Kubernetes resources needed for capture. The
// NetworkPolicy is explicit even when the namespace already has a default
// deny policy, so the no-egress property travels with the child plan.
type Plan struct {
	Job           *batchv1.Job
	NetworkPolicy *networkingv1.NetworkPolicy
	SpecDigest    string
	JobName       string
	Output        OutputContract
}

// ResultEnvelope is the bounded machine-readable record emitted with a patch.
// ChangedPaths is sorted and unique. LinesChanged counts added and deleted
// content lines, excluding the ---/+++ file headers. PatchDigest is sha256 of
// the exact patch bytes, including line endings.
type ResultEnvelope struct {
	SchemaVersion  string   `json:"schemaVersion"`
	RunUID         string   `json:"runUID"`
	SpecDigest     string   `json:"specDigest"`
	BaseSHA        string   `json:"baseSHA"`
	PatchDigest    string   `json:"patchDigest"`
	PatchBytes     int64    `json:"patchBytes"`
	ManifestDigest string   `json:"manifestDigest"`
	ManifestBytes  int64    `json:"manifestBytes"`
	FilesChanged   int64    `json:"filesChanged"`
	LinesChanged   int64    `json:"linesChanged"`
	HasBinaryFiles bool     `json:"hasBinaryFiles"`
	ChangedPaths   []string `json:"changedPaths"`
}

// CapturedOutput is the decoded stdout-frame payload returned by an
// OutputReader. All three byte slices are copied before validation.
type CapturedOutput struct {
	ResultJSON []byte
	Patch      []byte
	Manifest   []byte
}

// PatchEvidence is independently derived from the patch bytes by the
// controller. It is intentionally not taken on trust from ResultEnvelope.
type PatchEvidence struct {
	ChangedPaths   []string
	LinesChanged   int64
	HasBinaryFiles bool
}

// ValidatedOutput is produced only by Validate. The private marker prevents a
// caller from manufacturing an accepted result and passing it directly to
// Persist. Byte and file accessors return defensive copies.
type ValidatedOutput struct {
	Envelope         ResultEnvelope
	Evidence         PatchEvidence
	patch            []byte
	manifest         []byte
	files            []publish.FileChange
	manifestArtifact *v1alpha1.ArtifactRef
	binding          string
	valid            bool
}

func (v ValidatedOutput) PatchBytes() []byte { return append([]byte(nil), v.patch...) }

func (v ValidatedOutput) ManifestBytes() []byte { return append([]byte(nil), v.manifest...) }

func (v ValidatedOutput) Files() []publish.FileChange { return cloneFileChanges(v.files) }

func (v ValidatedOutput) ManifestArtifact() *v1alpha1.ArtifactRef {
	if v.manifestArtifact == nil {
		return nil
	}
	ref := *v.manifestArtifact
	return &ref
}

// WithManifestArtifact binds the controller-persisted manifest reference to
// the validated value carried through CaptureOutcome without making the
// artifact reference part of the untrusted frame.
func (v ValidatedOutput) WithManifestArtifact(ref v1alpha1.ArtifactRef) (ValidatedOutput, error) {
	if !v.valid || ref.Digest != v.Envelope.ManifestDigest || ref.Kind != "patch-manifest" || ref.Name != "patch-manifest.json" || ref.MediaType != ManifestMediaType || ref.SizeBytes != int64(len(v.manifest)) || !safeArtifactURI(ref.URI) {
		return ValidatedOutput{}, ErrInvalidArtifact
	}
	copyRef := ref
	v.manifestArtifact = &copyRef
	return v, nil
}

// PersistedArtifacts are the two controller-owned immutable capture objects.
type PersistedArtifacts struct {
	Patch    v1alpha1.ArtifactRef
	Manifest v1alpha1.ArtifactRef
}

// ValidationLimits bounds the controller-side output reader and validator.
// Zero values select safe defaults. MaxPatchBytes can only tighten the
// AgentRun's own MaxPatchBytes limit; it can never widen it.
type ValidationLimits struct {
	MaxPatchBytes    int64
	MaxResultBytes   int64
	MaxManifestBytes int64
}

// ArtifactSink is the controller-only immutable artifact boundary. It is
// intentionally identical to internal/artifacts.Store so the existing S3/R2
// adapter can be injected without importing provider details into capture.
type ArtifactSink interface {
	Put(context.Context, string, []byte, string) (created bool, uri string, err error)
	Get(context.Context, string) ([]byte, error)
}

var _ artifacts.Store = (ArtifactSink)(nil)

// OutputReader is the narrow retrieval seam for the controller. Its
// implementation should request logs from the fixed capture container with
// timestamps disabled, stream at most maxBytes, and return the raw frame. It
// must not accept a user-controlled command, path, or container name. The
// capture package then decodes and validates the frame before any upload.
type OutputReader interface {
	ReadCaptureOutput(context.Context, string, string, int64) ([]byte, error)
}

// Build creates a deterministic, one-shot Job and a matching deny-all
// NetworkPolicy. The Job mounts exactly one PVC, has no init containers,
// Secrets, service-account token, or permitted network egress, and runs a
// trusted digest-pinned capture image as UID 1000.
func Build(snapshot resolved.Snapshot, baseSHA string, options Options) (Plan, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return Plan{}, err
	}
	if err := validateBaseSHA(baseSHA); err != nil {
		return Plan{}, err
	}
	if snapshot.BaseSHA != baseSHA {
		return Plan{}, fmt.Errorf("%w: base SHA does not match immutable resolved snapshot", ErrInvalidInput)
	}
	limits, timeout, err := normalizeOptions(snapshot, options)
	if err != nil {
		return Plan{}, err
	}
	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil || !canonical.ValidDigest(specDigest) {
		return Plan{}, fmt.Errorf("%w: compute resolved spec digest: %v", ErrInvalidInput, err)
	}
	jobName, err := JobName(snapshot.Run.UID)
	if err != nil {
		return Plan{}, err
	}

	specJSON, err := json.Marshal(captureSpec{
		Version:            2,
		Protocol:           OutputProtocol,
		ResolvedSpecDigest: specDigest,
		BaseSHA:            baseSHA,
		RepoPath:           RepoPath,
		OutputDir:          OutputDir,
		PatchPath:          PatchPath,
		ManifestPath:       ManifestPath,
		ResultPath:         ResultPath,
		Scope:              snapshot.Spec.Scope,
		Requirements:       snapshot.Gate.Require,
		MaxPatchBytes:      limits.MaxPatchBytes,
		MaxResultBytes:     limits.MaxResultBytes,
		MaxManifestBytes:   limits.MaxManifestBytes,
	})
	if err != nil || len(specJSON) > maxSpecJSONBytes {
		return Plan{}, fmt.Errorf("%w: capture execution contract is too large", ErrInvalidInput)
	}
	inputDigest := captureInputDigest(specDigest, baseSHA, options.Image, options.WorkspaceClaimName, timeout, specJSON)
	labels := map[string]string{
		"agents.astatide.com/run-uid":           snapshot.Run.UID,
		"agents.astatide.com/role":              Role,
		"agents.astatide.com/spec-digest-short": digestShort(specDigest),
		EgressLabelKey:                          EgressLabelValue,
	}
	annotations := map[string]string{
		"agents.astatide.com/spec-digest":          specDigest,
		"agents.astatide.com/capture-input-digest": inputDigest,
	}
	owner := ownerReference(snapshot)
	activeDeadline := int64(math.Ceil(timeout.Seconds()))
	if activeDeadline <= 0 {
		return Plan{}, fmt.Errorf("%w: capture timeout is too small", ErrInvalidInput)
	}
	output := OutputContract{
		Protocol:         OutputProtocol,
		ResultPath:       ResultPath,
		PatchPath:        PatchPath,
		ManifestPath:     ManifestPath,
		MaxFrameBytes:    MaxEncodedFrameBytes(limits.MaxResultBytes, limits.MaxPatchBytes, limits.MaxManifestBytes),
		MaxResultBytes:   limits.MaxResultBytes,
		MaxPatchBytes:    limits.MaxPatchBytes,
		MaxManifestBytes: limits.MaxManifestBytes,
	}

	container := corev1.Container{
		Name:            Role,
		Image:           options.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{CaptureEntrypoint},
		Env: []corev1.EnvVar{
			{Name: "AGW_CAPTURE_PROTOCOL", Value: OutputProtocol},
			{Name: "AGW_CAPTURE_RUN_UID", Value: snapshot.Run.UID},
			{Name: "AGW_CAPTURE_SPEC_JSON", Value: string(specJSON)},
			{Name: "AGW_CAPTURE_SPEC_DIGEST", Value: specDigest},
			{Name: "AGW_CAPTURE_BASE_SHA", Value: baseSHA},
			{Name: "AGW_CAPTURE_REPO_PATH", Value: RepoPath},
			{Name: "AGW_CAPTURE_OUTPUT_DIR", Value: OutputDir},
			{Name: "AGW_CAPTURE_PATCH_PATH", Value: PatchPath},
			{Name: "AGW_CAPTURE_MANIFEST_PATH", Value: ManifestPath},
			{Name: "AGW_CAPTURE_RESULT_PATH", Value: ResultPath},
			{Name: "AGW_CAPTURE_MAX_PATCH_BYTES", Value: strconv.FormatInt(limits.MaxPatchBytes, 10)},
			{Name: "AGW_CAPTURE_MAX_RESULT_BYTES", Value: strconv.FormatInt(limits.MaxResultBytes, 10)},
			{Name: "AGW_CAPTURE_MAX_MANIFEST_BYTES", Value: strconv.FormatInt(limits.MaxManifestBytes, 10)},
		},
		WorkingDir:      RepoPath,
		Resources:       resourceRequirements(),
		SecurityContext: regularSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{{
			Name: WorkspaceVolumeName, MountPath: WorkspaceMountPath, ReadOnly: false,
		}},
	}

	selector := &metav1.LabelSelector{MatchLabels: copyStringMap(labels)}
	podSpec := corev1.PodSpec{
		HostUsers:                    boolPtr(false),
		AutomountServiceAccountToken: boolPtr(false),
		HostNetwork:                  false,
		HostPID:                      false,
		HostIPC:                      false,
		NodeSelector:                 map[string]string{NodeLabelKey: NodeLabelValue},
		Tolerations: []corev1.Toleration{{
			Key: NodeLabelKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		}},
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: int64Ptr(10),
		EnableServiceLinks:            boolPtr(false),
		DNSPolicy:                     corev1.DNSNone,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   boolPtr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{{
			Name: WorkspaceVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: options.WorkspaceClaimName,
			}},
		}},
	}
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName, Namespace: snapshot.Run.Namespace,
			Labels: labels, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: batchv1.JobSpec{
			Parallelism:           int32Ptr(1),
			Completions:           int32Ptr(1),
			BackoffLimit:          int32Ptr(0),
			ActiveDeadlineSeconds: &activeDeadline,
			ManualSelector:        boolPtr(true),
			Selector:              selector,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyStringMap(labels), Annotations: copyStringMap(annotations)},
				Spec:       podSpec,
			},
		},
	}
	networkPolicy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName + "-deny", Namespace: snapshot.Run.Namespace,
			Labels: labels, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *selector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		},
	}
	return Plan{Job: job, NetworkPolicy: networkPolicy, SpecDigest: specDigest, JobName: jobName, Output: output}, nil
}

// JobName returns a stable DNS-safe child name derived from the immutable run
// UID. The raw UID is never embedded in a name beyond Kubernetes label-safe
// validation.
func JobName(runUID string) (string, error) {
	if runUID == "" || len(runUID) > maxRunUIDBytes || len(validation.IsValidLabelValue(runUID)) != 0 {
		return "", fmt.Errorf("%w: run UID is not label-safe", ErrInvalidInput)
	}
	sum := sha256.Sum256([]byte(Role + "\x00" + runUID))
	return "agw-capture-" + hex.EncodeToString(sum[:10]), nil
}

// MarshalResult emits the canonical JSON result that a trusted capture image
// must place in the output frame. It validates structural bounds but cannot
// validate run-specific identity without a resolved snapshot.
func MarshalResult(result ResultEnvelope) ([]byte, error) {
	normalized, err := normalizeEnvelope(result)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal result: %v", ErrInvalidResult, err)
	}
	body, err = strictjson.Normalize(body)
	if err != nil || len(body) > int(HardMaxResultBytes) {
		return nil, fmt.Errorf("%w: canonicalize result", ErrInvalidResult)
	}
	return body, nil
}

// Validate decodes an output frame payload, recomputes patch evidence from
// the exact patch bytes, strictly decodes the canonical changed-file manifest,
// and enforces the run scope and Gate patch rules. Nothing is uploaded until
// this function returns successfully.
func Validate(snapshot resolved.Snapshot, baseSHA string, output CapturedOutput, limits ValidationLimits) (ValidatedOutput, error) {
	var zero ValidatedOutput
	if err := validateSnapshot(snapshot); err != nil {
		return zero, err
	}
	if err := validateBaseSHA(baseSHA); err != nil {
		return zero, err
	}
	if snapshot.BaseSHA != baseSHA {
		return zero, fmt.Errorf("%w: base SHA does not match immutable resolved snapshot", ErrInvalidInput)
	}
	effective, err := normalizeValidationLimits(snapshot, limits)
	if err != nil {
		return zero, err
	}
	if int64(len(output.ResultJSON)) > effective.MaxResultBytes || int64(len(output.Patch)) > effective.MaxPatchBytes || int64(len(output.Manifest)) > effective.MaxManifestBytes || len(output.Patch) == 0 || len(output.Manifest) == 0 {
		return zero, fmt.Errorf("%w: output exceeds configured byte bound", ErrDiffLimit)
	}
	envelope, err := decodeResult(output.ResultJSON, effective.MaxResultBytes)
	if err != nil {
		return zero, err
	}
	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		return zero, fmt.Errorf("%w: compute resolved spec digest: %v", ErrInvalidInput, err)
	}
	if envelope.RunUID != snapshot.Run.UID {
		return zero, fmt.Errorf("%w: run UID does not match", ErrInvalidResult)
	}
	if envelope.SpecDigest != specDigest {
		return zero, fmt.Errorf("%w: result spec digest does not match", ErrDigestMismatch)
	}
	if envelope.BaseSHA != baseSHA {
		return zero, fmt.Errorf("%w: result base SHA does not match", ErrDigestMismatch)
	}
	if envelope.PatchBytes != int64(len(output.Patch)) {
		return zero, fmt.Errorf("%w: result patch byte count does not match", ErrDigestMismatch)
	}
	patchDigest := digestBytes(output.Patch)
	if envelope.PatchDigest != patchDigest {
		return zero, fmt.Errorf("%w: result patch digest does not match", ErrDigestMismatch)
	}
	if envelope.ManifestBytes != int64(len(output.Manifest)) {
		return zero, fmt.Errorf("%w: result manifest byte count does not match", ErrDigestMismatch)
	}
	files, err := publish.DecodePatchManifest(output.Manifest, envelope.ManifestDigest)
	if err != nil {
		return zero, fmt.Errorf("%w: canonical patch manifest is invalid", ErrInvalidResult)
	}
	if err := validateManifestFiles(files); err != nil {
		return zero, err
	}
	evidence, err := analyzePatch(output.Patch)
	if err != nil {
		return zero, err
	}
	if !equalStrings(evidence.ChangedPaths, envelope.ChangedPaths) || evidence.LinesChanged != envelope.LinesChanged || evidence.HasBinaryFiles != envelope.HasBinaryFiles {
		return zero, fmt.Errorf("%w: result evidence does not match the patch", ErrDigestMismatch)
	}
	if envelope.FilesChanged != int64(len(evidence.ChangedPaths)) {
		return zero, fmt.Errorf("%w: result file count does not match the patch", ErrPathEvidence)
	}
	manifestPaths := make([]string, len(files))
	for index := range files {
		manifestPaths[index] = files[index].Path
	}
	if envelope.FilesChanged != int64(len(files)) || !equalStrings(evidence.ChangedPaths, manifestPaths) {
		return zero, fmt.Errorf("%w: manifest paths do not match patch evidence", ErrPathEvidence)
	}
	if envelope.FilesChanged > gate.MaxObservedFiles || envelope.LinesChanged > gate.MaxObservedLines {
		return zero, fmt.Errorf("%w: patch evidence exceeds hard bounds", ErrDiffLimit)
	}
	if snapshot.Gate.Require.MaxFilesChanged < 0 || snapshot.Gate.Require.MaxDiffLines < 0 {
		return zero, fmt.Errorf("%w: Gate diff bound is negative", ErrInvalidInput)
	}
	if snapshot.Gate.Require.MaxFilesChanged > 0 && envelope.FilesChanged > int64(snapshot.Gate.Require.MaxFilesChanged) {
		return zero, fmt.Errorf("%w: changed file count exceeds Gate bound", ErrDiffLimit)
	}
	if snapshot.Gate.Require.MaxDiffLines > 0 && envelope.LinesChanged > int64(snapshot.Gate.Require.MaxDiffLines) {
		return zero, fmt.Errorf("%w: changed line count exceeds Gate bound", ErrDiffLimit)
	}
	if envelope.HasBinaryFiles {
		return zero, ErrBinaryPatch
	}
	if snapshot.Gate.Require.ScopeRespected && !pathsRespectScope(evidence.ChangedPaths, snapshot.Spec.Scope) {
		return zero, ErrScopeViolation
	}
	canonicalResult, err := MarshalResult(envelope)
	if err != nil {
		return zero, fmt.Errorf("%w: canonical result binding: %v", ErrInvalidResult, err)
	}
	return ValidatedOutput{
		Envelope: envelope,
		Evidence: evidence,
		patch:    append([]byte(nil), output.Patch...),
		manifest: append([]byte(nil), output.Manifest...),
		files:    cloneFileChanges(files),
		binding:  outputBinding(canonicalResult, output.Patch, output.Manifest),
		valid:    true,
	}, nil
}

// ReadAndValidate retrieves the fixed stdout frame through the controller
// seam and applies the same validation as Validate. The reader's maxBytes is
// derived from the bounded output contract; callers cannot request an
// unbounded stream.
func ReadAndValidate(ctx context.Context, reader OutputReader, snapshot resolved.Snapshot, baseSHA, namespace, podName string, limits ValidationLimits) (ValidatedOutput, error) {
	var zero ValidatedOutput
	if ctx == nil || reader == nil {
		return zero, ErrOutputUnavailable
	}
	effective, err := normalizeValidationLimits(snapshot, limits)
	if err != nil {
		return zero, err
	}
	if namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 || podName == "" || len(validation.IsDNS1123Subdomain(podName)) != 0 {
		return zero, fmt.Errorf("%w: invalid pod identity", ErrOutputUnavailable)
	}
	frame, err := reader.ReadCaptureOutput(ctx, namespace, podName, MaxEncodedFrameBytes(effective.MaxResultBytes, effective.MaxPatchBytes, effective.MaxManifestBytes))
	if err != nil {
		return zero, fmt.Errorf("%w: read capture frame: %v", ErrOutputUnavailable, err)
	}
	if int64(len(frame)) > MaxEncodedFrameBytes(effective.MaxResultBytes, effective.MaxPatchBytes, effective.MaxManifestBytes) {
		return zero, fmt.Errorf("%w: frame exceeded retrieval bound", ErrInvalidFrame)
	}
	output, err := DecodeOutputFrame(frame, effective.MaxResultBytes, effective.MaxPatchBytes, effective.MaxManifestBytes)
	if err != nil {
		return zero, err
	}
	return Validate(snapshot, baseSHA, output, effective)
}

// Persist stores a previously validated patch and manifest through the
// controller-side sink. Existing content is fetched and compared byte-for-
// byte when immutable creation reports that either object already exists.
func Persist(ctx context.Context, sink ArtifactSink, result ValidatedOutput) (PersistedArtifacts, error) {
	var zero PersistedArtifacts
	if ctx == nil || sink == nil || !result.valid || int64(len(result.patch)) > HardMaxPatchBytes || int64(len(result.manifest)) > HardMaxManifestBytes {
		return zero, ErrInvalidArtifactInput
	}
	canonicalResult, err := MarshalResult(result.Envelope)
	if err != nil || result.binding == "" || result.binding != outputBinding(canonicalResult, result.patch, result.manifest) {
		return zero, ErrInvalidArtifactInput
	}
	files, err := publish.DecodePatchManifest(result.manifest, result.Envelope.ManifestDigest)
	if err != nil || !equalFileChanges(files, result.files) {
		return zero, ErrInvalidArtifactInput
	}
	if result.Envelope.PatchDigest != digestBytes(result.patch) || result.Envelope.PatchBytes != int64(len(result.patch)) || result.Envelope.ManifestBytes != int64(len(result.manifest)) {
		return zero, ErrDigestMismatch
	}
	runUID := result.Envelope.RunUID
	specDigest := result.Envelope.SpecDigest
	if !safeSegment(runUID) || !canonical.ValidDigest(specDigest) {
		return zero, ErrInvalidArtifactInput
	}
	patchHex := strings.TrimPrefix(result.Envelope.PatchDigest, canonical.DigestPrefix)
	manifestHex := strings.TrimPrefix(result.Envelope.ManifestDigest, canonical.DigestPrefix)
	specHex := strings.TrimPrefix(specDigest, canonical.DigestPrefix)
	patchKey := "runs/" + runUID + "/patches/" + specHex + "/" + patchHex + ".diff"
	patchURI, err := persistImmutable(ctx, sink, patchKey, result.patch, PatchMediaType)
	if err != nil {
		return zero, err
	}
	manifestKey := "runs/" + runUID + "/patches/" + specHex + "/" + manifestHex + ".manifest.json"
	manifestURI, err := persistImmutable(ctx, sink, manifestKey, result.manifest, ManifestMediaType)
	if err != nil {
		return zero, err
	}
	return PersistedArtifacts{
		Patch: v1alpha1.ArtifactRef{
			URI: patchURI, Digest: result.Envelope.PatchDigest, Kind: "patch", Name: "patch.diff",
			MediaType: PatchMediaType, SizeBytes: int64(len(result.patch)),
		},
		Manifest: v1alpha1.ArtifactRef{
			URI: manifestURI, Digest: result.Envelope.ManifestDigest, Kind: "patch-manifest", Name: "patch-manifest.json",
			MediaType: ManifestMediaType, SizeBytes: int64(len(result.manifest)),
		},
	}, nil
}

func persistImmutable(ctx context.Context, sink ArtifactSink, key string, body []byte, mediaType string) (string, error) {
	created, uri, err := sink.Put(ctx, key, append([]byte(nil), body...), mediaType)
	if err != nil {
		return "", fmt.Errorf("capture: put immutable artifact: %w", err)
	}
	if !created {
		existing, getErr := sink.Get(ctx, key)
		if getErr != nil {
			return "", fmt.Errorf("capture: verify existing immutable artifact: %w", getErr)
		}
		if !bytes.Equal(existing, body) {
			return "", ErrArtifactConflict
		}
	}
	if !safeArtifactURI(uri) {
		return "", fmt.Errorf("%w: sink returned unsafe URI", ErrInvalidArtifact)
	}
	return uri, nil
}

type captureSpec struct {
	Version            int                       `json:"version"`
	Protocol           string                    `json:"protocol"`
	ResolvedSpecDigest string                    `json:"resolvedSpecDigest"`
	BaseSHA            string                    `json:"baseSHA"`
	RepoPath           string                    `json:"repoPath"`
	OutputDir          string                    `json:"outputDir"`
	PatchPath          string                    `json:"patchPath"`
	ManifestPath       string                    `json:"manifestPath"`
	ResultPath         string                    `json:"resultPath"`
	Scope              v1alpha1.ScopeSpec        `json:"scope"`
	Requirements       v1alpha1.GateRequirements `json:"requirements"`
	MaxPatchBytes      int64                     `json:"maxPatchBytes"`
	MaxResultBytes     int64                     `json:"maxResultBytes"`
	MaxManifestBytes   int64                     `json:"maxManifestBytes"`
}

func validateSnapshot(snapshot resolved.Snapshot) error {
	if snapshot.SchemaVersion != resolved.SchemaVersion {
		return fmt.Errorf("%w: unsupported resolved snapshot schema version", ErrInvalidInput)
	}
	if snapshot.Run.Namespace == "" || len(validation.IsDNS1123Label(snapshot.Run.Namespace)) != 0 {
		return fmt.Errorf("%w: invalid run namespace", ErrInvalidInput)
	}
	if snapshot.Run.Name == "" || len(validation.IsDNS1123Subdomain(snapshot.Run.Name)) != 0 {
		return fmt.Errorf("%w: invalid run name", ErrInvalidInput)
	}
	if snapshot.Run.UID == "" || len(snapshot.Run.UID) > maxRunUIDBytes || len(validation.IsValidLabelValue(snapshot.Run.UID)) != 0 {
		return fmt.Errorf("%w: invalid run UID", ErrInvalidInput)
	}
	if !githubRepoPattern.MatchString(snapshot.Spec.Source.Repo) {
		return fmt.Errorf("%w: invalid source repository", ErrInvalidInput)
	}
	if snapshot.Spec.Source.BaseRef == "" || strings.ContainsAny(snapshot.Spec.Source.BaseRef, "\x00\r\n") {
		return fmt.Errorf("%w: invalid source base ref", ErrInvalidInput)
	}
	if !resolved.ValidBaseSHA(snapshot.BaseSHA) {
		return fmt.Errorf("%w: immutable base SHA is missing or invalid", ErrInvalidInput)
	}
	if snapshot.Spec.Limits.Timeout == "" {
		return fmt.Errorf("%w: run timeout is required", ErrInvalidInput)
	}
	if _, err := time.ParseDuration(snapshot.Spec.Limits.Timeout); err != nil {
		return fmt.Errorf("%w: invalid run timeout", ErrInvalidInput)
	}
	return nil
}

func validateBaseSHA(value string) error {
	if (len(value) != 40 && len(value) != maxBaseSHABytes) || !shaPattern.MatchString(value) {
		return fmt.Errorf("%w: base SHA must be lowercase 40 or 64 hex characters", ErrInvalidInput)
	}
	return nil
}

func normalizeOptions(snapshot resolved.Snapshot, options Options) (ValidationLimits, time.Duration, error) {
	if options.Image == "" || len(options.Image) > maxImageBytes || !resolved.ValidPinnedImage(options.Image) {
		return ValidationLimits{}, 0, fmt.Errorf("%w: capture image must be digest pinned", ErrInvalidInput)
	}
	if options.WorkspaceClaimName == "" || len(options.WorkspaceClaimName) > maxClaimNameSize || len(validation.IsDNS1123Subdomain(options.WorkspaceClaimName)) != 0 {
		return ValidationLimits{}, 0, fmt.Errorf("%w: workspace PVC name is invalid", ErrInvalidInput)
	}
	runTimeout, err := time.ParseDuration(snapshot.Spec.Limits.Timeout)
	if err != nil || runTimeout <= 0 {
		return ValidationLimits{}, 0, fmt.Errorf("%w: run timeout is invalid", ErrInvalidInput)
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
		if runTimeout < timeout {
			timeout = runTimeout
		}
	}
	if timeout <= 0 || timeout > MaxTimeout || timeout > runTimeout {
		return ValidationLimits{}, 0, fmt.Errorf("%w: capture timeout must be positive, <= %s, and <= run timeout", ErrInvalidInput, MaxTimeout)
	}
	limits, err := normalizeValidationLimits(snapshot, ValidationLimits{MaxPatchBytes: options.MaxPatchBytes, MaxResultBytes: options.MaxResultBytes, MaxManifestBytes: options.MaxManifestBytes})
	if err != nil {
		return ValidationLimits{}, 0, err
	}
	return limits, timeout, nil
}

func normalizeValidationLimits(snapshot resolved.Snapshot, limits ValidationLimits) (ValidationLimits, error) {
	maxPatch := limits.MaxPatchBytes
	if maxPatch == 0 {
		maxPatch = snapshot.Spec.Limits.MaxPatchBytes
	}
	if maxPatch == 0 {
		maxPatch = DefaultMaxPatchBytes
	}
	if snapshot.Spec.Limits.MaxPatchBytes > 0 && maxPatch > snapshot.Spec.Limits.MaxPatchBytes {
		maxPatch = snapshot.Spec.Limits.MaxPatchBytes
	}
	if maxPatch <= 0 || maxPatch > HardMaxPatchBytes {
		return ValidationLimits{}, fmt.Errorf("%w: patch byte bound must be in 1..%d", ErrInvalidInput, HardMaxPatchBytes)
	}
	maxResult := limits.MaxResultBytes
	if maxResult == 0 {
		maxResult = DefaultMaxResultBytes
	}
	if maxResult <= 0 || maxResult > HardMaxResultBytes {
		return ValidationLimits{}, fmt.Errorf("%w: result byte bound must be in 1..%d", ErrInvalidInput, HardMaxResultBytes)
	}
	maxManifest := limits.MaxManifestBytes
	if maxManifest == 0 {
		maxManifest = DefaultMaxManifestBytes
	}
	if maxManifest <= 0 || maxManifest > HardMaxManifestBytes {
		return ValidationLimits{}, fmt.Errorf("%w: manifest byte bound must be in 1..%d", ErrInvalidInput, HardMaxManifestBytes)
	}
	return ValidationLimits{MaxPatchBytes: maxPatch, MaxResultBytes: maxResult, MaxManifestBytes: maxManifest}, nil
}

func normalizeEnvelope(input ResultEnvelope) (ResultEnvelope, error) {
	output := input
	if output.SchemaVersion != ResultSchemaVersion || !canonical.ValidDigest(output.SpecDigest) || !canonical.ValidDigest(output.PatchDigest) || !canonical.ValidDigest(output.ManifestDigest) || !validBaseSHA(output.BaseSHA) {
		return ResultEnvelope{}, fmt.Errorf("%w: result identity fields are invalid", ErrInvalidResult)
	}
	if output.PatchBytes <= 0 || output.ManifestBytes <= 0 || output.ManifestBytes > HardMaxManifestBytes || output.FilesChanged <= 0 || output.LinesChanged < 0 || len(output.ChangedPaths) > gate.MaxChangedPaths || output.FilesChanged != int64(len(output.ChangedPaths)) {
		return ResultEnvelope{}, fmt.Errorf("%w: result counters are invalid", ErrInvalidResult)
	}
	output.ChangedPaths = append([]string(nil), input.ChangedPaths...)
	sort.Strings(output.ChangedPaths)
	for index, path := range output.ChangedPaths {
		if !gate.ValidateRepoPath(path) || (index > 0 && output.ChangedPaths[index-1] == path) {
			return ResultEnvelope{}, ErrPathEvidence
		}
	}
	return output, nil
}

func decodeResult(body []byte, maxBytes int64) (ResultEnvelope, error) {
	var zero ResultEnvelope
	if len(body) == 0 || int64(len(body)) > maxBytes || strictjson.ValidateObject(body) != nil {
		return zero, fmt.Errorf("%w: result JSON is not a bounded object", ErrInvalidResult)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil || !bytes.Equal(normalized, body) {
		return zero, ErrNonCanonicalResult
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result ResultEnvelope
	if err := decoder.Decode(&result); err != nil {
		return zero, fmt.Errorf("%w: decode result: %v", ErrInvalidResult, err)
	}
	normalizedResult, err := normalizeEnvelope(result)
	if err != nil {
		return zero, err
	}
	canonicalBody, err := MarshalResult(normalizedResult)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return zero, ErrNonCanonicalResult
	}
	return normalizedResult, nil
}

func analyzePatch(patch []byte) (PatchEvidence, error) {
	var evidence PatchEvidence
	if len(patch) == 0 {
		return evidence, nil
	}
	if !utf8.Valid(patch) {
		return evidence, fmt.Errorf("%w: patch is not valid UTF-8; binary evidence cannot be trusted", ErrInvalidPatch)
	}
	text := string(patch)
	pathSet := make(map[string]struct{})
	lines := strings.Split(text, "\n")
	headerCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			headerCount++
			paths, err := parseDiffHeader(line)
			if err != nil {
				return evidence, fmt.Errorf("%w: %v", ErrPathEvidence, err)
			}
			for _, path := range paths {
				if _, exists := pathSet[path]; exists {
					return evidence, fmt.Errorf("%w: duplicate path %q", ErrPathEvidence, path)
				}
				pathSet[path] = struct{}{}
			}
		}
		if strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
			evidence.HasBinaryFiles = true
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			evidence.LinesChanged++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			evidence.LinesChanged++
		}
		if evidence.LinesChanged > gate.MaxObservedLines {
			return evidence, ErrDiffLimit
		}
	}
	if headerCount == 0 {
		return evidence, fmt.Errorf("%w: non-empty patch has no git diff headers", ErrInvalidPatch)
	}
	evidence.ChangedPaths = make([]string, 0, len(pathSet))
	for path := range pathSet {
		evidence.ChangedPaths = append(evidence.ChangedPaths, path)
	}
	sort.Strings(evidence.ChangedPaths)
	return evidence, nil
}

func parseDiffHeader(line string) ([]string, error) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if rest == line || rest == "" {
		return nil, errors.New("missing diff header paths")
	}
	var left, right string
	if strings.HasPrefix(rest, "\"") {
		var consumed int
		var err error
		left, consumed, err = parseGitToken(rest)
		if err != nil {
			return nil, err
		}
		rest = rest[consumed:]
		if len(rest) == 0 || rest[0] != ' ' {
			return nil, errors.New("diff header has no second path")
		}
		right, consumed, err = parseGitToken(rest[1:])
		if err != nil || consumed != len(rest)-1 {
			return nil, errors.New("diff header has trailing path data")
		}
	} else {
		for index := 0; index < len(rest); index++ {
			if rest[index] != ' ' || !strings.HasPrefix(rest[index+1:], "b/") {
				continue
			}
			candidateLeft := rest[:index]
			candidateRight := rest[index+1:]
			if strings.HasPrefix(candidateLeft, "a/") && strings.HasPrefix(candidateRight, "b/") {
				left, right = candidateLeft, candidateRight
				break
			}
		}
		if left == "" || right == "" {
			return nil, errors.New("diff header paths are not parseable")
		}
	}
	left = strings.TrimPrefix(left, "a/")
	right = strings.TrimPrefix(right, "b/")
	if !gate.ValidateRepoPath(left) || !gate.ValidateRepoPath(right) {
		return nil, errors.New("diff header contains an unsafe repository path")
	}
	if left == right {
		return []string{left}, nil
	}
	return []string{left, right}, nil
}

func parseGitToken(value string) (string, int, error) {
	if value == "" || value[0] != '"' {
		return "", 0, errors.New("quoted git path is missing")
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '"' || value[index-1] == '\\' {
			continue
		}
		quoted := value[:index+1]
		decoded, err := strconv.Unquote(quoted)
		if err != nil {
			return "", 0, fmt.Errorf("decode quoted git path: %v", err)
		}
		return decoded, index + 1, nil
	}
	return "", 0, errors.New("unterminated quoted git path")
}

func pathsRespectScope(paths []string, scope v1alpha1.ScopeSpec) bool {
	for _, path := range paths {
		allowed := false
		for _, pattern := range scope.Paths {
			if gate.MatchGlob(pattern, path) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
		for _, pattern := range scope.Forbidden {
			if gate.MatchGlob(pattern, path) {
				return false
			}
		}
	}
	return true
}

func ownerReference(snapshot resolved.Snapshot) metav1.OwnerReference {
	controller := true
	block := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupName + "/" + v1alpha1.Version,
		Kind:               "AgentRun",
		Name:               snapshot.Run.Name,
		UID:                types.UID(snapshot.Run.UID),
		Controller:         &controller,
		BlockOwnerDeletion: &block,
	}
}

func resourceRequirements() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resourceMustParse(defaultCaptureCPURequest), corev1.ResourceMemory: resourceMustParse(defaultCaptureMemoryRequest)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resourceMustParse(defaultCaptureCPULimit), corev1.ResourceMemory: resourceMustParse(defaultCaptureMemoryLimit)},
	}
}

func regularSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: int64Ptr(1000), RunAsGroup: int64Ptr(1000), RunAsNonRoot: boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func captureInputDigest(specDigest, baseSHA, image, claim string, timeout time.Duration, spec []byte) string {
	hash := sha256.New()
	for _, value := range []string{specDigest, baseSHA, image, claim, timeout.String()} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(spec)
	return canonical.DigestPrefix + hex.EncodeToString(hash.Sum(nil))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func outputBinding(resultJSON, patch, manifest []byte) string {
	hash := sha256.New()
	_, _ = hash.Write(resultJSON)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(patch)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(manifest)
	return canonical.DigestPrefix + hex.EncodeToString(hash.Sum(nil))
}

func validateManifestFiles(files []publish.FileChange) error {
	if len(files) == 0 || len(files) > publish.MaxPatchFiles {
		return ErrPathEvidence
	}
	for _, file := range files {
		if file.Delete {
			if file.Content != nil {
				return ErrInvalidResult
			}
			continue
		}
		if file.Content == nil || !utf8.Valid(file.Content) || bytes.IndexByte(file.Content, 0) >= 0 {
			return ErrBinaryPatch
		}
	}
	return nil
}

func cloneFileChanges(files []publish.FileChange) []publish.FileChange {
	clone := make([]publish.FileChange, len(files))
	for index, file := range files {
		clone[index] = publish.FileChange{Path: file.Path, Mode: file.Mode, Delete: file.Delete}
		if !file.Delete {
			clone[index].Content = make([]byte, len(file.Content))
			copy(clone[index].Content, file.Content)
		}
	}
	return clone
}

func equalFileChanges(left, right []publish.FileChange) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Path != right[index].Path || left[index].Mode != right[index].Mode || left[index].Delete != right[index].Delete || !bytes.Equal(left[index].Content, right[index].Content) || (left[index].Content == nil) != (right[index].Content == nil) {
			return false
		}
	}
	return true
}

func validBaseSHA(value string) bool {
	return (len(value) == 40 || len(value) == maxBaseSHABytes) && shaPattern.MatchString(value)
}

func safeSegment(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func safeArtifactURI(value string) bool {
	if len(value) == 0 || len(value) > maxArtifactURIBytes || strings.ContainsAny(value, "\x00\r\n@?#") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "s3" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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

func boolPtr(value bool) *bool    { return &value }
func int32Ptr(value int32) *int32 { return &value }
func int64Ptr(value int64) *int64 { return &value }

// resourceMustParse is kept local so the pure builder cannot accidentally
// accept a resource profile from user input.
func resourceMustParse(value string) resource.Quantity {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		panic(err)
	}
	return quantity
}
