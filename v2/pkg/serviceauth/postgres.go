package serviceauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/identity"
)

type PostgreSQL struct{ Database *sql.DB }

func (p PostgreSQL) Authenticate(ctx context.Context, accountID, secret string) (identity.ServiceAccount, error) {
	if p.Database == nil || accountID == "" || secret == "" {
		return identity.ServiceAccount{}, errors.New("invalid service credentials")
	}
	var organizationID, projectID, role string
	var digest []byte
	err := p.Database.QueryRowContext(ctx, `
		SELECT organization_id::text, project_id::text, role, credential_digest
		FROM service_accounts
		WHERE id=$1 AND disabled_at IS NULL`, accountID,
	).Scan(&organizationID, &projectID, &role, &digest)
	if err != nil {
		return identity.ServiceAccount{}, errors.New("invalid service credentials")
	}
	encoded, err := EncodedDigest(digest)
	if err != nil || !identity.VerifyCredential(secret, encoded) {
		return identity.ServiceAccount{}, errors.New("invalid service credentials")
	}
	accountRole := authz.Role(role)
	switch accountRole {
	case authz.RoleProjectEditor, authz.RoleApprover, authz.RoleViewer:
	default:
		return identity.ServiceAccount{}, errors.New("invalid service credentials")
	}
	_, _ = p.Database.ExecContext(ctx, "UPDATE service_accounts SET last_used_at=now() WHERE id=$1", accountID)
	return identity.ServiceAccount{
		ID:           accountID,
		ProjectRoles: map[string]authz.Role{organizationID + "/" + projectID: accountRole},
	}, nil
}

// Create inserts a service account and returns its one-time plaintext
// credential. The caller is responsible for displaying it exactly once.
func (p PostgreSQL) Create(ctx context.Context, organizationID, projectID, name string, role authz.Role) (accountID, secret string, err error) {
	if p.Database == nil || organizationID == "" || projectID == "" || name == "" {
		return "", "", errors.New("database and service account scope are required")
	}
	switch role {
	case authz.RoleProjectEditor, authz.RoleApprover, authz.RoleViewer:
	default:
		return "", "", errors.New("invalid service account role")
	}
	credential, err := identity.NewCredential()
	if err != nil {
		return "", "", err
	}
	digest, err := base64.RawURLEncoding.DecodeString(credential.Digest)
	if err != nil {
		return "", "", err
	}
	err = p.Database.QueryRowContext(ctx, `
		INSERT INTO service_accounts (organization_id,project_id,name,role,credential_digest)
		VALUES ($1,$2,$3,$4,$5) RETURNING id::text`,
		organizationID, projectID, name, string(role), digest,
	).Scan(&accountID)
	if err != nil {
		return "", "", fmt.Errorf("create service account: %w", err)
	}
	return accountID, credential.Secret, nil
}
