// Package status provides bounded projections for v3 API status fields.
//
// AgentRun status is deliberately a current-state summary. It contains small
// immutable references and counters, not event history, logs, patches, full
// reports, or effect-ledger records. Those larger and append-only values live
// in content-addressed external artifacts.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/fsm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// MaxStatusBytes is the hard Kubernetes status budget. Encoded status must
	// remain strictly smaller than this value so the budget is not exhausted by
	// the boundary itself.
	MaxStatusBytes = 64 << 10
	// StatusByteBudget is the largest encoded status accepted by Encode.
	StatusByteBudget = MaxStatusBytes - 1
	// MaxStatusSize is a compatibility alias for the strict encoded budget.
	MaxStatusSize = StatusByteBudget

	MaxStatusConditions = v1alpha1.MaxStatusConditions
	MaxStatusChecks     = v1alpha1.MaxStatusChecks
	MaxStatusMessage    = v1alpha1.MaxStatusMessage

	MaxStatusMessageBytes    = v1alpha1.MaxStatusMessage
	MaxFailureCodeBytes      = 128
	MaxReasonBytes           = 64
	MaxConditionTypeBytes    = 64
	MaxGateCheckNameBytes    = 128
	MaxGateCheckMessageBytes = 1024
	MaxChildNameBytes        = 253
	MaxChildKindBytes        = 63
	MaxArtifactURIBytes      = 1024
	MaxDigestBytes           = len("sha256:") + 64
	MaxBaseSHABytes          = 64
	MaxPullRequestURLBytes   = 1024
)

