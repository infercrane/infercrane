# Modal GPU proof

Derive a compact workload profile from the public 91 GB Chutes trace without
downloading the dataset to the local machine:

```bash
modal run --profile yasintoy tools/modal-kernel-poc/chutes_trace_modal.py \
  --row-groups 3 --rows-per-group 50000 --max-context-tokens 2048
```

Modal reads selected Parquet ranges directly from public S3 and returns only
content-free percentiles and three bounded benchmark shapes. Validate a saved
result before using it as a screening input:

```bash
infercrane optimize workload-profile --file chutes-profile.json
```

Run the same selected public-trace shapes against two pinned open-weight models
on remote L40S GPUs (weights are baked into Modal images, never the laptop):

```bash
modal run --profile yasintoy tools/modal-kernel-poc/qwen_aiperf_modal.py \
  --mode trace-two-models --profile-file chutes-profile.json \
  --profile-names public-interactive,public-decode-heavy
```

This does not convert public aggregate concurrency into a customer capacity
claim. Production qualification still requires a privacy-preserving customer
replay on the exact model/runtime/GPU tuple.

Run the inexpensive compatibility campaign first:

```bash
modal run tools/modal-kernel-poc/kernel_modal.py --gpu l40s
```

Compare against vLLM's production fused RMSNorm implementation:

```bash
modal run tools/modal-kernel-poc/kernel_modal.py --gpu vendor-l40s
```

Run the exact Hopper vendor comparison only after the L40S result passes:

```bash
modal run tools/modal-kernel-poc/kernel_modal.py --gpu vendor-h100
```

Screen the visible handwritten CUDA C++ implementation against both Triton and
the vLLM vendor operator:

```bash
modal run tools/modal-kernel-poc/kernel_modal.py --gpu cuda-h100
```

Run the paired eager-versus-compiled end-to-end campaign:

```bash
modal run tools/modal-kernel-poc/qwen_aiperf_modal.py --mode paired
```

The two public-shape modes compare balanced and throughput-oriented scheduling:

```bash
modal run tools/modal-kernel-poc/qwen_aiperf_modal.py --mode public-reference
modal run tools/modal-kernel-poc/qwen_aiperf_modal.py --mode throughput-reference
```

Runtime lanes can be run independently so a failed candidate does not discard
the qualified baseline:

```bash
modal run tools/modal-kernel-poc/qwen_aiperf_modal.py --mode vllm-default
modal run tools/modal-kernel-poc/qwen_aiperf_modal.py --mode sglang-stable
```

`sglang-stable` uses a CUDA 12.8 development image, installs `libnuma1`, and
disables SGLang 0.5.10's experimental piecewise CUDA graphs while retaining
ordinary CUDA graphs. The pinned Qwen3/H100 tuple did not complete AIPerf in the
bounded campaign and is retained as negative compatibility evidence, not a
qualified recipe.

The functions are single-container, have hard timeouts, and use a two-second
scale-down window. They return evidence to the caller and do not deploy a web
endpoint. Every H100 request uses `H100!` so Modal cannot transparently
substitute an H200 during a reproducibility-sensitive benchmark. The model is
downloaded into the image at an immutable revision before GPU allocation.
