# Rules, rung 1: acceptance offline on Qwen3-1.7B, 26 November 2026 ($0)

Written before the run. The ladder: this rung ($0, this laptop) → rung 2 (train on one L4, $1–3) →
rung 3 (Qwen3-8B on one L4 through the judge, $3–6) → H100 only if rung 3 shows a gain above the
copy's noise. The earlier H100 plan (`rules-qwen3-8b-oasst1-2026-11-26.md`) is withdrawn as a first
step and kept for rung 4.

## Question

On held-out chat of one kind, how many tokens does a verify step write with each drafting method,
and does a draft fine-tuned on that kind of chat write more than the published draft it starts from?
Acceptance length predicts speculation's gain and needs no GPU to estimate. Speed is not measured
here (rung 2).

## Fixed

| Item | Value |
|---|---|
| Model | `Qwen/Qwen3-1.7B` @ `70d244cc86ccca08cf5af4e1e306ecf908b1ad5e` (Apache-2.0), 28 layers, thinking off |
| Why this model | the smallest Qwen3 with a widely used public EAGLE-3 draft, and speculators lists Qwen3 dense verifiers. Checked on Hugging Face 26 Nov: 0.6B has two community drafts with about 300 downloads; 1.7B has `AngelSlim/Qwen3-1.7B_eagle3` with 67,755 |
| Published draft | `AngelSlim/Qwen3-1.7B_eagle3` @ `94441b48acc5804677ae12259617c83323b543a9` (Apache-2.0 per its licence file, third-party parts excepted), converted with `speculators convert --algorithm eagle3` |
| Its reported acceptance | 2.13 to 2.20 tokens a step at 2 draft tokens on vLLM 0.11.2, H20, batch 1 (**project-reported**); a conversion far below this is a conversion fault, not a result |
| Traffic | oasst1 tree roots at `fdf72ae0...` (Apache-2.0): 512 training prompts drawn with seed 20261126 from the train split, the first 128 held-out prompts of the validation split (the same split as `td-1`) |
| Answers | the model's own greedy answers (temperature 0), up to 320 tokens, HF transformers on this Mac's GPU (MPS), bfloat16 |
| Hidden states | layers 2, 14, 25 and the final pre-norm state (28), written by transformers in speculators' file format (`hs_{i}.safetensors`); vLLM's convention is the input to that layer |
| Trainer | speculators 0.9.0 on CPU, `--draft-attn-impl sdpa`, 3 draft positions (`ttt_steps` 3) |

## Arms (all at 3 draft tokens unless said)

1. **No draft:** 1.00 token a step by definition.
2. **N-gram (prompt lookup):** vLLM's rule (`prompt_lookup_min` 2, `prompt_lookup_max` 5): the longest n-gram match of the last n tokens in the request's own earlier tokens proposes the tokens after it. Simulated exactly against the greedy answer.
3. **Suffix decoding:** a simulation of vLLM's `method: suffix`: longest suffix match (depth 24) in the request's own tokens and in earlier answers of the run, the most frequent continuation, up to 3 and up to 8 tokens. An **approximation** of vLLM's implementation; labelled so.
4. **Published draft:** speculators' validation on the held-out rows with learning rate 0 (the draft unchanged): per-position conditional accuracy along the model's greedy path.
5. **Fine-tuned draft:** the published draft fine-tuned on the 512 training answers (2 epochs, learning rate 1e-4, cosine), scored the same way on the same held-out rows.

Tokens a step = 1 + a0 + a0·a1 + a0·a1·a2 from conditional per-position acceptance a0..a2; for the
simulated arms, generated tokens divided by verify steps.

## Pass bar to climb to rung 2

| Check | Pass |
|---|---|
| Conversion sanity | the published draft writes at least 1.9 tokens a step at 2 draft tokens on held-out rows (its card: 2.13 to 2.20 on other traffic) |
| Drafts are the lever on this traffic | the published draft's tokens a step at 3 draft tokens beat the best zero-training arm by at least 0.2 |
| Fine-tuning moves the right way | the fine-tuned draft's tokens a step are not below the published draft's by more than 0.02 |