var (
	ErrNilStatus         = errors.New("AgentRun status is nil")
	ErrStatusTooLarge    = errors.New("AgentRun status exceeds its bounded size")
	ErrInvalidStatus     = errors.New("invalid AgentRun status")
	ErrStatusSecretURI   = errors.New("status artifact URI contains credential material")
	ErrTooManyConditions = errors.New("AgentRun status has too many conditions")
	ErrTooManyGateChecks = errors.New("AgentRun status has too many gate checks")
	hexSHA               = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	decimalUSD           = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})([.][0-9]{1,6})?$`)
	descriptorName       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// BoundedAgentRunStatus returns a deep-copied status with all bounded strings,
// conditions, checks, and child collections capped for the API contract. It
// does not silently repair invalid enum or digest values; Validate must still
// be called before persistence.
func BoundedAgentRunStatus(in v1alpha1.AgentRunStatus) v1alpha1.AgentRunStatus {
	out := cloneStatus(in)
	out.SpecDigest = boundedString(out.SpecDigest, MaxDigestBytes)
	out.BaseSHA = boundedString(out.BaseSHA, MaxBaseSHABytes)

	if out.ResolvedSpecRef != nil {
		out.ResolvedSpecRef = boundedArtifactRef(out.ResolvedSpecRef)
	}
	if out.ContextPackRef != nil {
		out.ContextPackRef = boundedArtifactRef(out.ContextPackRef)
	}
	if out.EventStreamRef != nil {
		out.EventStreamRef = boundedArtifactRef(out.EventStreamRef)
	}
	if out.WorkSandboxRef != nil {
		out.WorkSandboxRef.Name = boundedString(out.WorkSandboxRef.Name, MaxChildNameBytes)
		out.WorkSandboxRef.Kind = boundedString(out.WorkSandboxRef.Kind, MaxChildKindBytes)
		out.WorkSandboxRef.PlanFingerprint = boundedString(out.WorkSandboxRef.PlanFingerprint, MaxDigestBytes)
	}
	if out.VerifySandboxRef != nil {
		out.VerifySandboxRef.Name = boundedString(out.VerifySandboxRef.Name, MaxChildNameBytes)
		out.VerifySandboxRef.Kind = boundedString(out.VerifySandboxRef.Kind, MaxChildKindBytes)
		out.VerifySandboxRef.PlanFingerprint = boundedString(out.VerifySandboxRef.PlanFingerprint, MaxDigestBytes)
	}
	if len(out.Children) > MaxStatusConditions {
		out.Children = out.Children[:MaxStatusConditions]
	}
	for index := range out.Children {
		out.Children[index].Name = boundedString(out.Children[index].Name, MaxChildNameBytes)
		out.Children[index].Kind = boundedString(out.Children[index].Kind, MaxChildKindBytes)
		out.Children[index].PlanFingerprint = boundedString(out.Children[index].PlanFingerprint, MaxDigestBytes)
	}
	if len(out.Artifacts) > MaxStatusConditions {
		out.Artifacts = out.Artifacts[:MaxStatusConditions]
	}
	for index := range out.Artifacts {
		out.Artifacts[index] = *boundedArtifactRef(&out.Artifacts[index])
	}
	if out.Patch != nil {
		out.Patch.FilesChanged = maxInt32(out.Patch.FilesChanged)
		out.Patch.LinesChanged = maxInt32(out.Patch.LinesChanged)
		if out.Patch.Ref != nil {
			out.Patch.Ref = boundedArtifactRef(out.Patch.Ref)
		}
		if out.Patch.ManifestRef != nil {
			out.Patch.ManifestRef = boundedArtifactRef(out.Patch.ManifestRef)
		}
	}
	if out.Gate != nil {
		out.Gate.Name = boundedString(out.Gate.Name, MaxChildNameBytes)
		out.Gate.UID = boundedString(out.Gate.UID, MaxBaseSHABytes)
		out.Gate.Verdict = boundedString(out.Gate.Verdict, MaxReasonBytes)
		if out.Gate.ReportRef != nil {
			out.Gate.ReportRef = boundedArtifactRef(out.Gate.ReportRef)
		}
		if len(out.Gate.Checks) > MaxStatusChecks {
			out.Gate.Checks = out.Gate.Checks[:MaxStatusChecks]
		}
		for index := range out.Gate.Checks {
			out.Gate.Checks[index].Name = boundedString(out.Gate.Checks[index].Name, MaxGateCheckNameBytes)
			out.Gate.Checks[index].Message = boundedString(out.Gate.Checks[index].Message, MaxGateCheckMessageBytes)
		}
	}
	if out.Effect != nil {
		out.Effect.Key = boundedString(out.Effect.Key, MaxDigestBytes)
		out.Effect.PullRequestURL = boundedString(out.Effect.PullRequestURL, MaxPullRequestURLBytes)
	}
	if out.Failure != nil {
		out.Failure.Code = boundedString(out.Failure.Code, MaxFailureCodeBytes)
		out.Failure.Message = boundedString(out.Failure.Message, MaxStatusMessageBytes)
	}
	out.Conditions = boundedConditions(out.Conditions)
	return out
}

// Bounded is a concise alias for BoundedAgentRunStatus.
func Bounded(in v1alpha1.AgentRunStatus) v1alpha1.AgentRunStatus {
	return BoundedAgentRunStatus(in)
}

// Project is the status projection entry point used by controllers.
func Project(in v1alpha1.AgentRunStatus) v1alpha1.AgentRunStatus {
	return BoundedAgentRunStatus(in)
}

// ValidateAgentRunStatus validates enum, digest, reference, condition, and
// size constraints without persisting or retaining any event history.
func ValidateAgentRunStatus(in v1alpha1.AgentRunStatus) error {
	if in.Phase != "" {
		if err := fsm.ValidatePhase(in.Phase); err != nil {
			return fmt.Errorf("%w: phase: %v", ErrInvalidStatus, err)
		}
	}
	if in.SpecDigest != "" && !canonical.ValidDigest(in.SpecDigest) {
		return fmt.Errorf("%w: specDigest must be a lowercase sha256 digest", ErrInvalidStatus)
	}
	if err := validateBoundedText("baseSHA", in.BaseSHA, MaxBaseSHABytes); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
	}
	if in.BaseSHA != "" && !hexSHA.MatchString(in.BaseSHA) {
		return fmt.Errorf("%w: baseSHA must be 40-64 lowercase hexadecimal characters", ErrInvalidStatus)
	}
	if in.CostUSD != "" && !decimalUSD.MatchString(in.CostUSD) {
		return fmt.Errorf("%w: costUsd must be a bounded decimal string", ErrInvalidStatus)
	}
	if err := validateArtifactRef(in.ResolvedSpecRef); err != nil {
		return fmt.Errorf("%w: resolvedSpecRef: %v", ErrInvalidStatus, err)
	}
	if err := validateArtifactRef(in.ContextPackRef); err != nil {
		return fmt.Errorf("%w: contextPackRef: %v", ErrInvalidStatus, err)
	}
	if err := validateArtifactRef(in.EventStreamRef); err != nil {
		return fmt.Errorf("%w: eventStreamRef: %v", ErrInvalidStatus, err)
	}
	if err := validateChildRef(in.WorkSandboxRef); err != nil {
		return fmt.Errorf("%w: workSandboxRef: %v", ErrInvalidStatus, err)
	}
	if err := validateChildRef(in.VerifySandboxRef); err != nil {
		return fmt.Errorf("%w: verifySandboxRef: %v", ErrInvalidStatus, err)
	}
	if len(in.Children) > MaxStatusConditions {
		return fmt.Errorf("%w: children exceed %d", ErrInvalidStatus, MaxStatusConditions)
	}
	for index, child := range in.Children {
		if err := validateChildRef(&child); err != nil {
			return fmt.Errorf("%w: children[%d]: %v", ErrInvalidStatus, index, err)
		}
	}
	if len(in.Artifacts) > MaxStatusConditions {
		return fmt.Errorf("%w: artifacts exceed %d", ErrInvalidStatus, MaxStatusConditions)
	}
	for index, artifact := range in.Artifacts {
		if err := validateArtifactRef(&artifact); err != nil {
			return fmt.Errorf("%w: artifacts[%d]: %v", ErrInvalidStatus, index, err)
		}
	}
	if in.Patch != nil {
		if in.Patch.FilesChanged < 0 || in.Patch.LinesChanged < 0 {
			return fmt.Errorf("%w: patch counters cannot be negative", ErrInvalidStatus)
		}
		if err := validateArtifactRef(in.Patch.Ref); err != nil {
			return fmt.Errorf("%w: patch.ref: %v", ErrInvalidStatus, err)
		}
		if err := validateArtifactRef(in.Patch.ManifestRef); err != nil {
			return fmt.Errorf("%w: patch.manifestRef: %v", ErrInvalidStatus, err)
		}
	}
	if in.Gate != nil {
		if err := validateBoundedText("gate.name", in.Gate.Name, MaxChildNameBytes); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
		}
		if err := validateBoundedText("gate.uid", in.Gate.UID, MaxBaseSHABytes); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
		}
		if in.Gate.Generation < 0 {
			return fmt.Errorf("%w: gate.generation cannot be negative", ErrInvalidStatus)
		}
		switch in.Gate.Mode {
		case "", v1alpha1.GateShadow, v1alpha1.GateEnforcing:
		default:
			return fmt.Errorf("%w: unsupported gate mode %q", ErrInvalidStatus, in.Gate.Mode)
		}
		if in.Gate.Verdict != "" && in.Gate.Verdict != "Accepted" && in.Gate.Verdict != "Rejected" {
			return fmt.Errorf("%w: unsupported gate verdict %q", ErrInvalidStatus, in.Gate.Verdict)
		}
		if len(in.Gate.Checks) > MaxStatusChecks {
			return fmt.Errorf("%w: %d > %d", ErrTooManyGateChecks, len(in.Gate.Checks), MaxStatusChecks)
		}
		if err := validateArtifactRef(in.Gate.ReportRef); err != nil {
			return fmt.Errorf("%w: gate.reportRef: %v", ErrInvalidStatus, err)
		}
		for index, check := range in.Gate.Checks {
			if err := validateBoundedText("gate check name", check.Name, MaxGateCheckNameBytes); err != nil {
				return fmt.Errorf("%w: gate.checks[%d]: %v", ErrInvalidStatus, index, err)
			}
			if err := validateBoundedText("gate check message", check.Message, MaxGateCheckMessageBytes); err != nil {
				return fmt.Errorf("%w: gate.checks[%d]: %v", ErrInvalidStatus, index, err)
			}
		}
	}
	if in.Effect != nil {
		if in.Effect.Key != "" && !canonical.ValidDigest(in.Effect.Key) {
			return fmt.Errorf("%w: effect.key must be a lowercase sha256 digest", ErrInvalidStatus)
		}
		switch in.Effect.State {
		case "", v1alpha1.EffectPending, v1alpha1.EffectSucceeded, v1alpha1.EffectFailed, v1alpha1.EffectUnknown:
		default:
			return fmt.Errorf("%w: unsupported effect state %q", ErrInvalidStatus, in.Effect.State)
		}
		if err := validateSafeURL("effect.pullRequestUrl", in.Effect.PullRequestURL, MaxPullRequestURLBytes); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
		}
	}
	if in.Failure != nil {
		if err := validateBoundedText("failure code", in.Failure.Code, MaxFailureCodeBytes); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
		}
		if err := validateBoundedText("failure message", in.Failure.Message, MaxStatusMessageBytes); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
		}
	}
	if len(in.Conditions) > MaxStatusConditions {
		return fmt.Errorf("%w: %d > %d", ErrTooManyConditions, len(in.Conditions), MaxStatusConditions)
	}
	for index, condition := range in.Conditions {
		if err := validateCondition(condition); err != nil {
			return fmt.Errorf("%w: conditions[%d]: %v", ErrInvalidStatus, index, err)
		}
	}

	encoded, err := marshalRaw(in)
	if err != nil {
		return fmt.Errorf("%w: encode: %v", ErrInvalidStatus, err)
	}
	if len(encoded) >= MaxStatusBytes {
		return ErrStatusTooLarge
	}
	return nil
}

// Validate is an alias for ValidateAgentRunStatus.
func Validate(in v1alpha1.AgentRunStatus) error {
	return ValidateAgentRunStatus(in)
}

// ValidateStatus is an alias for ValidateAgentRunStatus.
func ValidateStatus(in v1alpha1.AgentRunStatus) error {
	return ValidateAgentRunStatus(in)
}

// Encode returns the bounded JSON projection suitable for a status update.
// It never emits an oversized status and never includes an event list.
func Encode(in v1alpha1.AgentRunStatus) ([]byte, error) {
	bounded := BoundedAgentRunStatus(in)
	if err := ValidateAgentRunStatus(bounded); err != nil {
		return nil, err
	}
	encoded, err := marshalRaw(bounded)
	if err != nil {
		return nil, err
	}
	if len(encoded) >= MaxStatusBytes {
		return nil, ErrStatusTooLarge
	}
	return encoded, nil
}

// MarshalAgentRunStatus is an alias for Encode.
func MarshalAgentRunStatus(in v1alpha1.AgentRunStatus) ([]byte, error) {
	return Encode(in)
}

// EncodeStatus is an alias for Encode.
func EncodeStatus(in v1alpha1.AgentRunStatus) ([]byte, error) {
	return Encode(in)
}

// Size returns the encoded size of the bounded status projection.
func Size(in v1alpha1.AgentRunStatus) (int, error) {
	encoded, err := Encode(in)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}

// StatusSize is an alias for Size.
func StatusSize(in v1alpha1.AgentRunStatus) (int, error) {
	return Size(in)
}

// Fits reports whether the bounded projection can be persisted under the
// status budget.
func Fits(in v1alpha1.AgentRunStatus) bool {
	_, err := Encode(in)
	return err == nil
}

// SetPhase applies a table-checked phase transition without inventing a
// timestamp. Use SetPhaseAt when the controller has a reconciliation clock.
func SetPhase(status *v1alpha1.AgentRunStatus, phase v1alpha1.Phase) error {
	return SetPhaseAt(status, phase, time.Time{})
}

// SetPhaseAt applies a table-checked phase transition and fills the bounded
// lifecycle timestamps when now is non-zero.
func SetPhaseAt(status *v1alpha1.AgentRunStatus, phase v1alpha1.Phase, now time.Time) error {
	if status == nil {
		return ErrNilStatus
	}
	copy := cloneStatus(*status)
	if copy.Phase == "" {
		if phase != v1alpha1.PhasePending {
			return fmt.Errorf("%w: empty status may only start at Pending", fsm.ErrInvalidTransition)
		}
	} else if err := fsm.TransitionTo(copy.Phase, phase); err != nil {
		return err
	}
	copy.Phase = phase
	if !now.IsZero() {
		now = now.UTC()
		if copy.StartedAt == nil && phase != v1alpha1.PhasePending {
			started := metav1.NewTime(now)
			copy.StartedAt = &started
		}
		if fsm.IsTerminal(phase) && copy.CompletedAt == nil {
			completed := metav1.NewTime(now)
			copy.CompletedAt = &completed
		}
	}
	*status = copy
	return nil
}

// SetFailure records a bounded failure and transitions the run to Failed.
func SetFailure(status *v1alpha1.AgentRunStatus, code, message string) error {
	return SetFailureAt(status, code, message, time.Time{})
}

// SetFailureAt records a bounded failure, transitions to Failed, and records
// the terminal timestamp from the controller's reconciliation clock.
func SetFailureAt(status *v1alpha1.AgentRunStatus, code, message string, now time.Time) error {
	if status == nil {
		return ErrNilStatus
	}
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: failure code is required", ErrInvalidStatus)
	}
	if err := validateBoundedText("failure code", code, MaxFailureCodeBytes); err != nil {
		return err
	}
	if err := validateBoundedText("failure message", message, MaxStatusMessageBytes); err != nil {
		return err
	}
	copy := cloneStatus(*status)
	if err := SetPhaseAt(&copy, v1alpha1.PhaseFailed, now); err != nil {
		return err
	}
	copy.Failure = &v1alpha1.FailureStatus{Code: code, Message: message}
	*status = copy
	return nil
}

// SetCondition replaces the current condition of the same type or appends a
// new one. It never appends an event-history entry and keeps conditions sorted
// by type for deterministic status output.
func SetCondition(status *v1alpha1.AgentRunStatus, condition v1alpha1.Condition) error {
	if status == nil {
		return ErrNilStatus
	}
	if err := validateCondition(condition); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
	}
	copy := cloneStatus(*status)
	condition.Type = boundedString(condition.Type, MaxConditionTypeBytes)
	condition.Reason = boundedString(condition.Reason, MaxReasonBytes)
	condition.Message = boundedString(condition.Message, MaxStatusMessageBytes)
	for index := range copy.Conditions {
		if copy.Conditions[index].Type == condition.Type {
			copy.Conditions[index] = condition
			sort.Slice(copy.Conditions, func(i, j int) bool { return copy.Conditions[i].Type < copy.Conditions[j].Type })
			*status = copy
			return nil
		}
	}
	if len(copy.Conditions) >= MaxStatusConditions {
		return ErrTooManyConditions
	}
	copy.Conditions = append(copy.Conditions, condition)
	sort.Slice(copy.Conditions, func(i, j int) bool { return copy.Conditions[i].Type < copy.Conditions[j].Type })
	*status = copy
	return nil
}

// SetResolvedSpecRef updates the immutable resolved-spec artifact reference
// after validating that it contains no embedded credential material.
func SetResolvedSpecRef(status *v1alpha1.AgentRunStatus, ref v1alpha1.ArtifactRef) error {
	if status == nil {
		return ErrNilStatus
	}
	return setArtifactRef(status, &status.ResolvedSpecRef, ref)
}

// SetContextPackRef updates the context evidence reference only after its
// caller has independently validated the immutable object contents.
func SetContextPackRef(status *v1alpha1.AgentRunStatus, ref v1alpha1.ArtifactRef) error {
	if status == nil {
		return ErrNilStatus
	}
	return setArtifactRef(status, &status.ContextPackRef, ref)
}

// SetEventStreamRef updates the bounded runtime event-stream reference.
func SetEventStreamRef(status *v1alpha1.AgentRunStatus, ref v1alpha1.ArtifactRef) error {
	if status == nil {
		return ErrNilStatus
	}
	return setArtifactRef(status, &status.EventStreamRef, ref)
}

func setArtifactRef(status *v1alpha1.AgentRunStatus, destination **v1alpha1.ArtifactRef, ref v1alpha1.ArtifactRef) error {
	if status == nil {
		return ErrNilStatus
	}
	if err := validateArtifactRef(&ref); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStatus, err)
	}
	copy := ref
	*destination = &copy
	return nil
}

func boundedArtifactRef(ref *v1alpha1.ArtifactRef) *v1alpha1.ArtifactRef {
	if ref == nil {
		return nil
	}
	return &v1alpha1.ArtifactRef{
		URI:       boundedString(ref.URI, MaxArtifactURIBytes),
		Digest:    boundedString(ref.Digest, MaxDigestBytes),
		Kind:      boundedString(ref.Kind, 64),
		Name:      boundedString(ref.Name, 128),
		MediaType: boundedString(ref.MediaType, 128),
		SizeBytes: ref.SizeBytes,
	}
}

func boundedConditions(conditions []v1alpha1.Condition) []v1alpha1.Condition {
	bounded := make([]v1alpha1.Condition, 0, min(len(conditions), MaxStatusConditions))
	indices := make(map[string]int, MaxStatusConditions)
	for _, condition := range conditions {
		condition.Type = boundedString(condition.Type, MaxConditionTypeBytes)
		condition.Reason = boundedString(condition.Reason, MaxReasonBytes)
		condition.Message = boundedString(condition.Message, MaxStatusMessageBytes)
		if index, exists := indices[condition.Type]; exists {
			bounded[index] = condition
			continue
		}
		if len(bounded) >= MaxStatusConditions {
			continue
		}
		indices[condition.Type] = len(bounded)
		bounded = append(bounded, condition)
	}
	sort.SliceStable(bounded, func(i, j int) bool { return bounded[i].Type < bounded[j].Type })
	return bounded
}

func validateArtifactRef(ref *v1alpha1.ArtifactRef) error {
	if ref == nil {
		return nil
	}
	if err := validateBoundedText("artifact URI", ref.URI, MaxArtifactURIBytes); err != nil {
		return err
	}
	if ref.URI == "" {
		return errors.New("artifact URI is required")
	}
	if err := validateSafeURL("artifact URI", ref.URI, MaxArtifactURIBytes); err != nil {
		return err
	}
	if !canonical.ValidDigest(ref.Digest) {
		return errors.New("artifact digest must be a lowercase sha256 digest")
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "artifact kind", value: ref.Kind, limit: 64},
		{name: "artifact name", value: ref.Name, limit: 128},
		{name: "artifact media type", value: ref.MediaType, limit: 128},
	} {
		if err := validateBoundedText(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if ref.Kind != "" && !descriptorName.MatchString(ref.Kind) {
		return errors.New("artifact kind has an invalid shape")
	}
	if ref.Name != "" && !descriptorName.MatchString(ref.Name) {
		return errors.New("artifact name has an invalid shape")
	}
	if ref.SizeBytes < 0 || ref.SizeBytes > 1<<40 {
		return errors.New("artifact size is outside the supported range")
	}
	return nil
}

func validateChildRef(ref *v1alpha1.ChildRef) error {
	if ref == nil {
		return nil
	}
	if err := validateBoundedText("child name", ref.Name, MaxChildNameBytes); err != nil {
		return err
	}
	if err := validateBoundedText("child kind", ref.Kind, MaxChildKindBytes); err != nil {
		return err
	}
	if ref.Name == "" || ref.Kind == "" {
		return errors.New("child name and kind are required")
	}
	if ref.Kind != "Sandbox" && ref.Kind != "Job" {
		return fmt.Errorf("unsupported child kind %q", ref.Kind)
	}
	if err := validateBoundedText("child UID", ref.UID, 64); err != nil {
		return err
	}
	if err := validateBoundedText("child role", ref.Role, 64); err != nil {
		return err
	}
	if ref.SpecDigest != "" && !canonical.ValidDigest(ref.SpecDigest) {
		return errors.New("child specDigest must be a lowercase sha256 digest")
	}
	if !canonical.ValidDigest(ref.PlanFingerprint) {
		return fmt.Errorf("%s child planFingerprint must be a lowercase sha256 digest", ref.Kind)
	}
	return nil
}

func validateCondition(condition v1alpha1.Condition) error {
	if err := validateBoundedText("condition type", condition.Type, MaxConditionTypeBytes); err != nil {
		return err
	}
	if condition.Type == "" {
		return errors.New("condition type is required")
	}
	if condition.Status != v1alpha1.ConditionTrue && condition.Status != v1alpha1.ConditionFalse && condition.Status != v1alpha1.ConditionUnknown {
		return fmt.Errorf("condition status %q is unsupported", condition.Status)
	}
	if err := validateBoundedText("condition reason", condition.Reason, MaxReasonBytes); err != nil {
		return err
	}
	return validateBoundedText("condition message", condition.Message, MaxStatusMessageBytes)
}

func validateSafeURL(label, value string, maxBytes int) error {
	if value == "" {
		return nil
	}
	if err := validateBoundedText(label, value, maxBytes); err != nil {
		return err
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s is not a valid URI", label)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s contains URI userinfo", ErrStatusSecretURI, label)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: %s must not contain a query or fragment", ErrStatusSecretURI, label)
	}
	switch parsed.Scheme {
	case "s3":
		if label != "artifact URI" || parsed.Host == "" {
			return fmt.Errorf("%s uses an unsupported S3 URI", label)
		}
	case "https":
		if parsed.Host == "" {
			return fmt.Errorf("%s requires an HTTPS host", label)
		}
	default:
		return fmt.Errorf("%s uses unsupported URI scheme %q", label, parsed.Scheme)
	}
	for key := range parsed.Query() {
		normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))
		for _, sensitive := range []string{"token", "secret", "credential", "password", "apikey", "accesskey", "signature", "authorization"} {
			if strings.Contains(normalized, sensitive) {
				return fmt.Errorf("%w: %s query contains %q", ErrStatusSecretURI, label, key)
			}
		}
	}
	return nil
}

func validateBoundedText(label, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s contains an unsafe control character", label)
	}
	return nil
}

func boundedString(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func maxInt32(value int32) int32 {
	if value < 0 {
		return 0
	}
	return value
}

func marshalRaw(in v1alpha1.AgentRunStatus) ([]byte, error) {
	return json.Marshal(in)
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// cloneStatus intentionally lives here instead of relying on generated API
// deepcopy code. Status projection must remain safe even while generated API
// artifacts are being regenerated by the v3 build.
func cloneStatus(in v1alpha1.AgentRunStatus) v1alpha1.AgentRunStatus {
	out := in
	if in.ResolvedSpecRef != nil {
		ref := *in.ResolvedSpecRef
		out.ResolvedSpecRef = &ref
	}
	if in.WorkSandboxRef != nil {
		child := *in.WorkSandboxRef
		out.WorkSandboxRef = &child
	}
	if in.VerifySandboxRef != nil {
		child := *in.VerifySandboxRef
		out.VerifySandboxRef = &child
	}
	if in.Patch != nil {
		patch := *in.Patch
		if in.Patch.Ref != nil {
			ref := *in.Patch.Ref
			patch.Ref = &ref
		}
		if in.Patch.ManifestRef != nil {
			ref := *in.Patch.ManifestRef
			patch.ManifestRef = &ref
		}
		out.Patch = &patch
	}
	if in.Gate != nil {
		gate := *in.Gate
		if in.Gate.ReportRef != nil {
			ref := *in.Gate.ReportRef
			gate.ReportRef = &ref
		}
		gate.Checks = append([]v1alpha1.GateCheck(nil), in.Gate.Checks...)
		out.Gate = &gate
	}
	if in.Effect != nil {
		effect := *in.Effect
		out.Effect = &effect
	}
	if in.EventStreamRef != nil {
		ref := *in.EventStreamRef
		out.EventStreamRef = &ref
	}
	if in.Failure != nil {
		failure := *in.Failure
		out.Failure = &failure
	}
	if in.StartedAt != nil {
		started := *in.StartedAt
		out.StartedAt = &started
	}
	if in.CompletedAt != nil {
		completed := *in.CompletedAt
		out.CompletedAt = &completed
	}
	out.Conditions = append([]v1alpha1.Condition(nil), in.Conditions...)
	out.Children = append([]v1alpha1.ChildRef(nil), in.Children...)
	out.Artifacts = append([]v1alpha1.ArtifactRef(nil), in.Artifacts...)
	return out
}
