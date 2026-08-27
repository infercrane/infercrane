ALTER TABLE release_guard_policies
    ADD COLUMN quality_comparison_mode TEXT NOT NULL DEFAULT 'threshold'
        CHECK(quality_comparison_mode IN ('threshold','bootstrap')),
    ADD COLUMN quality_bootstrap_alpha DOUBLE PRECISION NOT NULL DEFAULT 0.05
        CHECK(quality_bootstrap_alpha > 0 AND quality_bootstrap_alpha < 0.5),
    ADD COLUMN quality_bootstrap_min_samples INTEGER NOT NULL DEFAULT 30
        CHECK(quality_bootstrap_min_samples BETWEEN 2 AND 100000),
    ADD COLUMN quality_bootstrap_seed BIGINT NOT NULL DEFAULT 20260827;

ALTER TABLE revision_quality_evidence
    ADD COLUMN distribution_json JSONB NOT NULL DEFAULT '{}'::jsonb;
