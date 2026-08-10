package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PostgreSQL implements Store without binding the package to a specific SQL
// driver. cmd/agw-server registers pgx's database/sql driver at composition
// time, keeping this package directly testable.
type PostgreSQL struct{ db *sql.DB }

func NewPostgreSQL(db *sql.DB) (*PostgreSQL, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	return &PostgreSQL{db: db}, nil
}

func (p *PostgreSQL) tenantTx(ctx context.Context, scope Scope, fn func(*sql.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('agw.organization_id', $1, true), set_config('agw.project_id', $2, true)",
		scope.OrganizationID, scope.ProjectID,
	); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant transaction: %w", err)
	}
	return nil
}

func (p *PostgreSQL) ApplyResource(ctx context.Context, resource Resource) (result Resource, err error) {
	if resource.Kind == "" || resource.Name == "" || resource.Digest == "" || len(resource.Document) == 0 || resource.AppliedBy == "" {
		return Resource{}, errors.New("kind, name, digest, document, and applied-by are required")
	}
	if err := ValidateJSONDocument(resource.Document); err != nil {
		return Resource{}, fmt.Errorf("resource document is not valid JSON: %w", err)
	}
	normalizedDocument, err := NormalizeJSONDocument(resource.Document)
	if err != nil {
		return Resource{}, fmt.Errorf("normalize resource document: %w", err)
	}
	resource.Document = normalizedDocument
	err = p.tenantTx(ctx, resource.Scope, func(tx *sql.Tx) error {
		var definitionID string
		var currentRevision int64
		err := tx.QueryRowContext(ctx, `
			SELECT id::text, current_revision FROM definitions
			WHERE organization_id=$1 AND project_id=$2 AND kind=$3 AND name=$4
			FOR UPDATE`, resource.OrganizationID, resource.ProjectID, resource.Kind, resource.Name,
		).Scan(&definitionID, &currentRevision)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `
				INSERT INTO definitions (organization_id,project_id,kind,name)
				VALUES ($1,$2,$3,$4) RETURNING id::text,current_revision`,
				resource.OrganizationID, resource.ProjectID, resource.Kind, resource.Name,
			).Scan(&definitionID, &currentRevision)
		}
		if err != nil {
			return fmt.Errorf("lock resource definition: %w", err)
		}
		if currentRevision > 0 {
			var digest string
			var document string
			var appliedBy string
			var createdAt sql.NullTime
			if err := tx.QueryRowContext(ctx, `
				SELECT digest,document::text,applied_by,created_at
				FROM definition_revisions WHERE definition_id=$1 AND revision=$2`,
				definitionID, currentRevision,
			).Scan(&digest, &document, &appliedBy, &createdAt); err != nil {
				return fmt.Errorf("read current revision: %w", err)
			}
			if digest == resource.Digest {
				if !JSONDocumentsEqual([]byte(document), resource.Document) {
					return ErrConflict
				}
				result = resource
				result.Revision, result.Document, result.AppliedBy = currentRevision, []byte(document), appliedBy
				if createdAt.Valid {
					result.CreatedAt = createdAt.Time
				}
				return nil
			}
		}

		revision := currentRevision + 1
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO definition_revisions
			(definition_id,organization_id,project_id,revision,digest,document,applied_by)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7) RETURNING created_at`,
			definitionID, resource.OrganizationID, resource.ProjectID, revision,
			resource.Digest, string(resource.Document), resource.AppliedBy,
		).Scan(&resource.CreatedAt); err != nil {
			return fmt.Errorf("insert resource revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE definitions SET current_revision=$2 WHERE id=$1", definitionID, revision); err != nil {
			return fmt.Errorf("advance current revision: %w", err)
		}
		resource.Revision = revision
		result = resource
		return nil
	})
	return result, err
}

func (p *PostgreSQL) GetResource(ctx context.Context, scope Scope, kind, name string) (result Resource, err error) {
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		result.Scope, result.Kind, result.Name = scope, kind, name
		var document string
		err := tx.QueryRowContext(ctx, `
			SELECT r.revision,r.digest,r.document::text,r.applied_by,r.created_at
			FROM definitions d JOIN definition_revisions r
			  ON r.definition_id=d.id AND r.revision=d.current_revision
			WHERE d.organization_id=$1 AND d.project_id=$2 AND d.kind=$3 AND d.name=$4`,
			scope.OrganizationID, scope.ProjectID, kind, name,
		).Scan(&result.Revision, &result.Digest, &document, &result.AppliedBy, &result.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get resource: %w", err)
		}
		result.Document = []byte(document)
		return nil
	})
	return result, err
}

func (p *PostgreSQL) ListResources(ctx context.Context, scope Scope, kind string, page Page) (result []Resource, hasMore bool, err error) {
	page, err = page.Normalize()
	if err != nil {
		return nil, false, err
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT d.kind,d.name,r.revision,r.digest,r.document::text,r.applied_by,r.created_at
			FROM definitions d
			JOIN definition_revisions r ON r.definition_id=d.id AND r.revision=d.current_revision
			WHERE d.organization_id=$1 AND d.project_id=$2 AND ($3='' OR d.kind = ANY(string_to_array($3, ',')))
			ORDER BY r.created_at DESC,d.kind ASC,d.name ASC
			LIMIT $4 OFFSET $5`, scope.OrganizationID, scope.ProjectID, kind, page.Limit+1, page.Offset)
		if queryErr != nil {
			return fmt.Errorf("list resources: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			resource := Resource{Scope: scope}
			var document string
			if scanErr := rows.Scan(&resource.Kind, &resource.Name, &resource.Revision, &resource.Digest, &document, &resource.AppliedBy, &resource.CreatedAt); scanErr != nil {
				return scanErr
			}
			resource.Document = []byte(document)
			result = append(result, resource)
		}
		if scanErr := rows.Err(); scanErr != nil {
			return scanErr
		}
		if len(result) > page.Limit {
			hasMore = true
			result = result[:page.Limit]
		}
		return nil
	})
	return result, hasMore, err
}

