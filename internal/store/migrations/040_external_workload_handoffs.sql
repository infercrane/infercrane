ALTER TABLE principals
  DROP CONSTRAINT principals_kind_check,
  ADD CONSTRAINT principals_kind_check
    CHECK(kind IN ('service_account','inference_token')),
  ADD COLUMN expires_at TIMESTAMPTZ,
  ADD COLUMN endpoint_names_json JSONB NOT NULL DEFAULT '[]';

ALTER TABLE principals
  ADD CONSTRAINT principals_endpoint_names_array
    CHECK(jsonb_typeof(endpoint_names_json) = 'array');

CREATE TABLE sandbox_references (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  external_id TEXT NOT NULL,
  external_revision TEXT NOT NULL DEFAULT '',
  endpoint_name TEXT NOT NULL,
  principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'referenced'
    CHECK(status IN ('referenced','stopped','unknown')),
  metadata_json JSONB NOT NULL DEFAULT '{}',
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,principal_id)
);

CREATE INDEX sandbox_references_tenant_created_idx
  ON sandbox_references(tenant_id,created_at DESC,id DESC);

CREATE UNIQUE INDEX sandbox_references_live_external_identity_idx
  ON sandbox_references(tenant_id,provider,external_id)
  WHERE status='referenced';

CREATE TABLE training_artifact_handoffs (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
  model_artifact_id TEXT NOT NULL REFERENCES model_artifacts(id),
  provider TEXT NOT NULL,
  external_run_id TEXT NOT NULL,
  repository TEXT NOT NULL,
  immutable_revision TEXT NOT NULL,
  artifact_digest TEXT NOT NULL,
  base_model_identity TEXT NOT NULL DEFAULT '',
  method TEXT NOT NULL DEFAULT '',
  framework TEXT NOT NULL DEFAULT '',
  framework_version TEXT NOT NULL DEFAULT '',
  dataset_fingerprint TEXT NOT NULL DEFAULT '',
  payload_digest TEXT NOT NULL,
  signature TEXT NOT NULL,
  public_key TEXT NOT NULL,
  algorithm TEXT NOT NULL,
  key_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,revision_id)
);

CREATE INDEX training_artifact_handoffs_tenant_created_idx
  ON training_artifact_handoffs(tenant_id,created_at DESC,id DESC);

CREATE INDEX training_artifact_handoffs_source_idx
  ON training_artifact_handoffs(tenant_id,provider,external_run_id,artifact_digest);
