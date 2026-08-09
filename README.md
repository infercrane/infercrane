# InferCrane

Open-source inference deployment control plane.

The current implementation target is the Stage 1 proof of concept: register existing vLLM
servers, attach them to a persistent logical deployment, route OpenAI-compatible requests
through a stable alias, and report live health and metrics. Automatic GPU provisioning and
autoscaling are not yet claimed as implemented.

See [docs/architecture.md](docs/architecture.md) for the design and current scope.