func (p *PostgreSQL) CreateRun(ctx context.Context, run Run) (result Run, err error) {
	if run.ID == "" || run.Kind == "" || run.DefinitionDigest == "" || run.RequestedBy == "" {
		return Run{}, errors.New("run identity, kind, definition digest, and requester are required")
	}
	if run.Status == "" {
		run.Status = "Pending"
	}
	err = p.tenantTx(ctx, run.Scope, func(tx *sql.Tx) error {
		if run.IdempotencyKey != "" {
			existing, scanErr := scanRun(tx.QueryRowContext(ctx, `
				SELECT id::text,kind,definition_digest,requested_by,status::text,
				       coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at
				FROM runs WHERE organization_id=$1 AND project_id=$2 AND idempotency_key=$3`,
				run.OrganizationID, run.ProjectID, run.IdempotencyKey), run.Scope)
			if scanErr == nil {
				result = existing
				return nil
			}
			if !errors.Is(scanErr, sql.ErrNoRows) {
				return scanErr
			}
		}
		created, scanErr := scanRun(tx.QueryRowContext(ctx, `
			INSERT INTO runs
			(id,organization_id,project_id,kind,definition_digest,requested_by,status,condition,idempotency_key)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))
			RETURNING id::text,kind,definition_digest,requested_by,status::text,
			          coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at`,
			run.ID, run.OrganizationID, run.ProjectID, run.Kind, run.DefinitionDigest,
			run.RequestedBy, run.Status, run.Condition, run.IdempotencyKey), run.Scope)
		if scanErr != nil {
			return fmt.Errorf("create run: %w", scanErr)
		}
		result = created
		return nil
	})
	return result, err
}

func (p *PostgreSQL) GetRun(ctx context.Context, scope Scope, id string) (result Run, err error) {
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		result, err = scanRun(tx.QueryRowContext(ctx, `
			SELECT id::text,kind,definition_digest,requested_by,status::text,
			       coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at
			FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3`,
			scope.OrganizationID, scope.ProjectID, id), scope)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return result, err
}

