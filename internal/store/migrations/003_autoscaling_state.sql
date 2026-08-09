CREATE TABLE autoscaling_state (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
 consecutive_high INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_high >= 0),
 consecutive_low INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_low >= 0),
 last_scaled_at TIMESTAMPTZ,
 updated_at TIMESTAMPTZ NOT NULL
);
