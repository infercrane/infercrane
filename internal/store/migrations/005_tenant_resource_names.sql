ALTER TABLE targets DROP CONSTRAINT targets_name_key;
ALTER TABLE targets DROP CONSTRAINT targets_url_key;
ALTER TABLE deployments DROP CONSTRAINT deployments_name_key;
ALTER TABLE targets ADD CONSTRAINT targets_tenant_name_key UNIQUE(tenant_id,name);
ALTER TABLE targets ADD CONSTRAINT targets_tenant_url_key UNIQUE(tenant_id,url);
ALTER TABLE deployments ADD CONSTRAINT deployments_tenant_name_key UNIQUE(tenant_id,name);
