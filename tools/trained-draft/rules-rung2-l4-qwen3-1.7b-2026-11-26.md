# Rules, rung 2: train a draft for Qwen3-1.7B on one L4, 26 November 2026 (about $1–3)

Written before any server of this rung ran. Plan `l4-qwen3-1.7b` in `draftlib.PLANS`. A change after
a stage starts goes under "Changes after launch"; nothing above that heading changes.

## Question

Does an EAGLE-3 draft fine-tuned on 4,000 chat prompts of one kind accept more of the model's tokens,
and serve that kind of chat faster, than the published draft it started from, on prompts it never
saw? And how do both compare with the drafting methods that need no training (n-gram, suffix)?

## Fixed

| Item | Value |
|---|---|
| Model | `Qwen/Qwen3-1.7B` @ `70d244cc86ccca08cf5af4e1e306ecf908b1ad5e` (Apache-2.0), BF16, thinking off |
| Published draft | `AngelSlim/Qwen3-1.7B_eagle3` @ `94441b48acc5804677ae12259617c83323b543a9` (Apache-2.0 per its licence file, third-party parts excepted), served by repository and commit, the core's exact flag form |
| Engine | vLLM 0.34.0, `vllm/vllm-openai@sha256:20fdb089...` |
| Trainer | speculators 0.9.0 (Apache-2.0), from the published draft converted with `speculators convert --algorithm eagle3`; hidden states from vLLM online (`extract_hidden_states`, layers 2, 14, 25, 28) |
| GPU | one Modal L4, 4 cores, 32 GiB, $1.25 an hour |
| Training prompts | 4,000 oasst1 train-split tree roots drawn with seed 20261126 (run `td-1`'s prompt files) |
| Training data | the model's own answers at temperature 0, up to 512 tokens, as token ids |
| Training | 2 epochs, learning rate 1e-4 cosine with 5% warm-up, 4,096 tokens a step, 3 draft positions; stopped at 20 minutes if not done, newest checkpoint kept |
| Held-out prompts | the 512 validation-split tree roots of `td-1` |

## Arms

`none` (no speculation), `ngram` (`{"method":"ngram","num_speculative_tokens":3,"prompt_lookup_min":2,"prompt_lookup_max":5}`),
`suffix` (`{"method":"suffix","num_speculative_tokens":8}`, without adaptive verification: below 32
at once it trims too much on small models, and it needs attention backends an L4 does not default to),
`published` and `trained` (EAGLE-3, 3 draft tokens).

## Measurement

- **Same machine.** Two windows on the one L4, each its servers at once (`--gpu-memory-utilization 0.30`,
  `--max-num-seqs 64`, `--max-model-len 2048`, `--no-enable-prefix-caching`), only one working at a time:
  window 1 is `none` with `ngram` and `suffix`; window 2 is `none` with `published` and `trained`.
- **Order.** Control first and last: control, candidate A, control, candidate B, control, ... four runs of
  each candidate. Loads 1 and 8 at once; 8 and 32 requests a run; answers up to 256 tokens, streamed;
  every run of a load sends the same held-out prompts.
- **Judge.** `optimizationcampaign.JudgeRecordedWindow` (paired log ratio, Bonferroni for 2
  candidates, the 0.75% machine allowance, a window void on a failed run or 15% control drift).
- **Acceptance.** vLLM's speculation counters per arm: tokens a verify step writes, and per position
  the share accepted given the earlier ones, over each window and over the answer pass below.
- **Identical answers.** 128 held-out prompts at temperature 0, 256 tokens, on every arm, and on the
  control twice (the floor numerics give).

## Pass bar to climb to rung 3

| Check | Pass |
|---|---|
| Validity | 0 failed requests, no window void at either load |
| Training ran | the trainer left a checkpoint, and the trained draft served |
| Acceptance moved | the trained draft writes at least 0.05 more tokens a verify step than the published draft on the 128 held-out prompts |
| It serves faster than no draft | the judge says `trained` is faster than `none` at 1 at once |
| Answers held | the trained draft's share of identical greedy answers is within 10 points of the control's own second pass |

All five pass: climb to rung 3 (Qwen3-8B on one L4 through the judge). Reported beside them, never
gated: every arm against `none` at both loads, and whether the trained draft's gain over `none`
exceeds the published draft's by more than both noise floors (a direct trained-against-published
speed claim needs that). If "Acceptance moved" fails, the trained-draft track stops for chat and the
report says so.

## Stages, caps and stops

