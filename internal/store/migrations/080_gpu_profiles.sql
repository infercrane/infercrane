-- A GPU profile capture recorded against the revision that was active when it
-- arrived. The document holds the bounded, content-free call-stack samples
-- (infercrane.gpu-profile/v1); the columns hold what reads filter and order on.
-- At most a few captures are kept per deployment; recording a new one removes
-- the oldest in the same transaction.
CREATE TABLE gpu_profiles (
  id TEXT PRIMARY KEY CHECK(id ~ '^gpc-[0-9a-f]{32}$'),
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
  tool TEXT NOT NULL CHECK(char_length(tool) BETWEEN 1 AND 64),
  evidence_class TEXT NOT NULL CHECK(evidence_class='measured'),
  started_at TIMESTAMPTZ NOT NULL,
  ended_at TIMESTAMPTZ NOT NULL CHECK(ended_at > started_at),
  document JSONB NOT NULL,
  document_sha256 TEXT NOT NULL CHECK(document_sha256 ~ '^[0-9a-f]{64}$'),
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,deployment_id,revision_id,tool,started_at,ended_at)
);

CREATE INDEX gpu_profiles_latest_idx
  ON gpu_profiles(tenant_id,deployment_id,revision_id,ended_at DESC,created_at DESC);