func (p *PostgreSQL) ListRuns(ctx context.Context, scope Scope, page Page) (result []Run, hasMore bool, err error) {
	page, err = page.Normalize()
	if err != nil {
		return nil, false, err
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT id::text,kind,definition_digest,requested_by,status::text,
			       coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at
			FROM runs
			WHERE organization_id=$1 AND project_id=$2
			ORDER BY updated_at DESC,id DESC
			LIMIT $3 OFFSET $4`, scope.OrganizationID, scope.ProjectID, page.Limit+1, page.Offset)
		if queryErr != nil {
			return fmt.Errorf("list runs: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			run, scanErr := scanRun(rows, scope)
			if scanErr != nil {
				return scanErr
			}
			result = append(result, run)
		}
		if scanErr := rows.Err(); scanErr != nil {
			return scanErr
		}
		if len(result) > page.Limit {
			hasMore = true
			result = result[:page.Limit]
		}
		return nil
	})
	return result, hasMore, err
}

func (p *PostgreSQL) SetRunStatus(ctx context.Context, scope Scope, id, status, condition string) (result Run, err error) {
	if !validRunStatus(status) {
		return Run{}, errors.New("invalid run status")
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		current, scanErr := scanRun(tx.QueryRowContext(ctx, `
			SELECT id,kind,definition_digest,requested_by,status::text,
			       coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at
			FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3
			FOR UPDATE`, scope.OrganizationID, scope.ProjectID, id), scope)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if scanErr != nil {
			return fmt.Errorf("lock run: %w", scanErr)
		}
		if current.Status == status && current.Condition == condition {
			result = current
			return nil
		}
		if !validRunTransition(current.Status, status) {
			return fmt.Errorf("%w: invalid run transition %s -> %s", ErrConflict, current.Status, status)
		}
		updated, updateErr := scanRun(tx.QueryRowContext(ctx, `
			UPDATE runs SET status=$4,condition=NULLIF($5,''),updated_at=now()
			WHERE organization_id=$1 AND project_id=$2 AND id=$3
			RETURNING id,kind,definition_digest,requested_by,status::text,
			          coalesce(condition,''),coalesce(idempotency_key,''),created_at,updated_at`,
			scope.OrganizationID, scope.ProjectID, id, status, condition), scope)
		if updateErr != nil {
			return fmt.Errorf("update run status: %w", updateErr)
		}
		result = updated
		payload, marshalErr := json.Marshal(map[string]string{"status": status, "condition": condition})
		if marshalErr != nil {
			return marshalErr
		}
		if _, insertErr := tx.ExecContext(ctx, `
			INSERT INTO run_events (organization_id,project_id,run_id,event_type,payload)
			VALUES ($1,$2,$3,'run.status_changed',$4::jsonb)`,
			scope.OrganizationID, scope.ProjectID, id, string(payload)); insertErr != nil {
			return fmt.Errorf("append run status event: %w", insertErr)
		}
		return nil
	})
	return result, err
}

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner, scope Scope) (Run, error) {
	run := Run{Scope: scope}
	err := row.Scan(&run.ID, &run.Kind, &run.DefinitionDigest, &run.RequestedBy, &run.Status, &run.Condition, &run.IdempotencyKey, &run.CreatedAt, &run.UpdatedAt)
	return run, err
}

func (p *PostgreSQL) AppendEvent(ctx context.Context, event Event) (result Event, err error) {
	if event.RunID == "" || event.Type == "" {
		return Event{}, errors.New("run and event type are required")
	}
	if len(event.Payload) == 0 {
		event.Payload = []byte(`{}`)
	}
	normalizedPayload, err := NormalizeJSONDocument(event.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("event payload is not valid JSON: %w", err)
	}
	event.Payload = normalizedPayload
	err = p.tenantTx(ctx, event.Scope, func(tx *sql.Tx) error {
		result = event
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO run_events (organization_id,project_id,run_id,event_type,payload)
			SELECT $1,$2,id,$4,$5::jsonb FROM runs
			WHERE organization_id=$1 AND project_id=$2 AND id=$3
			RETURNING sequence,created_at`,
			event.OrganizationID, event.ProjectID, event.RunID, event.Type, string(event.Payload),
		).Scan(&result.Sequence, &result.CreatedAt); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("append run event: %w", err)
		}
		return nil
	})
	return result, err
}

func (p *PostgreSQL) ListEvents(ctx context.Context, scope Scope, runID string, after int64) (result []Event, err error) {
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT true FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3", scope.OrganizationID, scope.ProjectID, runID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT sequence,event_type,payload::text,created_at FROM run_events WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND sequence>$4 ORDER BY sequence`, scope.OrganizationID, scope.ProjectID, runID, after)
		if err != nil {
			return fmt.Errorf("list run events: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			event := Event{Scope: scope, RunID: runID}
			var payload string
			if err := rows.Scan(&event.Sequence, &event.Type, &payload, &event.CreatedAt); err != nil {
				return err
			}
			event.Payload = []byte(payload)
			result = append(result, event)
		}
		return rows.Err()
	})
	return result, err
}

