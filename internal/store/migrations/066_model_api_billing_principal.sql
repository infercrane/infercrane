-- Pin the supplier-side billing account separately from a rotatable API token.
-- Existing non-Hugging-Face bindings remain valid with an empty value; routed
-- Hugging Face publication fails closed until a new binding names its payer.
ALTER TABLE model_api_target_bindings
  ADD COLUMN billing_principal TEXT NOT NULL DEFAULT ''
    CHECK(billing_principal='' OR billing_principal ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$');
