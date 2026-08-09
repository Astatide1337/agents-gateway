package toolpolicy

import "testing"

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
