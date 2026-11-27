# Qwen3-8B with FP8 on one L4: faster on fresh servers, refused on answers

On 26 November 2026, FP8 weights with an FP8 KV cache, both quantized by vLLM at load, served
`Qwen/Qwen3-8B` 62.1% to 158.7% more output tokens per second than vLLM's defaults on an NVIDIA L4,
in each of five traffic cells, with three fresh servers per setup. On a 1,000-item task set
(GSM8K and MMLU), the same three servers scored 1.10 points lower than the defaults' three: 85.07%
against 86.17%, 95% interval −2.20 to −0.002 points. FP8 weights alone were 48.2% to 110.8% faster
and scored 1.37 points lower (interval −2.32 to −0.42). The run's rules, committed before it
started, confirm a setup nowhere if it is worse on the task set, whatever its speed. Neither FP8
setup was confirmed.

This page is the receipt. The rules were committed before any server of the run started (commit
`f61466c8`). The harness that ran it (`tools/fast-track-map`) is on an unreleased branch, so this
page records what ran; it is not a reproduction script in this release. The run applied its rules
in that harness, not through the product's campaign.

- [Packed evidence](https://github.com/infercrane/infercrane/blob/main/docs/testing/evidence/qwen3-8b-fp8-l4-confirmation-2026-11-26.json): setups, machines, every
  server's numbers per cell, and every server's task-set result as right or wrong per item. No
  request rows, prompts or answer texts.
- [Summary](https://github.com/infercrane/infercrane/blob/main/docs/testing/evidence/qwen3-8b-fp8-l4-confirmation-2026-11-26-summary.json): the tables below, as
  the run's report script computed them from the nine servers' results.

Local paths are anonymised as `<evidence>` and `<corpus>`. Each JSON file names what was removed
for publication and the sha256 of the files it was made from. The packed evidence recomputes the
task-set comparison from the servers' answers with the run's own scoring code; it matches the
summary exactly.

## What ran

| Field | Value |
|---|---|
| Date | 2026-11-26, 03:57 to 06:54 CET |
| Model | `Qwen/Qwen3-8B` at revision `b968826d9c46dd6066d109eabc6255188de91218`, BF16 weights. Apache-2.0 by its model card (read 2026-11-27; the run's files do not record the licence). |
| Hardware | One NVIDIA L4 (23,034 MiB) per server on Modal, 4 CPU cores and 32 GiB per container. Nine servers: three fresh servers per setup, each in its own container on its own GPU, started in three batches that each hold one server of every setup. Eight servers ran on NVIDIA driver 610.57.04, which the rules ask for; the current setup's third server ran on 620.70.06 after two refused hosts, as the rules allow. Every server's GPU identifier, driver, region and cloud are in the evidence. |
| vLLM | 0.34.0, `vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484` |
| Cells | Short chat at 16 and 64 requests at once: first-turn messages written by people (`OpenAssistant/oasst1` and `oasst2`, Apache-2.0), answers up to 295 tokens. Long context at 4 and 16: `abisee/cnn_dailymail` articles (Apache-2.0) joined to 3,900 tokens with one fixed briefing task, answers up to 295 tokens. Long output at 16: one article of at most 900 tokens and a task asking for a long feature article, answer limits 826 to 1,928 tokens. Every request is one user message, temperature 0, reasoning off, streamed. No cell reads another cell's requests. Prompt sets: sha256 `c3c912dfb64e963fdae49f175bc2e400feef6a73a1210a70f5fe7d9f6d84c989` (v1) and `2f6b05635659520db775d85d218d0b2ded0104cecc634183e318fe87d4fac984` (v2). No text from them is included here. |
| Load | A fixed number of requests in flight per cell, each worker sending its next request when its last one ends. Warm-up 30 seconds (45 at `long/16`), then a measured window of 90 seconds for the short cells and 180 seconds for the long-context and long-output cells. The load generator ran in the server's container. |
| Task set | 1,000 items: 500 from `openai/gsm8k` (test split, revision `740312ad`, MIT) and 500 from `cais/mmlu` (test split, revision `c30699e8`, MIT), sha256 `83c3df7b82f8d8a3b9fa892f1fdcd81679f93646337c8b78d456a828101d72e4`. Every server answered all 1,000, 32 at once, temperature 0, reasoning off, answers up to 1,024 tokens. An answer is right when its last `Answer:` line gives the gold number or letter. |
| Baseline | vLLM 0.34.0's defaults. Every setup adds `--max-model-len 8192`: the model's own 40,960-token context needs more KV cache than one L4 holds next to BF16 weights, and vLLM refuses to start then. The longest request is under 5,000 tokens. |
| Rules | `rules-l4-qwen3-8b-confirm-2026-11-25.md`, sha256 `56ac29bfc2918847939adc1598ff48e0523e4cfb1fc667ae9cdf39e77316d6ca`, commit `f61466c8927b6feedcc5e088c35076da4880991b`. The full text is in the appendix. |

The prompt files were sized with another model's tokenizer (Qwen3.8-27B); Qwen3-8B's counts differ
a little. Every request's own counts were recorded.

**The third servers were measured twice.** At 05:58 CET the laptop that ran the harness crashed
(a kernel watchdog), and the run's app stopped with it, while the third server of each setup was
measuring: they had four of the five cells and no task set. The rules do not say what happens
then. The three third servers were started again from the same commit under a linked run ID
(`l4-8b-confirm-1-s3`, 06:18 to 06:54 CET), and only those records count. The cut servers' four
cells, as the run log printed them, are in the evidence beside the reruns. They are within 2.2%
of the reruns' (the log rounds to whole tokens per second).

### The setups

Each server ran `vllm serve` on the pinned weights, served as `Qwen/Qwen3-8B`.

| Setup | Arguments |
|---|---|
| 1. `l4-8b-defaults`: vLLM 0.34.0 defaults, the current setup | `--max-model-len 8192` |
| 2. `l4-8b-fp8-weights`: FP8 weights | `--quantization fp8 --max-model-len 8192` |
| 3. `l4-8b-fp8-both`: FP8 weights and an FP8 KV cache | `--quantization fp8 --kv-cache-dtype fp8 --max-model-len 8192` |

The two FP8 setups were chosen after a one-machine screen of five setups on 25 November put them
ahead of the defaults in every cell. That screen is not published; this run is the step that
could turn them into measured gains.

## Results

Output tokens per second inside the window: the median of three servers, with the slowest and
fastest in brackets. The change is median against median.

| Cell | 1. Defaults | 2. FP8 weights | 3. FP8 weights and FP8 KV cache | 2 against 1 | 3 against 1 | 3 against 2 |
|---|---:|---:|---:|---:|---:|---:|
| short, 16 at once | 242.7 (242.6–242.8) | 399.2 (397.7–400.2) | 405.0 (403.3–405.9) | +64.5% | +66.9% | +1.5% |
| short, 64 at once | 791.2 (788.9–791.6) | 1,172.3 (1,166.8–1,178.0) | 1,295.5 (1,285.8–1,310.6) | +48.2% | +63.7% | +10.5% |
| long, 4 at once | 45.7 (45.4–46.0) | 68.3 (67.7–68.4) | 74.1 (73.6–74.8) | +49.5% | +62.1% | +8.5% |
| long, 16 at once | 60.0 (59.6–60.0) | 126.5 (124.9–126.6) | 155.2 (153.8–158.4) | +110.8% | +158.7% | +22.7% |
| output, 16 at once | 199.8 (199.4–200.0) | 297.7 (297.0–298.6) | 337.4 (336.0–338.7) | +49.0% | +68.9% | +13.3% |

In every cell every comparison is clearly ahead: the faster setup's slowest server is faster than
the slower setup's fastest. No request failed in any cell. The current setup's servers each sent at
least 20 requests inside every window (28 to 263), the number the product's readiness check needs.

Counting only requests whose first token came within one second ("useful output"), the short cells
are the same. In the long cells most first tokens took longer than a second: with long context at
4 at once the defaults delivered none (all three servers 0.0), FP8 weights 68.3 (8.2–68.4) and FP8
with an FP8 KV cache 74.1 (73.6–74.8); at 16 at once no setup is separated (medians 0.0, 0.0 and
0.6, one server of setup 3 at 112.8). With long output at 16: 169.3, 286.1 and 328.1, clearly ahead
in both comparisons.

### Answers

The task set, scored item by item: each item's score on a side is the share of that side's three
servers that got it right, and the interval is 1.96 standard errors of the per-item differences.

| Setup | Servers right | Score | Difference against the defaults (95% interval) | Items the defaults' servers got right more often / the setup's did | Verdict by the rules |
|---|---|---:|---|---|---|
| 1. Defaults | 86.1%, 86.3%, 86.1% | 86.17% | | | |
| 2. FP8 weights | 85.1%, 84.6%, 84.7% | 84.80% | −1.37 points (−2.32 to −0.42) | 60 / 36 | worse |
| 3. FP8 weights and FP8 KV cache | 84.8%, 85.4%, 85.0% | 85.07% | −1.10 points (−2.20 to −0.002) | 64 / 39 | worse |

The rules' verdict "worse" means the whole interval lies below zero. For setup 3 it does so by
0.002 points at its upper end: the loss is small, and three servers per setup were needed to show
it at all. One pair of servers answering 1,000 items gives an interval about ±1.5 points wide
([semantic quality](/features/semantic-quality)), so a single check cannot show a loss of about a
point either way.

InferCrane's own rule for a setup that changes answers (another precision or checkpoint) reads the
same interval: one wholly below zero is "worse" and blocked, and a setup is held only when the lower
end lies above −1 point ([semantic quality](/features/semantic-quality)). By that rule both FP8
setups here would read "worse". The product did not run these servers; the run's own rules decided.

Exact text is not a usable test here. Two servers of the defaults gave word-for-word the same answer
on 316 to 322 of the 1,000 items; a server of the defaults and one of setup 3, on 22 to 29.

## What this does and doesn't show

It shows that on one NVIDIA L4 with vLLM 0.34.0, quantizing Qwen3-8B's weights to FP8 at load, with
or without an FP8 KV cache, made every measured cell clearly faster on three fresh servers per
setup, and that on this task set both setups scored about a point lower, so the run's rules refused
both.

It does not show:

- A published FP8 checkpoint. Weights here are quantized by vLLM at load. Two published FP8
  checkpoints were checked on answers in a separate run:
  [the FP8 checkpoint receipt](/testing/qwen3-8b-fp8-checkpoints-l4-2026-11-26).
- A result on another model, GPU, engine version or traffic.
- Answer quality on other tasks, or on answers longer than 1,024 tokens.
- That the product proposes or refuses these setups: the setups were chosen from a screen, not by
  the planner, and the product's campaign did not run them.

## Commands

The run and the rerun of the third servers, from `<worktree>` at commit `f61466c8`, with the
spend-limit flags left out:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-confirm uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-confirm-1
```

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-confirm uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py --action measure --only l4-8b-defaults:3,l4-8b-fp8-weights:3,l4-8b-fp8-both:3 --run-id l4-8b-confirm-1-s3
```

The rerun's three records were copied into the first run's folder, and the tables came from:

```bash
python3 tools/fast-track-map/confirm_report.py <evidence> --tasks <corpus>/fast-track-quality-v1.json
```

## Appendix: the rules as committed

The rules below are the committed file, with headings moved down two levels to fit this page, the
spend amounts and list prices removed, and two local paths replaced. The files they name
(`setups.py`, `confirm_report.py`, `quality.py`, the screen's rules) are on the same unreleased
branch.

### Rules: confirming step 2's finalists on fresh servers (Qwen3-8B on L4)

Written 2026-11-25 07:19 CET, before any server of this run. Internal; nothing here is
published. It follows `rules-l4-qwen3-8b-2026-11-25.md` and its screen (`l4-8b-screen-1`, 25 Nov
04:09 to 06:29 CET), whose result page is not published.

#### Say this first

- **This run confirms finalists chosen from a screen.** FP8 weights and FP8 weights with an FP8
  KV cache ("both") are measured because the screen put them ahead of the current setup in
  every cell. The choice was made after that screen was seen, so it is not a prediction, and
  this run cannot score the planner. The planner's score on this model stays the screen's: 0 of
  5.
- FP8 weights is also the planner's first proposal in four of the five cells (`short/16`,
  `short/64`, `long/4`, `output/16`). "Both" was the best setup the screen measured in all five.
- A screen measures each setup once, on one machine, in one 60-second window per cell. This run
  is the step that can turn a finalist into a measured gain, or show that it is not one.

#### What is asked

Plan `l4-qwen3-8b-confirm` in `setups.py`:

- **Setups:** `l4-8b-defaults` (vLLM 0.34.0's defaults, the current setup),
  `l4-8b-fp8-weights` (`--quantization fp8`) and `l4-8b-fp8-both` (`--quantization fp8
  --kv-cache-dtype fp8`). The commands are the screen's, byte for byte, with the forced
  `--max-model-len 8192` on all three alike. Model `Qwen/Qwen3-8B` at `b968826d`, BF16 weights
  from the run's volume; FP8 weights are quantized by vLLM at load.
- **Servers:** three fresh servers for each setup, nine in all, each on its own L4. They start
  in three batches, each holding one server of every setup, so no setup has all its servers at
  one time of day.
- **Cells:** the screen's five, on the same requests: short chat at 16 and 64 at once, long
  context (3,900-token prompts) at 4 and 16, long output at 16. Warm-up 30 seconds (45 at
  `long/16`). The window is 90 seconds for the two short cells and **180 seconds for `long/4`,
  `long/16` and `output/16`**. There the current setup sent 14, 18 and 13 requests in phase B's
  90-second windows and 9, 12 and 12 in the screen's 60, under the 20 the product's readiness
  check needs. At phase B's rate 180 seconds holds about 28, 36 and 26.
- **Task set:** the 1,000 items (GSM8K and MMLU), 32 at once, reasoning off, on every server.
- **Driver:** NVIDIA 610.57.04, the driver of phases B and C. A server that lands on a host with
  another driver is stopped before the model loads and started again, up to twice; after that
  it is taken on the host it got, and the report names it.

#### What counts

- **A setup's value in a cell** is each server's output tokens per second inside the window. A
  second column counts only the requests whose first token came within one second ("useful
  output", as in the screen).
- **Clearly ahead:** the setup's slowest server is faster than the other setup's fastest.
  **Clearly behind:** the mirror case. Anything else is **not separated**. A setup with fewer
  than three measured servers in a cell is **unknown** there. Compared: FP8 weights against the
  current setup, both against the current setup, and both against FP8 weights.
- **Task set:** each setup's three servers against the current setup's three, item by item
  (`quality.py`: the share of servers right on each item, the 95% interval of the paired
  difference, a 3-point margin). A setup that is worse, or not settled, on the task set is
  confirmed nowhere, whatever its speed.
- **Confirmed:** a setup is confirmed in a cell when it is clearly ahead of the current setup
  there and the task set does not hold it back. The gain reported is the median server against
  the median server, with each setup's slowest and fastest server beside it.
- **Ready:** the current setup is ready in a cell when each of its three servers sent 20 or more
  requests inside the window. Each cell says whether it was. A cell that is not ready changes no
  verdict; it is named, because the planner cannot be asked from it.
- **Cost per million output tokens:** [removed for publication: a list price divided by the median
  server's output in the cell].

`confirm_report.py` applies these rules and nothing else.

#### What it cannot show

- **The planner's skill.** The setups were chosen from a measurement, not predicted.
- **Other models, GPUs or engines.** One model, one GPU type, vLLM 0.34.0.
- **A published FP8 checkpoint.** FP8 weights here are quantized at load.
- **Exact token counts.** The prompt files were sized with the 27B model's tokenizer; every
  request's own counts are recorded.
- **Traffic other than these five cells,** and a server that sits idle part of the day.

#### Commands

From a clean worktree at the commit that holds this file, with `FAST_TRACK_PLAN=l4-qwen3-8b-confirm`
in front of each and `uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py`
as the command:

```bash
... --action dry --setups-csv l4-8b-defaults,l4-8b-fp8-weights,l4-8b-fp8-both --run-id l4-8b-confirm-dry
```

```bash
... --action measure --run-id l4-8b-confirm-1
```

After it, no GPU:

```bash
python3 tools/fast-track-map/confirm_report.py .infercrane/fast-track-map/l4-8b-confirm-1 --tasks <corpus>/fast-track-quality-v1.json
```

#### Money

[Removed for publication: the run's estimate, its spend cap and the list price they use.]
