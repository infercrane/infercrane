# Durable Sessions

Durable Session identity is deferred to v0.2. It is not part of the v0.1 release candidate because adding affinity state before real elastic and serverless qualification would risk lifecycle correctness.

The future V0 contract will persist bounded logical session identity and a best-effort preferred-worker hint, never conversation bodies by default. Reliability will override affinity, and it will not claim durable KV state. InferCrane v0.1 does not integrate LMCache.
