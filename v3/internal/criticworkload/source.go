package criticworkload

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
)

var (
	ErrSourceConfig          = errors.New("invalid critic evidence source configuration")
	ErrSourceUnavailable     = errors.New("critic evidence source is temporarily unavailable")
	ErrSourceUnauthenticated = errors.New("critic evidence producer is not authenticated")
	ErrSourceMissing         = errors.New("critic evidence object is missing")
	ErrSourceConflict        = errors.New("critic evidence object conflicts with its producer")
)

// SourceOptions configures an authenticated object source. Reader must be an
// API client with permission to read only the AGW run namespace; Store must be
// an authenticated immutable object-store client.
type SourceOptions struct {
	Reader         client.Reader
	Store          artifacts.Store
	Auth           RecordAuthenticator
	Namespace      string
	Bucket         string
	Prefix         string
	MaxOutputBytes int64
}

// Source authenticates the Job/Pod producer through Kubernetes ownership and
// then verifies the immutable object and controller-authored output record.
// Strict JSON decoding occurs only after the producer identity and object
// digest have passed those checks.
type Source struct {
	reader    client.Reader
	store     artifacts.Store
	auth      RecordAuthenticator
	namespace string
	bucket    string
	prefix    string
	max       int64
}

func NewSource(options SourceOptions) (*Source, error) {
	if options.Reader == nil || options.Store == nil || options.Auth == nil || options.Namespace == "" || options.Bucket == "" {
		return nil, ErrSourceConfig
	}
	max := options.MaxOutputBytes
	if max == 0 {
		max = MaxOutputBytes
	}
	if max <= 0 || max > MaxOutputBytes {
		return nil, ErrSourceConfig
	}
	prefix := strings.Trim(options.Prefix, "/")
	if strings.Contains(prefix, "//") || strings.Contains(prefix, "..") {
		return nil, ErrSourceConfig
	}
	return &Source{reader: options.Reader, store: options.Store, auth: options.Auth, namespace: options.Namespace, bucket: options.Bucket, prefix: prefix, max: max}, nil
}

// Read authenticates and returns only canonical CorroborationInput bytes. A
// caller cannot obtain a trusted result, verdict, score, or Gate decision from
// this source.
func (s *Source) Read(ctx context.Context, ref v1alpha1.ArtifactRef, maxBytes int64) (EvidenceArtifact, error) {
	if s == nil || s.reader == nil || s.store == nil || s.auth == nil || ctx == nil || maxBytes <= 0 || maxBytes > s.max {
		return EvidenceArtifact{}, ErrSourceConfig
	}
	if err := validateArtifact(ref, InputKind, maxBytes); err != nil {
		return EvidenceArtifact{}, ErrSourceConflict
	}
	logicalKey, runUID, jobName, jobUID, err := s.parseOutputURI(ref.URI)
	if err != nil {
		return EvidenceArtifact{}, ErrSourceConflict
	}
	var job batchv1.Job
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: jobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return EvidenceArtifact{}, ErrSourceMissing
		}
		return EvidenceArtifact{}, fmt.Errorf("%w: read critic Job: %v", ErrSourceUnavailable, err)
	}
	if err := authenticateJob(&job, s.namespace, runUID, jobName, jobUID); err != nil {
		return EvidenceArtifact{}, err
	}
	if job.Status.Failed > 0 {
		return EvidenceArtifact{}, ErrJobFailed
	}
	if job.Status.Succeeded <= 0 {
		return EvidenceArtifact{}, ErrJobNotReady
	}
	pod, err := s.findAuthenticatedPod(ctx, &job)
	if err != nil {
		return EvidenceArtifact{}, err
	}
	// The API-server ownership and terminal-state checks above are the
	// authentication step. Only now do we parse controller-authored JSON.
	recordBody, err := s.store.Get(ctx, logicalKey+".record.json")
	if err != nil {
		return EvidenceArtifact{}, ErrSourceMissing
	}
	if len(recordBody) == 0 || len(recordBody) > MaxRecordBytes {
		return EvidenceArtifact{}, ErrOutputOversized
	}
	signature, err := s.store.Get(ctx, logicalKey+".record.json.sig")
	if err != nil || len(signature) == 0 || len(signature) > MaxSignatureBytes || s.auth.Verify(recordBody, signature) != nil {
		return EvidenceArtifact{}, ErrSourceUnauthenticated
	}
	record, err := ParseCanonicalRecord(recordBody)
	if err != nil {
		return EvidenceArtifact{}, err
	}
	jobIdentity := JobIdentity{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)}
	podIdentity := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}
	if err := ValidateRecordForOutput(record, ref, logicalKey, jobIdentity, podIdentity, maxBytes); err != nil {
		return EvidenceArtifact{}, err
	}
	if err := validateJobAnnotations(&job, record, logicalKey); err != nil {
		return EvidenceArtifact{}, err
	}
	contractBody, err := CanonicalInputBytes(record.Input)
	if err != nil || !jobInputEnvEquals(&job.Spec.Template.Spec, string(contractBody)) {
		return EvidenceArtifact{}, ErrSourceConflict
	}
	body, err := s.store.Get(ctx, logicalKey)
	if err != nil {
		return EvidenceArtifact{}, ErrSourceMissing
	}
	if len(body) == 0 || int64(len(body)) > maxBytes || int64(len(body)) != ref.SizeBytes || digestBytes(body) != ref.Digest {
		return EvidenceArtifact{}, ErrArtifactConflict
	}
	// Integrity and producer authentication are complete. This is the first
	// point at which critic bytes are decoded as a CorroborationInput.
	if _, err := decodeCriticInput(body); err != nil {
		return EvidenceArtifact{}, err
	}
	return EvidenceArtifact{Input: append([]byte(nil), body...), Authenticated: true, Binding: record.Input.Binding(), Record: record}, nil
}

