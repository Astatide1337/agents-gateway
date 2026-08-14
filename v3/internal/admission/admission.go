// Package admission contains the validation decisions that sit in front of
// the v3 AgentRun controller. It deliberately does not contain a webhook
// server, manifests, or controller reconciliation.
package admission

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

const (
	// Error and violation bounds are part of the admission contract. The
	// webhook adapter can safely serialize ValidationErrors without allowing
	// object contents or API error strings to grow the response indefinitely.
	MaxValidationErrors       = 8
	MaxValidationFieldBytes   = 128
	MaxValidationMessageBytes = 256
	MaxValidationErrorBytes   = 2048
)

// Code is a stable, bounded admission reason. Values are intentionally
// independent of Kubernetes API error text.
type Code string

const (
	CodeInvalidObject                Code = "InvalidObject"
	CodeReferenceRequired            Code = "ReferenceRequired"
	CodeReferenceInvalid             Code = "ReferenceInvalid"
	CodeReferenceMissing             Code = "ReferenceMissing"
	CodeReferenceUnavailable         Code = "ReferenceUnavailable"
	CodeReferenceTerminating         Code = "ReferenceTerminating"
	CodePolicyReferenceMismatch      Code = "PolicyReferenceMismatch"
	CodeCancelReversal               Code = "CancelReversal"
	CodeCancelAfterTerminal          Code = "CancelAfterTerminal"
	CodeFinalizerProtected           Code = "FinalizerProtected"
	CodeSpecImmutable                Code = "SpecImmutable"
	CodeTaskSourceInvalid            Code = "TaskSourceInvalid"
	CodeTaskContentInvalid           Code = "TaskContentInvalid"
	CodeTaskContentEmpty             Code = "TaskContentEmpty"
	CodeTaskContentTooLong           Code = "TaskContentTooLong"
	CodePreflightMissing             Code = "PreflightMissing"
	CodePreflightMalformed           Code = "PreflightMalformed"
	CodePreflightFailed              Code = "PreflightFailed"
	CodePreflightStale               Code = "PreflightStale"
	CodePreflightFuture              Code = "PreflightFutureTimestamp"
	CodePreflightFingerprintMismatch Code = "PreflightFingerprintMismatch"
	CodePreflightUnavailable         Code = "PreflightUnavailable"
	CodePreflightConfiguration       Code = "PreflightConfiguration"
	CodeMCPServerEndpointInvalid     Code = "MCPServerEndpointInvalid"
	CodeMCPServerEndpointNotHTTPS    Code = "MCPServerEndpointNotHTTPS"
	CodeMCPServerEndpointUserInfo    Code = "MCPServerEndpointUserInfo"
	CodeMCPServerEndpointQuery       Code = "MCPServerEndpointQuery"
	CodeMCPServerEndpointFragment    Code = "MCPServerEndpointFragment"
	CodeMCPServerEndpointHost        Code = "MCPServerEndpointHost"
	CodeMCPServerEndpointLocal       Code = "MCPServerEndpointLocal"
	CodeMCPServerEndpointAddress     Code = "MCPServerEndpointAddress"
	CodeExactArgumentsInvalid        Code = "ExactArgumentsInvalid"
	CodeCredentialLikeArgumentKey    Code = "CredentialLikeArgumentKey"
	CodeConfigMapReferenceInvalid    Code = "ConfigMapReferenceInvalid"
	CodeConfigMapMissing             Code = "ConfigMapMissing"
	CodeConfigMapKeyMissing          Code = "ConfigMapKeyMissing"
	CodeConfigMapUnavailable         Code = "ConfigMapUnavailable"
	CodeConfigMapTerminating         Code = "ConfigMapTerminating"
	CodeImageNotPinned               Code = "ImageNotPinned"
	CodeGateSignalsInvalid           Code = "GateSignalsInvalid"
	CodeCriticRouteRequired          Code = "CriticModelRouteRequired"
	CodeCriticRouteNotDistinct       Code = "CriticModelRouteNotDistinct"
)