| Stage | What | Wall clock (its own Modal timeout) | Hard cap | Estimate |
|---|---|---|---|---|
| `download` | weights to the volume, CPU only | 30 min | about $0.03 | $0.01 |
| `smoke` | 64 answers, 10 training steps, both drafts and the model-free arms served for 8 requests | 20 min | $0.42 | $0.20–0.30 |
| `train` | 4,000 answers, up to 2 epochs (stops at 20 min) | 50 min | $1.04 | $0.60–0.85 |
| `measure` | two windows, two loads, identical-answer pass | 45 min | $0.94 | $0.50–0.75 |
| **Total** | | | **$2.40** (plus CPU download) | **$1.30–1.90** |

`train` starts only when `smoke` passed; `measure` only when `train` passed. Each stage stops its
optional work 4 minutes before its cap and writes its record.

## Changes after launch

- 26 Nov 06:45, smoke `r2-1` (launched 06:05, cut by Modal's 20-minute timeout, about $0.42): every
  step worked up to the last of four served arms. 64 answers; prepare-data; conversion; the hidden-state
  server and 10 real training steps with hidden states from vLLM online (the first run of that path on
  CUDA); n-gram, published and suffix served 8 of 8 requests each; the trained draft's server was cut
  while starting. Each vLLM start took 2 to 3 minutes (compile and CUDA graphs), six in a row. The
  `EngineDeadError` lines in the server logs are the stop the harness sends (SIGTERM). Found: the
  draft converted from AngelSlim behaves almost at random inside the trainer (validation loss 6.17,
  first-position accuracy 10.8% after 10 steps), far below its card. Changes: the smoke stage serves in
  eager mode (no compile or graphs) and stops serving 4 minutes before its cap; a `diagnose` stage
  (20 minutes, $0.42) serves the published draft as shipped, the converted draft with and without
  `norm_before_residual`, and the smoke's 10-step draft, and reads each one's acceptance from vLLM.
  Train and measure wait for its answer.
- 26 Nov 06:50, `diagnose` `r2-1` (launched 06:31, about $0.16): acceptance from vLLM's counters on
  16 held-out prompts, eager mode, 3 draft tokens. Published as shipped 1.656 (per position 0.38,
  0.46, 0.52); converted 1.656, the same counters; converted with `norm_before_residual` 1.073; the
  smoke's 10-step draft 1.125 (first position 0.12). Conversion is sound and `norm_before_residual`
  stays false. Training from the converted draft makes it worse as vLLM serves it, so the trainer
  fits something vLLM does not compute. Train and measure stay on hold until that is found.
- 26 Nov 06:55, before any further stage: the measure stage also writes `acceptance.json` (vLLM's
  speculation counter readings per arm and load, and the acceptance derived from them) and
  `traffic-shape.json` (held-out prompt ids per run, loads and answer caps, no text), and records the
  harness commit, the precision and its own command. The judge writes the technique contract's
  record of the trained draft at each load with the `draft-acceptance` evidence (the sha256 of
  `acceptance.json`) that the technique's manifest now requires. The pass bar is unchanged.
- 26 Nov 07:55: the trainer's fault is found (rung 1's rules, same date): the converted draft trained
  with zero token embeddings. `train_draft` now writes the verifier's embedding into the converted
  draft before training (`draft_embedding.py`), so smoke and train start from the draft vLLM
  serves. The smoke reruns at the fixed commit before train. Rung 3's published draft
  (`RedHatAI/Qwen3-8B-speculator.eagle3`) carries its own `embed_tokens` (read from its file
  header) and is not affected.
- 26 Nov 08:50, before `train` and `measure` start: two changes to the measure stage. A vLLM start
  with compilation and CUDA graphs took about 2.5 minutes in the first smoke, so one window (three
  starts, 17 timed runs at each load, warm-ups, and the identical-answer pass on four labels) comes
  to about 22 minutes. Two windows do not fit the 41 minutes left inside the 45-minute cap.
  (1) The drafts' window (`none` with `published` and `trained`) now runs first, the model-free one
  second, so a cut stage can only cost the window the pass bar does not read. (2) The measure stage's
  wall clock rises from 45 to 60 minutes, hard cap $0.94 to **$1.25**; the rung's total cap is
  **$2.71**. Nothing else changes: same arms, loads, runs, prompts and pass bar.
- 26 Nov 09:00, smoke `r2-2` at 79d4d508 (06:38 to 06:50 UTC, about $0.26): gate not passed, on
  time only. 64 answers, the verifier's embedding added to the converted draft, the extraction
  server and 10 training steps worked; n-gram (1.20 tokens a step) and published (1.63) served 8/8.
  Suffix and the trained draft were skipped at the 4-minute stop: each eager vLLM start took about
  82 seconds. The 10-step draft's own validation reads 38.8% first position (loss 3.47), against 10.8%
  (loss 6.17) before the fix, so vLLM's online hidden states agree with rung 1's. Changes: the smoke
  serves the trained draft first, then published, suffix, n-gram; its wall clock rises from 20 to 25
  minutes, hard cap $0.42 to **$0.52**; the rung's total cap is **$2.81**. The smoke reruns before train.
