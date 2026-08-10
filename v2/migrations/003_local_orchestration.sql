BEGIN;

-- The local engine stores the compiled, immutable manifest separately from
-- the public run row. This keeps a restart independent of Temporal history.
-- Workers poll only scopes supplied by an already-authorized control-plane
-- path. Every query therefore runs under its actual tenant context; this
-- migration deliberately provides no worker/session-GUC RLS bypass.
ALTER TABLE runs
    ADD CONSTRAINT runs_scope_id_unique UNIQUE (organization_id, project_id, id);

CREATE TABLE local_workflows (
    run_id text PRIMARY KEY,
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    manifest jsonb NOT NULL,
    input_ref text NOT NULL DEFAULT '' CHECK (length(input_ref) <= 4096),
    state jsonb NOT NULL,
    status run_status NOT NULL DEFAULT 'Pending',
    wake_at timestamptz,
    lease_owner text,
    lease_token text,
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((lease_owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    FOREIGN KEY (organization_id, project_id, run_id)
        REFERENCES runs(organization_id, project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX local_workflows_scope_run_idx
    ON local_workflows (organization_id, project_id, run_id);
CREATE INDEX local_workflows_claim_idx
    ON local_workflows (status, wake_at, lease_expires_at, created_at, run_id)
    WHERE status NOT IN ('Succeeded','Failed','Cancelled','Lost');

CREATE TABLE local_workflow_commands (
    id bigserial PRIMARY KEY,
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL,
    idempotency_key_hash text NOT NULL CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL CHECK (kind IN ('cancel','approval','reply')),
    target text NOT NULL CHECK (target IN ('workflow','agent')),
    payload jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','consumed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    consumed_at timestamptz,
    UNIQUE (organization_id, project_id, run_id, idempotency_key_hash),
    FOREIGN KEY (organization_id, project_id, run_id)
        REFERENCES runs(organization_id, project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);
CREATE INDEX local_workflow_commands_pending_idx
    ON local_workflow_commands (organization_id, project_id, run_id, id)
    WHERE state = 'pending';

ALTER TABLE local_workflows ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_workflows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON local_workflows
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    );

ALTER TABLE local_workflow_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_workflow_commands FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON local_workflow_commands
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    );

COMMIT;
