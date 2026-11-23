# Rules for the product-judged campaigns on real text (written before any of them runs)

Written 2026-11-23. This file is committed before the first paid campaign; its commit hash goes
into every result. A change after a campaign has run is a new file, not an edit.

**Changed on 2026-11-23, about 07:40 CET, before any campaign result existed.** What had run by
then: serving checks, rehearsals on stand-in servers, and one 40-request smoke on H100s that
compared nothing, because the product counted no output tokens for answers that were all
reasoning (fixed after it, commit `f3b8e006`). The changes, and why:

- **Two kinds of traffic instead of three**, and **the planner's own first proposal is a measured
  setup** in each campaign. The budget for this track does not pay for three campaigns of four
  setups. The direct measurements of the map already answer which setup wins on which traffic;
  only a campaign can answer whether the product's planner picks it unaided, and that needs the
  planner's first proposal measured, not only recorded. The long-output shape is the one
  dropped: it is the shape most favourable to speculation, and the map measures short chat and
  a long prompt with a short answer, so these two shapes can be read beside it.
- **The current setup carries `--max-num-seqs 512`**, after the serving check (below).
- **What output tokens are, and what the answer check is**, are now stated (below), after the
  smoke showed both needed saying.

## What the campaigns answer

One model, one GPU type, traffic with real text. Three questions, each answered whatever the
numbers are:

1. Does the product's planner, asked the way a customer asks it, propose the setup that turns
   out best, and does the product's judge confirm it on fresh servers? The planner's first
   proposal is measured whatever it is, and a first proposal that does not gain, or that loses
   to the documented baseline, is reported as a miss.
2. By how much does the confirmed setup beat the publisher's plain command?
3. By how much does it beat the better of the publisher's plain command and the publisher's
   command with its documented speculation option? This is the number that counts. If the
   planner's setup is that documented option, the answer to this question is "nothing", and
   that is published as it is.

A campaign that keeps the current setup is a result and is reported like any other.

## Fixed for every campaign

- **Model:** `Qwen/Qwen3.8-27B-FP8` at `c3a462cc3d38cf351f3b26cbd763469eeb74ef81`, weights on the
  Modal volume `infercrane-poc-modal-weights`.
- **GPU:** one NVIDIA H100 SXM5 80GB per setup on Modal (`gpu="H100!"`), all on one NVIDIA
  driver (the broker pins the first host's and refuses others).
- **Engine:** vLLM 0.34.0, `vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1`
  (`--engine vllm-0.34.0`). The product's own default engine version is not what is measured.
- **Current setup:** the publisher's command as published on
  `recipes.vllm.ai/Qwen/Qwen3.8-27B` for one H100 in FP8, with its environment
  (`VLLM_USE_RUST_FRONTEND=1`):
  `vllm serve <weights> --tensor-parallel-size 1 --enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3 --mm-encoder-tp-mode data --max-num-seqs 512`.
  The last argument is not the publisher's. The serving check of 2026-11-23 showed the
  command as published does not start on one H100 with vLLM 0.34.0: "max_num_seqs (1024)
  exceeds available Mamba cache blocks (904) ... lower max_num_seqs to at most 904". A
  campaign holds at most 32 requests at once, so 512 changes nothing it measures; it is the
  same value the 0.22.1 run used.
- **Documented baseline:** that command, with the same `--max-num-seqs 512`, plus
  `--speculative-config '{"method":"mtp","num_speculative_tokens":3}'`, the line on the same
  page. It is a measured setup in every campaign.
- **Setups to try:** the first setup that `infercrane optimize propose --diagnosis-endpoint
  --allow-model-metadata-network` returns for the loaded current setup and that the GPU can run
  as measured (`--planner-setups 1`), applied as one change to the current setup. Every
  proposal is recorded with its rule, priority and reason, and a skipped one with why. Nothing
  is added by hand except the documented baseline and the copy. If the planner's first
  proposal is the documented baseline itself, the campaign has three setups, not four.
- **The copy:** the current setup again on its own server with only `--seed 1` added. If the
  judge calls it better, the campaign has no decision.
- **Traffic:** the first 200 rows of the Mooncake FAST'25 trace (Apache-2.0) at their recorded
  arrival times, with their shared prefixes: the first 100 rows (`--requests 100`), which is
  what the budget pays for with four setups. One campaign per traffic shape (`--shape`); a
  shape scales the rows so their median input and output lengths land on the medians of the
  product's public workload shapes (Chutes data, CC BY 4.0), keeping the trace's spread. Two
  shapes: `interactive` (short chat: 3,919 tokens in and 295 out) first, then `long-prefill`
  (7,897 in and 295 out). Inputs are capped at 30,000 tokens and outputs at four times the
  median.
  Text from the prompt corpus `fast-track-v1-corpus.json`, sha256
  `4acd33074d11f45f255bf89c2378166557fd0dd7e77da9ae2146dbd16e39efc7`: 1,500 CNN/DailyMail
  articles and 1,000 OpenAssistant oasst1 prompts, the same text the fast-track map measures
  with, both sources Apache-2.0 on their dataset cards. Answers end where the model ends
  them. The campaign replays the recorded shape at 32 requests at once (profile `throughput`).
  A campaign repeated at 8 or 4 requests at once (`--requests-at-once`, profiles `balanced` and
  `long-context`) is a separate campaign with its own record; the product has no profile
  with 12.
- **Reasoning:** requests do not switch the model's reasoning off, so it reasons before it
  answers, as a request without that switch does in production. Output tokens are every token
  the model generated, reasoning included, and tokens per second count them. The direct
  measurements of the map switch reasoning off for every request. Tokens per second from these
  campaigns are therefore not comparable with the map's; gains between setups inside one
  campaign are what a campaign reports.
- **Judge:** the product's, unchanged: four paired rounds, a 0.75% allowance for the
  difference between two machines, a second window on fresh servers before a gain counts, the
  built-in answer check, a signed report.
- **Answer check:** the product's built-in one. It asks the setup and the current setup the same
  24 fixed short prompts, at most 64 tokens each, greedy with a fixed seed and reasoning switched
  off, and scores word overlap with the current setup's answers (token F1 after lowercasing and
  dropping punctuation); 95% mean agreement passes and under 90% blocks. It is a guard against a
  setup that breaks answers. It does not compare the exact text of long answers and is not
  evidence that long answers are unchanged.

