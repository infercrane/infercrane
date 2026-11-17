-- A fully deleted deployment keeps its row as history: events, revisions,
-- evidence, and operations stay attached to its ID. It no longer owns its
-- name, so the same name can be deployed again as a new deployment with a new
-- ID. A deployment that is still being deleted keeps its name until provider
-- cleanup has finished.
ALTER TABLE deployments DROP CONSTRAINT deployments_tenant_name_key;
CREATE UNIQUE INDEX deployments_tenant_live_name_key
    ON deployments(tenant_id,name)
    WHERE NOT (desired_state='deleted' AND observed_state='deleted');

-- Deleted endpoints stay as history in the same way.
ALTER TABLE endpoints DROP CONSTRAINT endpoints_tenant_id_name_key;
CREATE UNIQUE INDEX endpoints_tenant_live_name_key
    ON endpoints(tenant_id,name)
    WHERE desired_state<>'deleted';

-- An endpoint InferCrane created for a deployment records that deployment as
-- its owner. When the deployment is deleted, an owned endpoint with no other
-- backend is deleted with it. Endpoints an operator created stay untouched.
ALTER TABLE endpoints
    ADD COLUMN owner_deployment_id TEXT REFERENCES deployments(id) ON DELETE SET NULL;
UPDATE endpoints e SET owner_deployment_id=d.id
FROM deployments d
WHERE e.id=d.id || '-endpoint';
CREATE INDEX endpoints_owner_deployment_idx
    ON endpoints(owner_deployment_id)
    WHERE owner_deployment_id IS NOT NULL;

-- Owned endpoints left behind by deployments that were already deleted can
-- never serve again. Delete them now so their names can be deployed again.
UPDATE endpoints e SET desired_state='deleted',observed_state='deleted',updated_at=NOW()
FROM deployments d
WHERE e.owner_deployment_id=d.id
  AND d.desired_state='deleted' AND d.observed_state='deleted'
  AND e.desired_state<>'deleted'
  AND NOT EXISTS (
      SELECT 1 FROM backend_bindings b
      WHERE b.endpoint_id=e.id AND (b.kind<>'deployment' OR b.deployment_id<>d.id)
  );

-- A deployment name deployed again needs its own prepaid hold. Settled and
-- released holds stay as history; only one open hold may exist per resource.
ALTER TABLE managed_spend_reservations
    DROP CONSTRAINT managed_spend_reservations_tenant_id_resource_type_resource_key;
CREATE UNIQUE INDEX managed_spend_reservations_open_resource_key
    ON managed_spend_reservations(tenant_id,resource_type,resource_name)
    WHERE state IN ('reserved','pending_reconciliation');
CREATE INDEX managed_spend_reservations_resource_idx
    ON managed_spend_reservations(tenant_id,resource_type,resource_name,created_at DESC);

-- Unchanged from 079 except that a name deployed again reuses the logical
-- model its earlier deployment created, and the endpoint records its owner.
CREATE OR REPLACE FUNCTION infercrane_create_legacy_endpoint() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 environment_key TEXT := NEW.tenant_id || '-environment-production';
 model_key TEXT := NEW.id || '-logical-model';
 endpoint_key TEXT := NEW.id || '-endpoint';
 binding_key TEXT := NEW.id || '-binding';
 plan_key TEXT := NEW.id || '-plan-1';
 plan_spec JSONB;
BEGIN
 INSERT INTO environments(id,tenant_id,name,created_at,updated_at)
 VALUES(environment_key,NEW.tenant_id,'production',NEW.created_at,NEW.created_at)
 ON CONFLICT(tenant_id,name) DO NOTHING;
 SELECT id INTO environment_key FROM environments WHERE tenant_id=NEW.tenant_id AND name='production';
 INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at)
 VALUES(model_key,NEW.tenant_id,NEW.name,'',NEW.created_at,NEW.created_at)
 ON CONFLICT(tenant_id,name) DO NOTHING;
 SELECT id INTO model_key FROM logical_models WHERE tenant_id=NEW.tenant_id AND name=NEW.name;
 INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,owner_deployment_id,created_at,updated_at)
 VALUES(endpoint_key,NEW.tenant_id,model_key,environment_key,NEW.name,'serving','pending',NEW.id,NEW.created_at,NEW.created_at);
 INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,deployment_id,created_at,updated_at)
 VALUES(binding_key,NEW.tenant_id,endpoint_key,'primary','deployment','lifecycle-managed',NEW.id,NEW.created_at,NEW.created_at);
 plan_spec := jsonb_build_object('routing_policy','manual','bindings',jsonb_build_array(jsonb_build_object('binding_id',binding_key,'priority',0,'weight',100)));
 INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at)
 VALUES(plan_key,NEW.tenant_id,endpoint_key,1,'manual',plan_spec,'sha256:' || encode(sha256(convert_to(plan_spec::text,'UTF8')),'hex'),NEW.created_at);
 INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight) VALUES(plan_key,binding_key,0,100);
 UPDATE endpoints SET active_serving_plan_id=plan_key WHERE id=endpoint_key;
 RETURN NEW;
END;
$$;
