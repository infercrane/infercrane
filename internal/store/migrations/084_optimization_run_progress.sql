-- Live progress of an optimization run, recorded on each candidate so the
-- dashboard and CLI can say where a run is without guessing:
--   state_entered_at      first time the candidate entered each state
--   measurement_progress  the interleaved measurement's runs so far
--   cost_quote            the sourced hourly quote that authorized the
--                         candidate's provider mutation
-- All three are written only by the campaign worker and are display evidence;
-- no decision reads them.
ALTER TABLE optimization_candidate_runs
 ADD COLUMN state_entered_at JSONB NOT NULL DEFAULT '{}'::jsonb,
 ADD COLUMN measurement_progress JSONB NOT NULL DEFAULT '{}'::jsonb,
 ADD COLUMN cost_quote JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Every existing candidate was proposed when its row was created. Later
-- states were never timed, so they stay absent rather than invented.
UPDATE optimization_candidate_runs
SET state_entered_at = jsonb_build_object('proposed', to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
