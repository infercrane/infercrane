# Durable Sessions

InferCrane v2 implements bounded logical session identity as Context Passport. It persists expiry and
best-effort binding/target hints, never conversation bodies. Reliability overrides affinity and it does not
claim durable KV state or integrate LMCache.

See [Context Passport and Burst Guard](/features/context-passport-burst).
