CREATE TABLE release_guard_policies (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 minimum_requests INTEGER NOT NULL DEFAULT 20 CHECK(minimum_requests > 0),
 max_ttft_regression_percent DOUBLE PRECISION NOT NULL DEFAULT 15 CHECK(max_ttft_regression_percent >= 0),
 max_latency_regression_percent DOUBLE PRECISION NOT NULL DEFAULT 15 CHECK(max_latency_regression_percent >= 0),
 max_error_rate_increase DOUBLE PRECISION NOT NULL DEFAULT 0.01 CHECK(max_error_rate_increase >= 0 AND max_error_rate_increase <= 1),
 max_output_throughput_drop_percent DOUBLE PRECISION NOT NULL DEFAULT 20 CHECK(max_output_throughput_drop_percent >= 0),
 updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE release_guard_evaluations (
 id TEXT PRIMARY KEY,
 deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 active_revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
 candidate_revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
 decision TEXT NOT NULL CHECK(decision IN ('ACCEPT','REJECT','WAIT')),
 reason_codes_json JSONB NOT NULL,
 metrics_json JSONB NOT NULL,
 policy_json JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);

INSERT INTO release_guard_policies(deployment_id,updated_at) SELECT id,NOW() FROM deployments;

CREATE FUNCTION infercrane_create_release_guard_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO release_guard_policies(deployment_id,updated_at) VALUES(NEW.id,NEW.created_at);
 RETURN NEW;
END;
$$;
CREATE TRIGGER deployments_create_release_guard_policy
 AFTER INSERT ON deployments
 FOR EACH ROW EXECUTE FUNCTION infercrane_create_release_guard_policy();

CREATE INDEX idx_guard_evaluations_deployment_created ON release_guard_evaluations(deployment_id,created_at DESC);