// ValidationError is one safe, bounded admission violation.
type ValidationError struct {
	Code    Code
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	value := string(e.Code)
	if e.Field != "" {
		value += ": " + bound(e.Field, MaxValidationFieldBytes)
	}
	if e.Message != "" {
		value += ": " + bound(e.Message, MaxValidationMessageBytes)
	}
	return bound(value, MaxValidationErrorBytes)
}

// ValidationErrors is an ordered, bounded collection of violations. It is a
// value type so callers can return it as an error without sharing mutable
// internal state.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	parts := make([]string, 0, len(e))
	for index := 0; index < len(e) && index < MaxValidationErrors; index++ {
		parts = append(parts, e[index].Error())
	}
	message := strings.Join(parts, "; ")
	if len(e) > MaxValidationErrors {
		message += "; additional validation errors omitted"
	}
	return bound(message, MaxValidationErrorBytes)
}

// Err converts an empty collection to a nil error, which is convenient for
// adapters that return the standard error interface.
func (e ValidationErrors) Err() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Violations returns a copy suitable for metrics or a bounded API response.
func (e ValidationErrors) Violations() []ValidationError {
	count := len(e)
	if count > MaxValidationErrors {
		count = MaxValidationErrors
	}
	return append([]ValidationError(nil), e[:count]...)
}

// DynamicOptions supplies the admission-time clock, freshness policy, and
// expected node/runtime identity. A zero clock or non-positive TTL fails
// closed through the preflight decision.
type DynamicOptions struct {
	Now                 time.Time
	PreflightTTL        time.Duration
	ExpectedFingerprint preflight.Fingerprint
	PreflightNamespace  string
}

// ReferenceState is the result of one Kubernetes object lookup. Unknown is
// deliberately a denial state: dynamic validation must never treat an
// unperformed lookup as success.
type ReferenceState uint8

const (
	ReferenceUnknown ReferenceState = iota
	ReferenceFound
	ReferenceMissing
	ReferenceUnavailable
	ReferenceInvalid
	ReferenceTerminating
)

// DynamicState is the pure input to ValidateDynamicState. AgentRef and
// GateRef are the direct AgentRun references. The two nested references are
// read from the referenced Agent and are required by the v3 API contract.
type DynamicState struct {
	Agent            ReferenceState
	Gate             ReferenceState
	ToolSet          ReferenceState
	ModelRoute       ReferenceState
	CriticModelRoute ReferenceState
	ContextStrategy  ReferenceState

	ToolSetRef           string
	ModelRouteRef        string
	CriticModelRouteRef  string
	ContextStrategyRef   string
	GateSignals          *v1alpha1.GateSignalsSpec
	WorkerModelRoute     *v1alpha1.ModelRouteSpec
	CriticModelRouteSpec *v1alpha1.ModelRouteSpec
	PolicyRefs           []string
	PolicyStates         []ReferenceState
	PolicyRefsMatch      bool

	Preflight preflight.Decision
}

