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
python3 tools/poc-modal/poc_modal.py ask-planner --budget-usd 2           # one GPU: traffic, hold, the planner's plan; no campaign
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

### Traffic shape

`--shape` scales the trace's lengths so their medians land on one of the product's public
workload shapes (medians of the Chutes data): `interactive` (3,919 tokens in, 295 out),
`long-prefill` (7,897 in, 295 out) or `decode-heavy` (3,919 in, 1,377 out). The trace's
spread, arrival times and shared prefixes stay. The default, `mooncake`, is the trace as the
2026-11-20 run replayed it: inputs halved, outputs capped at 512. In the first 200 rows half
the answers are 35 tokens or shorter, so that shape says little about a setup that only
speeds up writing.

`--requests-at-once 4`, `8` or `32` (the default) sets the load every measured run of the
campaign holds, through the product's built-in benchmark profiles; there is none with 12. `16`
and `64` have no profile: the proposal names them with `--target-concurrency` on the throughput
profile, and the product measures a campaign at its proposal's target concurrency.

### Engine version and GPU type

`--engine vllm-0.34.0` serves every setup with vLLM 0.34.0, the newest release on 2026-11-23,
pinned by digest in `models.py`; the default stays 0.22.1, the version the product pins. On
0.34.0 the publisher's Rust frontend starts, so the environment is the publisher's
(`VLLM_USE_RUST_FRONTEND=1`). Its command as published still does not start on one H100
(2026-11-23 check): vLLM refuses its default of 1,024 requests at once against 904
recurrent-state cache blocks, so the current setup adds `--max-num-seqs 512` there too.
`--gpu H200` runs every setup on an H200 instead of an H100. The judge's allowance for
the difference between two machines was measured on H100s only. Check a new combination on
one GPU before paying for a campaign:

```bash
export INFERCRANE_POC_ENGINE=vllm-0.34.0   # and INFERCRANE_POC_GPU=H200 for an H200
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8 --setup speculation
uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/modal_gpu.py::main --action check --model Qwen/Qwen3.8-27B-FP8 --change="--kv-cache-dtype fp8"
```

`--change` checks the current setup with one change, written as vLLM arguments: a setup the
planner proposed, before a campaign rents servers for it. Only arguments a campaign's server may
be started with are accepted.

`--setup speculation` starts the publisher's command with its documented speculation option
and also reports the drafted and accepted tokens of one longer answer. The 2026-11-23 checks of
both setups on vLLM 0.34.0 and one H100, including the one that did not start, are in
`docs/testing/evidence/qwen38-27b-fp8-h100-vllm034-serving-check-2026-11-23.json`.

Every setup of a run serves on one NVIDIA driver, because a different driver alone can change
speed. By default that is the first host's; `--driver 620.70.06` names it instead. A host with
another driver is refused before the model loads and the server is started again elsewhere (four
tries a start). Name the driver most hosts carry: a run pinned to a rarer one spends its starts
looking for hosts.

### A small model on one L4

`--model Qwen/Qwen3-8B --gpu L4 --engine vllm-0.34.0` runs the cheap rung of the ladder: Qwen3-8B
in BF16 on one L4 (24 GB). The model's own 40,960-token context does not fit next to its weights
there, so the current setup adds `--max-model-len 8192`, as the fast-track map's L4 runs did
(`models.py` keys that setup to vLLM 0.34.0 on an L4 only). An L4 costs $0.000222 a second at list
price, $1.18 an hour with the container's CPU and memory. The L4 runs of the map used NVIDIA
driver 610.57.04; pass it with `--driver`. Stage the weights once with `download --model
Qwen/Qwen3-8B` (a CPU container; it does nothing when they are on the volume already).

The prompt corpus is counted with the served model's tokenizer:
`fast-track-v1-corpus-qwen3-8b.json` holds the same text as `fast-track-v1-corpus.json`, counted
with Qwen3-8B's (`corpus.py from-fast-track ... Qwen/Qwen3-8B`).

On an L4 a short request takes about 20 seconds, so a campaign on a map cell replays fewer of the
cell's requests in every measured run: `--cell-requests 64` at 16 at once and `192` at 64 hold each
run to four and three rounds. The rules of the first such pair, a band study at 16 and 64, are in
`rules-l4-band-study-2026-11-26.md`.

### An L40S when L4s are scarce

`--gpu L40S` serves Qwen3-8B on one L40S (48 GB): the publisher's command as it is, with the
model's own 40,960-token context. `models.py` keeps no price for it. Read Modal's list price for
the GPU alone on modal.com/pricing when you launch, and give it with `--gpu-usd-per-second`; a
run refuses to start without it, and the broker's estimate and the control plane's rate use it.
There is no serving check on an L40S yet: run `modal_gpu.py --action check` before a paid
campaign. An L40S is barely loaded at 16 requests at once, so its campaign runs at 64 (`--cell
short/64 --cell-requests 192`), after `ask-planner` shows the planner proposes something there.

### Smoke run and stand-in run

