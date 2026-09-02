CREATE TABLE managed_funding_intents (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  amount_microusd BIGINT NOT NULL CHECK(amount_microusd>0),
  currency TEXT NOT NULL CHECK(currency='USD'),
  status TEXT NOT NULL CHECK(status IN ('pending','completed')),
  checkout_session_id TEXT,
  checkout_url TEXT,
  checkout_expires_at TIMESTAMPTZ,
  lease_token TEXT,
  lease_expires_at TIMESTAMPTZ,
  attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt>=0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(provider,tenant_id,idempotency_key),
  CHECK((lease_token IS NULL) = (lease_expires_at IS NULL)),
  CHECK(
    (status='pending' AND checkout_session_id IS NULL AND checkout_url IS NULL AND checkout_expires_at IS NULL)
    OR
    (status='completed' AND checkout_session_id IS NOT NULL AND checkout_url IS NOT NULL AND checkout_expires_at IS NOT NULL AND lease_token IS NULL)
  )
);

CREATE UNIQUE INDEX managed_funding_intents_provider_session_idx
  ON managed_funding_intents(provider,checkout_session_id)
  WHERE checkout_session_id IS NOT NULL;

CREATE INDEX managed_funding_intents_tenant_created_idx
  ON managed_funding_intents(tenant_id,created_at DESC);

ALTER TABLE managed_payment_credits
  ADD COLUMN funding_intent_id TEXT REFERENCES managed_funding_intents(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX managed_payment_credits_funding_intent_idx
  ON managed_payment_credits(funding_intent_id)
  WHERE funding_intent_id IS NOT NULL;
