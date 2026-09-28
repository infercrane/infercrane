ALTER TABLE targets DROP CONSTRAINT targets_tenant_url_key;
CREATE INDEX targets_tenant_url_idx ON targets(tenant_id,url);
