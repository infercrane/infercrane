UPDATE deployment_revisions r
SET spec_json = r.spec_json || jsonb_strip_nulls(jsonb_build_object(
 'compute_mode','elastic',
 'cloud',o.request_json->>'cloud',
 'gpu',o.request_json->>'gpu',
 'region',NULLIF(o.request_json->>'region',''),
 'runtime_version',NULLIF(o.request_json->>'runtime_version',''),
 'runtime_args',o.request_json->'runtime_args',
 'model_revision',NULLIF(o.request_json->>'model_revision',''),
 'port',NULLIF(o.request_json->>'port','')::integer
))
FROM deployments d
JOIN LATERAL (
 SELECT request_json
 FROM operations
 WHERE tenant_id=d.tenant_id AND resource_name=d.name AND kind='deployment.converge'
 ORDER BY created_at ASC
 LIMIT 1
) o ON TRUE
WHERE r.id=d.active_revision_id;

UPDATE deployment_revisions
SET spec_json = spec_json || jsonb_build_object('compute_mode','existing')
WHERE NOT (spec_json ? 'compute_mode');
