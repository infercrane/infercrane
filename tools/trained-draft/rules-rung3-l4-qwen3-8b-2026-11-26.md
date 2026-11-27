# Rules, rung 3: a trained draft for Qwen3-8B end to end on one L4, 26 November 2026 (about $3–4)

Written before any server of this rung ran. Plan `l4-qwen3-8b` in `draftlib.PLANS`. It runs only
if rung 2 passed. A change after a stage starts goes under "Changes after launch".

## Question

On a popular model and a cheap GPU, does a draft fine-tuned on 2,000 chat prompts of one kind serve
held-out chat of that kind faster than the current setup and than the published draft, at a low and
a high load, with answers that hold? Speculation can lose at high load; this rung says where.

## Fixed

| Item | Value |
|---|---|
| Model | `Qwen/Qwen3-8B` @ `b968826d9c46dd6066d109eabc6255188de91218` (Apache-2.0), thinking off, **FP8 weights** (`--quantization fp8`) on every server, training included |
| Why FP8 | BF16 weights (16.4 GB) and a draft leave about 1 GB of KV cache on a 24 GB L4; step 2 (25 Nov) measured FP8 weights among the best L4 setups for this model. The current setup here is "FP8 weights, no speculation" |
| Published draft | `RedHatAI/Qwen3-8B-speculator.eagle3` @ `08610ffa01dd9f16731fe8f627b85905b6aa51c4` (Apache-2.0), by repository and commit |
| Engine | vLLM 0.34.0, `vllm/vllm-openai@sha256:20fdb089...` |
| Trainer | speculators 0.9.0, from the published draft, hidden states from the FP8 server online (layers 2, 18, 33, 36) |
| GPU | one Modal L4, 4 cores, 64 GiB host memory (sleeping servers keep their weights there), $1.50 an hour |
| Training | 2,000 oasst1 train-split roots (seed 20261126), the model's own temperature-0 answers up to 512 tokens, 2 epochs, learning rate 1e-4 cosine, 2,048 tokens a step; stopped at 25 minutes, newest checkpoint kept |
| Held-out prompts | run `td-1`'s 512 validation-split roots |

## Measurement

- **One machine, one arm awake at a time.** Arms `none` (control: FP8 weights, no speculation),
  `published`, `trained`, each `--gpu-memory-utilization 0.72`, `--max-num-seqs 64`,
  `--max-model-len 4096`, `--no-enable-prefix-caching`, `--enable-sleep-mode`. Before each run the
  arm that runs is woken and the others sleep (vLLM sleep level 1: weights to host memory, cache
  discarded). Wake and sleep times are recorded. Nothing sends load to a sleeping server.
- **Order.** Control first and last; control, published, control, trained, control, ... four runs of
  each candidate. Loads **1** and **32** at once; 8 and 96 requests a run; answers up to 256 tokens;
  every run of a load sends the same held-out prompts.
- **Judge.** `optimizationcampaign.JudgeRecordedWindow` as in rung 2.
- **Answers.** 128 held-out prompts at temperature 0 on every arm and on the control twice; and 500
  task-set items (every second item of `fast-track-quality-v1.json`: 260 GSM8K, 240 MMLU) on every arm,
  `qualitycheck.PairedTask` against the control (95% interval, 3-point margin).

## Pass bar (per load)

| Check | Pass |
|---|---|
| Validity | 0 failed requests, window not void |
| Faster than the current setup | the judge says `trained` is faster than `none` |
| Quality | the task rule says no measurable difference or better for `trained` against `none` |
| Better than the published draft | `trained`'s gain over `none` exceeds `published`'s by more than both noise floors |

The technique passes rung 3 at a load when the first three hold there; "better than the published
draft" decides whether training was worth it there. Climb to an H100 only if the trained draft's gain
over the current setup at a load exceeds its noise floor and the published draft's gain by more than
both floors (the coordinator's rule: a gain above the copy's noise). A load where it is slower is
reported as a load where speculation should be off.

## Stages, caps and stops

