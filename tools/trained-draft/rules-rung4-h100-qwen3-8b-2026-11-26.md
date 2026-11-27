# Rules, rung 4: rung 3's trained draft on one H100, 26 November 2026

Written before any server of this rung, and before rung 3's confirmation ended. It runs only after
rung 3's `confirm` and the coordinator's approval. Plan `h100-qwen3-8b-fp8` in `draftlib.PLANS`. A change
after a stage starts goes under "Changes after launch"; nothing above that heading changes. It
replaces the H100 plan of `rules-qwen3-8b-oasst1-2026-11-26.md`, which was written before the ladder
and never ran.

## Question

Does the draft trained in rung 3 keep its gain over no speculation, and over the published draft, on
an H100? Do its answers hold within 1 point by the product's rule for setups that change answers? And
how much of rung 3's greedy-text change (the drafts matched no speculation's text on 2 and 3 of 128
prompts) comes from serving in another process rather than from speculation?

## Fixed

| Item | Value |
|---|---|
| Model | `Qwen/Qwen3-8B` @ `b968826d9c46dd6066d109eabc6255188de91218` (Apache-2.0), FP8 weights quantized at load as in rung 3 (the draft was trained against them), thinking off |
| Published draft | `RedHatAI/Qwen3-8B-speculator.eagle3` @ `08610ffa01dd9f16731fe8f627b85905b6aa51c4` (Apache-2.0) |
| Trained draft | run `r3-3`'s draft, adopted without training (`trained_draft.py adopt --from r3-3 --run-id r4-1`); the smoke and the measure stage check its files against the artifact digest in its training report, and stop on a mismatch |
| Engine | vLLM 0.34.0, `vllm/vllm-openai@sha256:20fdb089...` |
| GPU | one Modal H100 (`H100!`), 8 cores, 64 GiB, $4.7101 an hour |
| Held-out prompts | `td-1`'s validation roots, as in rungs 2 and 3 |
| Task set | `fast-track-quality-v1.json` (sha256 `83c3df7b...`), the same 500 items as rung 3 (an even stride over GSM8K and MMLU), answer_line grading |

## Arms

`none` (the control), `published` and `trained` (EAGLE-3, 3 draft tokens), and `none_b`: a second
server without speculation, started on its own, measured like a candidate.

## Measurement

- **Same machine.** One window, four servers at once at `--gpu-memory-utilization 0.22` each
  (`--max-num-seqs 64`, `--max-model-len 4096`, prefix caching off); only one works at a time. Control
  first and last, every candidate run between two control runs, four runs of each. Loads 1 and 32 at
  once (8 and 96 requests a run), answers up to 256 tokens, streamed, the same prompts every run of a
  load.
- **Judge.** `optimizationcampaign.JudgeRecordedWindow` (paired log ratio, Bonferroni for 3
  candidates, the 0.75% machine allowance, a window void on a failed run or 15% control drift).
- **Acceptance.** vLLM's speculation counters per arm and load, and over the identical-answer pass.
- **Identical answers.** 128 held-out prompts at temperature 0, 256 tokens, on every arm and on `none`
  twice. `none_b` against `none` is what another process alone changes; `published` and `trained`
  against that are what speculation adds.
- **Task score, the product's rule for setups that change answers.** The 500 items on three servers
  of each of `none`, `published` and `trained`: the window's servers, then two passes on freshly started
  servers, 24 items at a time. The per-item differences of the three server pairs are pooled and read
  by `qualitycheck.RequireHeld`: worse when the 95% interval is below zero, better when above zero,
  held only when its lower end is above −1 point, otherwise "not shown to hold within 1 point",
  reported as **unknown**. `none_b` answers the set once, beside `none`: what a second server alone
  changes in the score.
- **Fresh machine.** As in rung 3: `confirm` runs the trained draft against `none` again in a new
  container on another GPU, timed runs at both loads.

## Pass bar (per load)

| Check | Pass |
|---|---|
| Validity | 0 failed requests, window not void |
| Faster than the current setup | the judge says `trained` is faster than `none` |
| Answers held | `RequireHeld` on three servers each says held or better for `trained` against `none` |
| Better than the published draft | `trained`'s gain over `none` exceeds `published`'s by more than both noise floors |
| Copy noise | reported, never gated: `none_b` against `none` in speed and in identical answers |

