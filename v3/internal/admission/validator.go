// Package admission contains the fail-closed AgentRun admission boundary.
package admission

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator is a controller-runtime admission handler. It fails closed when
// the preflight ConfigMap cannot be read, is stale, or has the wrong runtime
// fingerprint.
type Validator struct {
	Decoder            webhookadmission.Decoder
	Reader             client.Reader
	TTL                time.Duration
	Expected           preflight.Fingerprint
	Clock              func() time.Time
	Namespace          string
	PreflightNamespace string
}

func NewValidator(scheme *runtime.Scheme, reader client.Reader, ttl time.Duration, expected preflight.Fingerprint) *Validator {
	return &Validator{
		Decoder:  webhookadmission.NewDecoder(scheme),
		Reader:   reader,
		TTL:      ttl,
		Expected: expected,
		Clock:    time.Now,
	}
}

func (v *Validator) Handle(ctx context.Context, req webhookadmission.Request) webhookadmission.Response {
	if req.Operation == admissionv1.Delete {
		return webhookadmission.Allowed("deletion does not create an execution boundary")
	}

	run := &v1alpha1.AgentRun{}
	if err := v.Decoder.Decode(req, run); err != nil {
		return webhookadmission.Errored(400, fmt.Errorf("decode AgentRun: %w", err))
	}
	if run.Namespace == "" {
		run.Namespace = req.Namespace
	}
	if v.Namespace != "" && run.Namespace != v.Namespace {
		return webhookadmission.Denied("AgentRun namespace is not managed by this operator")
	}
	clock := time.Now
	if v.Clock != nil {
		clock = v.Clock
	}

	var previous *v1alpha1.AgentRun
	if req.Operation == admissionv1.Update {
		if len(req.OldObject.Raw) == 0 {
			return webhookadmission.Errored(400, fmt.Errorf("previous AgentRun is required for update validation"))
		}
		previous = &v1alpha1.AgentRun{}
		if err := v.Decoder.DecodeRaw(req.OldObject, previous); err != nil {
			return webhookadmission.Errored(400, fmt.Errorf("decode previous AgentRun: %w", err))
		}
	}
	if err := ValidateAgentRunStatic(run, previous); err != nil {
		return webhookadmission.Denied(err.Error())
	}
	// Cancellation is an availability and safety operation: once its one-way
	// transition is validated, stale preflight state or a deleted configuration
	// reference must not prevent the operator from stopping active work.
	if previous != nil && !previous.Spec.CancelRequested && run.Spec.CancelRequested {
		return webhookadmission.Allowed("one-way cancellation request is valid")
	}
	if err := ValidateAgentRunDynamic(ctx, v.Reader, run, DynamicOptions{
		Now: clock(), PreflightTTL: v.TTL, ExpectedFingerprint: v.Expected, PreflightNamespace: v.PreflightNamespace,
	}); err != nil {
		return webhookadmission.Denied(err.Error())
	}
	return webhookadmission.Allowed("preflight, references, and static AgentRun invariants are valid")
}

var _ webhookadmission.Handler = (*Validator)(nil)