| Stage | What | Wall clock (its own Modal timeout) | Hard cap | Estimate |
|---|---|---|---|---|
| `download` | weights to the volume, CPU only | 30 min | about $0.03 | $0.02 |
| `smoke` | 64 answers, 10 training steps, both drafts served for 8 requests | 25 min | $0.62 | $0.35–0.50 |
| `train` | 2,000 answers, up to 2 epochs (stops at 25 min) | 60 min | $1.50 | $1.00–1.30 |
| `measure` | one window, two loads, identical answers, 500 task items per arm | 70 min | $1.75 | $1.20–1.55 |
| **Total** | | | **$3.87** | **$2.55–3.35** |

`train` starts only when `smoke` passed; `measure` only when `train` passed.

## Changes after launch

- 26 Nov 10:10, before any rung-3 stage, from rung 2's measured timings (`r2-3` on an L4): a vLLM
  start with compilation and CUDA graphs took 136 to 162 seconds for Qwen3-1.7B; no speculation
  decoded 67.7 tokens a second at 1 at once; the measure stage took 40.5 minutes for two windows.
  For Qwen3-8B in FP8 (about 30 tokens a second at 1 at once on an L4, from the 25 a stream at 16
  at once the L4 screen of 25 Nov measured) one window comes to about 63 minutes: three starts
  (about 11), load 1 (about 16, plus switches and warm-up), load 32 (about 13), identical answers
  (about 5) and the 500-item task set on three arms (about 13). That is within 70 minutes only
  without error, and the task set, which the quality check reads, runs last. So the `measure`
  stage's wall clock rises from 70 to 100 minutes, hard cap $1.75 to **$2.50**, and `smoke`'s
  from 25 to 30 minutes, hard cap $0.62 to **$0.75** (answers, extraction, 10 steps and two
  sleep-mode starts come to about 20 minutes before its 4-minute stop). `train` stays at 60
  minutes; its loop shortens to fit. The rung's total cap is **$4.75**. Arms, loads, runs, prompts,
  the task set and the pass bar do not change.
- 26 Nov 11:00, before any rung-3 stage: a `confirm` stage, since no result is shown without a fresh
  machine's confirmation (the technique contract since f0c79f39). After `measure` passes, a new
  container runs the trained draft against `none` again at both loads, timed runs only (no
  identical-answer pass, no task set), and records the GPU it ran on. The judge attaches its sha256
  as `fresh-machine-confirmation` at a load only when it names this run's measure record, both
  stages named their GPUs and the GPUs differ, and the judge finds the trained draft faster there
  too; the result then counts the smaller of the two changes against the larger noise floor. Wall
  clock 35 minutes, hard cap **$0.88** (two starts, 9 runs at each load). The rung's total cap is
  **$5.63**. The pass bar does not change; a load without a confirmation stays "not shown".
- 26 Nov 10:25: run `r3-1` (launched 09:49 at 3b42a7ff) ended in its smoke at 09:55, not on a result:
  the coordinating laptop slept at 09:52 and `modal run` without `--detach` ends its app when the
  client goes. Its partial smoke record stays on the volume under `r3-1` and counts for nothing. The
  rung relaunches as `r3-2` at 257c4b1c, every stage with `modal run --detach`: proved on a CPU
  stand-in (`launch --stage selftest`, cents) by killing the client 3 seconds after the stage
  started (`dt-4`: the stage wrote progress for 40 more seconds and its record with no client), and
  by a stage that never ends on its own (`dt-5`: Modal's timeout ended it at about 120 seconds, no
  client). Detached, nothing local cancels a container that crashes while starting (Modal retries
  it), so the coordinator checks for the stage's first progress line on the volume after each launch.
  Gates between stages are read from the volume (`trained_draft.py gate --run-id r3-2 --stage ...`).
  `confirm` refuses unless the judge's reading of `measure` passes (valid, answers held by the task
  rule, faster at one load or more) and, in its container before measuring, unless `measure` named
  its GPU and this one differs.
