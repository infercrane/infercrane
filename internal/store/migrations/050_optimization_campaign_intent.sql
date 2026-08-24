ALTER TABLE optimization_campaigns
    ADD COLUMN intent TEXT NOT NULL DEFAULT 'new_endpoint'
        CHECK(intent IN ('new_endpoint','evolve_endpoint')),
    ADD COLUMN target_deployment_name TEXT;

ALTER TABLE optimization_campaigns
    ADD CONSTRAINT optimization_campaign_target_boundary CHECK(
        (intent='new_endpoint' AND target_deployment_name IS NULL)
        OR
        (intent='evolve_endpoint' AND target_deployment_name IS NOT NULL)
    );

ALTER TABLE optimization_campaigns
    DROP CONSTRAINT optimization_campaigns_state_check,
    ADD CONSTRAINT optimization_campaigns_state_check CHECK(state IN (
        'awaiting_approval','approved','running','ranked','qualified','guard_passed',
        'rejected','inconclusive','promoted','observed','cancelled','failed','cleaned'
    ));

ALTER TABLE optimization_candidate_runs
    DROP CONSTRAINT optimization_candidate_runs_state_check,
    ADD CONSTRAINT optimization_candidate_runs_state_check CHECK(state IN (
        'proposed','provisioning','ready','measuring','validating','ranked','qualified',
        'guarding','guard_passed','rejected','inconclusive','promoted','observed',
        'failed','cancelled','cleaned'
    ));