// ValidateAgentRunStaticErrors validates the parts of an AgentRun that do not
// require Kubernetes reads: reference shape and one-way cancellation.
func ValidateAgentRunStaticErrors(run, previous *v1alpha1.AgentRun) ValidationErrors {
	var violations ValidationErrors
	if run == nil {
		add(&violations, CodeInvalidObject, "metadata", "AgentRun is required")
		return violations
	}

	if run.Name != "" && len(validation.IsDNS1123Subdomain(run.Name)) != 0 {
		add(&violations, CodeInvalidObject, "metadata.name", "metadata.name must be a DNS subdomain")
	}
	if run.Namespace != "" && len(validation.IsDNS1123Subdomain(run.Namespace)) != 0 {
		add(&violations, CodeInvalidObject, "metadata.namespace", "metadata.namespace must be a DNS subdomain")
	}
	validateReference(&violations, "spec.agentRef", run.Spec.AgentRef)
	validateReference(&violations, "spec.gateRef", run.Spec.GateRef)
	validateTaskSpec(&violations, run.Spec.Task)
	if previous != nil {
		if run.DeletionTimestamp == nil && containsFinalizer(previous.Finalizers, "agw.astatide.com/cleanup") && !containsFinalizer(run.Finalizers, "agw.astatide.com/cleanup") {
			add(&violations, CodeFinalizerProtected, "metadata.finalizers", "the controller cleanup finalizer cannot be removed by a normal AgentRun update")
		}
		previousSpec := previous.Spec
		currentSpec := run.Spec
		currentSpec.CancelRequested = previousSpec.CancelRequested
		if !reflect.DeepEqual(previousSpec, currentSpec) {
			add(&violations, CodeSpecImmutable, "spec", "AgentRun execution spec is immutable after creation")
		}
	}
	violations = appendBounded(violations, ValidateCancelTransition(previous, run)...)
	return violations
}

func containsFinalizer(finalizers []string, wanted string) bool {
	for _, finalizer := range finalizers {
		if finalizer == wanted {
			return true
		}
	}
	return false
}

// ValidateAgentRunStatic is the standard error-returning form of the static
// check. It is intentionally separate from dynamic validation so a webhook
// can report malformed user input before attempting Kubernetes reads.
func ValidateAgentRunStatic(run, previous *v1alpha1.AgentRun) error {
	return ValidateAgentRunStaticErrors(run, previous).Err()
}

// ValidateCancelTransition is a pure one-way cancellation decision. A true
// value may remain true, but it can never be cleared. A newly requested
// cancellation is also rejected once the run is terminal.
func ValidateCancelTransition(previous, current *v1alpha1.AgentRun) ValidationErrors {
	var violations ValidationErrors
	if current == nil {
		add(&violations, CodeInvalidObject, "metadata", "AgentRun is required")
		return violations
	}
	if previous != nil && previous.Spec.CancelRequested && !current.Spec.CancelRequested {
		add(&violations, CodeCancelReversal, "spec.cancelRequested", "cancelRequested is one-way and cannot be cleared")
		return violations
	}

	wasRequested := previous != nil && previous.Spec.CancelRequested
	newRequest := current.Spec.CancelRequested && !wasRequested
	if newRequest && (isTerminal(previousPhase(previous)) || isTerminal(current.Status.Phase)) {
		add(&violations, CodeCancelAfterTerminal, "spec.cancelRequested", "a terminal AgentRun cannot be cancelled")
	}
	return violations
}

