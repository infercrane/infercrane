-- Qwen3.8 is a preview contract only. Catalog identity and a bounded context do
-- not authorize traffic; a current qualified publication, rate, and entitlement
-- remain mandatory before the customer API can mark this product available.
INSERT INTO model_api_products(
  id,display_name,publisher,description,protocol,tasks_json,
  capability_contract_json,input_modalities_json,output_modalities_json,
  context_window_tokens,availability,self_host_eligibility,created_at,updated_at
) VALUES (
  'qwen3.8-27b','Qwen3.8 27B','Qwen','Cataloged for text chat workloads.','openai',
  '["chat"]',
  '[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]',
  '["text"]','["text"]',18432,'catalog_only','unknown',NOW(),NOW()
)
ON CONFLICT(id) DO NOTHING;

-- Remove the unmeasured cost/latency wording from the durable GLM preview.
UPDATE model_api_products
SET description='Planned for text chat and coding workloads.', updated_at=NOW()
WHERE id='glm-5.3-flash' AND availability='catalog_only';
