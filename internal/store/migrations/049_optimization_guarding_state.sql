ALTER TABLE optimization_candidate_runs
    DROP CONSTRAINT optimization_candidate_runs_state_check,
    ADD CONSTRAINT optimization_candidate_runs_state_check
        CHECK(state IN ('proposed','provisioning','ready','measuring','validating','ranked','guarding','guard_passed','rejected','inconclusive','promoted','observed','failed','cancelled','cleaned'));
