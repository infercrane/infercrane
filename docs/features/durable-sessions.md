# Durable Sessions

Durable Session identity remains deferred beyond the v1 release candidate because adding affinity
state before multi-provider lifecycle qualification would risk lifecycle correctness.

The future V0 contract will persist bounded logical session identity and a best-effort preferred-worker
hint, never conversation bodies by default. Reliability will override affinity, and it will not
claim durable KV state. InferCrane v1 does not integrate LMCache.
