-- Durable, content-free authority for reconciling Model API requests whose
-- supplier outcome was ambiguous after the transmission fence. The public
-- request identifier is retained so an operator can correlate a reservation
-- with an authoritative supplier ledger without storing request content.
ALTER TABLE model_api_usage_reservations
  ADD COLUMN request_id TEXT NOT NULL DEFAULT '' CHECK(length(request_id)<=255),
  ADD COLUMN reconciliation_attempts INTEGER NOT NULL DEFAULT 0 CHECK(reconciliation_attempts>=0),
  ADD COLUMN reconciliation_last_attempted_at TIMESTAMPTZ,
  ADD CONSTRAINT model_api_usage_reservations_customer_identity UNIQUE(customer_tenant_id,id);

CREATE TABLE model_api_usage_reconciliation_evidence (
  reservation_id TEXT PRIMARY KEY,
  customer_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  supplier TEXT NOT NULL CHECK(supplier<>''),
  supplier_request_id TEXT NOT NULL DEFAULT '' CHECK(length(supplier_request_id)<=256),
  outcome TEXT NOT NULL CHECK(outcome IN ('usage','no_charge')),
  authority TEXT NOT NULL CHECK(authority IN ('supplier_adapter','operator_verified')),
  evidence_reference TEXT NOT NULL CHECK(evidence_reference<>'' AND length(evidence_reference)<=512),
  recorded_by TEXT NOT NULL CHECK(recorded_by<>'' AND length(recorded_by)<=255),
  input_tokens BIGINT CHECK(input_tokens IS NULL OR input_tokens>=0),
  cached_input_tokens BIGINT CHECK(cached_input_tokens IS NULL OR cached_input_tokens>=0),
  output_tokens BIGINT CHECK(output_tokens IS NULL OR output_tokens>=0),
  observed_at TIMESTAMPTZ NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  UNIQUE(customer_tenant_id,reservation_id),
  FOREIGN KEY(customer_tenant_id,reservation_id)
    REFERENCES model_api_usage_reservations(customer_tenant_id,id) ON DELETE RESTRICT,
  CHECK(
    (outcome='usage' AND input_tokens IS NOT NULL AND output_tokens IS NOT NULL AND
      (cached_input_tokens IS NULL OR cached_input_tokens<=input_tokens)) OR
    (outcome='no_charge' AND input_tokens IS NULL AND cached_input_tokens IS NULL AND output_tokens IS NULL)
  )
);

CREATE INDEX model_api_usage_reconciliation_evidence_supplier_idx
  ON model_api_usage_reconciliation_evidence(supplier,recorded_at);