// ValidateDynamicState is the pure dynamic admission decision. It maps every
// unknown, missing, terminating, or unavailable reference to a bounded
// violation and requires an allowed preflight decision.
func ValidateDynamicState(state DynamicState) ValidationErrors {
	var violations ValidationErrors
	appendPreflightViolation(&violations, state.Preflight)
	appendReferenceViolation(&violations, "spec.agentRef", state.Agent)
	appendReferenceViolation(&violations, "spec.gateRef", state.Gate)

	if state.Agent == ReferenceFound {
		if state.ToolSetRef == "" {
			add(&violations, CodeReferenceRequired, "agent.spec.toolSetRef", "referenced Agent has no toolSetRef")
		} else {
			appendReferenceViolation(&violations, "agent.spec.toolSetRef", state.ToolSet)
		}
		if state.ModelRouteRef == "" {
			add(&violations, CodeReferenceRequired, "agent.spec.modelRouteRef", "referenced Agent has no modelRouteRef")
		} else {
			appendReferenceViolation(&violations, "agent.spec.modelRouteRef", state.ModelRoute)
		}
		if state.ContextStrategyRef == "" {
			add(&violations, CodeReferenceRequired, "agent.spec.contextStrategyRef", "referenced Agent has no contextStrategyRef")
		} else {
			appendReferenceViolation(&violations, "agent.spec.contextStrategyRef", state.ContextStrategy)
		}
		if !state.PolicyRefsMatch {
			add(&violations, CodePolicyReferenceMismatch, "agent.spec.policyRefs", "Agent and Gate policyRefs must contain the same normalized set")
		}
		if len(state.PolicyStates) != len(state.PolicyRefs) {
			add(&violations, CodeReferenceUnavailable, "agent.spec.policyRefs", "policy references could not be verified")
		} else {
			for index, policyState := range state.PolicyStates {
				appendReferenceViolation(&violations, "agent.spec.policyRefs["+strconv.Itoa(index)+"]", policyState)
			}
		}
	}
	if state.Gate == ReferenceFound && state.GateSignals != nil {
		gateSpec := v1alpha1.GateSpec{Signals: state.GateSignals}
		if err := resolved.ValidateGateSignalsSpec(gateSpec); err != nil {
			add(&violations, CodeGateSignalsInvalid, "gate.spec.signals", "Gate signal weights or bounds are invalid")
		} else if state.GateSignals.Critic != nil {
			if state.CriticModelRouteRef == "" {
				add(&violations, CodeCriticRouteRequired, "gate.spec.signals.critic.modelRouteRef", "critic signal requires a model route reference")
			} else {
				appendReferenceViolation(&violations, "gate.spec.signals.critic.modelRouteRef", state.CriticModelRoute)
			}
			if state.ModelRoute == ReferenceFound && state.CriticModelRoute == ReferenceFound {
				if state.WorkerModelRoute == nil || state.CriticModelRouteSpec == nil {
					add(&violations, CodeCriticRouteNotDistinct, "gate.spec.signals.critic.modelRouteRef", "worker and critic model families cannot be proven distinct")
				} else if err := resolved.ValidateDistinctModelRoutes(*state.WorkerModelRoute, *state.CriticModelRouteSpec); err != nil {
					add(&violations, CodeCriticRouteNotDistinct, "gate.spec.signals.critic.modelRouteRef", "worker and critic model families cannot be proven distinct")
				}
			}
		}
	}
	return violations
}

