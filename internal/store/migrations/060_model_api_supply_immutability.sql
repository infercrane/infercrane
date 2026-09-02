-- Supplier offers, qualification evidence, and compiled plans are immutable
-- evidence. Corrections must create a new version/record so a published route
-- can always be reconstructed and audited against the exact historical input.
CREATE FUNCTION infercrane_reject_model_api_supply_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'model API supply evidence is immutable; publish a new version';
END;
$$;

CREATE TRIGGER model_api_supplier_offers_immutable
  BEFORE UPDATE OR DELETE ON model_api_supplier_offers
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_supply_mutation();

CREATE TRIGGER model_api_supply_qualifications_immutable
  BEFORE UPDATE OR DELETE ON model_api_supply_qualifications
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_supply_mutation();

CREATE TRIGGER model_api_supply_plans_immutable
  BEFORE UPDATE OR DELETE ON model_api_supply_plans
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_supply_mutation();

CREATE TRIGGER model_api_supply_plan_candidates_immutable
  BEFORE UPDATE OR DELETE ON model_api_supply_plan_candidates
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_supply_mutation();
