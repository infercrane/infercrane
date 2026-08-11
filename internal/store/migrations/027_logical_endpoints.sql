CREATE TABLE environments (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 name TEXT NOT NULL,
 policy_json JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id,name)
);

CREATE TABLE logical_models (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id,name)
);

CREATE TABLE endpoints (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 logical_model_id TEXT NOT NULL REFERENCES logical_models(id) ON DELETE RESTRICT,
 environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
 name TEXT NOT NULL,
 desired_state TEXT NOT NULL DEFAULT 'serving' CHECK(desired_state IN ('serving','suspended','deleted')),
 observed_state TEXT NOT NULL DEFAULT 'pending' CHECK(observed_state IN ('pending','serving','degraded','suspended','deleted')),
 active_serving_plan_id TEXT,
 candidate_serving_plan_id TEXT,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id,name)
);

CREATE TABLE backend_bindings (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 name TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('deployment','external')),
 ownership_mode TEXT NOT NULL CHECK(ownership_mode IN ('observe-only','traffic-managed','lifecycle-managed')),
 deployment_id TEXT REFERENCES deployments(id) ON DELETE RESTRICT,
 target_id TEXT REFERENCES targets(id) ON DELETE RESTRICT,
 config_json JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(endpoint_id,name),
 CHECK((kind='deployment' AND deployment_id IS NOT NULL AND target_id IS NULL) OR
       (kind='external' AND target_id IS NOT NULL AND deployment_id IS NULL))
);

CREATE TABLE serving_plans (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 version INTEGER NOT NULL CHECK(version > 0),
 routing_policy TEXT NOT NULL CHECK(routing_policy IN ('manual','primary-fallback','weighted')),
 spec_json JSONB NOT NULL,
 spec_digest TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 UNIQUE(endpoint_id,version),
 UNIQUE(endpoint_id,spec_digest)
);

CREATE TABLE serving_plan_bindings (
 serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE CASCADE,
 binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE RESTRICT,
 priority INTEGER NOT NULL DEFAULT 0 CHECK(priority >= 0),
 weight INTEGER NOT NULL DEFAULT 100 CHECK(weight > 0 AND weight <= 10000),
 PRIMARY KEY(serving_plan_id,binding_id),
 UNIQUE(serving_plan_id,priority)
);

ALTER TABLE endpoints
 ADD CONSTRAINT endpoints_active_plan_fk FOREIGN KEY(active_serving_plan_id) REFERENCES serving_plans(id) ON DELETE RESTRICT,
 ADD CONSTRAINT endpoints_candidate_plan_fk FOREIGN KEY(candidate_serving_plan_id) REFERENCES serving_plans(id) ON DELETE RESTRICT,
 ADD CONSTRAINT endpoints_distinct_plans CHECK(candidate_serving_plan_id IS NULL OR candidate_serving_plan_id <> active_serving_plan_id);

CREATE INDEX endpoints_tenant_environment_idx ON endpoints(tenant_id,environment_id,name);
CREATE INDEX backend_bindings_deployment_idx ON backend_bindings(deployment_id) WHERE deployment_id IS NOT NULL;
CREATE INDEX serving_plans_endpoint_version_idx ON serving_plans(endpoint_id,version DESC);

CREATE TABLE endpoint_release_guard_policies (
 endpoint_id TEXT PRIMARY KEY REFERENCES endpoints(id) ON DELETE CASCADE,
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 minimum_requests INTEGER NOT NULL DEFAULT 20 CHECK(minimum_requests > 0),
 max_ttft_regression_percent DOUBLE PRECISION NOT NULL DEFAULT 15 CHECK(max_ttft_regression_percent >= 0),
 max_latency_regression_percent DOUBLE PRECISION NOT NULL DEFAULT 15 CHECK(max_latency_regression_percent >= 0),
 max_error_rate_increase DOUBLE PRECISION NOT NULL DEFAULT 0.01 CHECK(max_error_rate_increase BETWEEN 0 AND 1),
 max_output_throughput_drop_percent DOUBLE PRECISION NOT NULL DEFAULT 15 CHECK(max_output_throughput_drop_percent >= 0),
 require_compatibility_evidence BOOLEAN NOT NULL DEFAULT TRUE,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE endpoint_release_guard_evaluations (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
 active_serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
 candidate_serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
 decision TEXT NOT NULL CHECK(decision IN ('PASS','REJECT','INCONCLUSIVE')),
 reason_codes_json JSONB NOT NULL,
 metrics_json JSONB NOT NULL,
 policy_json JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX endpoint_guard_evaluations_idx ON endpoint_release_guard_evaluations(endpoint_id,created_at DESC);

INSERT INTO environments(id,tenant_id,name,created_at,updated_at)
SELECT tenant_id || '-environment-production',tenant_id,'production',MIN(created_at),NOW()
FROM deployments GROUP BY tenant_id
ON CONFLICT(tenant_id,name) DO NOTHING;

INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at)
SELECT id || '-logical-model',tenant_id,name,'Migrated from v1 deployment alias',created_at,NOW()
FROM deployments;

INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,created_at,updated_at)
SELECT id || '-endpoint',tenant_id,id || '-logical-model',tenant_id || '-environment-production',name,
 CASE WHEN desired_state='deleted' THEN 'deleted' WHEN desired_state='suspended' THEN 'suspended' ELSE 'serving' END,
 CASE WHEN desired_state='deleted' THEN 'deleted' WHEN observed_state IN ('serving','degraded','suspended') THEN observed_state ELSE 'pending' END,
 created_at,NOW()