- 26 Nov 10:40, smoke `r3-2` (dc2fb835, about $0.17): gate not passed, a harness fault. 64 answers
  were written; the extraction server did not start: at `--gpu-memory-utilization 0.40` of the L4
  (about 9.0 GiB) Qwen3-8B's FP8 weights took 9.06 GiB and its activations about 1.7, leaving
  -1.77 GiB for the cache ("No available memory for the cache blocks"). The plan's extraction share
  becomes **0.55** (about 12.4 GiB: weights, activations and about 1 GiB of cache) and its context
  **2,304** tokens (training rows are at most 2,048), leaving about 10 GiB for the trainer on the same
  GPU, which an 8B draft's 10 smoke steps will test. The smoke reruns as `r3-3`; caps and the pass
  bar do not change.
- 26 Nov 13:15, **result before confirmation, run `r3-3` at ffb2b04d** (smoke $0.51, train $1.17,
  measure $1.39; rung 3 so far about $3.34 with `r3-1` and `r3-2`). Smoke and train passed their gates;
  measure ran one window (`none`, `published`, `trained`) at 1 and 32 at once, 17 runs each, 0 failed
  requests, nothing skipped, 46 sleep/wake switches of at most 4.1 seconds, and recorded its GPU
  (`GPU-a96192a6-...`). The judge, per load:

  | Check | Load 1 | Load 32 |
  |---|---|---|
  | Validity | pass | pass |
  | Faster than `none` | pass: +59.7% output tokens a second (26.7 to 42.6), noise floor 1.7% | **fail**: -3.0% (680 to 660), floor 2.2% |
  | Quality (task rule against `none`, 500 items) | held: 0.856 to 0.860, -1.9 to +2.7 points within the 3-point margin | held (the same task set) |
  | Better than the published draft | yes: +59.7% against +47.8% (39.4 tokens a second), floors 1.7% and 1.7% | both slower than `none`; published -7.2% |

  The technique passes rung 3 at 1 at once; at 32 at once speculation should be off on this L4. The
  H100 rule as written (the trained draft's gain beats its floor and the published draft's gain by
  more than both floors) holds at 1 at once, pending the fresh machine's confirmation. Acceptance
  from vLLM's counters: trained 2.00 against published 1.84 tokens a verify step at 1 at once, 1.810
  against 1.666 on the 128 held-out prompts. **Finding**: `none` repeats its own greedy answers on
  128 of 128 prompts, while the drafts match it on 2 (trained) and 3 (published) of 128: in this
  setup speculation does not keep greedy text identical, though the task score held. This design
  does not separate the verify pass's numerics (FP8, several tokens a forward) from serving in another
  process; a `none` server started a second time would. The contract records read "not shown" at
  both loads until `confirm` (`evidence.fresh-machine-confirmation`); load 32 stays not shown anyway.
- 26 Nov 13:55, **confirmation, `r3-3` confirm** at ffb2b04d (Modal L4, about $0.58; rung 3 in all about
  $3.92 with `r3-1` and `r3-2`). A new container on another GPU (`GPU-e9e96498-...` against measure's
  `GPU-a96192a6-...`, driver 620.70.06 against 610.57.04) ran the trained draft against `none`, 9 runs
  at each load, 0 failed: **+59.4% at 1 at once** (26.6 to 42.3 output tokens a second, noise floor
  1.5%), and slower at 32 at once again. The judge attaches the confirm record's sha256 as
  `fresh-machine-confirmation` at load 1: that contract record now reads **shown**, at +59.4% (the
  smaller change) against a 1.7% floor (the larger), basis `same-machine+fresh-servers`, quality by
  this rung's written task rule (within 3 points). Load 32 stays "not shown" (slower on both
  machines). Disclosed with it: the drafts' greedy text differs from no speculation's (2 and 3 of 128
  identical), and under the product's rule for setups that change answers, adopted today
  (held only when the interval's lower end is above -1 point), this rung's one-server-each task
  interval (-1.9 to +2.7 points) would read "not shown to hold within 1 point". Rung 4 applies that rule.