`--smoke` proves a paid campaign's path for a few dollars: 40 requests of at most 6,000 input
and 128 output tokens, spread over 150 seconds so the planner still sees a loaded server. The
judge is unchanged. It keeps its four paired rounds and measures a gain again on fresh
servers, because the product has no shorter mode and with one pair it cannot decide; each
round is short because the traffic is. A smoke run measures one setup next to the current
setup and its copy: with real text the speculation setup. Most of the cost is the servers'
cold start, about six minutes each, for the current setup, the three campaign servers, and
two fresh ones when a gain is measured again.

`--stand-in` runs the same path with CPU stand-in servers behind the same broker, tunnel and
proxy: no GPU, no model, cents. The stand-in answers with filler words at a scripted pace
and exports vLLM's metric families; started with a speculation option it writes two words a
step and counts drafted and accepted tokens. It proves that traffic, corpus, planner call,
campaign, measurement, counters and result fit together. Nothing it reports is a
measurement, and the result says so in its decision. The record of the first rehearsal that
passed, and the four defects earlier attempts found, is in
`docs/testing/evidence/poc-modal-stand-in-campaign-2026-11-23.json`.

`--stand-in local` runs the same stand-in servers on the laptop, one process per lease behind
the same broker and proxy, with no Modal app, no container and no spend. It rehearses
everything a run does on the laptop and in the cluster (gateway, traffic replay, capture,
planner call, campaign, AIPerf, judge, fresh servers), at full size if asked, and nothing of
the path through Modal: no tunnel, no cold start, no preemption. Use it to rehearse the exact
command of a paid campaign before it is launched, and when Modal is not available.

A stand-in holds its answer back for `stand_in_hold_seconds` (a field of the request body, at
most 120), to show what the path does with a request that sends nothing for a while. On
2026-11-23 neither Modal's tunnel (up to 90 s) nor the gateway and router (up to 120 s) cut one;
the record is in the evidence file above, under `later_checks`.

```bash
python3 tools/poc-modal/poc_modal.py run --stand-in --smoke --traffic real-text --engine vllm-0.34.0 --budget-usd 1
python3 tools/poc-modal/poc_modal.py run --stand-in local --traffic real-text --shape interactive --requests 100 --planner-setups 1 --corpus "$CORPUS" --engine vllm-0.34.0 --budget-usd 1
python3 tools/poc-modal/poc_modal.py run --smoke --traffic real-text --corpus "$CORPUS" --engine vllm-0.34.0 --budget-usd 5
```

### Asking the planner only

`ask-planner` does what `run` does up to the planner's answer, on one GPU, and stops: the
current setup, the replay, the recorded shape, the hold, the planner's plan with every
proposal's rule, priority and reason and whether this GPU could run it. No campaign runs and
nothing is measured. `--shape` and `--requests-at-once` (4, 8 or 32) say which traffic and which
load the planner is asked about; `--traffic`, `--corpus`, `--engine`, `--gpu` and `--stand-in`
work as for `run`. About ten minutes of one GPU.

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --traffic real-text --shape interactive --requests 100 --corpus "$CORPUS" --engine vllm-0.34.0 --budget-usd 2
```

With `--cell` the planner is asked about one cell of the fast-track map instead of the trace
replay: the product sees that cell's own requests, closed loop at the cell's load, sent as the
map's load generator sends them (one user message, a shared-prefix session's requests in order
from one worker, reasoning off unless the cell leaves it on). `--cells` names the map's cell file
and `--cell-prompts` the prompt file the cell names, which is checked against the cell file's
sha256; `--cells-sha256` is the hash the cell file itself must have. The cell sets the load, so `--shape`, `--traffic`, `--corpus` and `--requests-at-once`
play no part. 128 requests at once exists for this command, and for `run --cell` (below).
`--current-setup publisher-speculation` starts the check from the publisher's command with its
documented speculation option, to see what the planner names on top of it.

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --cell prefix/32 --cells "$CELLS" --cells-sha256 "$CELLS_SHA256" --cell-prompts "$PROMPTS_V2" --engine vllm-0.34.0 --budget-usd 2
```

A cell load records each failed request with the gateway's message (`traffic.failure_reasons`,
such as "503 Inference upstream is unavailable") and where it failed (`traffic.failures_by_hop`).
The harness reads every runtime proxy's `/proxy/stats` just before and just after the load:

- **on the network to the remote GPU:** the proxy answered 502 because the remote server did not
  answer (`failed_requests`, with the time, path and error of each);
- **answered by the remote side:** a 5xx from vLLM or the tunnel (`server_errors`); the GPU
  server's own count of its `/v1` answers, read from its access log when it stops, is
  `engine_answers` on the lease in the broker ledger. It reads the access lines of vLLM's
  Python front end (uvicorn) and of its Rust front end (`VLLM_USE_RUST_FRONTEND=1`).
  `access_lines` counts every access line it read, any path: with 0 lines, or `read` false,
  the server's answers are unknown, not free of errors;
- **before the runtime proxy:** the rest, failed in the gateway or the router.

