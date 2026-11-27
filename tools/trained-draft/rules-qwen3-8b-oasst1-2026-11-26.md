# Rules: a draft trained on chat traffic for Qwen3-8B, 26 November 2026

Written before any server of this proof ran. A change after a paid stage starts goes under
"Changes after launch" with its time and reason; nothing above that heading changes.

## The question

Does an EAGLE-3 draft fine-tuned on one kind of traffic serve that kind of traffic faster than
the published draft it started from, on prompts it never saw, with answer quality held? This
is the lever Together AI, Fireworks AI and Baseten sell on their own fleets: Together reports
1.23x to 1.45x over its base speculator after 20 to 50 million tokens of a customer's traffic,
Fireworks a 76% against 29% acceptance (both **company-reported**). Here it runs where a
customer would run it, and our judge decides.

## What is fixed

| Item | Value |
|---|---|
| Model | `Qwen/Qwen3-8B` at `b968826d9c46dd6066d109eabc6255188de91218` (Apache-2.0), BF16, thinking off |
| Published draft (the starting point and the control) | `RedHatAI/Qwen3-8B-speculator.eagle3` at `08610ffa01dd9f16731fe8f627b85905b6aa51c4` (Apache-2.0; trained on ShareGPT and UltraChat by Red Hat) |
| Engine | vLLM 0.34.0, `vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1` |
| Trainer | speculators 0.9.0 (Apache-2.0), `--from-pretrained` the published draft, hidden states from vLLM online (layers 2, 18, 33 and 36) |
| Draft tokens per step | 3 for both drafts |
| GPU | one Modal H100 (`H100!`), 8 cores, 48 GiB; $4.7101 an hour at list price |
| Traffic kind | first user turns of `OpenAssistant/oasst1` conversation trees at `fdf72ae0827c1cda404aff25b6603abec9e3399b` (Apache-2.0), all languages as published |
| Training prompts | the train split's tree roots (deduplicated, not deleted, not synthetic, at most 4,000 characters) |
| Held-out prompts | the validation split's tree roots, minus any whose text or tree is in training (none by construction); shuffled with seed 20261126 |
| Training data | the model's own answers at temperature 0, up to 1,024 tokens, as token ids |
| Training | 3 epochs, learning rate 1e-4 cosine with 5% warm-up, 8,192 tokens a step; stopped at 30 minutes if not done, newest checkpoint kept |
| Task set | `fast-track-quality-v1.json`, kept outside this repository and named by `TRAINED_DRAFT_TASKS` (sha256 `83c3df7b...72e4`): 500 GSM8K and 500 MMLU items, answer_line grading |

## Measurement

- **One GPU, three servers at once.** The published draft, the trained draft, and the same model
  without speculation each run with `--gpu-memory-utilization 0.30`, `--max-num-seqs 64`,
  `--max-model-len 4096` and `--no-enable-prefix-caching`, so the arms share the machine and
  prompt reuse between runs cannot help one arm. Only one server works at a time.
- **The window is a campaign's.** The control is the published draft: the current setup of a
  customer who already speculates. The candidates are the trained draft and no speculation. Order:
  control, trained, control, none, control, ... four runs of each candidate, every candidate run
  between two control runs. Two loads: 1 request at a time (8 requests a run) and 16 at a time
  (48 a run), answers up to 512 tokens, streamed. Every run of a load sends the same held-out
  prompts (the first 8, then the next 48), so runs and arms differ only in setup: a speculating
  server's speed depends on what it is asked, and different prompts per run would add that to
  the noise. Prefix caching is off, so a repeated prompt is computed again. Warm-up before each
  load: 16 or twice the load, 64 tokens, on prompts from the second half of the held-out list.
- **The judge.** `optimizationcampaign.JudgeRecordedWindow`: the campaign's paired log ratio,
  Bonferroni for 2 candidates (97.5%), the 0.75% machine allowance added although the arms share
  one machine, a window void when a run failed or control runs drift more than 15%.
- **Acceptance.** vLLM's speculation counters per arm per load: mean tokens a verify step writes,
  and per position the share accepted given the earlier ones. The training report's held-out
  acceptance is the same reading over the 200-prompt answer pass below. Reported, never a gain by
  itself.
- **Answers at temperature 0.** 200 held-out prompts, 256 tokens, on every arm, and on the
  control twice. Reported as the share of identical texts; the control's second pass is the floor
  that numerics alone give (speculation changed greedy answers on real servers before:
  `fast-track-result.md`, 4 of 48 identical on vLLM).
- **Task score.** All 1,000 items on each arm at 24 at a time; `qualitycheck.PairedTask` item by
  item against the control (95% interval, 3-point margin).

## Pass bar (per load)

| Check | Pass | Otherwise |
|---|---|---|
| Validity | every run 0 failed requests, window not void | that load is **unknown** |
| Headline | the judge says the trained draft is **improved** on output tokens per second against the published draft | "no measurable difference" or "slower", stated as measured |
| Quality | the task rule says **no measurable difference** or better for the trained draft against the published draft | "worse" fails the headline; "not settled" leaves quality **unknown** |
| Report | the training report passes the core's `speculator.DecodeReport` | the draft is not offered to the planner |

The headline at a load passes only when both the judge and the task rule pass. "No speculation
against the published draft" is reported beside it: what any draft is worth at that load. No
claim says "beats the vendor", and no number from this run is compared with a company's number
as if measured the same way.

## Stages, caps and stops

| Stage | What | Wall clock (each stage's own Modal timeout) | Hard cap | Estimate |
|---|---|---|---|---|
| `download` | weights to the volume, CPU only | 30 min | about $0.05 | $0.02 |
| `smoke` | 64 answers, 10 training steps, both drafts served for 8 requests | 25 min | $1.96 | $0.95 to $1.40 |
| `train` | every training prompt answered, up to 3 epochs (training stops at 30 min) | 60 min | $4.71 | $2.40 to $3.60 |
| `measure` | three servers, two windows, answers, task set | 42 min | $3.30 | $2.00 to $2.60 |
| **Total** | | | **$9.97** (plus the CPU download) | **$5.35 to $7.60** |

- Each stage stops its optional work 5 minutes before its cap, writes its record and commits
  the volume; the caller also cancels the call if it has not ended 15 minutes after the cap
  (scheduling and the image pull bill no GPU).
- `train` starts only when `smoke` passed: every request answered and both drafts accepted tokens.
- `measure` starts only when `train` left a draft with a config and weights.
- A stage that fails records `passed: false` and its error on the volume and stops; nothing is
  retried without a new entry here.

## What this proof does not show

- One traffic kind (public chat), one model, one GPU type, two loads. A customer's own traffic,
  other models, and loads above 16 at once are not measured.
- The model's own prediction head: Qwen3-8B's config declares none, so that arm does not exist
  here. Suffix decoding (built into vLLM 0.34, no training) is not measured: three 8B servers
  already fill the GPU.
- The training data is public: no customer prompt is captured. The capture path in
  `docs/features/trained-drafts.mdx` is designed, not built.
- Acceptance and speed on traffic of another kind than the draft was trained on.
- Whether a longer training run, more data (Together's 20 to 50 million tokens) or another
  learning rate would change the result.

## Changes after launch

- 26 Nov 14:00: superseded before it ran. Rung 4 is `rules-rung4-h100-qwen3-8b-2026-11-26.md` (plan
  `h100-qwen3-8b-fp8`): rung 3's trained draft on an H100, the product's held-within-1-point rule on three
  servers each, and a second server without speculation.

