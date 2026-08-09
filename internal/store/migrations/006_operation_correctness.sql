ALTER TABLE operations DROP CONSTRAINT operations_status_check;
ALTER TABLE operations ADD CONSTRAINT operations_status_check CHECK (
 status IN ('pending','leased','running','waiting','succeeded','failed','cancelling','cancelled')
);
ALTER TABLE operations ADD COLUMN lease_generation BIGINT NOT NULL DEFAULT 0 CHECK(lease_generation >= 0);
ALTER TABLE operations ADD COLUMN last_heartbeat_at TIMESTAMPTZ;
ALTER TABLE operations ADD COLUMN waiting_reason TEXT;
ALTER TABLE operations ADD COLUMN provider_request_id TEXT;

DROP INDEX idx_operations_claimable;
CREATE INDEX idx_operations_claimable ON operations(status,next_attempt_at,created_at)
 WHERE status IN ('pending','leased','running','waiting','cancelling');

CREATE TABLE operation_steps (
 operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
 step_name TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','running','waiting','succeeded','failed','cancelled')),
 attempt INTEGER NOT NULL DEFAULT 1 CHECK(attempt > 0),
 checkpoint_json JSONB NOT NULL DEFAULT '{}',
 error_code TEXT,
 started_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 completed_at TIMESTAMPTZ,
 PRIMARY KEY(operation_id,step_name)
);

CREATE TABLE operation_events (
 operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
 sequence BIGINT NOT NULL CHECK(sequence > 0),
 level TEXT NOT NULL CHECK(level IN ('debug','info','warn','error')),
 event_type TEXT NOT NULL,
 message TEXT NOT NULL,
 payload_json JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(operation_id,sequence)
);
CREATE INDEX idx_operation_events_created ON operation_events(operation_id,created_at,sequence);