All three pass: climb to rung 2. The first fails: fix the conversion, then rerun. The second
fails: report that a model-free method is as good on chat and stop the trained-draft track for chat.
The third fails: report it; rung 2 trains on 20 times the data and may differ, so climb only if
the coordinator agrees.

A gain at this rung is an **offline indication** on 512 training examples, never a speed claim.

## Changes after launch

- 26 Nov 06:10, before any result: the laptop run (started 05:27) died in the Mac's kernel panic at
  05:58 with no output; the coordinator then allowed one heavy local job at a time across sessions.
  Rung 1 moves, unchanged, to one Modal L4 (`gpu_rung1` in `trained_draft.py`, plan
  `l4-qwen3-1.7b`, 30-minute timeout, hard cap $0.62). Answers and hidden states come from
  transformers on CUDA instead of the Mac's GPU; the trainer keeps `--draft-attn-impl sdpa` and
  `--loss-implementation eager`. Validation numbers are read from the trainer's
  `val_metrics.json` files rather than its log.
- 26 Nov 06:30, after run `r2-1` (Modal L4, 06:05 to 06:22, about $0.34): **not a result.** The
  pass bar read conversion "fail" (the published draft at 1.07 tokens a step against its card's 2.1
  to 2.2), lever "fail", fine-tuning "pass". Cause, from the trainer's log: `prepare-data` shuffles
  rows (seed 0), but the hidden-state files were numbered in this run's order, so the trainer found
  the wrong file for most rows ("Loaded token ids ... don't match"), skipped them, and averaged its
  validation over mostly empty batches. The model-free replays do not use the trainer and stand:
  n-gram 1.185, suffix (approximate) 1.289 at 3 and 1.300 at 8 tokens a step over 36,503 held-out
  answer tokens. Fix: the prepared rows are put back in this run's order (training rows, then the
  held-out rows) and every hidden-state file is written from the row it belongs to; a rerun reuses
  the run's greedy answers. The pass bar is unchanged. The summary's `passed` now means every check
  passed (it meant "a pass bar was written").
- 26 Nov 06:50, after the rerun (Modal L4, 06:31 to 06:36, about $0.10) and the `diagnose` stage
  (about $0.16): the row order holds (no "don't match" lines), and the pass bar reads conversion
  "fail", lever "fail", fine-tuning "pass" again. The trainer's numbers are not the draft's.
  Served by vLLM on 16 held-out prompts, the published draft as shipped and the same draft after
  `speculators convert` write exactly 1.656 tokens a verify step at 3 draft tokens (first position
  38%), so conversion is sound; at learning rate 0 the trainer scores the same weights at 8.8% first
  position (this rerun) and 10.8% (the smoke, hidden states from vLLM). Ten training steps lower
  the served acceptance to 1.125. AngelSlim's vocabulary map is sorted and matches (read from the
  published file at $0). So the trainer's forward pass or targets differ from what vLLM serves, and
  the "conversion" and "lever" checks here measured that difference. Read with served numbers,
  drafts are the lever on this traffic (1.656 against the best model-free 1.300, +0.36), and the
  card's 1.9 at 2 draft tokens does not hold on oasst1 with thinking off even as shipped. The pass
  bar is unchanged; the coordinator decides whether conversion is judged by served acceptance.
- 26 Nov 07:20, $0 checks on this Mac's CPU before the next rung-1 run. speculators 0.9.0's draft
  forward equals a forward written from vLLM 0.34's `llama_eagle3` (sdpa, eager and flex attention;
  rows packed with document ids; compiled with `aot_eager` and `inductor`). The trainer loads every
  tensor of the published draft unchanged (bf16 rounding only) and the vocabulary maps exactly. Its
  accuracy metric equals a plain recount. transformers 5.19 and vLLM 0.34 take the same layers (the
  input to layers 2, 14 and 25, and the final state before the norm). One path is untested: on CUDA,
  speculators always compiles the draft's forward (`conditional_torch_compile`). The next run adds
  the trainer's validation and fine-tune with `TORCHDYNAMO_DISABLE=1` beside the compiled ones. It
  also scores the draft on the held-out rows without the trainer's loop (`trainer_view.py`): the
  forward compiled, uncompiled, and an independent forward, rows packed as the trainer packs them.
  It checks that the verifier's choice from the final hidden state is the next answer token. The
  pass bar is unchanged; `pass_bar_uncompiled` is reported beside it.