## What is recorded

Per setup: its exact arguments, output tokens per second, p95 latency and time to first token,
the judge's comparison in both windows, the answer check, the drafted and accepted tokens of
every run that speculates, and cost per million output tokens at Modal's H100 list price. Per
campaign: the planner's diagnosis, every setup it proposed with its reason and whether it was
measured or why not, whether the planner proposed the documented baseline itself, the corpus
digest every measured setup's evidence names, each server's GPU, driver and network round trip,
and the broker's ledger.

Latency is measured from the laptop through a tunnel. It is comparable between setups and is not
what a user next to the server sees; no absolute latency from these campaigns is published.

## Pass bar

A setup is "confirmed" when all of these hold: the judge says improved in the first window and
again on fresh servers, the gain on output tokens per second is at least 5%, the answer check
passes, and the copy did not win. A setup that speculates must also show accepted tokens in
its runs; without the counters its speed is reported as unexplained, not as a win. The judge
compares each setup with the current setup only. A difference between two setups that both
gained (the planner's pick and the documented baseline) is reported from their measured
throughput as unjudged, with both intervals, and is not called a win.

## Stop rules

- The copy wins: the campaign has no decision. One rerun; a second time means the judge's
  allowance is wrong for this setup, and that is reported.
- A measured setup's evidence names another text than the corpus above: no decision.
- More than half the replayed requests fail, or the current setup does not serve: stop, fix,
  rerun from the serving check.
- The broker's cap for the campaign is reached: every server stops; what was measured is kept.

## Order

1. Offline tests. 2. `poc_modal.py dry`. 3. Serving checks: the current setup and the
documented baseline, one H100 each. 4. Stand-in campaign (`--stand-in --smoke`, no GPU).
5. Smoke campaign on H100s (`--smoke`). 6. The campaigns, one at a time. No step is skipped.
Steps 1 to 5 are done. The smoke ran the path on H100s and was stopped once it was clear the
judge could compare nothing; the first campaign is therefore also the first run of the
judge's comparison and of the re-measurement on fresh servers on real GPUs, and it is stopped
if that path breaks.

## Not claimed from these campaigns

A universal speed-up, a result for another model, GPU or engine, absolute latency, or anything
about traffic that is not this trace's shape with this corpus's text.
