ALTER TABLE managed_wallets
  ADD COLUMN debt_microusd BIGINT NOT NULL DEFAULT 0 CHECK(debt_microusd>=0);

ALTER TABLE managed_wallet_ledger DROP CONSTRAINT managed_wallet_ledger_kind_check;
ALTER TABLE managed_wallet_ledger ADD CONSTRAINT managed_wallet_ledger_kind_check
  CHECK(kind IN ('credit','settlement','refund'));

ALTER TABLE managed_payment_credits
  ADD COLUMN payment_intent_id TEXT,
  ADD COLUMN refunded_microusd BIGINT NOT NULL DEFAULT 0 CHECK(refunded_microusd>=0),
  ADD CONSTRAINT managed_payment_credits_refund_ceiling
    CHECK(refunded_microusd<=amount_microusd);

CREATE UNIQUE INDEX managed_payment_credits_provider_payment_intent_idx
  ON managed_payment_credits(provider,payment_intent_id)
  WHERE payment_intent_id IS NOT NULL;