FROM deployments;

INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,deployment_id,created_at,updated_at)
SELECT id || '-binding',tenant_id,id || '-endpoint','primary','deployment','lifecycle-managed',id,created_at,NOW()
FROM deployments;

INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at)
SELECT id || '-plan-1',tenant_id,id || '-endpoint',1,'manual',
 jsonb_build_object('routing_policy','manual','bindings',jsonb_build_array(jsonb_build_object('binding_id',id || '-binding','priority',0,'weight',100))),
 'sha256:' || encode(sha256(convert_to(jsonb_build_object('routing_policy','manual','bindings',jsonb_build_array(jsonb_build_object('binding_id',id || '-binding','priority',0,'weight',100)))::text,'UTF8')),'hex'),
 created_at
FROM deployments;

INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight)
SELECT id || '-plan-1',id || '-binding',0,100 FROM deployments;

UPDATE endpoints SET active_serving_plan_id=replace(id,'-endpoint','-plan-1') WHERE active_serving_plan_id IS NULL;
INSERT INTO endpoint_release_guard_policies(endpoint_id,created_at,updated_at)
SELECT id,created_at,updated_at FROM endpoints;

CREATE FUNCTION infercrane_create_legacy_endpoint() RETURNS trigger LANGUAGE plpgsql AS $$
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
 VALUES(model_key,NEW.tenant_id,NEW.name,'Created for legacy deployment API compatibility',NEW.created_at,NEW.created_at);
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

CREATE TRIGGER deployments_create_legacy_endpoint
 AFTER INSERT ON deployments
 FOR EACH ROW EXECUTE FUNCTION infercrane_create_legacy_endpoint();

CREATE FUNCTION infercrane_create_endpoint_guard_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO endpoint_release_guard_policies(endpoint_id,created_at,updated_at)
 VALUES(NEW.id,NEW.created_at,NEW.updated_at) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;
CREATE TRIGGER endpoints_create_guard_policy
 AFTER INSERT ON endpoints
 FOR EACH ROW EXECUTE FUNCTION infercrane_create_endpoint_guard_policy();

ALTER TABLE request_records ADD COLUMN logical_model_id TEXT REFERENCES logical_models(id) ON DELETE SET NULL;
ALTER TABLE request_records ADD COLUMN environment_id TEXT REFERENCES environments(id) ON DELETE SET NULL;
ALTER TABLE request_records ADD COLUMN endpoint_id TEXT REFERENCES endpoints(id) ON DELETE SET NULL;
ALTER TABLE request_records ADD COLUMN serving_plan_id TEXT REFERENCES serving_plans(id) ON DELETE SET NULL;
ALTER TABLE request_records ADD COLUMN binding_id TEXT REFERENCES backend_bindings(id) ON DELETE SET NULL;
UPDATE request_records r SET
 logical_model_id=d.id || '-logical-model',
 environment_id=d.tenant_id || '-environment-production',
 endpoint_id=d.id || '-endpoint',
 serving_plan_id=d.id || '-plan-1',
 binding_id=d.id || '-binding'
FROM deployments d WHERE d.id=r.deployment_id;
CREATE INDEX request_records_endpoint_started_idx ON request_records(endpoint_id,started_at DESC) WHERE endpoint_id IS NOT NULL;

CREATE FUNCTION infercrane_reject_serving_plan_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'serving plans are immutable';
END;
$$;
CREATE TRIGGER serving_plans_immutable BEFORE UPDATE ON serving_plans
 FOR EACH ROW EXECUTE FUNCTION infercrane_reject_serving_plan_mutation();
CREATE TRIGGER serving_plan_bindings_immutable BEFORE UPDATE ON serving_plan_bindings
 FOR EACH ROW EXECUTE FUNCTION infercrane_reject_serving_plan_mutation();
