ALTER TABLE operations ADD COLUMN lease_owner TEXT;
ALTER TABLE operations ADD COLUMN lease_expires_at TIMESTAMPTZ;
ALTER TABLE operations ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE operations ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 5 CHECK(max_attempts > 0);
CREATE INDEX idx_operations_claimable ON operations(status,next_attempt_at,created_at)
 WHERE status IN ('pending','running');
