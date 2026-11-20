# Optimization campaign on Modal H100s (development only)

Runs one real optimization campaign through the real InferCrane control plane, with every
setup served by vLLM on its own H100. It is the GPU sibling of the laptop PoC
(`tools/poc-local`): the same kind cluster, Postgres, and `infercrane serve` with the
Kubernetes provider, the same Mooncake trace replay through the gateway, Replay capture,
campaign, built-in answer check, and ranking. It stops at the proposal and activates nothing.

Modal is not an InferCrane provider and this is not a way to run one. The control plane is
unchanged. Each model server pod it starts is a small proxy
(`internal/testtools/modal-gpu-proxy`). The proxy asks a broker on the laptop
(`broker.py`) for one remote server running exactly the pod's vLLM arguments. The broker
holds the developer's existing Modal login and starts one Modal container per pod
(`modal_gpu.py`: pinned vLLM 0.22.1 image, `gpu="H100!"`, `retries=0`). The proxy forwards
`/health`, `/v1/*`, and `/metrics` to that container through a Modal tunnel. Pods never see
a Modal credential.

```text
infercrane serve (kind) --> pod: modal-gpu-proxy --tunnel--> Modal H100: vllm serve <pod's args>
                              |  lease, renew, release
                              `--> broker.py (laptop, Modal login, spend cap)
```

## Run

Requirements: everything `tools/poc-local` needs, plus `uv` and a Modal login
(`modal token new`, done by the developer; nothing here creates or prints a token).

```bash
python3 tools/poc-modal/poc_modal.py up        # cluster, registry, proxy image, control plane (no GPU)
python3 tools/poc-modal/poc_modal.py download  # stage pinned Qwen3-8B weights on a Modal volume (CPU only)
python3 tools/poc-modal/poc_modal.py dry       # one lease end to end on a CPU stand-in (no GPU)
python3 tools/poc-modal/poc_modal.py run --budget-usd 10
python3 tools/poc-modal/poc_modal.py run --budget-usd 10 --reuse-capture  # campaign only, on the last recorded traffic
python3 tools/poc-modal/poc_modal.py stop-gpus                            # stop the broker and every remote GPU
python3 tools/poc-modal/poc_modal.py down
```

`run` deploys the current setup (vLLM defaults) on one H100. It replays 200 Mooncake
FAST'25 rows through the gateway at their recorded arrival times (inputs scaled 1/2, capped at
30,000 tokens), reads the server's own metrics to pick two candidates, records the traffic
shape, and removes the baseline. Then it runs a campaign of the current setup plus those
two candidates. The candidates come from FP8 weights, an FP8 KV cache, and bigger prefill
batches. The control plane measures each candidate against the live current setup in turns
(AIPerf, three runs each, 32 concurrent streams), checks answers with its built-in signed
check, and ranks the setups. Outputs land in `.infercrane/poc-modal/output/`: the campaign
record, a result summary, and the broker's ledger. Offline tests for the broker's accounting and argument allowlist:
`python3 -m unittest discover -s tools/poc-modal -p 'test_*.py'`.

## Spend

The broker is the spend guard. Every remote container stops when the first of these happens:

- the pod is deleted (the proxy releases the lease),
- the pod stops renewing its lease for 3 minutes (pod, broker, or laptop gone),
- the broker's estimated spend reaches `--budget-usd` (no new GPU within 10 minutes of the cap),
- the broker exits: the Modal app is ephemeral and stops with it (`poc_modal.py stop-gpus`),
- the function timeout of 90 minutes, the hard cap per container.

At most 3 H100s run at once (the node also advertises only 3 lease slots), and at most 6
start in one run. The estimate uses Modal's list prices: H100 SXM5 $0.001097/s, plus the
container's 4 cores and 24 GiB. It counts from the moment a container is requested until the
container reports its start, and from 2 minutes before that start once it is known. Modal does
not bill the wait for a GPU.

That wait is the main cost risk. On 2026-11-20, a pinned H100 (`H100!`) took from seconds to
25 minutes to schedule. The campaign keeps the current setup and every measured candidate
serving until it ranks them, so they stay billed while a later candidate waits for its GPU.
Size `--budget-usd` for that. The broker cannot raise its cap while it runs, because restarting
it stops every GPU. Modal's billing report is the authority:

```bash
modal billing report --for today --show-resources
```

The control plane's own approval amount is its ceiling at the stated GPU rate over the
approval window, not money spent.

## What the numbers mean

- Throughput and cost per token are H100 numbers. Cost per 1M output tokens uses Modal's H100
  list rate, GPU only.
- Latency is measured by the control plane on the laptop, so every request crosses the
  internet to Modal and back. Each pod records its own health-check round trip to its GPU
  (`/proxy/stats`), and the result summary reports it next to the latencies. Every setup goes
  through the same path, so the comparisons are fair. The absolute latencies are not what
  the same server would show next to its users.
- The trace has no content. Prompts are words generated from Mooncake's prefix block hashes,
  so prefix sharing is real and the text is not.
