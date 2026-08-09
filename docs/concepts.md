# Concepts

A **logical deployment** is the stable OpenAI-compatible name clients use. A **DeploymentSpec** is its desired model, immutable artifact identity, vLLM runtime configuration, compute mode, capacity bounds, and routing policy.

A **revision** is an immutable snapshot of that specification. Updates create a candidate beside the active revision. Release Guard evaluates persisted readiness and request measurements before an explicit promotion. Rejected or failed candidates are drained and deleted without changing the active revision.

A **replica intent** is InferCrane's durable claim that one external worker should exist. Its deterministic external key prevents duplicate provider resources during retry or restart. RunPod Serverless instead has one provider endpoint whose native worker fleet may scale to zero.

An **operation** is a leased, retryable lifecycle state machine with durable steps and events. CLI progress is a projection of those records. A **ModelArtifact** resolves a mutable Hugging Face reference to an immutable commit. Explanations use only persisted state, decisions, events, and measurements.
