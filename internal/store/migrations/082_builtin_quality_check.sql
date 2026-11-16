-- Built-in answer check for optimization campaigns.
--
-- source separates evidence InferCrane produced itself from evidence a customer
-- signed and attached. Only the control-plane worker writes 'infercrane'; the
-- attach API always writes 'customer'. reference_revision_id names the revision
-- whose answers built-in evidence was scored against.
ALTER TABLE revision_quality_evidence
    ADD COLUMN source TEXT NOT NULL DEFAULT 'customer' CHECK (source IN ('customer','infercrane')),
    ADD COLUMN reference_revision_id TEXT NOT NULL DEFAULT '' CHECK (octet_length(reference_revision_id) <= 128);

-- A customer eval set replaces the built-in prompts for a campaign that names
-- its digest. The exact uploaded bytes are kept so the digest stays verifiable.
CREATE TABLE quality_eval_sets (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128),
    version TEXT NOT NULL CHECK (octet_length(version) BETWEEN 1 AND 128),
    prompt_count INTEGER NOT NULL CHECK (prompt_count BETWEEN 1 AND 200),
    body TEXT NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 524288),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, digest)
);
