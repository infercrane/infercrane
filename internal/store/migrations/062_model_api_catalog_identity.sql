-- DeepSeek documents `deepseek-v4-flash` as its callable API model name and
-- DeepSeek-V4-Flash-0731 as the current model version. The earlier public
-- InferCrane ID combined that revision with an invented `-fast` suffix. Keep
-- the old row as an unavailable tombstone so historical rates, publications,
-- entitlements, and usage retain referential integrity; new contracts must use
-- the stable supplier-neutral product ID below.
INSERT INTO model_api_products(
  id,display_name,publisher,description,protocol,tasks_json,
  capability_contract_json,input_modalities_json,output_modalities_json,
  availability,self_host_eligibility,created_at,updated_at
) VALUES (
  'deepseek-v4-flash','DeepSeek-V4-Flash','DeepSeek',
  'Planned for high-throughput workloads after route qualification.','openai',
  '["chat","coding","throughput"]',
  '[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]',
  '["text"]','["text"]','catalog_only','unknown',NOW(),NOW()
)
ON CONFLICT(id) DO NOTHING;

UPDATE model_api_products
SET availability='unavailable', updated_at=NOW()
WHERE id='deepseek-v4-flash-0731-fast';
