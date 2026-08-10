BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE organizations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, name),
    UNIQUE (organization_id, id)
);

CREATE TABLE memberships (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id uuid,
    principal_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('org-admin','project-editor','approver','viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (organization_id, project_id, principal_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);

CREATE TABLE service_accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    role text NOT NULL CHECK (role IN ('project-editor','approver','viewer')),
    credential_digest bytea NOT NULL CHECK (octet_length(credential_digest) = 32),
    disabled_at timestamptz,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, project_id, name),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);

CREATE TABLE definitions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    current_revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, project_id, kind, name),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);

CREATE TABLE definition_revisions (
    definition_id uuid NOT NULL REFERENCES definitions(id) ON DELETE CASCADE,
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    revision bigint NOT NULL,
    digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    document jsonb NOT NULL,
    applied_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (definition_id, revision),
    UNIQUE (organization_id, project_id, digest)
);

CREATE TYPE run_status AS ENUM (
    'Pending','Scheduled','Starting','Running','WaitingApproval',
    'WaitingCapacity','Succeeded','Failed','Cancelled','Lost'
);

CREATE TABLE runs (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('AgentRun','WorkflowRun')),
    definition_digest text NOT NULL,
    requested_by text NOT NULL,
    status run_status NOT NULL DEFAULT 'Pending',
    condition text,
    temporal_workflow_id text UNIQUE,
    idempotency_key text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, project_id, idempotency_key),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);

CREATE TABLE run_events (
    sequence bigserial PRIMARY KEY,
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    event_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_events_stream_idx ON run_events (organization_id, project_id, run_id, sequence);

CREATE TABLE approvals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    effect_key text NOT NULL,
    requested_by text NOT NULL,
    decided_by text,
    decision text CHECK (decision IN ('approved','denied')),
    request jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    UNIQUE (run_id, effect_key)
);

CREATE TABLE effects (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    effect_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('claimed','succeeded','failed','unknown')),
    request_digest text NOT NULL,
    result_ref text,
    result jsonb,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (run_id, effect_key)
);

CREATE TABLE credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    owner_principal_id text,
    name text NOT NULL,
    credential_type text NOT NULL,
    envelope jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,
    UNIQUE (organization_id, name)
);

CREATE TABLE entitlements (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    owner_principal_id text,
    provider text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('subscription','api')),
    credential_id uuid NOT NULL REFERENCES credentials(id) ON DELETE RESTRICT,
    models jsonb NOT NULL DEFAULT '[]'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    cooldown_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((mode = 'subscription' AND owner_principal_id IS NOT NULL) OR
           (mode = 'api' AND owner_principal_id IS NULL))
);

CREATE TABLE artifacts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name text NOT NULL,
    media_type text NOT NULL,
    digest text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    object_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, name)
);

CREATE TABLE runners (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    isolation_grade text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    certificate_serial text NOT NULL UNIQUE,
    last_heartbeat_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runner_leases (
    run_id text PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    runner_id uuid NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    fencing_token bigint NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
    sequence bigserial PRIMARY KEY,
    organization_id uuid NOT NULL,
    project_id uuid,
    principal_id text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    decision text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Defense in depth: the API role must set both tenant settings inside every
-- project transaction. Organization-scoped credential tables deliberately do
-- not use a project predicate; all project-owned tables do. FORCE prevents a
-- table owner accidentally bypassing policies during ordinary application
-- queries. The separate authentication lookup role must have BYPASSRLS.
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON organizations
    USING (id = nullif(current_setting('agw.organization_id', true), '')::uuid)
    WITH CHECK (id = nullif(current_setting('agw.organization_id', true), '')::uuid);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON projects
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND id = nullif(current_setting('agw.project_id', true), '')::uuid
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND id = nullif(current_setting('agw.project_id', true), '')::uuid
    );

DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'service_accounts','definitions','definition_revisions','runs',
        'run_events','approvals','effects','artifacts'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I USING ('
            || 'organization_id = nullif(current_setting(''agw.organization_id'', true), '''')::uuid '
            || 'AND project_id = nullif(current_setting(''agw.project_id'', true), '''')::uuid) '
            || 'WITH CHECK ('
            || 'organization_id = nullif(current_setting(''agw.organization_id'', true), '''')::uuid '
            || 'AND project_id = nullif(current_setting(''agw.project_id'', true), '''')::uuid)',
            table_name
        );
    END LOOP;
END $$;

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND (project_id IS NULL OR project_id = nullif(current_setting('agw.project_id', true), '')::uuid)
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND (project_id IS NULL OR project_id = nullif(current_setting('agw.project_id', true), '')::uuid)
    );

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_events
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND (project_id IS NULL OR project_id = nullif(current_setting('agw.project_id', true), '')::uuid)
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND (project_id IS NULL OR project_id = nullif(current_setting('agw.project_id', true), '')::uuid)
    );

DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['credentials','entitlements'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I USING '
            || '(organization_id = nullif(current_setting(''agw.organization_id'', true), '''')::uuid) '
            || 'WITH CHECK (organization_id = nullif(current_setting(''agw.organization_id'', true), '''')::uuid)',
            table_name
        );
    END LOOP;
END $$;

COMMIT;
