CREATE UNIQUE INDEX one_unresolved_deployment_operation
 ON operations(tenant_id,resource_name)
 WHERE resource_type='deployment'
 AND status IN ('pending','leased','running','waiting','cancelling');
