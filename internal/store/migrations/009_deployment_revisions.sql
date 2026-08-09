CREATE TABLE deployment_revisions (
 id TEXT PRIMARY KEY,
 deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 revision_number INTEGER NOT NULL CHECK(revision_number > 0),
 status TEXT NOT NULL CHECK(status IN ('candidate','active','superseded','rejected','failed')),
 spec_json JSONB NOT NULL,
 source_revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE SET NULL,
 reason TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL,
 activated_at TIMESTAMPTZ,
 completed_at TIMESTAMPTZ,
 UNIQUE(deployment_id,revision_number)
);

INSERT INTO deployment_revisions(id,deployment_id,revision_number,status,spec_json,created_at,activated_at)
SELECT id || '-rev-1',id,1,'active',jsonb_build_object(
 'model',model,
 'runtime',runtime,
 'routing_strategy',routing_strategy,
 'min_replicas',min_replicas,
 'max_replicas',max_replicas,
 'autoscaling_enabled',autoscaling_enabled
),created_at,created_at
FROM deployments;

ALTER TABLE deployments ADD COLUMN active_revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE RESTRICT;
ALTER TABLE deployments ADD COLUMN candidate_revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE RESTRICT;
UPDATE deployments SET active_revision_id=id || '-rev-1';

CREATE FUNCTION infercrane_create_initial_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO deployment_revisions(id,deployment_id,revision_number,status,spec_json,created_at,activated_at)
 VALUES(NEW.id || '-rev-1',NEW.id,1,'active',jsonb_build_object(
  'model',NEW.model,
  'runtime',NEW.runtime,
  'routing_strategy',NEW.routing_strategy,
  'min_replicas',NEW.min_replicas,
  'max_replicas',NEW.max_replicas,
  'autoscaling_enabled',NEW.autoscaling_enabled
 ),NEW.created_at,NEW.created_at);
 UPDATE deployments SET active_revision_id=NEW.id || '-rev-1' WHERE id=NEW.id;
 RETURN NEW;
END;
$$;
CREATE TRIGGER deployments_create_initial_revision
 AFTER INSERT ON deployments
 FOR EACH ROW EXECUTE FUNCTION infercrane_create_initial_revision();

ALTER TABLE replicas ADD COLUMN revision_id TEXT REFERENCES deployment_revisions(id) ON DELETE CASCADE;
UPDATE replicas SET revision_id=deployment_id || '-rev-1';
ALTER TABLE replicas ALTER COLUMN revision_id SET NOT NULL;
ALTER TABLE replicas DROP CONSTRAINT replicas_deployment_id_ordinal_key;
ALTER TABLE replicas ADD CONSTRAINT replicas_revision_ordinal_key UNIQUE(revision_id,ordinal);
CREATE INDEX idx_revisions_deployment_status ON deployment_revisions(deployment_id,status,revision_number DESC);
CREATE INDEX idx_replicas_revision_state ON replicas(revision_id,lifecycle_state,ordinal);
