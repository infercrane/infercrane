# Optimization campaign on Modal GPUs (development only)

Runs one real optimization campaign through the real InferCrane control plane, with every
setup served by vLLM on its own GPU. It is the GPU sibling of the laptop PoC
(`tools/poc-local`): the same kind cluster, Postgres, and `infercrane serve` with the
Kubernetes provider, the same Mooncake trace replay through the gateway, Replay capture,
campaign, built-in answer check, and ranking. It stops at the proposal and activates nothing.

Modal is not an InferCrane provider and this is not a way to run one. The control plane is
unchanged. Each model server pod it starts is a small proxy
(`internal/testtools/modal-gpu-proxy`). The proxy asks a broker on the laptop
(`broker.py`) for one remote server running exactly the pod's vLLM arguments. The broker
holds the developer's existing Modal login and starts one Modal container per pod
(`modal_gpu.py`: the run's pinned vLLM image, a pinned GPU type, `retries=0`). `models.py` holds
the engine images, the GPU types, the models it may serve, their publishers' commands, and the
vLLM arguments it accepts. The proxy forwards
`/health`, `/v1/*`, and `/metrics` to that container through a Modal tunnel. Pods never see
a Modal credential.

```text
infercrane serve (kind) --> pod: modal-gpu-proxy --tunnel--> Modal GPU: vllm serve <pod's args>
                              |  lease, renew, release
                              `--> broker.py (laptop, Modal login, spend cap)
```

## Run

Requirements: everything `tools/poc-local` needs, plus `uv` and a Modal login
(`modal token new`, done by the developer; nothing here creates or prints a token).

```bash
python3 tools/poc-modal/poc_modal.py choose-model  # live: is the pinned model still the pick? (no GPU)
python3 tools/poc-modal/poc_modal.py up            # cluster, registry, proxy image, control plane (no GPU)
python3 tools/poc-modal/poc_modal.py download      # stage the pinned weights on a Modal volume (CPU only)
python3 tools/poc-modal/poc_modal.py dry           # one lease end to end on a CPU stand-in (no GPU)
python3 tools/poc-modal/poc_modal.py run --budget-usd 15
python3 tools/poc-modal/poc_modal.py run --budget-usd 15 --reuse-capture  # campaign only, on the last recorded traffic
python3 tools/poc-modal/poc_modal.py stop-gpus                            # stop the broker and every remote GPU
python3 tools/poc-modal/poc_modal.py down
```

`run` as above is the run of 2026-11-21: vLLM 0.22.1, one H100 per setup, prompts made of
generated words, no speculative setup. The next sections change one of those at a time.

### Real text, so speculation can be measured

The Mooncake trace has no content, and neither has what InferCrane records from traffic. A
setup that speculates is fast only when its drafted tokens are accepted, which depends on
what the model writes, so on generated words or on AIPerf's random text slices its speed
means nothing. With `--traffic real-text` every request is composed from a prompt corpus:
real passages with a question about one of them, or a standalone prompt
(`docs/features/optimization.mdx`, "Real text from a prompt corpus"). The trace still supplies
the arrival times, the lengths and which requests share a prefix.

```bash
CORPUS=/path/to/prompt-corpus.json   # default: testdata/stand-in-corpus.json, rehearsal only
python3 tools/poc-modal/poc_modal.py up --corpus "$CORPUS"                 # the control plane reads the corpus at start
python3 tools/poc-modal/poc_modal.py run --traffic real-text --corpus "$CORPUS" --engine vllm-0.34.0 --budget-usd 20
```

- **Two places use the corpus, and they must be the same file.** The harness composes the
  traffic it replays through the gateway (`corpus.py`), and the control plane composes the
  campaign's measured requests (`INFERCRANE_REPLAY_PROMPT_CORPUS_FILE`, set by `up --corpus`).
  `run` refuses to start when the control plane holds another file, and the result is "no
  decision" when a measured setup's evidence names another text than the run's corpus.
- **Answers end where the model ends them**, capped at the recorded length. The word replay
  forces every answer to its recorded length; forced text past the model's own ending is not
  what a drafter sees in production.
- **Speculation is measured, and its acceptance is recorded.** A planned setup with
  `--speculative-config` is no longer skipped. Only the model's own prediction head is
  accepted (`{"method":"mtp",...}` with 1 to 8 tokens); a draft model would need weights this
  harness does not stage. The result lists, per setup and window, the drafted and accepted
  tokens from the engine counters the product stored with every run
  (`vllm:spec_decode_num_draft_tokens`, `..._accepted_tokens`), and warns when a setup that
  speculates has none: throughput alone cannot tell a working drafter from one that loaded
  and was ignored.
- **The publisher's documented speculation option is always measured**
  (`models.py`, `publisher_speculation`: vLLM's recipe page says to add
  `--speculative-config '{"method":"mtp","num_speculative_tokens":3}'`). It is the baseline a
  result has to beat. When the planner proposes exactly that setup, it is the planner's; when
  it does not, the harness adds it and the record says so (`planner.documented_speculation`).
  `--current-setup publisher-speculation` makes it the current setup instead, so the planner
  has to find something better than it.
- **Up to two of the planner's setups** are measured with real text (`--planner-setups`), next
  to the current setup and its copy: four servers at once.

The corpus file states its licence, source and tokenizer, and the result records its digest.
`corpus.py from-fast-track` builds the corpus from the prompt set the fast-track map measures
with (`tools/fast-track-map`: OpenAssistant oasst1 prompts and CNN/DailyMail articles, both
Apache-2.0 on their dataset cards), so both runs use the same text. That corpus is not in this
repository; the one that is, `testdata/stand-in-corpus.json`, is a dozen short passages written
for it and is too small to measure with: a long request repeats passages.

### Engine version and GPU type

`--engine vllm-0.34.0` serves every setup with vLLM 0.34.0, the newest release on 2026-11-23,
pinned by digest in `models.py`; the default stays 0.22.1, the version the product pins. On
0.34.0 the current setup is the publisher's command and environment as published, with nothing
added. `--gpu H200` runs every setup on an H200 instead of an H100. The judge's allowance for
the difference between two machines was measured on H100s only. Check a new combination on
one GPU before paying for a campaign:

```bash
export INFERCRANE_POC_ENGINE=vllm-0.34.0   # and INFERCRANE_POC_GPU=H200 for an H200
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8 --setup speculation
```

`--setup speculation` starts the publisher's command with its documented speculation option
and also reports the drafted and accepted tokens of one longer answer.

### Smoke run and stand-in run

`--smoke` proves a paid campaign's path for a few dollars: 40 requests of at most 6,000 input
and 128 output tokens, spread over 150 seconds so the planner still sees a loaded server. The
judge is unchanged. It keeps its four paired rounds and measures a gain again on fresh
servers, because the product has no shorter mode and with one pair it cannot decide; each
round is short because the traffic is. Most of the cost is the servers' cold start.

`--stand-in` runs the same path with CPU stand-in servers behind the same broker, tunnel and
proxy: no GPU, no model, cents. The stand-in answers with filler words at a scripted pace
and exports vLLM's metric families; started with a speculation option it writes two words a
step and counts drafted and accepted tokens. It proves that traffic, corpus, planner call,
campaign, measurement, counters and result fit together. Nothing it reports is a
measurement, and the result says so in its decision.

```bash
python3 tools/poc-modal/poc_modal.py run --stand-in --smoke --traffic real-text --engine vllm-0.34.0 --budget-usd 1
python3 tools/poc-modal/poc_modal.py run --smoke --traffic real-text --corpus "$CORPUS" --engine vllm-0.34.0 --budget-usd 5
```

### What a run measures

- **The model** is the most downloaded FP8 checkpoint from Qwen, Z.ai and DeepSeek that fits
  one H100 (weights at most 60% of its memory) and has a published one-H100 vLLM command
  without speculative decoding. `choose-model` ranks them live from Hugging Face and vLLM's
  recipes. On 2026-11-21 that was `Qwen/Qwen3.8-27B-FP8` (6.5 million downloads, 30.9 GB),
  pinned in `models.py` at `c3a462cc` with its weights staged on the volume.
- **The current setup** is the publisher's own command: vLLM's recipe for this model on one
  H100 in FP8, the page its model card links, with its environment
  (`VLLM_USE_RUST_FRONTEND=1`). `run` reads the recipe again and records whether it changed
  since the pin; it always runs the pinned command. On one H100 with the pinned vLLM 0.22.1
  that command does not start as it is: 48 of the model's 64 layers keep a recurrent state,
  one cache block per running request, and vLLM had 818 blocks against its default of 1,024
  requests at once. The current setup adds `--max-num-seqs 512`, the change vLLM's own error
  asks for. The campaign runs 32 requests at once, so it changes nothing measured. A planned
  setup that asks for more than 818 is skipped for the same reason. The publisher's Rust HTTP
  frontend then exited at startup in the pinned image, so every setup uses vLLM's Python
  frontend, which the recipe also offers for images without the Rust one.
- **What to try** comes from the product's planner, as a customer gets it:
  `optimize propose --diagnosis-endpoint --allow-model-metadata-network` on the current
  setup's endpoint; the second flag lets the planner read the model's pinned `config.json`,
  which is how it knows the model has its own prediction head. `run` asks while
  the replayed traffic still runs, once the control plane's engine scrape has measured the
  loaded server and half the requests have finished. Each of the planner's setups is its
  starting setup with one change, and that change is applied to the publisher's command. The
  harness only skips what the remote GPU cannot measure honestly, and records why: more than
  one GPU, another engine environment, speculative decoding when the replayed traffic has no
  real text (a draft's acceptance would mean nothing), more requests at once than the model fits,
  or a change the current setup already makes. It measures the planner's first remaining setup. When the planner chooses
  nothing, no campaign runs.
- **A copy of the current setup**, on its own server with only `--seed 1` added, is measured
  like any candidate. It must not win. If it does, the judge crowned a difference between two
  machines, and the result says the run proves nothing.

`run` replays 200 Mooncake FAST'25 rows through the gateway at their recorded arrival times
(inputs scaled 1/2, capped at 30,000 tokens), records the traffic shape, and removes the
baseline. The campaign then measures each candidate against the live current setup in turns,
with AIPerf at 32 concurrent streams. It measures a gain again on fresh servers, checks answers
with the built-in signed check, and ranks the setups. Outputs land in
`.infercrane/poc-modal/output/`: the campaign record, a result summary (model choice, current
setup source, the planner's diagnosis and every setup it chose or skipped, the copy's verdict),
and the broker's ledger.

Before a paid run, check that the GPU side can serve the pinned model with its publisher's
command. One H100 for at most 18 minutes ($1.30); about 6 minutes and $0.45 when it serves:

```bash
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8
```

It starts the server, checks that a request without the key is refused, sends a chat request
and a tool call, and lists which vLLM metric families the control plane's engine scrape reads
are exported. The 2026-11-21 checks of `Qwen/Qwen3.8-27B-FP8`, including the two that did not
start, are in `docs/testing/evidence/qwen38-27b-fp8-h100-serving-check-2026-11-21.json`. Offline tests for the broker's accounting, the argument allowlist, and the
planner's setups: `python3 -m unittest discover -s tools/poc-modal -p 'test_*.py'`.

## Spend

The broker is the spend guard. Every remote container stops when the first of these happens:

- the pod is deleted (the proxy releases the lease),
- the pod stops renewing its lease for 3 minutes (pod, broker, or laptop gone),
- the broker's estimated spend reaches `--budget-usd` (no new GPU within 10 minutes of the cap),
- the broker exits: the Modal app is ephemeral and stops with it (`poc_modal.py stop-gpus`),
- the function timeout of 150 minutes, the hard cap per container.

At most 4 GPUs run at once (the node also advertises only 4 lease slots), and at most 6
start in one run, 10 with real text. The estimate uses Modal's list prices: H100 SXM5 $0.001097/s or H200
$0.001261/s, plus the container's 4 cores and 24 GiB. It counts from the moment a container is requested until the
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

## One driver per run

The setups of a run are compared across separate machines, and a different NVIDIA driver
alone can change speed: on 2026-11-20 the current setup ran on driver 610.57.04 and both
candidates on 620.70.06. So every container reads its GPU model, memory, NVIDIA driver, and
CUDA version before it starts vLLM and publishes them on its lease. The broker pins the
driver of the first host that reports one (the baseline, which runs alone first), or the one
given with `--driver`. A container on another driver stops within seconds, before loading the
model, and the broker starts the lease again on another host, at most 3 times. The refused
container's time stays in the estimate. Each lease's `gpu_facts` and `refused_hosts` are in
the broker's ledger.

## How much machines differ

`machine_variance.py` measures how much H100s differ when they serve the same setup, which the
campaign judge must allow for because the control and every candidate run on separate machines.
It serves Qwen3-8B and needs only its weights on the volume (`modal run
tools/poc-modal/modal_gpu.py::main --action download --model Qwen/Qwen3-8B`), not the cluster
or the broker:

```bash
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/machine_variance.py --machines 8 --budget-usd 5.25
```

Each machine is one Modal container (`H100!`, one call per container, `retries=0`) that serves the
current setup and benchmarks itself with `vllm bench serve` in the same container: fixed 4,800-token
inputs and 192-token outputs, 32 concurrent streams, a warmup and three runs of 256 requests. It
records the GPU, driver, VBIOS, CPU, region, and clocks and power during each run. The run refuses
to start when machines x the 9-minute container limit x the list rate is above `--budget-usd`, and
every call is cancelled at its wall-clock limit. One machine takes about 6 minutes, about $0.45.
The output in `.infercrane/poc-modal/output/` holds every run and the spread per metric: run to run,
between machines, and the 90% upper bound. The 2026-11-20 result is in
`docs/testing/evidence/h100-machine-variance-modal-2026-11-20.json`.

## What the numbers mean

- Throughput and cost per token are numbers for the run's GPU type. Cost per 1M output tokens
  uses Modal's list rate for that GPU, GPU only.
- Latency is measured by the control plane on the laptop, so every request crosses the
  internet to Modal and back. Each pod records its own health-check round trip to its GPU
  (`/proxy/stats`), and the result summary reports it next to the latencies. Every setup goes
  through the same path, so the comparisons are fair. The absolute latencies are not what
  the same server would show next to its users.
- The trace has no content. By default prompts are words generated from Mooncake's prefix
  block hashes, so prefix sharing is real and the text is not; no speculative setup is
  measured then. With `--traffic real-text` the text is the prompt corpus's, the result names
  the corpus by digest, licence and source, and input lengths follow the trace to the nearest
  whole passage.
- A stand-in run (`--stand-in`) has no numbers that mean anything, and prints no cost per token.
