-- Deployments create their endpoint's logical model through the trigger from
-- 027. Its description is customer-visible, so leave it empty instead of
-- describing an internal compatibility path. Only the description changes.
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
 VALUES(model_key,NEW.tenant_id,NEW.name,'',NEW.created_at,NEW.created_at);
 INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,created_at,updated_at)
 VALUES(endpoint_key,NEW.tenant_id,model_key,environment_key,NEW.name,'serving','pending',NEW.created_at,NEW.created_at);
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

UPDATE logical_models
SET description='', updated_at=NOW()
WHERE description='Created for legacy deployment API compatibility';