// ValidateAgentRunDynamic performs the Kubernetes-backed checks for a typed
// v3 AgentRun. It reads the v3 API objects directly and keeps the preflight
// decision independent from the reference lookup decisions.
func ValidateAgentRunDynamic(ctx context.Context, reader client.Reader, run *v1alpha1.AgentRun, options DynamicOptions) error {
	var violations ValidationErrors
	if run == nil {
		add(&violations, CodeInvalidObject, "metadata", "AgentRun is required")
		return violations.Err()
	}

	state := DynamicState{
		Preflight: preflight.CheckConfigMapInNamespace(ctx, reader, options.PreflightNamespace, options.Now, options.PreflightTTL, options.ExpectedFingerprint),
	}
	var agentObject *v1alpha1.Agent
	var gateObject *v1alpha1.Gate
	var toolSetObject *v1alpha1.ToolSet
	var modelRouteObject *v1alpha1.ModelRoute
	var criticModelRouteObject *v1alpha1.ModelRoute
	if validReference(run.Spec.AgentRef) {
		var object client.Object
		state.Agent, object = lookup(ctx, reader, run.Namespace, run.Spec.AgentRef, "Agent")
		agentObject, _ = object.(*v1alpha1.Agent)
	} else {
		state.Agent = ReferenceInvalid
	}
	if validReference(run.Spec.GateRef) {
		var object client.Object
		state.Gate, object = lookup(ctx, reader, run.Namespace, run.Spec.GateRef, "Gate")
		gateObject, _ = object.(*v1alpha1.Gate)
	} else {
		state.Gate = ReferenceInvalid
	}

	if state.Agent == ReferenceFound {
		if agentObject == nil {
			state.ToolSet = ReferenceInvalid
			state.ModelRoute = ReferenceInvalid
		} else {
			state.ToolSetRef = agentObject.Spec.ToolSetRef
			state.ModelRouteRef = agentObject.Spec.ModelRouteRef
			state.ContextStrategyRef = agentObject.Spec.ContextStrategyRef
			state.PolicyRefs, _ = resolved.NormalizePolicyRefs(agentObject.Spec.PolicyRefs)
			if state.ContextStrategyRef != "" && validReference(state.ContextStrategyRef) {
				state.ContextStrategy, _ = lookup(ctx, reader, run.Namespace, state.ContextStrategyRef, "ContextStrategy")
			} else {
				state.ContextStrategy = ReferenceInvalid
			}
			if validReference(state.ToolSetRef) {
				var object client.Object
				state.ToolSet, object = lookup(ctx, reader, run.Namespace, state.ToolSetRef, "ToolSet")
				toolSetObject, _ = object.(*v1alpha1.ToolSet)
			} else {
				state.ToolSet = ReferenceInvalid
			}
			if validReference(state.ModelRouteRef) {
				var object client.Object
				state.ModelRoute, object = lookup(ctx, reader, run.Namespace, state.ModelRouteRef, "ModelRoute")
				modelRouteObject, _ = object.(*v1alpha1.ModelRoute)
			} else {
				state.ModelRoute = ReferenceInvalid
			}
		}
	}
	if state.Gate == ReferenceFound && gateObject != nil {
		state.GateSignals = gateObject.Spec.Signals
		if resolved.CriticModelRouteEnabled(gateObject.Spec) {
			state.CriticModelRouteRef = gateObject.Spec.Signals.Critic.ModelRouteRef
			if validReference(state.CriticModelRouteRef) {
				var object client.Object
				state.CriticModelRoute, object = lookup(ctx, reader, run.Namespace, state.CriticModelRouteRef, "ModelRoute")
				criticModelRouteObject, _ = object.(*v1alpha1.ModelRoute)
			} else {
				state.CriticModelRoute = ReferenceInvalid
			}
		}
	}
	if modelRouteObject != nil {
		state.WorkerModelRoute = &modelRouteObject.Spec
	}
	if criticModelRouteObject != nil {
		state.CriticModelRouteSpec = &criticModelRouteObject.Spec
	}
	if state.Agent == ReferenceFound && state.Gate == ReferenceFound && agentObject != nil && gateObject != nil {
		agentPolicies, agentErr := resolved.NormalizePolicyRefs(agentObject.Spec.PolicyRefs)
		gatePolicies, gateErr := resolved.NormalizePolicyRefs(gateObject.Spec.PolicyRefs)
		if agentErr == nil && gateErr == nil && reflect.DeepEqual(agentPolicies, gatePolicies) {
			state.PolicyRefs = agentPolicies
			state.PolicyRefsMatch = true
		}
		if agentErr != nil {
			add(&violations, CodeReferenceInvalid, "agent.spec.policyRefs", "Agent policyRefs are invalid")
		}
		if gateErr != nil {
			add(&violations, CodeReferenceInvalid, "gate.spec.policyRefs", "Gate policyRefs are invalid")
		}
		for _, ref := range state.PolicyRefs {
			policyState, _ := lookup(ctx, reader, run.Namespace, ref, "Policy")
			state.PolicyStates = append(state.PolicyStates, policyState)
		}
	}

	violations = appendBounded(violations, ValidateDynamicState(state)...)
	appendConfigMapReferenceViolation(ctx, reader, run.Namespace, run.Spec.Task.ConfigMapRef, "spec.task.configMapRef", resolved.TaskConfigMapKey, &violations)
	if state.Agent == ReferenceFound && agentObject != nil {
		appendConfigMapReferenceViolation(ctx, reader, run.Namespace, agentObject.Spec.Instructions.ConfigMapRef, "agent.spec.instructions.configMapRef", resolved.InstructionsMapKey, &violations)
	}
	if state.ToolSet == ReferenceFound && toolSetObject != nil {
		violations = appendBounded(violations, ValidateToolSetSecurity(toolSetObject)...)
	}
	if state.Agent == ReferenceFound && agentObject != nil && !resolved.ValidPinnedImage(agentObject.Spec.Runtime.Image) {
		add(&violations, CodeImageNotPinned, "agent.spec.runtime.image", "runtime image must use a non-placeholder sha256 digest")
	}
	if state.Gate == ReferenceFound && gateObject != nil && !resolved.ValidPinnedImage(gateObject.Spec.Verify.Image) {
		add(&violations, CodeImageNotPinned, "gate.spec.verify.image", "verifier image must use a non-placeholder sha256 digest")
	}
	if state.Gate == ReferenceFound && gateObject != nil {
		if err := resolved.ValidateGateSignalsSpec(gateObject.Spec); err != nil {
			add(&violations, CodeGateSignalsInvalid, "gate.spec.signals", "Gate signal weights or bounds are invalid")
		}
	}
	return violations.Err()
}

