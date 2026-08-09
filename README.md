# InferCrane

Open-source inference deployment control plane.

The current implementation target is the Stage 1 proof of concept: register existing vLLM
servers, attach them to a persistent logical deployment, route OpenAI-compatible requests
through a stable alias, and report live health and metrics. Automatic GPU provisioning and
autoscaling are not yet claimed as implemented.

See [docs/architecture.md](docs/architecture.md) for the design and current scope.

## Development

Run the GPU-free test suite directly:

```bash
python3.12 -m venv .venv
.venv/bin/pip install -e '.[dev]'
.venv/bin/pytest
```

Or start the complete fake-worker development topology in containers:

```bash
docker compose up --build
```

This creates the logical alias `qwen-prod` and exposes it at `http://localhost:8080/v1`.
The fake workers are for functional testing only and produce no performance evidence.
