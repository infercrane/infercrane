# Qwen3.8 OpenRouter provider candidate

This package turns the workload-qualified SGLang 0.5.20 recipe into an
OpenRouter provider endpoint. The public edge accepts only the declared model,
streams without buffering, emits SSE heartbeats during quiet generation,
propagates cancellation, and returns HTTP 429 before the model server queues
past its safe boundary. The edge starts at eight concurrent requests, can grow
to twelve after healthy saturated windows, and can fall as low as four when
measured p95-oriented TTFT or request reliability regresses. A separate
estimated-prefill budget prevents
large prompts from occupying every scheduler slot; that budget is released at
the first streamed token while the request remains counted against decode
concurrency.

The catalog remains `is_ready: false` until the exact target deployment passes
the provider qualifier and a production soak. Modal H100/H200 measurements are
screening evidence; they do not qualify a RunPod deployment.

The current launch boundary is one H200 with adaptive admission from four to
twelve decode requests, starting at eight. The three-second p95 TTFT objective
is intended to settle around the measured twelve-request frontier and reject
load before the runtime builds an unbounded queue. The catalog intentionally
declares only 32,768 input tokens, 2,048 output tokens, and text input. Longer
context, image/video inputs, and higher concurrency stay disabled until they
pass their own exact-target qualification. The base price is $0.10/M input and
$2.20/M output. The staged catalog applies a 19% launch discount, so users
initially see $0.081/M input and $1.782/M output. Keeping the base price explicit
allows the discount to be reduced after measured OpenRouter traffic establishes
utilization, reliability, and contribution margin.

Portable JIT artifacts are restored from an immutable, SHA-256 verified release
scoped to the exact runtime image, model revision, CUDA version, and GPU compute
capability. Triton, DeepGEMM, FlashInfer, Inductor, CUDA, TileLang, TVM FFI, and
CuTe caches are staged onto local disk before the runtime starts. CUDA graph
capture remains a per-process startup operation and is not misrepresented as a
portable cache.

The image also carries a fail-closed source patch for the three GDN gate paths
where SGLang 0.5.20 narrows an FP32 sigmoid through BF16 before updating the
persistent recurrent state. Every input file must match the exact v0.5.20 hash;
the image build records before/after hashes and the two upstream review commits.
This patch is a correctness candidate until the long-state, runtime-parity, and
full workload gates pass on the target GPU. It is not promoted from the source
change alone.

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

Production workers should additionally set:

```text
INFERCRANE_COMPILE_CACHE_RELEASE=<immutable release name>
INFERCRANE_COMPILE_CACHE_MANIFEST_SHA256=<manifest digest>
INFERCRANE_RUNTIME_IMAGE_DIGEST=sha256:<final provider image digest>
INFERCRANE_QUALIFIED_OUTPUT_TPS=<exact-host SLO-qualified capacity>
INFERCRANE_GPU_HOURLY_COST_USD=<all-in hourly host cost>
INFERCRANE_OPENROUTER_METRICS_KEY_FILE=<owner-only credential path>
INFERCRANE_MAX_PREFILL_TOKENS_IN_FLIGHT=65536
```

The protected `/metrics` endpoint reports current admission capacity, fail-fast
rejections, successful and SLO-qualified token totals, productive utilization,
prefill pressure/rejections, implied revenue, GPU cost, and gross margin.
Productive utilization deliberately excludes failed responses and successful
streams that miss the configured TTFT objective. Production dashboards should
use a rolling `rate()` over the token counters; the process-lifetime ratio is a
diagnostic that includes startup and recovery time.

Qualify the public HTTPS endpoint from outside the provider network:

```bash
go run ./tools/openrouter-provider-qualifier \
  --url https://provider.example.com \
  --api-key-file ~/.config/infercrane/openrouter-provider-token \
  --requests 64 \
  --concurrency 12 \
  --output evidence/provider-qualification.json
```

Promotion requires all gates to pass, a 24-hour soak without mid-stream or
server errors, token/billing reconciliation, and cost measurement on the exact
GPU offer. Only then may the deployed catalog set `is_ready` to `true`.
