-- Tail latency and streaming stalls for each benchmark. NULL means the run
-- did not measure the value: p99 needs at least 100 samples, and buffered runs
-- stream no chunks.
ALTER TABLE benchmark_results
    ADD COLUMN ttft_p99_ms DOUBLE PRECISION,
    ADD COLUMN tpot_p99_ms DOUBLE PRECISION,
    ADD COLUMN latency_p99_ms DOUBLE PRECISION,
    ADD COLUMN stream_gap_p99_ms DOUBLE PRECISION,
    ADD COLUMN stream_gap_max_ms DOUBLE PRECISION;
