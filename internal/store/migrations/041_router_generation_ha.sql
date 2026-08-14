ALTER TABLE router_generations
  DROP CONSTRAINT IF EXISTS router_generations_deployment_id_generation_key;

ALTER TABLE router_generations
  ADD CONSTRAINT router_generations_deployment_owner_generation_key
  UNIQUE(deployment_id, owner_id, generation);
