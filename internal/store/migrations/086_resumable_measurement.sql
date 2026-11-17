-- Optimization runs survive losing a measured machine.
--   measurement_checkpoint  the measuring candidate's completed paired turns
--                           and any interruption being recovered. The worker
--                           resumes from it, so it is decision evidence,
--                           unlike measurement_progress.
--   machine_replacements    machines of this candidate's setup that were lost
--                           and replaced, one entry per lost replica. The
--                           worker bounds replacements by this list.
ALTER TABLE optimization_candidate_runs
 ADD COLUMN measurement_checkpoint JSONB NOT NULL DEFAULT '{}'::jsonb,
 ADD COLUMN machine_replacements JSONB NOT NULL DEFAULT '[]'::jsonb;

-- A replaced machine keeps its deleted replica row and provider identity for
-- audit and spend. Only live replicas must be unique per revision ordinal, so
-- a replacement can take the same ordinal with a new provider identity.
ALTER TABLE replicas DROP CONSTRAINT replicas_revision_ordinal_key;
CREATE UNIQUE INDEX replicas_live_revision_ordinal_key ON replicas(revision_id, ordinal) WHERE lifecycle_state <> 'deleted';