func (p *PostgreSQL) AppendAudit(ctx context.Context, event AuditEvent) error {
	if event.OrganizationID == "" || event.PrincipalID == "" || event.Action == "" || event.ResourceType == "" || event.ResourceID == "" || event.Decision == "" {
		return errors.New("complete audit event is required")
	}
	if len(event.Metadata) == 0 {
		event.Metadata = []byte(`{}`)
	}
	normalizedMetadata, err := NormalizeJSONDocument(event.Metadata)
	if err != nil {
		return fmt.Errorf("audit metadata is not valid JSON: %w", err)
	}
	event.Metadata = normalizedMetadata
	return p.tenantTx(ctx, event.Scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO audit_events (organization_id,project_id,principal_id,action,resource_type,resource_id,decision,metadata) VALUES ($1,NULLIF($2,'')::uuid,$3,$4,$5,$6,$7,$8::jsonb)`, event.OrganizationID, event.ProjectID, event.PrincipalID, event.Action, event.ResourceType, event.ResourceID, event.Decision, string(event.Metadata))
		if err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		return nil
	})
}

func (p *PostgreSQL) ListAudit(ctx context.Context, scope Scope, page Page) (result []AuditEvent, hasMore bool, err error) {
	page, err = page.Normalize()
	if err != nil {
		return nil, false, err
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT principal_id,action,resource_type,resource_id,decision,metadata::text,created_at
			FROM audit_events
			WHERE organization_id=$1 AND (project_id IS NULL OR project_id=$2)
			ORDER BY sequence DESC
			LIMIT $3 OFFSET $4`, scope.OrganizationID, scope.ProjectID, page.Limit+1, page.Offset)
		if queryErr != nil {
			return fmt.Errorf("list audit events: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			event := AuditEvent{Scope: scope}
			var metadata string
			if scanErr := rows.Scan(&event.PrincipalID, &event.Action, &event.ResourceType, &event.ResourceID, &event.Decision, &metadata, &event.CreatedAt); scanErr != nil {
				return scanErr
			}
			event.Metadata = []byte(metadata)
			result = append(result, event)
		}
		if scanErr := rows.Err(); scanErr != nil {
			return scanErr
		}
		if len(result) > page.Limit {
			hasMore = true
			result = result[:page.Limit]
		}
		return nil
	})
	return result, hasMore, err
}

func (p *PostgreSQL) GetUsage(ctx context.Context, scope Scope) (usage Usage, err error) {
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		if scanErr := tx.QueryRowContext(ctx, `
			SELECT count(*)::bigint,
			       count(*) FILTER (WHERE status NOT IN ('Succeeded','Failed','Cancelled','Lost'))::bigint
			FROM runs WHERE organization_id=$1 AND project_id=$2`, scope.OrganizationID, scope.ProjectID).
			Scan(&usage.TotalRuns, &usage.ActiveRuns); scanErr != nil {
			return fmt.Errorf("count run usage: %w", scanErr)
		}
		if scanErr := tx.QueryRowContext(ctx, `
			SELECT count(*)::bigint,coalesce(sum(octet_length(document::text)),0)::bigint
			FROM artifact_versions WHERE organization_id=$1 AND project_id=$2`, scope.OrganizationID, scope.ProjectID).
			Scan(&usage.ArtifactVersions, &usage.ArtifactBytes); scanErr != nil {
			return fmt.Errorf("count artifact usage: %w", scanErr)
		}
		return nil
	})
	return usage, err
}

