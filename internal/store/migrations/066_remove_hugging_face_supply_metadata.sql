ALTER TABLE model_api_supplier_offers
  DROP COLUMN IF EXISTS hf_repository_id,
  DROP COLUMN IF EXISTS hf_revision,
  DROP COLUMN IF EXISTS hf_license,
  DROP COLUMN IF EXISTS hf_source_url,
  DROP COLUMN IF EXISTS hf_observed_at;
