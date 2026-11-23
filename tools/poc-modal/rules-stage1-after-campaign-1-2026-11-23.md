# Rules for the runs after campaign 1: the planner is asked under load

Written 2026-11-23 while campaign 1 was running; the commit's timestamp is the record of when.
This file adds to `rules-stage1-2026-11-23.md`, which says a change after a campaign has run is a
new file. Everything that file fixes (model, GPU, engine, current setup, documented baseline, the
copy, traffic, corpus, reasoning, judge, answer check, what is recorded, pass bar, stop rules) stays
as it is. Campaign 1 ran and is reported under that file alone, at commit `e55df535`.

## What was known when this was written

- Campaign 1 (short chat) had started at 08:43 CET. Its traffic step: 100 requests in 59 seconds,
  none failed, every answer ran to its cap (56,462 output tokens).
- **Its planner was asked after the traffic had ended**, within about 12 seconds of it. The control plane
  reads the engine every 30 seconds and held no windowed measurements before the burst was over.
  The planner takes the number of running requests from the engine's latest reading; it read 0,
  called the server lightly loaded ("Only 0 requests ran at once per server (threshold 4)") and
  put speculative decoding with the model's own prediction head first, at priority 90, for that
  reason. Under the first file's rule, campaign 1's question 1 is reported as not answered for a
  loaded server.
- Of campaign 1's measurements, only the current setup's own first two turns existed: 80 seconds
  a turn, about 790 output tokens per second at 32 requests at once. The speculation setup's
  server had not started. **No comparison between two setups existed.**
- From the H100 smoke (40 requests stretched over 150 seconds, buffered, answers capped at 128
  tokens): asked during that traffic, the planner's diagnosis was "prefill and first token" and
  its first proposal was "bigger prompt chunks".
- From the fast-track measurements of the same model on H200 (next to the server, reasoning
  off, not through the product): the publisher's command with its documented speculation option
  was the fastest of four setups in all six cells.

## Change 1: the server is held under load while the planner is asked

Forced by campaign 1's planner timing, and made before any run after campaign 1 has a result.

The replay stays as it is: the first 100 rows at their recorded arrival times. Its shape is
recorded as soon as it ends, so what a campaign replays and measures does not change. Then the
harness keeps the current setup loaded: the same rows again in a closed loop at the load every
setup is measured at (`--requests-at-once`, 32 unless a run says otherwise), with other text of
the same shapes from the same corpus, so the engine's prefix cache does not answer them. These
requests are not part of the recorded shape.

The planner is asked once the control plane holds windowed engine measurements and the hold has
run for at least 45 seconds, so the engine reading it uses was taken under the hold. The hold
goes on until the planner has answered. If the hold reaches 300 seconds first, the planner is
asked anyway, the record says so, and the first file's rule applies: its first proposal is
measured and question 1 is reported as not answered for a loaded server.

The record holds when the planner was asked, how long the hold ran, and how many requests it
sent. A "fixed point inside the replay" was considered and cannot work: on an H100 the replay is
over in about a minute, before the control plane has measured it.

What this changes in what a result means: the planner now diagnoses a server at the load its
proposal is then measured at, which is what a customer's steady traffic shows. It no longer
diagnoses a 15-second burst or the idle server after it.

## Change 2: a planner-only check

`poc_modal.py ask-planner` rents one H100, starts the current setup, replays the traffic, records
the shape, holds the load, asks the planner, records its plan with every proposal's rule,
priority, reason and whether this GPU could run it, and stops. No campaign runs and nothing is
measured. Its parameters are the traffic shape (`--shape`) and the load (`--requests-at-once`: 4,
8 or 32, the loads the product has profiles for).

What its answer may be used for:

- It says what the product's planner proposes first for one traffic shape at one load. That is
  all it says. A proposal no run measured is reported as a proposal, never as a result.
- For short chat at 32 requests at once: if the planner's first proposal this GPU can run is the
  publisher's documented speculation option, campaign 1 has measured that pick, and campaign 1's
  question 1 is answered with it. If it is anything else, question 1 for short chat reads "the
  planner's first pick under load was X; campaign 1 did not measure X", and X is measured
  elsewhere or stays unmeasured, and the report says which.
- A check is not repeated to get another answer. One rerun is allowed only when a check failed
  to ask (the server did not start, the hold reached its limit, most requests failed), and both
  attempts are reported.

First check: short chat (`--shape interactive`) at 32. Further checks are decided from its answer
and the budget, and each is written into the ledger before it runs.

## Campaign 2

Long shared prefix (`--shape long-prefill`), as in the first file, with change 1. Before its
launch its exact command is rehearsed at full size on stand-in servers. Whether it is launched,
and when, is decided after campaign 1's result and from what the Modal workspace's spend limit
leaves.

## Added after campaign 1 and the first planner-only check, before campaign 2

Added 2026-11-23; the commit's timestamp is the record. Campaign 1's result and the first
planner-only check's answer were known when this was added. Nothing above changes.

- **Campaign 2 sets no limit on time to first token.** In campaign 1 the confirmed setup was
  slower to its first token at p95, and the product's judge has no guard on that for a throughput
  objective. A limit can be set on a campaign. None is set for campaign 2: no limit that a
  customer stated exists for this traffic, and one chosen now would be fitted to campaign 1's
  numbers. Time to first token is reported for both windows next to the result, with the judge's
  word for each, as for campaign 1.
- **What "answers matched" means is stated with every result**: the mean of 24 word-overlap
  scores on short prompts, not a comparison of exact text.

## Added after campaign 2's first start was stopped, before any campaign 2 traffic

Added 2026-11-23; the commit's timestamp is the record. Campaign 2 was started at 12:08:47 CET and
stopped by hand at 12:13:13, while its first server was still loading the model: no request had
been sent, the planner had not been asked, nothing was measured.

- **Campaign 2 names the NVIDIA driver its servers must have: 620.70.06** (`--driver`). The first
  file's rule is "all on one NVIDIA driver (the broker pins the first host's and refuses
  others)". The stopped start's first host had 610.57.04, which 5 of this track's 16 first
  landings on H100 hosts had today; the other 11, and every server of campaign 1, had
  620.70.06. A campaign of four setups needs up to nine servers on one driver, and a run pinned
  to the driver fewer hosts carry can use up its starts looking for them late in the run. Naming
  the driver changes which hosts are refused, not what is measured, and puts both campaigns on
  the same driver.
- The run's limit on server starts is 16, was 12, for the same reason. The dollar cap bounds
  the spend.

## Two product findings behind these changes (not fixed here)

- The planner decides between its low-load and its loaded plans from one latest reading of
  "requests running". A reading over the measurement window (its mean, or its peak) would not
  flip when a burst has just ended.
- The control plane held no windowed engine measurements within 59 seconds of traffic. A
  customer who asks the planner right after a burst gets the plan for an idle server.
