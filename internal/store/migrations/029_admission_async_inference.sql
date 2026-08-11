CREATE TABLE endpoint_admission_policies (
 endpoint_id TEXT PRIMARY KEY REFERENCES endpoints(id) ON DELETE CASCADE,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 max_concurrency INTEGER NOT NULL DEFAULT 32 CHECK(max_concurrency BETWEEN 1 AND 10000),
 max_queue_depth INTEGER NOT NULL DEFAULT 64 CHECK(max_queue_depth BETWEEN 0 AND 100000),
 queue_timeout_ms INTEGER NOT NULL DEFAULT 5000 CHECK(queue_timeout_ms BETWEEN 1 AND 300000),
 max_request_bytes INTEGER NOT NULL DEFAULT 16777216 CHECK(max_request_bytes BETWEEN 1024 AND 16777216),
 max_output_tokens INTEGER NOT NULL DEFAULT 8192 CHECK(max_output_tokens BETWEEN 1 AND 1048576),
 allowed_priorities_json JSONB NOT NULL DEFAULT '["normal"]'::jsonb,
 retry_budget INTEGER NOT NULL DEFAULT 0 CHECK(retry_budget BETWEEN 0 AND 3),
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id, endpoint_id)
);

CREATE TABLE async_inference_jobs (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE RESTRICT,
 request_id TEXT NOT NULL,
 protocol TEXT NOT NULL CHECK(protocol IN ('chat','responses','embeddings','completions','batch')),
 status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled','expired')),
 priority INTEGER NOT NULL DEFAULT 0 CHECK(priority BETWEEN -100 AND 100),
 idempotency_key TEXT NOT NULL,
 payload_ciphertext BYTEA NOT NULL,
 payload_nonce BYTEA NOT NULL,
 result_ciphertext BYTEA,
 result_nonce BYTEA,
 encryption_key_reference TEXT NOT NULL,
 webhook_url TEXT,
 webhook_secret_reference_id TEXT REFERENCES secret_references(id) ON DELETE RESTRICT,
 webhook_status TEXT NOT NULL DEFAULT 'not_configured' CHECK(webhook_status IN ('not_configured','pending','delivered','failed')),
 webhook_attempts INTEGER NOT NULL DEFAULT 0 CHECK(webhook_attempts BETWEEN 0 AND 3),
 webhook_error_code TEXT,
 execution_deadline TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 lease_owner TEXT,
 lease_token TEXT,
 lease_expires_at TIMESTAMPTZ,
 attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt BETWEEN 0 AND 3),
 error_code TEXT,
 error_message TEXT,
 created_at TIMESTAMPTZ NOT NULL,
 started_at TIMESTAMPTZ,
 completed_at TIMESTAMPTZ,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id, idempotency_key),
 UNIQUE(tenant_id, request_id),
 CHECK((webhook_url IS NULL) = (webhook_secret_reference_id IS NULL)),
 CHECK((webhook_url IS NULL AND webhook_status='not_configured') OR (webhook_url IS NOT NULL AND webhook_status<>'not_configured')),
 CHECK(expires_at > execution_deadline)
);

CREATE INDEX async_inference_jobs_claim_idx
 ON async_inference_jobs(priority DESC, created_at)
 WHERE status='queued';
CREATE INDEX async_inference_jobs_lease_idx
 ON async_inference_jobs(lease_expires_at)
 WHERE status='running';
CREATE INDEX async_inference_jobs_expiry_idx ON async_inference_jobs(expires_at);
