CREATE TABLE marketplace_request_receipts (
  channel TEXT NOT NULL,
  request_id TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  model TEXT NOT NULL,
  upstream_model TEXT NOT NULL,
  stream BOOLEAN NOT NULL,
  status_code INTEGER NOT NULL CHECK(status_code BETWEEN 100 AND 599),
  outcome TEXT NOT NULL,
  duration_ms BIGINT NOT NULL CHECK(duration_ms >= 0),
  time_to_first_token_ms BIGINT NOT NULL DEFAULT 0 CHECK(time_to_first_token_ms >= 0),
  prompt_tokens BIGINT NOT NULL DEFAULT 0 CHECK(prompt_tokens >= 0),
  completion_tokens BIGINT NOT NULL DEFAULT 0 CHECK(completion_tokens >= 0),
  total_tokens BIGINT NOT NULL DEFAULT 0 CHECK(total_tokens >= 0),
  cost_nano_usd BIGINT NOT NULL DEFAULT 0 CHECK(cost_nano_usd >= 0),
  finish_reason TEXT NOT NULL DEFAULT '',
  receipt_digest TEXT NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY(channel, request_id)
);

CREATE INDEX marketplace_request_receipts_recorded_idx
  ON marketplace_request_receipts(channel, recorded_at DESC);

CREATE INDEX marketplace_request_receipts_completed_idx
  ON marketplace_request_receipts(channel, request_id)
  WHERE status_code BETWEEN 200 AND 399 AND outcome='completed';