func (p *PostgreSQL) ClaimRunSignal(ctx context.Context, scope Scope, runID, keyHash, fingerprint, claimTokenHash string, lease time.Duration) (result RunSignalClaim, err error) {
	if runID == "" || keyHash == "" || fingerprint == "" || claimTokenHash == "" || lease <= 0 {
		return RunSignalClaim{}, errors.New("run, signal hashes, claim token, and positive lease are required")
	}
	leaseMillis := lease.Milliseconds()
	if leaseMillis < 1 {
		leaseMillis = 1
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		if scanErr := tx.QueryRowContext(ctx, `
			INSERT INTO run_signal_claims
			(organization_id,project_id,run_id,idempotency_key_hash,request_fingerprint,state,claim_token_hash,claimed_at,lease_expires_at)
			SELECT $1,$2,id,$4,$5,'pending',$6,now(),now()+($7::bigint * interval '1 millisecond')
			FROM runs
			WHERE organization_id=$1 AND project_id=$2 AND id=$3
			ON CONFLICT (organization_id,project_id,run_id,idempotency_key_hash) DO NOTHING
			RETURNING state`, scope.OrganizationID, scope.ProjectID, runID, keyHash, fingerprint, claimTokenHash, leaseMillis).Scan(&result.State); scanErr == nil {
			result.Owner = true
			return nil
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("claim run signal: %w", scanErr)
		}

		var current struct {
			Fingerprint    string
			State          string
			ClaimTokenHash string
		}
		selectErr := tx.QueryRowContext(ctx, `
			SELECT request_fingerprint,state,claim_token_hash
			FROM run_signal_claims
			WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND idempotency_key_hash=$4
			FOR UPDATE`, scope.OrganizationID, scope.ProjectID, runID, keyHash).Scan(&current.Fingerprint, &current.State, &current.ClaimTokenHash)
		if errors.Is(selectErr, sql.ErrNoRows) {
			var exists bool
			if runErr := tx.QueryRowContext(ctx, `SELECT true FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3`, scope.OrganizationID, scope.ProjectID, runID).Scan(&exists); errors.Is(runErr, sql.ErrNoRows) {
				return ErrNotFound
			} else if runErr != nil {
				return fmt.Errorf("check run for signal claim: %w", runErr)
			}
			return fmt.Errorf("claim run signal disappeared: %w", ErrConflict)
		}
		if selectErr != nil {
			return fmt.Errorf("read run signal claim: %w", selectErr)
		}
		if current.Fingerprint != fingerprint {
			return ErrConflict
		}
		if current.State == RunSignalAccepted {
			result.State = RunSignalAccepted
			return nil
		}
		if updateErr := tx.QueryRowContext(ctx, `
				UPDATE run_signal_claims
				SET claim_token_hash=$5,claimed_at=now(),lease_expires_at=now()+($6::bigint * interval '1 millisecond')
				WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND idempotency_key_hash=$4
				  AND state='pending' AND lease_expires_at <= now()
				RETURNING state`, scope.OrganizationID, scope.ProjectID, runID, keyHash, claimTokenHash, leaseMillis).Scan(&result.State); updateErr == nil {
			result.Owner = true
			return nil
		} else if !errors.Is(updateErr, sql.ErrNoRows) {
			return fmt.Errorf("reclaim run signal: %w", updateErr)
		}
		result.State = RunSignalPending
		return nil
	})
	return result, err
}

func (p *PostgreSQL) AcceptRunSignal(ctx context.Context, scope Scope, runID, keyHash, fingerprint, claimTokenHash string) error {
	if runID == "" || keyHash == "" || fingerprint == "" || claimTokenHash == "" {
		return errors.New("run, signal hashes, and claim token are required")
	}
	return p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		var updated int64
		if err := tx.QueryRowContext(ctx, `
			WITH changed AS (
				UPDATE run_signal_claims
				SET state='accepted',accepted_at=coalesce(accepted_at,now())
				WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND idempotency_key_hash=$4
				  AND request_fingerprint=$5 AND claim_token_hash=$6 AND state='pending'
				RETURNING 1
			)
			SELECT count(*) FROM changed`, scope.OrganizationID, scope.ProjectID, runID, keyHash, fingerprint, claimTokenHash).Scan(&updated); err != nil {
			return fmt.Errorf("accept run signal: %w", err)
		}
		if updated == 1 {
			return nil
		}
		var state, existingFingerprint, existingToken string
		if err := tx.QueryRowContext(ctx, `
			SELECT state,request_fingerprint,claim_token_hash
			FROM run_signal_claims
			WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND idempotency_key_hash=$4`, scope.OrganizationID, scope.ProjectID, runID, keyHash).Scan(&state, &existingFingerprint, &existingToken); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("read accepted run signal: %w", err)
		}
		if existingFingerprint != fingerprint {
			return ErrConflict
		}
		if state == RunSignalAccepted {
			return nil
		}
		return ErrSignalClaimLost
	})
}