func jobInputEnvEquals(spec *corev1.PodSpec, expected string) bool {
	if spec == nil {
		return false
	}
	seen := false
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, container := range containers {
			for _, env := range container.Env {
				if env.Name != InputEnv {
					continue
				}
				if seen || env.ValueFrom != nil || env.Value != expected {
					return false
				}
				seen = true
			}
		}
	}
	return seen
}

func (s *Source) parseOutputURI(value string) (logicalKey, runUID, jobName, jobUID string, err error) {
	u, parseErr := url.Parse(value)
	if parseErr != nil || u.Scheme != "s3" || u.Host != s.bucket || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return "", "", "", "", ErrSourceConflict
	}
	full := strings.TrimPrefix(u.Path, "/")
	if s.prefix != "" {
		prefix := s.prefix + "/"
		if !strings.HasPrefix(full, prefix) {
			return "", "", "", "", ErrSourceConflict
		}
		full = strings.TrimPrefix(full, prefix)
	}
	parts := strings.Split(full, "/")
	if len(parts) != 6 || parts[0] != "runs" || parts[2] != "critic" || parts[5] != "input.json" || !validPathSegment(parts[1]) || !validPathSegment(parts[3]) || !validPathSegment(parts[4]) {
		return "", "", "", "", ErrSourceConflict
	}
	return full, parts[1], parts[3], parts[4], nil
}

func authenticateJob(job *batchv1.Job, namespace, runUID, jobName, jobUID string) error {
	if job == nil || job.Namespace != namespace || job.Name != jobName || string(job.UID) != jobUID || job.UID == "" || job.Labels["agents.astatide.com/role"] != Role || job.Annotations[RunUIDAnnotationKey] != runUID || job.Annotations[OutputKeyPrefixAnnotationKey] != "runs/"+runUID+"/critic/"+jobName {
		return ErrSourceUnauthenticated
	}
	owned := false
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "AgentRun" && owner.UID == clientUID(runUID) && owner.Controller != nil && *owner.Controller {
			owned = true
		}
	}
	if !owned {
		return ErrSourceUnauthenticated
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Completions == nil || *job.Spec.Completions != 1 || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.NonIndexedCompletion || job.Spec.ManualSelector == nil || !*job.Spec.ManualSelector || len(job.OwnerReferences) != 1 {
		return ErrSourceUnauthenticated
	}
	if err := validateAuthenticatedJobShape(job); err != nil {
		return ErrSourceUnauthenticated
	}
	return nil
}

func (s *Source) findAuthenticatedPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := s.reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{jobControllerUIDLabel: string(job.UID)}); err != nil {
		return nil, fmt.Errorf("%w: list critic Pods: %v", ErrSourceUnavailable, err)
	}
	if len(pods.Items) == 0 {
		return nil, ErrOutputNotReady
	}
	if len(pods.Items) != 1 {
		return nil, ErrDuplicateOutput
	}
	pod := &pods.Items[0]
	if pod.Namespace != job.Namespace || pod.UID == "" || pod.Status.Phase != corev1.PodSucceeded || pod.Labels["agents.astatide.com/role"] != Role || pod.Annotations[ContractDigestAnnotationKey] == "" || pod.Annotations[ContractDigestAnnotationKey] != job.Annotations[ContractDigestAnnotationKey] || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Job" || pod.OwnerReferences[0].UID != job.UID || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller || validatePodRuntimeShape(pod.Spec) != nil || !podSpecCriticalEqual(pod.Spec, job.Spec.Template.Spec) {
		return nil, ErrSourceUnauthenticated
	}
	return pod, nil
}

func validateJobAnnotations(job *batchv1.Job, record OutputRecord, logicalKey string) error {
	annotations := job.Annotations
	if annotations[RunUIDAnnotationKey] != record.Input.Run.UID || annotations[RunGenerationAnnotationKey] != fmt.Sprint(record.Input.Run.Generation) || annotations[SpecDigestAnnotationKey] != record.Input.SpecDigest || annotations[PatchDigestAnnotationKey] != record.Input.Patch.Digest || annotations[ContextDigestAnnotationKey] != record.Input.Context.Digest || annotations[ContractDigestAnnotationKey] != record.InputDigest || annotations[BindingDigestAnnotationKey] != record.BindingDigest || annotations[OutputKeyPrefixAnnotationKey] != record.Input.RunUIDOutputPrefix(job.Name) || annotations[CriticRouteNameAnnotationKey] != record.Input.CriticRoute.Name || annotations[CriticRouteUIDAnnotationKey] != record.Input.CriticRoute.UID || annotations[CriticRouteGenerationAnnotation] != fmt.Sprint(record.Input.CriticRoute.Generation) || annotations[CriticRouteFamilyAnnotationKey] != record.Input.CriticRoute.Selected.Family || annotations[WorkerRouteFamilyAnnotationKey] != routeFamilies(record.Input.WorkerRoute) || record.Object.Key != logicalKey {
		return ErrSourceConflict
	}
	return nil
}

func decodeCriticInput(body []byte) (findingcorroboration.CorroborationInput, error) {
	input, err := findingcorroboration.ParseCanonicalInput(body)
	if err != nil {
		return findingcorroboration.CorroborationInput{}, fmt.Errorf("%w: canonical corroboration input: %v", ErrOutputMalformed, err)
	}
	return input, nil
}

func clientUID(value string) types.UID { return types.UID(value) }

// RunUIDOutputPrefix is a small helper used by source validation and avoids
// reconstructing the controller's naming convention in multiple callers.
func (input Input) RunUIDOutputPrefix(jobName string) string {
	return "runs/" + input.Run.UID + "/critic/" + jobName
}
