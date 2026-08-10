BEGIN;

-- The runner's runtime sequence is independent from the public run_events
-- bigserial sequence. Keeping both lets API consumers see one ordered event
-- stream while retries remain idempotent against the runner source stream.
-- The source value is task-qualified because each runner task starts its own
-- sequence at one; a workflow may contain more than one runner task.
ALTER TABLE run_events
    ADD COLUMN source text,
    ADD COLUMN source_sequence bigint,
    ADD CONSTRAINT run_events_source_fields_check CHECK (
        (source IS NULL AND source_sequence IS NULL)
        OR (source IS NOT NULL AND source_sequence IS NOT NULL AND source_sequence > 0)
    ),
    ADD CONSTRAINT run_events_source_sequence_unique
        UNIQUE (organization_id, project_id, run_id, source, source_sequence);

ALTER TABLE local_workflows
    ADD COLUMN runner_event_task_id text NOT NULL DEFAULT '',
    ADD COLUMN runner_event_cursor bigint NOT NULL DEFAULT 0
        CHECK (runner_event_cursor >= 0),
    ADD CONSTRAINT local_workflows_runner_event_task_check CHECK (
        runner_event_cursor = 0 OR length(runner_event_task_id) > 0
    );

COMMIT;