`within_attempt_4_gate` says whether the load meets step 1's gate for run B (every failure on
the network to the remote GPU, at most four; see the step 1 rules, "Attempt 4"). The proxy drops
a connection that has been idle for 4 s, before vLLM's 5 s keep-alive: Go does not send a POST
again on a reused connection the server closed meanwhile, and through the tunnel that close
arrives late. A stand-in cell load fails on any failed request. With
`STAND_IN_DROP_CHAT_REQUESTS=300,700` the stand-ins close the connection on those chat requests
(counted from 1 per server) without answering; the rehearsal then passes only if each is placed
on the network to the remote GPU.

### A campaign on a map cell

`run --cell` does what `ask-planner --cell` does, then runs a campaign on that cell's traffic:

- The planner is asked while the cell's own requests run, as above.
- The shape of those requests (lengths and order, never text, up to 1,000, or `--cell-requests`)
  is then recorded, and the campaign measures every setup on that shape at the cell's load. A
  cell's `slice` is applied as the map applies it.
- The control plane composes each measured request's text from its prompt corpus, so `--corpus`
  names the file `up --corpus` mounted. `run` refuses to start when the control plane holds
  another one.
- The recorded shape does not keep the reasoning switch. The planner sees the cell as it is; the
  campaign's requests leave reasoning on, each answer capped at its recorded length.
- A cell at 128 requests at once is the only way to run a campaign at that load. At 128 the
  product warms each run up with 256 requests before it measures the shape's.

`--plain-arm`, with `--current-setup publisher-speculation`, also measures the publisher's plain
command. Then a result is compared with both of the publisher's documented setups. It takes one
of the two slots, so the campaign measures the current setup, the planner's first setup, the
plain command and the copy: four servers.

```bash
python3 tools/poc-modal/poc_modal.py run --cell short/128 --cells "$CELLS" --cells-sha256 "$CELLS_SHA256" --cell-prompts "$PROMPTS_V2" --corpus "$CORPUS" --current-setup publisher-speculation --plain-arm --planner-setups 1 --engine vllm-0.34.0 --budget-usd 30
```

The rules of the first such campaign are in `rules-step1-one-draft-at-128-2026-11-25.md`.

### Answers checked on a task set

`run --task-set FILE` checks answers on a task set rather than the built-in prompts. FILE is the
fast-track map's task set (`fast-track-quality-v1.json`, converted with
`tools/task-set/from_fast_track.py`) or a task eval set. The harness uploads it and names its
digest in the proposal (`optimize propose --eval-set`). The campaign's check then asks the current
setup and each setup every item, 32 at a time, and judges them with the product's paired rule:
the same 1,000 items and the same rule that decided FP8 at load in the fast-track map's step 2.
A 1,000-item set takes about 12 minutes of each server on an L4.

### A live endpoint after the campaign

`run --keep-endpoint MINUTES` (at most 60) serves the current setup again after the campaign, as
a new deploy with its own idempotency key. The run's traffic goes through the gateway the whole
time: the cell's own requests, or the trace's rows with the run's text at the campaign's load.
A dashboard pointed at the control plane then shows a live endpoint with real numbers next to
the finished campaign. None of it is measured or recorded in the campaign. The broker's cap
bounds it, and the endpoint is removed at the end.

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
  which is how it knows the model has its own prediction head. `run` asks it under load:
  the replay is a burst that a GPU finishes in about a minute, before the control plane (which
  reads the engine every 30 seconds) has measured it, and the planner takes the number of
  running requests from the engine's latest reading, so asked after the burst it plans for an
  idle server. After the replay, and after its shape is recorded, the harness therefore holds
  the server at the campaign's load: the same rows again in a closed loop at
  `--requests-at-once`, with other text of the same shapes, not recorded. The planner is asked
  once the control plane has windowed measurements and the hold has run 45 seconds, and the
  hold goes on until it has answered (limit 300 seconds). The result records when it was asked
  and how long the hold ran, and warns if the hold ended first. Each of the planner's setups is its
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
- the function timeout, the hard cap per container: 6 hours, or less with `--gpu-timeout-minutes`
  (30 to 360). The broker defines the app, so the run's value reaches every container.

At most 4 GPUs run at once (the node also advertises only 4 lease slots), and at most 6
start in one run, 12 with real text. The estimate uses Modal's list prices: H100 SXM5 $0.001097/s or H200
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
  and the requests that failed on the way to it (`/proxy/stats`), and the result summary reports
  the round trip next to the latencies. Every setup goes
  through the same path, so the comparisons are fair. The absolute latencies are not what
  the same server would show next to its users.
- The trace has no content. By default prompts are words generated from Mooncake's prefix
  block hashes, so prefix sharing is real and the text is not; no speculative setup is
  measured then. With `--traffic real-text` the text is the prompt corpus's, the result names
  the corpus by digest, licence and source, and input lengths follow the trace to the nearest
  whole passage.
- A stand-in run (`--stand-in`, `--stand-in local`) has no numbers that mean anything, and prints
  no cost per token.
