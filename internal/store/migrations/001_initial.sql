CREATE TABLE targets (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, url TEXT NOT NULL UNIQUE,
 provider TEXT NOT NULL, runtime TEXT NOT NULL, upstream_model_name TEXT,
 health TEXT NOT NULL, provider_resource_id TEXT, provider_details_json JSONB,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE deployments (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, model TEXT NOT NULL, runtime TEXT NOT NULL,
 routing_strategy TEXT NOT NULL, desired_state TEXT NOT NULL, observed_state TEXT NOT NULL,
 min_replicas INTEGER NOT NULL DEFAULT 1 CHECK(min_replicas > 0),
 max_replicas INTEGER NOT NULL DEFAULT 1 CHECK(max_replicas >= min_replicas),
 autoscaling_enabled BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE deployment_targets (
 deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE RESTRICT,
 PRIMARY KEY(deployment_id,target_id)
);
CREATE TABLE deployment_events (
 id TEXT PRIMARY KEY, deployment_id TEXT REFERENCES deployments(id) ON DELETE CASCADE,
 target_id TEXT REFERENCES targets(id) ON DELETE SET NULL, event_type TEXT NOT NULL,
 summary TEXT NOT NULL, payload_json JSONB NOT NULL DEFAULT '{}', created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE request_records (
 request_id TEXT PRIMARY KEY, deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 target_id TEXT REFERENCES targets(id) ON DELETE SET NULL, started_at TIMESTAMPTZ NOT NULL,
 completed_at TIMESTAMPTZ, status_code INTEGER, latency_ms DOUBLE PRECISION,
 input_tokens INTEGER, output_tokens INTEGER, retry_count INTEGER NOT NULL DEFAULT 0, error_type TEXT
);
CREATE TABLE router_generations (
 id TEXT PRIMARY KEY, deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE, owner_id TEXT NOT NULL,
 generation INTEGER NOT NULL, strategy TEXT NOT NULL, worker_set_hash TEXT NOT NULL,
 internal_endpoint TEXT NOT NULL, status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
 UNIQUE(deployment_id,generation)
);
CREATE UNIQUE INDEX one_active_router_generation ON router_generations(deployment_id,owner_id) WHERE status='active';
CREATE INDEX idx_events_deployment_created ON deployment_events(deployment_id,created_at DESC);
CREATE INDEX idx_requests_deployment_started ON request_records(deployment_id,started_at DESC);
CREATE INDEX idx_requests_started_brin ON request_records USING BRIN(started_at);
CREATE INDEX idx_router_generation_active ON router_generations(deployment_id,owner_id,status,generation DESC);

CREATE TABLE scaling_policies (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
 enabled BOOLEAN NOT NULL, min_replicas INTEGER NOT NULL CHECK(min_replicas > 0),
 max_replicas INTEGER NOT NULL CHECK(max_replicas >= min_replicas), queue_threshold DOUBLE PRECISION NOT NULL,
 low_load_threshold DOUBLE PRECISION NOT NULL, scale_up_intervals INTEGER NOT NULL,
 scale_down_intervals INTEGER NOT NULL, cooldown_seconds INTEGER NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE scaling_decisions (
 id TEXT PRIMARY KEY, deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 action TEXT NOT NULL, old_replicas INTEGER NOT NULL, new_replicas INTEGER NOT NULL,
 reason TEXT NOT NULL, signals_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL
);
