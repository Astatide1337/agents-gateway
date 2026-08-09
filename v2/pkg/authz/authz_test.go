package authz

import "testing"

func TestTenantAuthorization(t *testing.T) {
	p := Principal{
		ID: "user-1", Type: PrincipalHuman,
		ProjectRoles: map[string]Role{"org-a/project-a": RoleProjectEditor},
		OrgRoles:     map[string]Role{"org-a": RoleViewer},
	}

	if got := Authorize(p, Request{OrganizationID: "org-a", ProjectID: "project-a", Action: ActionRun}); !got.Allowed {
		t.Fatalf("expected project editor to run: %s", got.Reason)
	}
	if got := Authorize(p, Request{OrganizationID: "org-b", ProjectID: "project-a", Action: ActionRead}); got.Allowed {
		t.Fatal("cross-organization read was allowed")
	}
	if got := Authorize(p, Request{OrganizationID: "org-a", Action: ActionCredential}); got.Allowed {
		t.Fatal("viewer was allowed to manage credentials")
	}
}

func TestApprovalRoleCannotRun(t *testing.T) {
	p := Principal{ID: "approver", OrgRoles: map[string]Role{"org": RoleApprover}}
	if got := Authorize(p, Request{OrganizationID: "org", ProjectID: "project", Action: ActionApprove}); !got.Allowed {
		t.Fatalf("approver should approve: %s", got.Reason)
	}
	if got := Authorize(p, Request{OrganizationID: "org", ProjectID: "project", Action: ActionRun}); got.Allowed {
		t.Fatal("approver unexpectedly started a run")
	}
}
