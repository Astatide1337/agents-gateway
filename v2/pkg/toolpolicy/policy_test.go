package toolpolicy

import (
	"encoding/json"
	"testing"
)

func TestUnknownEffectRequiresApproval(t *testing.T) {
	grants := []Grant{{Server: "github", Tool: "call", Resources: []string{"github:repo:owner/*"}, Effect: EffectUnknown}}
	decision := Evaluate(grants, Request{Server: "github", Tool: "call", Resource: "github:repo:owner/repo"})
	if decision.Allowed || !decision.NeedsApproval || decision.EffectiveEffect != EffectWrite {
		t.Fatalf("unexpected decision %#v", decision)
	}
	decision = Evaluate(grants, Request{Server: "github", Tool: "call", Resource: "github:repo:owner/repo", Approved: true})
	if !decision.Allowed {
		t.Fatalf("approved call denied: %#v", decision)
	}
}

func TestResourceBoundary(t *testing.T) {
	grants := []Grant{{Server: "github", Tool: "read", Resources: []string{"github:repo:owner/allowed"}, Effect: EffectRead, Approval: ApprovalAllow}}
	if got := Evaluate(grants, Request{Server: "github", Tool: "read", Resource: "github:repo:owner/other"}); got.Allowed {
		t.Fatal("resource outside grant was allowed")
	}
}

func TestEvaluateExactArgumentsIsSemanticAndExact(t *testing.T) {
	grant := Grant{
		Server:    "github",
		Tool:      "create_branch",
		Effect:    EffectWrite,
		Approval:  ApprovalAllow,
		Arguments: json.RawMessage(`{"owner":"acme","options":{"force":false,"labels":["one",2]},"repo":"gateway"}`),
	}
	tests := []struct {
		name string
		args string
		want bool
	}{
		{name: "object member order and numeric spelling", args: `{"repo":"gateway","options":{"labels":["one",2.0],"force":false},"owner":"acme"}`, want: true},
		{name: "missing field", args: `{"owner":"acme","repo":"gateway"}`, want: false},
		{name: "extra field", args: `{"owner":"acme","repo":"gateway","options":{"force":false,"labels":["one",2]},"unexpected":"nope"}`, want: false},
		{name: "nested value mismatch", args: `{"owner":"acme","repo":"gateway","options":{"force":true,"labels":["one",2]}}`, want: false},
		{name: "arbitrary precision equal", args: `{"repo":"gateway","options":{"labels":["one",20e-1],"force":false},"owner":"acme"}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Evaluate([]Grant{grant}, Request{Server: "github", Tool: "create_branch", Arguments: json.RawMessage(test.args), Approved: true})
			if decision.Allowed != test.want {
				t.Fatalf("allowed=%v, want %v; decision=%#v", decision.Allowed, test.want, decision)
			}
		})
	}
}

func TestEvaluateExactArgumentsUsesBoundedDecimalArithmetic(t *testing.T) {
	grant := Grant{Server: "math", Tool: "compare", Effect: EffectRead, Approval: ApprovalAllow, Arguments: json.RawMessage(`{"huge":90071992547409931234567890.1234500,"exponent":1e4096}`)}
	for _, test := range []struct {
		name    string
		args    string
		allowed bool
	}{
		{name: "mathematically equal", args: `{"exponent":10e4095,"huge":90071992547409931234567890.12345}`, allowed: true},
		{name: "adjacent large value", args: `{"exponent":1e4096,"huge":90071992547409931234567890.12346}`, allowed: false},
		{name: "nested duplicate", args: `{"exponent":1e4096,"huge":90071992547409931234567890.1234500,"nested":{"key":1,"key":1}}`, allowed: false},
		{name: "out of budget", args: `{"exponent":1e4097,"huge":90071992547409931234567890.1234500}`, allowed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := Evaluate([]Grant{grant}, Request{Server: "math", Tool: "compare", Arguments: json.RawMessage(test.args)})
			if decision.Allowed != test.allowed {
				t.Fatalf("allowed=%v, want %v", decision.Allowed, test.allowed)
			}
		})
	}
	if ValidJSONObject(json.RawMessage(`{"outer":{"duplicate":1,"duplicate":2}}`)) {
		t.Fatal("recursive duplicate keys were accepted")
	}
}

func TestEvaluateWithoutExactArgumentsPreservesExistingBehavior(t *testing.T) {
	decision := Evaluate([]Grant{{Server: "github", Tool: "read", Effect: EffectRead, Approval: ApprovalAllow}}, Request{
		Server: "github", Tool: "read", Arguments: json.RawMessage(`{"anything":[1,true]}`),
	})
	if !decision.Allowed {
		t.Fatalf("unconstrained grant was denied: %#v", decision)
	}
}