func validateReference(violations *ValidationErrors, field, value string) {
	if strings.TrimSpace(value) == "" {
		add(violations, CodeReferenceRequired, field, "reference is required")
		return
	}
	if !validReference(value) {
		add(violations, CodeReferenceInvalid, field, "reference must be a DNS subdomain name")
	}
}

func validReference(value string) bool {
	return value != "" && len(validation.IsDNS1123Subdomain(value)) == 0
}

func appendReferenceViolation(violations *ValidationErrors, field string, state ReferenceState) {
	switch state {
	case ReferenceFound:
		return
	case ReferenceMissing:
		add(violations, CodeReferenceMissing, field, "referenced resource does not exist")
	case ReferenceTerminating:
		add(violations, CodeReferenceTerminating, field, "referenced resource is terminating")
	case ReferenceInvalid:
		add(violations, CodeReferenceInvalid, field, "referenced resource has an invalid name or shape")
	default:
		add(violations, CodeReferenceUnavailable, field, "referenced resource could not be verified")
	}
}

func appendPreflightViolation(violations *ValidationErrors, decision preflight.Decision) {
	if decision.Allowed && decision.Reason == preflight.ReasonAllowed {
		return
	}
	switch decision.Reason {
	case preflight.ReasonMissing:
		add(violations, CodePreflightMissing, "preflight", "required preflight evidence is missing")
	case preflight.ReasonMalformed:
		add(violations, CodePreflightMalformed, "preflight", "preflight evidence is malformed")
	case preflight.ReasonFailed:
		add(violations, CodePreflightFailed, "preflight", "preflight checks did not pass")
	case preflight.ReasonStale:
		add(violations, CodePreflightStale, "preflight", "preflight evidence is stale")
	case preflight.ReasonFuture:
		add(violations, CodePreflightFuture, "preflight", "preflight timestamp is in the future")
	case preflight.ReasonFingerprintMismatch:
		add(violations, CodePreflightFingerprintMismatch, "preflight", "preflight fingerprint does not match the target")
	case preflight.ReasonUnavailable:
		add(violations, CodePreflightUnavailable, "preflight", "preflight ConfigMap could not be read")
	default:
		add(violations, CodePreflightConfiguration, "preflight", "preflight policy or decision is invalid")
	}
}

func lookup(ctx context.Context, reader client.Reader, namespace, name, kind string) (ReferenceState, client.Object) {
	if reader == nil {
		return ReferenceUnavailable, nil
	}
	var object client.Object
	switch kind {
	case "Agent":
		object = &v1alpha1.Agent{}
	case "Gate":
		object = &v1alpha1.Gate{}
	case "ToolSet":
		object = &v1alpha1.ToolSet{}
	case "ModelRoute":
		object = &v1alpha1.ModelRoute{}
	case "Policy":
		object = &v1alpha1.Policy{}
	case "ContextStrategy":
		object = &v1alpha1.ContextStrategy{}
	default:
		return ReferenceInvalid, nil
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return ReferenceMissing, nil
		}
		return ReferenceUnavailable, nil
	}
	if object.GetDeletionTimestamp() != nil {
		return ReferenceTerminating, object
	}
	return ReferenceFound, object
}

