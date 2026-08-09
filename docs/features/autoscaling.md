# Autoscaling

Elastic deployments use bounded queue-aware scaling between explicit minimum and maximum replicas. Persisted vLLM running/waiting signals, consecutive-interval thresholds, and cooldowns produce an auditable scaling decision. Scale-up creates durable replica intents. Scale-down withdraws the worker from the matching router generation, drains it, and then terminates it.

`infercrane explain scaling DEPLOYMENT` returns the latest action, old/new capacity, reason, signal snapshot, and timestamp. If evidence is insufficient or cooldown prevents a change, the persisted no-op decision explains why.

Serverless deployments delegate zero-to-N worker scheduling to the registered provider-native backend and retain one logical endpoint. InferCrane does not implement a GPU serverless scheduler. RunPod supplies the first qualified v0.1 implementation.