func (p *PostgreSQL) Claim(ctx context.Context, organizationID, projectID, runID, key, digest string) (claimed bool, err error) {
	scope := Scope{OrganizationID: organizationID, ProjectID: projectID}
	if runID == "" || key == "" || digest == "" {
		return false, errors.New("run, effect key, and request digest are required")
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		var inserted string
		err := tx.QueryRowContext(ctx, `
			INSERT INTO effects (organization_id,project_id,run_id,effect_key,state,request_digest)
			SELECT $1,$2,id,$4,'claimed',$5 FROM runs
			WHERE organization_id=$1 AND project_id=$2 AND id=$3
			ON CONFLICT (run_id,effect_key) DO NOTHING
			RETURNING effect_key`, organizationID, projectID, runID, key, digest).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			var runExists bool
			checkErr := tx.QueryRowContext(ctx, "SELECT true FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3", organizationID, projectID, runID).Scan(&runExists)
			if errors.Is(checkErr, sql.ErrNoRows) {
				return ErrNotFound
			}
			if checkErr != nil {
				return checkErr
			}
			claimed = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim effect: %w", err)
		}
		claimed = true
		return nil
	})
	return claimed, err
}

func (p *PostgreSQL) Complete(ctx context.Context, organizationID, projectID, runID, key, state string, result []byte) error {
	if state != "succeeded" && state != "failed" && state != "unknown" {
		return errors.New("invalid terminal effect state")
	}
	if len(result) == 0 {
		result = []byte(`null`)
	}
	if err := ValidateJSONDocument(result); err != nil {
		return fmt.Errorf("effect result must be valid JSON: %w", err)
	}
	scope := Scope{OrganizationID: organizationID, ProjectID: projectID}
	return p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		outcome, err := tx.ExecContext(ctx, `
			UPDATE effects SET state=$5,result=$6::jsonb,completed_at=now()
			WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND effect_key=$4 AND state='claimed'`,
			organizationID, projectID, runID, key, state, string(result))
		if err != nil {
			return fmt.Errorf("complete effect: %w", err)
		}
		rows, err := outcome.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 1 {
			return nil
		}
		var existingState, existingResult string
		err = tx.QueryRowContext(ctx, "SELECT state,coalesce(result::text,'null') FROM effects WHERE organization_id=$1 AND project_id=$2 AND run_id=$3 AND effect_key=$4", organizationID, projectID, runID, key).Scan(&existingState, &existingResult)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if existingState == state && JSONDocumentsEqual([]byte(existingResult), result) {
			return nil
		}
		return ErrConflict
	})
}

func (p *PostgreSQL) PutArtifactVersion(ctx context.Context, version ArtifactVersion) (result ArtifactVersion, err error) {
	if err := validateArtifactVersion(version); err != nil {
		return ArtifactVersion{}, err
	}
	normalizedDocument, err := NormalizeJSONDocument(version.Document)
	if err != nil {
		return ArtifactVersion{}, fmt.Errorf("normalize artifact document: %w", err)
	}
	version.Document = normalizedDocument
	err = p.tenantTx(ctx, version.Scope, func(tx *sql.Tx) error {
		result = version
		err := tx.QueryRowContext(ctx, `
			INSERT INTO artifact_versions
			(organization_id,project_id,run_id,artifact_id,version_id,version_number,document,content_object_key,source_object_key)
			SELECT $1,$2,id,$4,$5,$6,$7::jsonb,$8,$9
			FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3
			ON CONFLICT DO NOTHING
			RETURNING created_at`, version.OrganizationID, version.ProjectID, version.RunID,
			version.ArtifactID, version.VersionID, version.VersionNumber, string(version.Document),
			version.ContentObjectKey, version.SourceObjectKey).Scan(&result.CreatedAt)
		if err == nil {
			result.Document = append([]byte(nil), version.Document...)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("insert artifact version: %w", err)
		}
		var existing ArtifactVersion
		existing.Scope = version.Scope
		scanErr := scanArtifactVersion(tx.QueryRowContext(ctx, `
			SELECT artifact_id,version_id,run_id,version_number,document::text,
			       content_object_key,source_object_key,created_at
			FROM artifact_versions
			WHERE organization_id=$1 AND project_id=$2 AND version_id=$3`,
			version.OrganizationID, version.ProjectID, version.VersionID), &existing)
		if errors.Is(scanErr, sql.ErrNoRows) {
			var runExists bool
			if runErr := tx.QueryRowContext(ctx, `SELECT true FROM runs WHERE organization_id=$1 AND project_id=$2 AND id=$3`, version.OrganizationID, version.ProjectID, version.RunID).Scan(&runExists); errors.Is(runErr, sql.ErrNoRows) {
				return ErrNotFound
			} else if runErr != nil {
				return fmt.Errorf("check artifact run: %w", runErr)
			}
			return ErrConflict
		}
		if scanErr != nil {
			return fmt.Errorf("read artifact version conflict: %w", scanErr)
		}
		if existing.ArtifactID != version.ArtifactID || existing.RunID != version.RunID || existing.VersionNumber != version.VersionNumber || existing.ContentObjectKey != version.ContentObjectKey || existing.SourceObjectKey != version.SourceObjectKey || !JSONDocumentsEqual(existing.Document, version.Document) {
			return ErrConflict
		}
		result = existing
		return nil
	})
	return result, err
}

