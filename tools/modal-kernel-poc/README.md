# Modal GPU proof

Recorded results from these tools are in
[Public trace workload PoC](chutes-public-workload-poc.md) and
[Qwen3-0.6B H100 optimization PoC](qwen3-0.6b-h100-poc.md).

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

Check a reviewed pack's hidden evaluator on one H100, inside the pinned vLLM
0.22.1 image with `compute-sanitizer` added:

```bash
modal run tools/modal-kernel-poc/kernel_pack_modal.py --pack moe-experts --out moe-pack-h100.json
modal run tools/modal-kernel-poc/kernel_pack_modal.py --pack residual-rmsnorm --out rmsnorm-pack-h100.json
```

The pack's reference kernel must pass every stage and its wrong kernels must fail
at a correctness stage. It also times the reference kernel against vLLM's own
kernel, both replayed from CUDA graphs as vLLM replays decode. It loads no model
and generates no kernel. The function stops after 30 minutes, and the call is
cancelled after 35 minutes of wall clock. If `compute-sanitizer` cannot run a
known-good program on the host, the sanitizer stage is recorded as not run, never
as passed.

Serve Qwen3-30B-A3B-Instruct-2507 with and without the pack's kernel:

```bash
modal run tools/modal-kernel-poc/moe_e2e_modal.py --out moe-e2e-h100.json
```

It downloads the weights (61 GB) into the Modal volume
`infercrane-moe-e2e-weights` on a CPU-only function, then serves five arms on one
H100 from the pinned vLLM 0.22.1 image: vLLM's defaults, vLLM's Triton experts,
the pack's kernel for every call, the pack's kernel for calls up to 64 tokens,
and vLLM's defaults again. Each arm runs the same AIPerf load and InferCrane's
built-in answer check. A kernel arm whose marker file is missing did not run the
kernel. Results are written to the volume after every arm.

A function's timeout counts only time spent running, and Modal retries a
container that fails while starting without limit, even with `retries=0`. The
runner therefore cancels each call after its limit plus five minutes of wall
clock. Keep that guard in any new runner. Delete the volume when you no longer
need the weights.

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
