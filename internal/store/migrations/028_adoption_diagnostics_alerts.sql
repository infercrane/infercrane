ALTER TABLE request_records ALTER COLUMN deployment_id DROP NOT NULL;

ALTER TABLE request_records ADD COLUMN tenant_id TEXT REFERENCES tenants(id) ON DELETE CASCADE;
UPDATE request_records r SET tenant_id=d.tenant_id FROM deployments d WHERE d.id=r.deployment_id;
ALTER TABLE request_records ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE request_records
  ADD COLUMN queue_ms DOUBLE PRECISION CHECK(queue_ms IS NULL OR queue_ms >= 0),
  ADD COLUMN generation_ms DOUBLE PRECISION CHECK(generation_ms IS NULL OR generation_ms >= 0),
  ADD COLUMN fallback_reason TEXT;

CREATE TABLE adopted_workloads (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE CASCADE,
  target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE RESTRICT,
  ownership_mode TEXT NOT NULL CHECK(ownership_mode IN ('observe-only','traffic-managed')),
  source TEXT NOT NULL CHECK(source IN ('openai-compatible','vllm')),
  immutable_identity TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,endpoint_id)
);

CREATE TABLE diagnostic_findings (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  code TEXT NOT NULL,
  severity TEXT NOT NULL CHECK(severity IN ('info','warning','critical')),
  confidence TEXT NOT NULL CHECK(confidence IN ('low','medium','high')),
  summary TEXT NOT NULL,
  evidence_json JSONB NOT NULL,
  evidence_digest TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,endpoint_id,code,evidence_digest)
);
CREATE INDEX diagnostic_findings_endpoint_created_idx
  ON diagnostic_findings(tenant_id,endpoint_id,created_at DESC);

CREATE TABLE alert_policies (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  webhook_url TEXT NOT NULL,
  secret_reference_id TEXT NOT NULL REFERENCES secret_references(id) ON DELETE RESTRICT,
  minimum_severity TEXT NOT NULL CHECK(minimum_severity IN ('info','warning','critical')),
  enabled BOOLEAN NOT NULL,
  max_attempts INTEGER NOT NULL CHECK(max_attempts BETWEEN 1 AND 5),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,endpoint_id,name)
);

CREATE TABLE alert_deliveries (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  policy_id TEXT NOT NULL REFERENCES alert_policies(id) ON DELETE CASCADE,
  finding_id TEXT NOT NULL REFERENCES diagnostic_findings(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed')),
  attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
  response_status INTEGER,
  error_code TEXT,
  body_digest TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  delivered_at TIMESTAMPTZ,
  UNIQUE(policy_id,finding_id)
);
CREATE INDEX alert_deliveries_pending_idx ON alert_deliveries(status,updated_at);
CREATE INDEX request_records_tenant_request_idx ON request_records(tenant_id,request_id);
