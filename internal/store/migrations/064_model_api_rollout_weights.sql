-- A new immutable supply plan can gradually move a stable public model ID
-- between qualified targets. Existing plans remain first-candidate routes;
-- any newly weighted plan must total 10000 basis points at publication time.
ALTER TABLE model_api_supply_plan_candidates
  ADD COLUMN traffic_weight_bps INTEGER NOT NULL DEFAULT 0
  CHECK(traffic_weight_bps BETWEEN 0 AND 10000);
