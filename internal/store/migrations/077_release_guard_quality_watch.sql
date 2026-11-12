ALTER TABLE release_guard_policies
    ADD COLUMN quality_watch_regression_percent DOUBLE PRECISION,
    ADD CONSTRAINT release_guard_quality_watch_band CHECK (
        quality_watch_regression_percent IS NULL OR (
            max_quality_regression_percent IS NOT NULL AND
            quality_watch_regression_percent BETWEEN 0 AND max_quality_regression_percent
        )
    );