func validateTaskSpec(violations *ValidationErrors, task v1alpha1.TaskSpec) {
	inlineSet := task.Inline != nil
	configMapSet := task.ConfigMapRef != nil
	if inlineSet == configMapSet {
		add(violations, CodeTaskSourceInvalid, "spec.task", "exactly one of inline or configMapRef must be set")
	}
	if inlineSet {
		if code, message := validateTaskContent(*task.Inline); code != "" {
			add(violations, code, "spec.task.inline", message)
		}
	}
}

func validateTaskContent(value string) (Code, string) {
	if !utf8.ValidString(value) {
		return CodeTaskContentInvalid, "task content must be valid UTF-8"
	}
	if strings.TrimSpace(value) == "" {
		return CodeTaskContentEmpty, "task content must contain non-whitespace content"
	}
	if utf8.RuneCountInString(value) > v1alpha1.MaxTaskLength {
		return CodeTaskContentTooLong, "task content exceeds the maximum length"
	}
	return "", ""
}

func appendConfigMapReferenceViolation(ctx context.Context, reader client.Reader, namespace string, reference *string, field, requiredKey string, violations *ValidationErrors) {
	if reference == nil {
		return
	}
	if !validReference(*reference) {
		add(violations, CodeConfigMapReferenceInvalid, field, "ConfigMap reference must be a DNS subdomain name")
		return
	}
	if reader == nil {
		add(violations, CodeConfigMapUnavailable, field, "referenced ConfigMap could not be verified")
		return
	}
	object := &corev1.ConfigMap{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: *reference}, object)
	if err != nil {
		if apierrors.IsNotFound(err) {
			add(violations, CodeConfigMapMissing, field, "referenced ConfigMap does not exist")
			return
		}
		add(violations, CodeConfigMapUnavailable, field, "referenced ConfigMap could not be verified")
		return
	}
	if object.GetDeletionTimestamp() != nil {
		add(violations, CodeConfigMapTerminating, field, "referenced ConfigMap is terminating")
		return
	}
	if requiredKey == "" {
		return
	}
	value, ok := object.Data[requiredKey]
	if !ok {
		add(violations, CodeConfigMapKeyMissing, field, "referenced ConfigMap is missing the required task key")
		return
	}
	if code, message := validateTaskContent(value); code != "" {
		add(violations, code, field, message)
	}
}

func previousPhase(previous *v1alpha1.AgentRun) v1alpha1.Phase {
	if previous == nil {
		return ""
	}
	return previous.Status.Phase
}

func isTerminal(phase v1alpha1.Phase) bool {
	switch phase {
	case v1alpha1.PhaseSucceeded, v1alpha1.PhaseRejected, v1alpha1.PhaseFailed,
		v1alpha1.PhaseCancelled, v1alpha1.PhaseUnknownEffect:
		return true
	default:
		return false
	}
}

func add(violations *ValidationErrors, code Code, field, message string) {
	if violations == nil || len(*violations) >= MaxValidationErrors {
		return
	}
	*violations = append(*violations, ValidationError{
		Code:    code,
		Field:   bound(field, MaxValidationFieldBytes),
		Message: bound(message, MaxValidationMessageBytes),
	})
}

func appendBounded(dst ValidationErrors, src ...ValidationError) ValidationErrors {
	for _, violation := range src {
		if len(dst) >= MaxValidationErrors {
			break
		}
		add(&dst, violation.Code, violation.Field, violation.Message)
	}
	return dst
}

func bound(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= 3 {
		return string([]rune(value)[:maxBytes])
	}
	var builder strings.Builder
	for _, runeValue := range value {
		runeBytes := utf8.RuneLen(runeValue)
		if runeBytes < 0 || builder.Len()+runeBytes+3 > maxBytes {
			break
		}
		builder.WriteRune(runeValue)
	}
	return builder.String() + "..."
}
