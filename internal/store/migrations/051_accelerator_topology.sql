ALTER TABLE capacity_evidence
    ADD COLUMN gpu_count INTEGER NOT NULL DEFAULT 1 CHECK (gpu_count BETWEEN 1 AND 1024);

DROP INDEX capacity_evidence_lookup_idx;
CREATE INDEX capacity_evidence_lookup_idx
    ON capacity_evidence(tenant_id,provider,runtime,compute_mode,region,gpu,gpu_count,observed_at DESC);

ALTER TABLE capacity_operations
    ADD COLUMN gpu_count INTEGER NOT NULL DEFAULT 1 CHECK (gpu_count BETWEEN 1 AND 1024);

DROP INDEX capacity_operations_history_idx;
CREATE INDEX capacity_operations_history_idx
    ON capacity_operations(tenant_id,provider,runtime,compute_mode,region,gpu,gpu_count,completed_at DESC);
