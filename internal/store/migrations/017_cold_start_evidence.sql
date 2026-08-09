ALTER TABLE request_records
ADD COLUMN cold_start BOOLEAN,
ADD COLUMN provider_workers_at_arrival INTEGER CHECK(provider_workers_at_arrival >= 0),
ADD COLUMN provider_capacity_observed_at TIMESTAMPTZ;

CREATE INDEX request_records_cold_start_idx
ON request_records(deployment_id, cold_start, started_at DESC)
WHERE cold_start IS NOT NULL;
