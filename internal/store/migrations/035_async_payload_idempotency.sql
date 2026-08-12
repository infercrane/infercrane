ALTER TABLE async_inference_jobs
    ADD COLUMN payload_digest TEXT NOT NULL DEFAULT '';
