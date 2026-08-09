package cedarpolicy

import "testing"

func TestCedarPermitAndExplicitForbid(t *testing.T) {
	engine, err := New("policy.cedar", []byte(`
permit(principal, action == Action::"tool.call", resource)
when { principal.organization == resource.organization && context.approved == true };
forbid(principal, action, resource)
when { resource.effect == "destructive" };
`))
	if err != nil {
		t.Fatal(err)
	}
	request := Request{PrincipalType: "User", PrincipalID: "user", Action: "tool.call", ResourceType: "Tool", ResourceID: "github:create_pr", PrincipalAttributes: map[string]any{"organization": "org"}, ResourceAttributes: map[string]any{"organization": "org", "effect": "write"}, Context: map[string]any{"approved": true}}
	if decision := engine.Authorize(request); !decision.Allowed {
		t.Fatalf("expected allow: %#v", decision)
	}
	request.ResourceAttributes["effect"] = "destructive"
	if decision := engine.Authorize(request); decision.Allowed {
		t.Fatalf("explicit forbid did not override permit: %#v", decision)
	}
}

func TestCedarFailsClosedOnBadContext(t *testing.T) {
	engine, _ := New("policy.cedar", []byte(`permit(principal, action, resource);`))
	decision := engine.Authorize(Request{PrincipalType: "User", PrincipalID: "user", Action: "read", ResourceType: "Project", ResourceID: "project", Context: map[string]any{"unsupported": []string{"x"}}})
	if decision.Allowed || len(decision.EvaluationErrors) == 0 {
		t.Fatalf("decision=%#v", decision)
	}
}
