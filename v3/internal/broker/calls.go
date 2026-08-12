package broker

import (
	"context"
	"encoding/json"
	"errors"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

// ToolCallRequest is the trusted host-side representation of one tool call.
// HTTP callers cannot supply Server, Effect, or approval policy; the local MCP
// handler resolves those from the immutable ToolSet compiled at construction.
type ToolCallRequest struct {
	Server    string
	Tool      string
	Arguments json.RawMessage
	Approved  bool
}

// ToolCallResult is bounded and safe to return to the local runtime. Content
// is an MCP CallToolResult object copied from the upstream after validation.
type ToolCallResult struct {
	Content     json.RawMessage
	EffectKey   string
	EffectState string
	Reference   *ToolResultReference
}

// CallTool evaluates the immutable allowlist, validates exact JSON arguments,
// resolves only the fixed credential key, and performs the upstream call. A
// write/publish call is enclosed by one durable effect claim and one terminal
// commit. Once the external call may have happened, all uncertainty becomes
// ErrUnknownEffect and is never retried by this package.
func (b *Broker) CallTool(ctx context.Context, request ToolCallRequest) (ToolCallResult, error) {
	var zero ToolCallResult
	if b == nil || ctx == nil || request.Server == "" || request.Tool == "" {
		return zero, ErrInvalidRequest
	}
	tool, lookup := b.lookupToolForCall(request.Server, request.Tool)
	if lookup == toolLookupPhaseDenied {
		return zero, ErrPhaseDenied
	}
	if err := b.reserveToolCall(ctx); err != nil {
		return zero, err
	}
	arguments := request.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if len(arguments) > MaxCallArgumentsBytes || strictjson.ValidateObject(arguments) != nil {
		return zero, ErrInvalidRequest
	}
	normalized, err := strictjson.Normalize(arguments)
	if err != nil || strictjson.ValidateObject(normalized) != nil {
		return zero, ErrInvalidRequest
	}
	if lookup != toolLookupAllowed {
		return zero, ErrDenied
	}
	decision := toolpolicy.Evaluate([]toolpolicy.Grant{{
		Server: request.Server, Tool: request.Tool, Effect: tool.policyEffect,
		Approval: approvalMode(tool.effect, b.requireApproval), Arguments: tool.exactArgs,
	}}, toolpolicy.Request{
		Server: request.Server, Tool: request.Tool, Arguments: normalized,
		Approved: request.Approved || !b.requireApproval || tool.effect == v1alpha1.EffectRead,
	})
	if decision.NeedsApproval {
		return zero, ErrApprovalRequired
	}
	if !decision.Allowed {
		return zero, ErrDenied
	}
	server, ok := b.servers[request.Server]
	if !ok {
		return zero, ErrDenied
	}
	mutating := tool.effect != v1alpha1.EffectRead
	digest := requestDigest(request.Server, request.Tool, normalized)
	effectKey := ""
	if mutating {
		effectKey, err = effects.EffectKey(b.runUID, b.baseSHA, digest, effectOperation(tool.effect))
		if err != nil {
			return zero, ErrInvalidRequest
		}
		claim, claimErr := b.effects.Claim(ctx, effects.Claim{
			EffectKey: effectKey, RequestDigest: digest, Operation: effectOperation(tool.effect), RunUID: b.runUID,
		})
		if claimErr != nil {
			if errors.Is(claimErr, effects.ErrUnknown) || claim.Unknown {
				return zero, ErrUnknownEffect
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return zero, ctxErr
			}
			return zero, ErrEffectLedgerUnavailable
		}
		if claim.Unknown {
			return zero, ErrUnknownEffect
		}
		if !claim.Execute {
			return zero, ErrEffectAlreadyClaimed
		}
	}
	credential, credentialErr := b.resolveCredential(ctx, server.credentialRef)
	if credentialErr != nil {
		if mutating && b.commitEffect(ctx, effectKey, digest, effects.OutcomeFailed, "", "") != nil {
			return zero, ErrUnknownEffect
		}
		return zero, credentialErr
	}
	defer zeroBytes(credential)
	response, callErr := b.invokeMCP(ctx, server.endpoint, request.Server, request.Tool, normalized, credential)
	if callErr != nil {
		if mutating {
			_ = b.commitEffect(ctx, effectKey, digest, effects.OutcomeUnknown, "", "")
			return zero, ErrUnknownEffect
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, callErr
	}
	if response.isError && mutating {
		_ = b.commitEffect(ctx, effectKey, digest, effects.OutcomeUnknown, "", "")
		return zero, ErrUnknownEffect
	}
	shaped, shapeErr := b.shapeToolResult(ctx, response.content, response.isError, credential)
	if shapeErr != nil {
		if mutating {
			// The external operation may already have succeeded. A missing or
			// malformed result reference is therefore an unknown effect, never a
			// reason to replay the MCP write.
			_ = b.commitEffect(ctx, effectKey, digest, effects.OutcomeUnknown, "", "")
			return zero, ErrUnknownEffect
		}
		return zero, shapeErr
	}
	resultDigest := shaped.digest
	resultRef := ""
	if shaped.reference != nil {
		resultRef = shaped.reference.URI
	}
	if mutating {
		if err := b.commitEffect(ctx, effectKey, digest, effects.OutcomeSucceeded, resultDigest, resultRef); err != nil {
			// The upstream result is known, but the ledger result is not. The
			// broker must not replay the operation to find out.
			_ = b.commitEffect(ctx, effectKey, digest, effects.OutcomeUnknown, "", "")
			return zero, ErrUnknownEffect
		}
	}
	state := "read"
	if mutating {
		state = "succeeded"
	}
	return ToolCallResult{Content: append(json.RawMessage(nil), shaped.content...), EffectKey: effectKey, EffectState: state, Reference: shaped.reference}, nil
}

func approvalMode(effect v1alpha1.EffectKind, required bool) toolpolicy.ApprovalMode {
	if effect == v1alpha1.EffectRead || !required {
		return toolpolicy.ApprovalAllow
	}
	return toolpolicy.ApprovalRequired
}

func effectOperation(effect v1alpha1.EffectKind) string {
	if effect == v1alpha1.EffectPublish {
		return "mcp-publish"
	}
	return "mcp-write"
}

func (b *Broker) resolveCredential(ctx context.Context, logicalRef string) ([]byte, error) {
	if logicalRef == "" {
		return nil, nil
	}
	if b.credentials == nil {
		return nil, ErrCredentialUnavailable
	}
	value, err := b.credentials.ResolveToken(ctx, logicalRef)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrCredentialUnavailable
	}
	if !boundedCredential(value) {
		zeroBytes(value)
		return nil, ErrCredentialUnavailable
	}
	return append([]byte(nil), value...), nil
}

func (b *Broker) commitEffect(ctx context.Context, effectKey, requestDigest string, state effects.OutcomeState, resultDigest, resultRef string) error {
	if b == nil || b.effects == nil || effectKey == "" {
		return ErrEffectLedgerUnavailable
	}
	err := b.effects.Commit(ctx, effects.Outcome{
		EffectKey: effectKey, RequestDigest: requestDigest, State: state,
		ResultDigest: resultDigest, ResultRef: resultRef,
	})
	if err != nil {
		return ErrEffectLedgerUnavailable
	}
	return nil
}

func (b *Broker) reserveToolCall(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	if b.budget.toolCalls >= b.budget.maxToolCalls {
		return ErrBudgetExceeded
	}
	b.budget.toolCalls++
	return nil
}

// Usage returns a point-in-time bounded counter projection.
func (b *Broker) Usage() Usage {
	if b == nil {
		return Usage{}
	}
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	return Usage{ToolCalls: b.budget.toolCalls, ModelRequests: b.budget.modelRequests, ModelTokens: b.budget.modelTokens, ModelCostMicros: b.budget.modelCostMicros}
}
