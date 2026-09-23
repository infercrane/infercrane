# Qwen3.8 OpenRouter provider candidate

This package turns the workload-qualified SGLang 0.5.20 recipe into an
OpenRouter provider endpoint. The public edge accepts only the declared model,
streams without buffering, emits SSE heartbeats during quiet generation,
propagates cancellation, and returns HTTP 429 as soon as the twelve-request
admission boundary is full.

The catalog remains `is_ready: false` until the exact target deployment passes
the provider qualifier and a production soak. Modal H100/H200 measurements are
screening evidence; they do not qualify a RunPod deployment.

The current launch boundary is one H200 with twelve admitted requests. The
catalog intentionally declares only 32,768 input tokens, 2,048 output tokens,
and text input. Longer context, image/video inputs, and higher concurrency stay
disabled until they pass their own exact-target qualification. The initial
price is $0.10/M input and $2.20/M output: lower than most current providers,
but not an uneconomic attempt to match the absolute output-price floor.

Build from the repository root:

```bash
docker build -f deploy/openrouter/qwen38-sglang-0520/Dockerfile \
  -t ghcr.io/infercrane/qwen38-openrouter:sglang-0.5.20 .
```

Before starting a serving worker, start this same immutable image once with
`INFERCRANE_OPENROUTER_MODE=prepare-model` and the persistent volume mounted at
`/runpod-volume`. The image downloads the exact model revision, hashes every
file, writes the artifact manifest, and then exits. The serving worker requires
`INFERCRANE_OPENROUTER_API_KEY` as a provider secret and exposes the provider
API on port 8080. Configure the load balancer health probe for `/ping` on port
30001.

Qualify the public HTTPS endpoint from outside the provider network:

```bash
go run ./tools/openrouter-provider-qualifier \
  --url https://provider.example.com \
  --api-key-file ~/.config/infercrane/openrouter-provider-token \
  --requests 64 \
  --concurrency 16 \
  --output evidence/provider-qualification.json
```

Promotion requires all gates to pass, a 24-hour soak without mid-stream or
server errors, token/billing reconciliation, and cost measurement on the exact
GPU offer. Only then may the deployed catalog set `is_ready` to `true`.
