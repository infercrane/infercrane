# InferCrane

Open-source inference deployment control plane.

Stage 1 is a working proof of concept: register existing vLLM servers, attach them to a
persistent logical deployment, route OpenAI-compatible requests through a stable alias, and
report live health and metrics. Stage 2 includes an isolated SkyPilot/RunPod provisioner and
declarative deployment schema; its real-cloud path still requires credentialed acceptance.
Autoscaling is not implemented and is not claimed.

See [docs/architecture.md](docs/architecture.md) for the design and current scope.

Current guides:

- [Stage 1 existing-worker POC](docs/stage1-poc.md)
- [Stage 2 SkyPilot deployment](docs/stage2-skypilot.md)

## Development

Use `uv` for the fast GPU-free test loop:

```bash
uv sync --extra dev
uv run pytest
```

Or start the complete fake-worker development topology in containers:

```bash
docker compose up --build
```

This creates the logical alias `qwen-prod` and exposes it at `http://localhost:18000/v1`.
Set `INFERCRANE_DEV_PORT` to choose another host port.
The fake workers are for functional testing only and produce no performance evidence.
The image installs from `uv.lock`; Compose is the authoritative router/gateway integration
environment while `uv run pytest` is the shorter unit-test feedback loop.