Rung 4 passes at a load when validity, faster and answers held pass there. Its contract record is
shown only with the fresh machine's confirmation; anything less is reported as measured. No claim
says "beats the vendor"; the published draft is a third party's, measured here on this traffic.

## Stages, caps and stops

| Stage | What | Wall clock (its own Modal timeout) | Hard cap | Estimate |
|---|---|---|---|---|
| `download` | weights to the volume, CPU only | 30 min | about $0.03 | $0.01 |
| `adopt` | `r3-3`'s draft, report and training record copied on the volume | laptop | $0 | $0 |
| `smoke` | the draft's digest checked; `trained`, `published` and `none_b` served for 8 requests each | 15 min | $1.18 | $0.40–0.80 |
| `measure` | one window, two loads, identical answers, task set on three servers each | 60 min | $4.71 | $2.50–3.80 |
| `confirm` | trained against `none` on another GPU, timed runs only | 15 min | $1.18 | $0.40–0.80 |
| **Total** | | | **$7.07** | **$3.30–5.40** |

Every stage is launched with `modal run --detach` and gated from the volume
(`trained_draft.py gate --run-id r4-1 --stage ...`) before the next. `train` refuses on this plan.
`confirm` refuses unless the judge reads `measure`'s bar as passing and the container's GPU differs.

## Changes after launch

- 26 Nov 14:25, **result before confirmation, run `r4-1` at fe516df2** (smoke $0.22, measure $1.66). The
  adopted draft matched `r3-3`'s report (`sha256:dd24310b...`). Measure ran on `GPU-e4e34f29-...` for 21
  minutes: one window, both loads, 25 runs each, 0 failed requests, nothing skipped, two more task
  passes on fresh servers. The judge, per load:

  | Check | Load 1 | Load 32 |
  |---|---|---|
  | Validity | pass | pass |
  | Faster than `none` | pass: +54.4% output tokens a second (227.8 to 352.8), floor 1.8% | pass: +13.2% (about 5,600 to 6,350), floor 2.3% |
  | Answers held (`RequireHeld`, 3 servers each, 1,500 pairs) | held within 1 point: 0.855 to 0.864, interval -0.2 to +2.1 points | the same task set, held |
  | Better than the published draft | yes: +54.4% against +41.3%, floors 1.8% and 1.9% | yes: +13.2% against +6.3%, floors 2.3% and 2.0% |
  | Copy noise (`none_b` against `none`) | +0.1%, no measurable difference | +0.4%, no measurable difference |

  Rung 4 passes at both loads; its records read "not shown" until `confirm` adds a fresh machine.
  Acceptance: trained 1.95 against published 1.79 tokens a verify step at 1 at once, 1.803 against
  1.659 on the 128 held-out prompts. **Greedy text**, the question rung 3 left open: `none` matches its
  own second pass on 38 of 128 prompts and `none_b` on 30 of 128, so on this H100 serving numerics
  alone change most 256-token greedy answers; the drafts match `none` on 6 (trained) and 8 (published)
  of 128, and `none_b` on 5 and 7: speculation changes more, on top. Quality held within 1 point all
  the same. `none_b`'s own task comparison, on one server each, reads "not shown to hold within 1
  point" (-1.3 to +2.9 points): one pair of servers cannot settle a 1-point question.
- 26 Nov 14:35, **confirmation, `r4-1` confirm** (about $0.49; rung 4 in all about $2.37). A new
  container on another H100 (`GPU-f6622378-...` against measure's `GPU-e4e34f29-...`) ran the trained
  draft against `none`, 9 runs at each load, 0 failed: +54.6% at 1 at once (230.8 to 357.4 output
  tokens a second, floor 1.5%) and +13.0% at 32 at once (about 5,740 to 6,474, floor 1.6%). Both
  contract records now read **shown**: +54.4% at 1 at once against a 1.8% floor, +13.0% at 32 at once
  against a 2.3% floor (each the smaller change against the larger floor), basis
  `same-machine+fresh-servers`, answers held within 1 point by the product's rule on three servers each,
  evidence `draft-acceptance` and `fresh-machine-confirmation`. Disclosed with them: the comparison with
  the published draft is from the first machine only; exact greedy text is not stable even without
  speculation on this GPU (30 of 128 between two servers without it, 6 of 128 for the trained draft);
  one model, one GPU type, oasst1 chat as a stand-in for a customer's traffic, in-distribution held-out
  prompts.
