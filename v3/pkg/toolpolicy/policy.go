// Package toolpolicy evaluates the least-privilege capability boundary before
// a broker dispatches an MCP call. MCP annotations are inputs, not authority.
package toolpolicy

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

type Effect string

const (
	EffectRead        Effect = "read"
	EffectWrite       Effect = "write"
	EffectDestructive Effect = "destructive"
	EffectUnknown     Effect = "unknown"
)

type ApprovalMode string

const (
	ApprovalDeny          ApprovalMode = "deny"
	ApprovalAllow         ApprovalMode = "allow"
	ApprovalRequired      ApprovalMode = "approve"
	ApprovalProposeCommit ApprovalMode = "propose/commit"
)

type Grant struct {
	Server    string
	Tool      string
	Resources []string
	Effect    Effect
	Approval  ApprovalMode
	Arguments json.RawMessage
}

type Request struct {
	Server         string
	Tool           string
	Resource       string
	Arguments      json.RawMessage
	DeclaredEffect Effect
	Approved       bool
}

type Decision struct {
	Allowed, NeedsApproval bool
	Reason                 string
	EffectiveEffect        Effect
}

func Evaluate(grants []Grant, request Request) Decision {
	for _, grant := range grants {
		if grant.Server != request.Server || grant.Tool != request.Tool {
			continue
		}
		if !resourceAllowed(grant.Resources, request.Resource) {
			continue
		}
		if grant.Arguments != nil && !exactJSONEqual(grant.Arguments, request.Arguments) {
			continue
		}
		effect := grant.Effect
		if effect == "" || effect == EffectUnknown {
			effect = EffectWrite
		}
		mode := grant.Approval
		if mode == "" {
			if effect == EffectRead {
				mode = ApprovalAllow
			} else {
				mode = ApprovalRequired
			}
		}
		switch mode {
		case ApprovalDeny:
			return Decision{Reason: "capability is explicitly denied", EffectiveEffect: effect}
		case ApprovalRequired, ApprovalProposeCommit:
			if !request.Approved {
				return Decision{NeedsApproval: true, Reason: "approval is required", EffectiveEffect: effect}
			}
			return Decision{Allowed: true, Reason: "approved capability", EffectiveEffect: effect}
		case ApprovalAllow:
			return Decision{Allowed: true, Reason: "capability grant permits call", EffectiveEffect: effect}
		default:
			return Decision{Reason: "invalid approval mode", EffectiveEffect: effect}
		}
	}
	return Decision{Reason: "no matching capability grant", EffectiveEffect: EffectUnknown}
}

// exactJSONEqual compares JSON values structurally. Object member order is
// insignificant, arrays remain ordered, and JSON numbers compare by value so
// equivalent forms such as 1 and 1.0 are accepted. This is intentionally
// private: callers should only express constraints through ToolSet grants.
func exactJSONEqual(expected, actual json.RawMessage) bool {
	return strictjson.EqualObjects(expected, actual)
}

// ValidJSONObject reports whether raw is exactly one JSON object and contains
// no duplicate keys at any nesting depth. It never returns payload details.
func ValidJSONObject(raw json.RawMessage) bool {
	return strictjson.ValidateObject(raw) == nil
}

func resourceAllowed(patterns []string, resource string) bool {
	if len(patterns) == 0 {
		return resource == ""
	}
	for _, pattern := range patterns {
		if pattern == resource {
			return true
		}
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := path.Match(pattern, resource); err == nil && ok {
				return true
			}
		}
	}
	return false
}
