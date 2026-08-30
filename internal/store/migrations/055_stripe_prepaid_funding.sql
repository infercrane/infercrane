CREATE TABLE managed_payment_events (
  provider TEXT NOT NULL,
  event_id TEXT NOT NULL,
  tenant_id TEXT REFERENCES tenants(id) ON DELETE RESTRICT,
  session_id TEXT,
  event_type TEXT NOT NULL,
  payload_digest TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('received','ignored','applied')),
  error_code TEXT,
  metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL,
  processed_at TIMESTAMPTZ,
  PRIMARY KEY(provider,event_id)
);
CREATE INDEX managed_payment_events_tenant_created_idx
  ON managed_payment_events(tenant_id,created_at DESC);

CREATE TABLE managed_payment_credits (
  provider TEXT NOT NULL,
  session_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  event_id TEXT NOT NULL,
  ledger_id TEXT NOT NULL UNIQUE REFERENCES managed_wallet_ledger(id) ON DELETE RESTRICT,
  currency TEXT NOT NULL CHECK(currency='USD'),
  amount_microusd BIGINT NOT NULL CHECK(amount_microusd>0),
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY(provider,session_id),
  FOREIGN KEY(provider,event_id) REFERENCES managed_payment_events(provider,event_id) ON DELETE RESTRICT
);
CREATE INDEX managed_payment_credits_tenant_created_idx
  ON managed_payment_credits(tenant_id,created_at DESC);