- 26 Nov 07:30, the coordinator's decision: "conversion sanity" is judged by served acceptance,
  the shipped draft against the converted one on the same prompts from vLLM's counters, not by the
  trainer's validation. On `diagnose` `r2-1` both wrote 1.656 tokens a verify step: pass. The
  trainer's number stays in the record beside it. "Drafts are the lever" and "fine-tuning moves the
  right way" still read the trainer (compiled and uncompiled side by side). Served, the published
  draft's 1.656 is 0.36 above the best model-free 1.300.
  Checked at $0 for this entry: the trainer builds the draft from the checkpoint's own config
  (`norm_before_residual` false, target layers 2, 14, 25); the CLI's Eagle 3.1 defaults it prints
  are refused with `--from-pretrained`, not applied. The verifier's final norm is `x * w`, Qwen3's
  convention. speculators' tracker has no matching issue at 0.9.0, the latest release.
- 26 Nov 07:55, after the run at 0c430085 (`r2-1`, about $0.14): not a result. Compiled and
  uncompiled agree exactly (published 8.78% first position both ways; the independent forward
  8.77%), so compilation is not the fault. The data check failed: the verifier's choice from the
  final hidden state, read through the draft's copy of the embedding, matched the next answer token
  0.13% of the time. Cause, confirmed at $0 on CPU: with transformers 5.19,
  `Eagle3DraftModel.from_pretrained` fills the converted checkpoint's missing `embed_tokens` with
  zeros (one value, 0.0), and `load_verifier_weights` loads the verifier's embedding only into
  weights that are still NaN, so it never does. The draft trained and validated with zero token
  embeddings; the verifier's head and norm, and so the targets, were right. vLLM shares the
  verifier's embedding, hence 38% served. Fix: `draft_embedding.py` writes the verifier's embedding
  into the converted checkpoint before any training or validation (the trainer then loads it as a
  checkpoint weight, checked equal on CPU). The view now reads the verifier's embedding from its own
  file and reports whether the draft's copy equals it. The compiled/uncompiled pair is removed. The
  pass bar is unchanged.
- 26 Nov 08:30, result at 28b9f8be (`r2-1`, Modal L4, about $0.11): **rung 1 passes.** The verifier's
  embedding went into the converted draft (151,936 x 2,048, bf16), the draft's copy equals it, and
  the verifier's choice from the final hidden state is the next answer token at 98.8% of 36,503
  held-out positions. Over those positions (128 held-out oasst1 roots, Qwen3-1.7B, thinking off):

  | Draft | First position | Tokens a step, 2 draft tokens | Tokens a step, 3 draft tokens |
  |---|---|---|---|
  | Published, unchanged (lr 0) | 43.3% | 1.640 | 1.745 |
  | Fine-tuned on 512 training answers, 2 epochs | 51.2% | 1.799 | 1.968 |
  | Best model-free (suffix, approximate, 8 tokens) | | | 1.300 |

  Checks as decided: conversion sanity passes on served acceptance (shipped 1.656 = converted
  1.656; the trainer's 1.640 at 2 tokens stays below the card's 1.9, which was set on other traffic).
  Drafts are the lever: 1.745 against 1.300, +0.445 (bar 0.2), pass. Fine-tuning moves the right
  way: 1.968 against 1.745, pass. 80.5% of the next tokens are in the draft's 32,000-token
  vocabulary, a ceiling on any draft for this multilingual traffic. These are offline indications on
  512 examples, not speed. Climb to rung 2.
