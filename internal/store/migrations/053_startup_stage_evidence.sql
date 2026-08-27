ALTER TABLE capacity_operations
    ADD COLUMN model_identity TEXT NOT NULL DEFAULT '',
    ADD COLUMN runtime_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN runtime_args_digest TEXT NOT NULL DEFAULT '',
    ADD COLUMN stage_durations_json JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(stage_durations_json) = 'object');

DROP INDEX capacity_operations_history_idx;
CREATE INDEX capacity_operations_history_idx
    ON capacity_operations(
        tenant_id,provider,runtime,runtime_version,runtime_args_digest,
        model_identity,compute_mode,region,gpu,gpu_count,completed_at DESC
    );