func (p *PostgreSQL) ListArtifactVersions(ctx context.Context, scope Scope, artifactID string) (result []ArtifactVersion, err error) {
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT artifact_id,version_id,run_id,version_number,document::text,
			       content_object_key,source_object_key,created_at
			FROM artifact_versions
			WHERE organization_id=$1 AND project_id=$2 AND ($3='' OR artifact_id=$3)
			ORDER BY created_at DESC,version_id DESC
			LIMIT $4`, scope.OrganizationID, scope.ProjectID, artifactID, ArtifactCatalogListLimit)
		if err != nil {
			return fmt.Errorf("list artifact versions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			version := ArtifactVersion{Scope: scope}
			if err := scanArtifactVersion(rows, &version); err != nil {
				return err
			}
			result = append(result, version)
		}
		return rows.Err()
	})
	return result, err
}

func (p *PostgreSQL) ListArtifactVersionsByRun(ctx context.Context, scope Scope, runID string) (result []ArtifactVersion, err error) {
	if runID == "" {
		return nil, errors.New("run id is required")
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT artifact_id,version_id,run_id,version_number,document::text,
			       content_object_key,source_object_key,created_at
			FROM artifact_versions
			WHERE organization_id=$1 AND project_id=$2 AND run_id=$3
			ORDER BY created_at ASC,version_id ASC`, scope.OrganizationID, scope.ProjectID, runID)
		if err != nil {
			return fmt.Errorf("list run artifact versions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			version := ArtifactVersion{Scope: scope}
			if err := scanArtifactVersion(rows, &version); err != nil {
				return err
			}
			result = append(result, version)
		}
		return rows.Err()
	})
	return result, err
}

func (p *PostgreSQL) GetArtifactVersion(ctx context.Context, scope Scope, artifactID, versionID string) (result ArtifactVersion, err error) {
	if artifactID == "" {
		return ArtifactVersion{}, errors.New("artifact id is required")
	}
	err = p.tenantTx(ctx, scope, func(tx *sql.Tx) error {
		result.Scope = scope
		query := `
			SELECT artifact_id,version_id,run_id,version_number,document::text,
			       content_object_key,source_object_key,created_at
			FROM artifact_versions
			WHERE organization_id=$1 AND project_id=$2 AND artifact_id=$3`
		arguments := []any{scope.OrganizationID, scope.ProjectID, artifactID}
		if versionID == "" {
			query += ` ORDER BY version_number DESC LIMIT 1`
		} else {
			query += ` AND version_id=$4`
			arguments = append(arguments, versionID)
		}
		if err := scanArtifactVersion(tx.QueryRowContext(ctx, query, arguments...), &result); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("get artifact version: %w", err)
		}
		return nil
	})
	return result, err
}

type artifactVersionScanner interface {
	Scan(...any) error
}

func scanArtifactVersion(scanner artifactVersionScanner, version *ArtifactVersion) error {
	var document string
	if err := scanner.Scan(&version.ArtifactID, &version.VersionID, &version.RunID, &version.VersionNumber,
		&document, &version.ContentObjectKey, &version.SourceObjectKey, &version.CreatedAt); err != nil {
		return err
	}
	version.Document = []byte(document)
	return nil
}
