# Qwen3.8 workload optimization campaign

This directory is one campaign definition and Modal execution adapter, not the
InferCrane optimizer. The model-neutral planner, campaign state machine,
qualification policy, Release Guard, and accelerator-worker service live in
the Go control plane. This campaign supplies one pinned model, candidate set,
workload prior, and provider implementation to those contracts.

The campaign searches Qwen3.8 runtime recipes on Modal GPUs. Workload shapes come from the released
[Agentic Systems Open Data](https://data.agentic-system.org/) sources.
OpenRouter measurements are captured separately as an external production
target, never substituted for workload evidence.

Local validation does not rent a GPU:

```bash
python -m unittest discover -s tools/modal-openrouter-qwen38 -p 'test_*.py'
modal run tools/modal-openrouter-qwen38/modal_app.py --action preflight
```

Run the bounded first-stage screen:

```bash
modal run tools/modal-openrouter-qwen38/modal_app.py \
  --action screen \
  --workload public-interactive \
  --candidates all
```

Run the public decode-saturation diagnostic on the exact launch candidate. The
same command can compare exact H100, H200, and B200 hardware; the physical GPU
identity and current hourly cost are included in every receipt. The adapter
uses Modal's `H100!` request internally so a benchmark cannot be silently
upgraded and mislabeled as H100 evidence.

```bash
INFERCRANE_MODAL_GPU=H100 modal run tools/modal-openrouter-qwen38/modal_app.py \
  --action screen \
  --workload public-decode-saturation \
  --candidates sglang-0520-nextn-k4-bounded-graphs \
  --no-parallel

INFERCRANE_MODAL_GPU=H200 modal run tools/modal-openrouter-qwen38/modal_app.py \
  --action screen \
  --workload public-decode-saturation \
  --candidates sglang-0520-nextn-k4-bounded-graphs \
  --no-parallel

INFERCRANE_MODAL_GPU=B200 modal run tools/modal-openrouter-qwen38/modal_app.py \
  --action screen \
  --workload public-decode-saturation \
  --candidates sglang-0520-nextn-k4-bounded-graphs \
  --no-parallel
```

The saturation profile is a capacity diagnostic based on a released public
trace shape. It does not claim to reproduce OpenRouter's arrival distribution.
Promotion still requires control parity, target-provider qualification, and a
production canary measured through OpenRouter.

Every result remains tied to its model revision, runtime image, GPU, workload
digest, and measurement boundary. Modal loopback results are screening evidence;
only a production OpenRouter canary can establish OpenRouter routing position.

Adding another model should create another immutable campaign definition or
provider job. It must not add model-name checks to the optimizer or worker.

The reusable service boundary is `cmd/infercrane-accelerator-worker` plus
`internal/acceleratorlab`, `internal/optimizationcampaign`, and
`internal/optimizer`. A provider adapter receives typed `profile`, `generate`,
or `qualify` jobs and returns immutable evidence. This directory is the Modal
adapter and launch campaign for one exact Qwen3.8 tuple; deleting it would not
remove the optimization algorithm.

The current search includes SGLang and vLLM controls, native MTP, DFlash2,
cache/scheduler/graph variants, and measured kernel backends. It profiles the
winning control before custom-kernel work. A custom candidate is promoted only
if target-GPU correctness and full workload evidence beat the best existing
kernel; “the vendor kernel won” is a valid optimization result.
