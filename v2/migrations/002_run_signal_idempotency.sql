BEGIN;

-- The event stream is an audit trail, not a concurrency primitive. This table
-- owns public run-signal idempotency with one durable row per tenant/run/key.
-- Only hashes are stored; reply references, prompts, and other signal content
-- remain outside durable idempotency state.
CREATE TABLE run_signal_claims (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idempotency_key_hash text NOT NULL CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    request_fingerprint text NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    state text NOT NULL CHECK (state IN ('pending','accepted')),
    claim_token_hash text NOT NULL CHECK (claim_token_hash ~ '^[0-9a-f]{64}$'),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    lease_expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    PRIMARY KEY (organization_id, project_id, run_id, idempotency_key_hash)
);

CREATE INDEX run_signal_claims_pending_lease_idx
    ON run_signal_claims (lease_expires_at)
    WHERE state = 'pending';

ALTER TABLE run_signal_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_signal_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON run_signal_claims
    USING (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    )
    WITH CHECK (
        organization_id = nullif(current_setting('agw.organization_id', true), '')::uuid
        AND project_id = nullif(current_setting('agw.project_id', true), '')::uuid
    );

COMMIT;
