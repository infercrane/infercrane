ALTER TABLE request_records ADD COLUMN revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE SET NULL;
ALTER TABLE request_records ADD COLUMN provider TEXT;
ALTER TABLE request_records ADD COLUMN runtime TEXT;
ALTER TABLE request_records ADD COLUMN compute_mode TEXT;
ALTER TABLE request_records ADD COLUMN operation_name TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE request_records ADD COLUMN response_model TEXT;
ALTER TABLE request_records ADD COLUMN streaming BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE request_records ADD COLUMN ttft_ms DOUBLE PRECISION;

CREATE INDEX idx_requests_revision_started ON request_records(revision_id,started_at DESC) WHERE revision_id IS NOT NULL;
