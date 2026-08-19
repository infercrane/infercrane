CREATE TABLE operational_measurements (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
  replica_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  value DOUBLE PRECISION NOT NULL,
  unit TEXT NOT NULL,
  evidence_class TEXT NOT NULL CHECK(evidence_class IN ('measured','provider_reported')),
  source TEXT NOT NULL,
  sample_count INTEGER NOT NULL CHECK(sample_count > 0),
  observed_at TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ NOT NULL CHECK(valid_until > observed_at),
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,deployment_id,revision_id,replica_id,name,source,observed_at)
);

CREATE INDEX operational_measurements_endpoint_window_idx
  ON operational_measurements(tenant_id,deployment_id,name,observed_at DESC);
