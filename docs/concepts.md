# Concepts

An **endpoint** is the stable OpenAI-compatible model name clients use. A **logical model** names the
product-level workload, and an **environment** supplies its deployment context and policy. An
immutable **serving plan** binds the endpoint to one or more concrete backends. A **deployment** is a
lifecycle-managed serving realization; it is no longer required to be the application's identity.

A **DeploymentSpec** is a concrete deployment's desired model, immutable artifact identity, runtime
configuration, compute mode, capacity bounds, and replica routing policy.

A **revision** is an immutable snapshot of that specification. Updates create a candidate beside the active revision. Release Guard evaluates persisted readiness and request measurements before an explicit promotion. Rejected or failed candidates are drained and deleted without changing the active revision.

A **replica intent** is InferCrane's durable claim that one external worker should exist. Its deterministic external key prevents duplicate provider resources during retry or restart. In serverless mode, one provider endpoint may own a native worker fleet that scales to zero.

An **integration adapter** implements a narrow external capability such as replica provisioning, runtime inspection, serverless lifecycle, artifact resolution, or benchmark execution. A **qualification matrix** separately records which adapter combinations are supported by a release. Registration makes an implementation available to the process; it does not turn an untested combination into a product claim.

An **operation** is a leased, retryable lifecycle state machine with durable steps and events. CLI progress is a projection of those records. A **ModelArtifact** resolves a mutable Hugging Face reference to an immutable commit. Explanations use only persisted state, decisions, events, and measurements.
