-- Claude-style artifact catalog. Bytes stay in immutable object storage; this
-- table stores tenant-scoped presentation, version, and lineage contracts.
CREATE TABLE artifact_versions (
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    artifact_id text NOT NULL,
    version_id text NOT NULL,
    version_number bigint NOT NULL CHECK (version_number > 0),
    document jsonb NOT NULL,
    content_object_key text NOT NULL,
    source_object_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, project_id, version_id),
    UNIQUE (organization_id, project_id, artifact_id, version_number),
    CHECK (length(artifact_id) BETWEEN 1 AND 128),
    CHECK (length(version_id) BETWEEN 1 AND 128),
    CHECK (jsonb_typeof(document) = 'object')
);

CREATE INDEX artifact_versions_catalog_idx
    ON artifact_versions (organization_id, project_id, artifact_id, version_number DESC);
CREATE INDEX artifact_versions_recent_idx
    ON artifact_versions (organization_id, project_id, created_at DESC, version_id DESC);

ALTER TABLE artifact_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON artifact_versions
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    );
