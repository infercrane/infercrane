-- A workspace can keep its own secrets sealed in InferCrane, or name a secret
-- that stays in its own cluster or AWS account.
--   sealed               value_* hold the AES-256-GCM ciphertext, nonce and the
--                        reference of the key that sealed it; reference is ''.
--   kubernetes           reference is SECRET-NAME/KEY in the cluster namespace.
--   aws-secrets-manager  reference is a Secrets Manager ARN.
-- Workers read kubernetes and aws-secrets-manager values; the control plane never does.
ALTER TABLE secret_references DROP CONSTRAINT secret_references_resolver_check;
ALTER TABLE secret_references
  ADD COLUMN value_ciphertext BYTEA,
  ADD COLUMN value_nonce BYTEA,
  ADD COLUMN value_key_reference TEXT,
  ADD CONSTRAINT secret_references_resolver_check
    CHECK(resolver IN ('env','sealed','kubernetes','aws-secrets-manager')),
  ADD CONSTRAINT secret_references_sealed_value_check
    CHECK((resolver='sealed') = (value_ciphertext IS NOT NULL AND value_nonce IS NOT NULL AND value_key_reference IS NOT NULL));

-- Weights or a LoRA adapter in the customer's own S3 or GCS bucket, pinned by
-- the digest of a file manifest. The control plane never reads the bucket:
-- workers download the listed files with their own cloud identity and check
-- every file's sha256 before the server starts.
--   manifest_json  {"files":[{"path","size","sha256"}]}, no file contents.
--   checks_json    what the import verified and what stays unknown.
CREATE TABLE model_imports (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('weights','adapter')),
  source TEXT NOT NULL CHECK(source IN ('s3','gs')),
  uri TEXT NOT NULL,
  manifest_digest TEXT NOT NULL CHECK(manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
  manifest_json JSONB NOT NULL,
  checks_json JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id, uri, manifest_digest)
);
CREATE INDEX idx_model_imports_tenant ON model_imports(tenant_id, created_at DESC);
