ALTER TABLE endpoint_admission_policies
    ADD COLUMN request_timeout_ms INTEGER NOT NULL DEFAULT 300000
    CHECK(request_timeout_ms BETWEEN 1000 AND 3600000);
