ALTER TABLE optimization_candidate_runs
    ADD COLUMN lab_evaluation_id TEXT REFERENCES lab_evaluations(id) ON DELETE RESTRICT;

CREATE INDEX optimization_candidate_runs_lab_evaluation_idx
    ON optimization_candidate_runs(tenant_id, lab_evaluation_id)
    WHERE lab_evaluation_id IS NOT NULL;
