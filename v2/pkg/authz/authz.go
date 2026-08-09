// Package authz contains the tenant-aware authorization boundary used by the
// HTTP API, CLI service accounts, and broker calls. It intentionally has no
// transport dependencies so every entry point evaluates the same rules.
package authz

import "fmt"

type PrincipalType string

const (
	PrincipalHuman          PrincipalType = "human"
	PrincipalServiceAccount PrincipalType = "service_account"
	PrincipalRunner         PrincipalType = "runner"
	PrincipalRun            PrincipalType = "run"
)

type Role string

const (
	RoleInstanceAdmin Role = "instance-admin"
	RoleOrgAdmin      Role = "org-admin"
	RoleProjectEditor Role = "project-editor"
	RoleApprover      Role = "approver"
	RoleViewer        Role = "viewer"
)

type Action string

const (
	ActionRead       Action = "read"
	ActionApply      Action = "apply"
	ActionRun        Action = "run"
	ActionApprove    Action = "approve"
	ActionCredential Action = "credential.manage"
	ActionRunner     Action = "runner.manage"
)

type Principal struct {
	ID           string
	Type         PrincipalType
	InstanceRole Role
	OrgRoles     map[string]Role
	ProjectRoles map[string]Role
}

type Request struct {
	OrganizationID string
	ProjectID      string
	Action         Action
}

type Decision struct {
	Allowed bool
	Reason  string
}

func Authorize(principal Principal, request Request) Decision {
	if principal.ID == "" {
		return Decision{Reason: "missing principal identity"}
	}
	if request.OrganizationID == "" {
		return Decision{Reason: "missing organization scope"}
	}
	if principal.InstanceRole == RoleInstanceAdmin {
		return Decision{Allowed: true, Reason: "instance administrator"}
	}

	orgRole := principal.OrgRoles[request.OrganizationID]
	projectRole := principal.ProjectRoles[projectKey(request.OrganizationID, request.ProjectID)]
	role := strongest(orgRole, projectRole)
	if request.ProjectID == "" {
		role = orgRole
	}

	allowed := false
	switch request.Action {
	case ActionRead:
		allowed = role == RoleOrgAdmin || role == RoleProjectEditor || role == RoleApprover || role == RoleViewer
	case ActionApply, ActionRun:
		allowed = role == RoleOrgAdmin || role == RoleProjectEditor
	case ActionApprove:
		allowed = role == RoleOrgAdmin || role == RoleApprover
	case ActionCredential:
		allowed = role == RoleOrgAdmin
	case ActionRunner:
		allowed = false
	default:
		return Decision{Reason: fmt.Sprintf("unknown action %q", request.Action)}
	}
	if !allowed {
		return Decision{Reason: fmt.Sprintf("role %q cannot %s in requested scope", role, request.Action)}
	}
	return Decision{Allowed: true, Reason: "role permits action"}
}

func projectKey(organizationID, projectID string) string {
	return organizationID + "/" + projectID
}

func strongest(a, b Role) Role {
	rank := map[Role]int{
		RoleViewer: 1, RoleApprover: 2, RoleProjectEditor: 3, RoleOrgAdmin: 4,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
