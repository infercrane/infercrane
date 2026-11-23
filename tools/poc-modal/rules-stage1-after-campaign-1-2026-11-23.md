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

## Added before the first cell-driven planner check

Added 2026-11-23; the commit's timestamp is the record. No cell-driven check had run. Known when
this was added, and written down in the runs' internal result notes, not here: the results of
campaign 1 and campaign 2, the two planner-only checks on the trace replay, the fast track's
result on H200, and a first signal from the map's H100 measurements (one server per setup),
including its cells at 128 requests at once. That signal and campaign 2 are why the checks at
128 come first and why a check can start from the documented speculation option.

`poc_modal.py ask-planner --cell KEY` asks the planner while one cell of the fast-track map
runs. A cell is a kind of traffic at a load (for example `prefix/32`); the map measures every
setup on each cell next to the server.

- **What the product sees:** that cell's own requests and nothing else, from the prompt file the
  cell names (checked by sha256), sent the way the map's load generator sends them: one user
  message, temperature 0, streamed, a fixed number of workers each sending its next request when
  its last answer ends; a shared-prefix session's requests one after another from one worker,
  worker n starting its first session at question n modulo the session's length; the model's
  reasoning switched off or left on as the cell says. No trace replay, no recorded shape, no
  campaign. Nothing is measured.
- **When the planner is asked:** once the control plane holds windowed engine measurements and
  the cell's load has run for at least 45 seconds, or for the cell's own warm-up when that is
  longer (the map measures a cell only after its warm-up), on a fresh server that has seen no
  other traffic. The load goes on until the planner has answered; limit 300 seconds, as for the
  hold. Each worker's first answer limit is cut by the cell's seeded share, as the map cuts it.
- **Which cell file:** the one the fast track's rules for the map name by sha256. The check is
  given that hash and refuses a file with another one; the result records it.
- **Why the reasoning switch follows the cell:** the planner's verdict rests on shares of
  request time and on answer length, and reasoning changes both. The campaigns leave reasoning
  on because the product's traffic record does not keep the switch; the cells say it themselves.
- **What the answers are for:** one table. Per cell: the planner's first proposal this GPU can
  run, with its rule and reason, next to the setup the map measured as fastest in that cell. A
  hit is the same setup. Which cells are checked is decided before any of their answers is
  known, every check that ran is in the table, and a check is repeated only under the rule
  above (it failed to ask), with both attempts shown.
- **What differs from the map's own measurement, stated with the table:** the check's requests
  go through the product's gateway and a tunnel from a laptop, and the planner reads about one to
  two minutes of load; the map's generator runs next to the server and measures a 90-second
  window after 30 seconds of warm-up. The check says what the planner proposes for that traffic.
  It does not re-measure the cell.
- At 128 requests at once the planner is asked with the product's 128-concurrency profile. No
  campaign runs at 128.
- **The current setup of a check is one of two**, named with `--current-setup`: the publisher's
  command (`publisher`) or that command with its documented speculation option
  (`publisher-speculation`). They are the map's setups 1 and 2: the same flags, values,
  environment and image tag; only the place of `--max-num-seqs 512` in the command differs.
- **Order, fixed before any answer is known:** first `short/128` and `output/128` with the
  documented speculation option as the current setup; then the same two cells with the
  publisher's command; then cells at 4 and 32. The budget for all of them is $8; a cell that
  would pass it is not run.
- **How a row is read.**
  - Current setup the publisher's command: the planner's first proposal this GPU's argument
    check accepts, next to the setup the map measured as fastest in that cell. A hit is the same
    setup, by the map's full result for that cell and not by its first signal.
  - Current setup the documented speculation option: the row records what the planner names on
    top of it (an FP8 KV cache, fewer draft tokens, fewer requests at once, anything else).
    That is a proposal, not a result. It becomes a hit only if the map measures that setup in
    the same cell and it is faster than the documented option there; unmeasured, it is reported
    as unmeasured.
  - "This GPU can run it" means the argument check passed. A setup no server has started (for
    example speculation with an FP8 KV cache) is reported as not yet started anywhere.

## Added after two cell checks had answered and one had twice not started

Added 2026-11-23; the commit's timestamp is the record. Known when this was added: the answers
of the first two cell checks (both 128 cells with the documented speculation option as the
current setup). The third check, `short/128` with the publisher's command, had been started
twice and never reached the planner: the first time every host offered had another NVIDIA driver
than the one the run named, the second time the engine ran into its own 600-second startup limit
while the weights loaded slowly. No answer of that check or of the fourth exists.

- **A check that never reached the planner may be started again, more than once.** The limit of
  one rerun stays for a check that asked, or that sent traffic and then failed to ask: there an
  answer or a partial reading could tempt a second try. A server that never served shows
  nothing. Every attempt is listed with why it ended.
- **The cell checks name no NVIDIA driver from here on.** They measure nothing; the row says
  which driver the host had.

## Two product findings behind these changes (not fixed here)

- The planner decides between its low-load and its loaded plans from one latest reading of
  "requests running". A reading over the measurement window (its mean, or its peak) would not
  flip when a burst has just ended.
- The control plane held no windowed engine measurements within 59 seconds of traffic. A
  customer who asks the planner right after a burst gets the plan for an idle server.
