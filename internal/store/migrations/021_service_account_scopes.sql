ALTER TABLE principals
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'service_account'
    CHECK(kind IN ('service_account')),
  ADD COLUMN scopes_json JSONB NOT NULL DEFAULT '[]';

ALTER TABLE principals
  ADD CONSTRAINT principals_scopes_array CHECK(jsonb_typeof(scopes_json) = 'array');

-- Freeze the pre-v0.3 permissions of existing credentials. In particular,
-- introducing manage_secrets/manage_external must not silently escalate an
-- existing operator or administrator.
UPDATE principals
SET scopes_json = CASE role
  WHEN 'viewer' THEN '["read"]'::jsonb
  WHEN 'operator' THEN '["read","deploy","delete"]'::jsonb
  WHEN 'admin' THEN '["read","deploy","delete","manage_tenant"]'::jsonb
  ELSE '[]'::jsonb
END;