- 26 Nov 09:55, **result, run `r2-3` at 05356480** (Modal L4; smoke $0.32, train $0.61, measure $0.84,
  about $1.77). Smoke passed: trained 1.67, published 1.63, suffix 1.34, n-gram 1.20 tokens a
  verify step, 8/8 each. Train: 4,000 answers (1,711,301 answer tokens), the verifier's embedding
  added, 2 epochs in 849 seconds, report valid (artifact `sha256:91079f5c...`). Measure: both
  windows, both loads, 17 runs each, 0 failed requests, no void window, 40.5 minutes. The judge
  (`go run ./tools/trained-draft/judge`, d1b31def):

  | Check | Result |
  |---|---|
  | Validity | pass: 0 failed requests, no void or skipped measurement |
  | Training ran | pass: the report is valid and the trained draft served |
  | Acceptance moved | pass: trained 1.825 against published 1.518 tokens a verify step on the 128 held-out prompts, +0.308 (bar +0.05), vLLM's counters |
  | Faster than `none` at 1 at once | pass: +60.3% output tokens a second (67.7 to 108.5), noise floor 1.7% |
  | Answers held | pass as written, narrowly: no sign of changed answers (identical greedy answers 22 of 128 for the trained draft against 34 of 128 for `none`'s own second pass; bar within 10 points, 9.4 apart); **quality not shown** |

  **All five pass: climb to rung 3.** Reported beside them: at 8 at once the trained draft is
  +30.4% over `none` (floor 1.8%); the published draft is +37.3% at 1 and +9.8% at 8; the trained
  draft beats the published one by more than both floors at both loads (108.5 against 92.9 output
  tokens a second at 1, 570 against 480 at 8). n-gram is +27.2% at 1 and +4.0% at 8.
  Limits, written as found: (1) **Suffix is not measured**: every run sends the same prompts and
  suffix decoding keeps earlier answers across requests, so after its first run it replays them
  (3.9 tokens a step, 3.9 seconds a run against 19.2 for the first); its numbers are an artifact of
  this design, not speed on new traffic. The EAGLE drafts and n-gram keep no state across requests.
  (2) **The identical-answer check is weak**: at 256 tokens under batching, `none` repeats only 25 to
  34 of its own 128 greedy answers, and every drafting arm lands in the same range (published 21,
  n-gram 23, suffix 25), so it shows no sign of changed answers but cannot show unchanged quality.
  Rung 3's task set is the quality check. (3) Scope: Qwen3-1.7B in BF16 on one L4, vLLM 0.34.0, oasst1
  chat with thinking off, held-out prompts from the same distribution as training, answers up to
  256 tokens, 3 draft tokens.
  Evidence in `docs/testing/evidence/`: `trained-draft-rung2-r2-3-verdict-2026-11-26.json` (the
  judge's output), and the run's `acceptance.json` and `traffic-shape.json` as
  `trained-draft-rung2-r2-3-acceptance-2026-11-26.json` (sha256:e0e6f163...) and
  `trained-draft-rung2-r2-3-traffic-shape-2026-11-26.json` (sha256:9dcbbcf2...), the digests the
  contract records carry. Counts and prompt ids only; the answers stay on the volume.
- 26 Nov 10:40, correction to the 09:55 entry: the technique contract's records for `r2-3` read
  **not shown**. The 09:55 judgment (d1b31def) predates the rule that no result is shown without a
  fresh machine's confirmation (f0c79f39, applied here from the integration branch). Re-judged with
  that `ClaimOf`, identical to the integration head 1fb7e94b: both loads read "not shown", `unknown`
  `evidence.fresh-machine-confirmation`, everything else present (the draft-acceptance digest
  included). The answers line reads "no sign of changed answers; quality not shown": identical-answer
  agreement cannot carry a quality claim. The honest summary of rung 2: on Qwen3-1.7B on one L4, a
  draft trained on 4,000 in-distribution answers accepted 0.31 more tokens a verify step than the
  published draft and served 17% (load 1) and 19% (load 8) more output tokens a second than it; one
  machine; quality not yet shown. The evidence file `trained-draft-rung2-r2-3-verdict-2026-11-26.json`
  is the re-judged verdict.
- 26 Nov 11:00: the plan gains a `confirm` stage (20 minutes, hard cap $0.42; see rung 3's rules).
  It cannot confirm `r2-3`: that measure stage ran before stages recorded their GPU, so a fresh
  machine is not established and the judge would not attach it. `r2-3` stays one machine.
