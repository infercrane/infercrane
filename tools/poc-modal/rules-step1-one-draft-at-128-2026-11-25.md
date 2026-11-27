# Rules: does the product find "one draft token" at 128 requests at once, from the documented option

Written 2026-11-25 02:08 CET, before any server of this run started. Maintainer test protocol:
step 1 of the maintainer's optimizer plan. The map's and the runs' figures are kept in the
maintainer's run notes and said here in words.

## Say this first

**The planner's rule was built from this model's map.** The capacity test at `103ded87` was written
after the map on one H100 measured this exact cell, and its unit tests use the map's numbers. So
this run is the product repeating a known answer end to end, not a discovery. What it can add is
the first result that is found by the product, beyond what the publisher documents, and judged
by the product's own judge, all in one run. Whether the algorithm works on a model it was not
built from is step 2's question, not this one's.

## What is asked

- **Model and engine:** `Qwen/Qwen3.8-27B-FP8` at its pinned revision, vLLM 0.34.0 pinned by
  digest (`models.py`), one H100 per server, Modal.
- **Current setup:** the publisher's command with its documented speculation option, three draft
  tokens (`--current-setup publisher-speculation`). It is what a customer who followed the
  publisher's page runs at peak.
- **Traffic:** the map's cell `short/128`: short chat, 128 requests at once, closed loop,
  reasoning off, the cell's own requests. Cell file `fast-track-map-cells-h100.json`, sha256
  `2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4`; prompt file
  `fast-track-prompts-v2.json`, sha256
  `2f6b05635659520db775d85d218d0b2ded0104cecc634183e318fe87d4fac984`.
- **Run A, the planner check** (`ask-planner`): one server with the current setup runs the cell
  and the planner is asked after the cell's warm-up. Nothing is measured. It is the go or no-go
  for run B.
- **Run B, the campaign** (`run --cell short/128 --plain-arm`): the same start, the planner is
  asked again, the shape of the cell's requests (lengths and order, no text, up to 1,000) is
  recorded, and the product's campaign measures four setups at 128 at once, each on its own
  server:
  1. the current setup (three draft tokens);
  2. the planner's first proposal this GPU can run;
  3. the publisher's plain command (`--plain-arm`);
  4. a copy of the current setup with only `--seed 1` added. It must not win.
  The product's judge compares each with the current setup in paired turns and measures a gain
  again on fresh servers. The control plane composes each measured request's text from the
  prompt corpus `fast-track-v1-corpus.json` (sha256
  `4acd33074d11f45f255bf89c2378166557fd0dd7e77da9ae2146dbd16e39efc7`), the one `up --corpus`
  mounts. `run` refuses to start if the control plane holds another file.

## When run B starts

Run B is launched only if run A's first proposal this GPU can run is speculation with one draft
token. If run A names anything else first, run B is not launched and run A's record is the
result, written as a miss with what it named.

## What counts as a hit

Each is said on its own; none is added into a score.

1. **Planner hit:** in run B's own record (`planner.setups`), the first setup marked runnable is
   "Speculative decoding with one draft token from the model's own prediction head"
   (`"num_speculative_tokens":1`).
2. **Judged gain:** the judge qualifies that setup over the current setup on output tokens per
   second, confirmed on fresh servers, and the copy does not win. Expected from the map: a clear
   gain (medians of three servers). The run's own number is reported, whatever it is.
3. **Ahead of the plain command:** said only if the lower end of the one-draft setup's 95% interval
   against the current setup lies above the upper end of the plain command's. The map has it
   ahead of the plain command. Otherwise: "not separated from the plain command in this run".
4. **First token:** each arm's p95 time to first token as the product recorded it, beside the
   gain. Not judged.

A copy that wins makes the whole run "no decision", whatever else it shows.

## What it cannot show

- **That the planner works on other models.** See the first section.
- **Answer quality by a task score.** The product at this commit has only its built-in answer
  check: word overlap with the current setup's answers on 24 short prompts. The task score for
  this pair of setups comes from the map, not from this run: on 1,000 task items, neither one nor
  three draft tokens showed a measurable difference against the plain command. The task-scored
  gate is package 3 of the plan and is not in this commit.
- **Useful output under a first-token limit.** The objective at this commit is all output tokens
  per second. In the map at this cell, one draft token's p95 first token was over one second and
  the plain command's well under it: under a one-second limit the plain command delivers more
  usable output here. A judged gain on throughput is not a gain under that limit, and the result
  says so.
- **The map's exact traffic.** The planner sees the cell's own requests with reasoning off. The
  recorded shape does not keep the reasoning switch, so the campaign's measured requests leave
  reasoning on, each capped at its recorded length, with text from the corpus rather than the
  cell's own prompts. The first 128 requests of the cell have their answers cut by the cell's
  stagger, and the shape keeps those shorter lengths.
- **Other loads.** One cell, one load. The map has three drafts ahead up to 64 at once.
- **Network-free latency.** Requests cross from the laptop's control plane to Modal through a
  tunnel; first-token times include it.
- **Machine spread beyond the copy.** The judge allows 0.75% for machine difference; the map saw
  much larger differences between servers of one setup at high load on short chat. The copy arm is
  the guard.

## Commands

From a clean worktree at the commit that holds this file, with `uv` and `modal==1.6.4` as before.
`CORPUS=<corpus directory>`. The local path was anonymised; the rules are otherwise unchanged.

```bash
python3 tools/poc-modal/poc_modal.py up --corpus "$CORPUS/fast-track-v1-corpus.json" --engine vllm-0.34.0
```

Run A (cap $1.50):

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --current-setup publisher-speculation --engine vllm-0.34.0 --budget-usd 1.5
```

Run B (cap $30), only after run A as above:

```bash
python3 tools/poc-modal/poc_modal.py run --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --corpus "$CORPUS/fast-track-v1-corpus.json" --current-setup publisher-speculation --plain-arm --planner-setups 1 --engine vllm-0.34.0 --driver 620.70.06 --budget-usd 30
```

No driver is named for run A, as for the earlier cell checks. Run B names 620.70.06, the driver
most hosts carried on 23 November and the one every measured server of the map had.

## Money

- **Run A:** one server for about 9 to 12 minutes: about $0.70 to $0.85 (the eight cell checks of
  23 November billed $0.51 to $0.85 each). Broker hard cap $1.50.
- **Run B:** five servers to start (current setup for the planner, then the four campaign
  servers), held until the final ranking, and two fresh servers for each setup whose gain is
  measured again (one draft token and the plain command are both expected to gain over three
  draft tokens here). A turn is about 1,000 requests (256 to warm up, the rest measured) and
  should take 70 to 80 seconds. Campaign 1 (three campaign servers at 32) cost $13.41; with one
  more server and two confirmations this run should cost about $22 to $25. Broker hard cap $30:
  the broker refuses a new server that the cap cannot cover and stops every server when its
  estimate reaches the cap. The estimate uses Modal's list price for an H100 with the container's
  CPU and memory ($0.00120268 a second); Modal's billing report is the authority.

## Before it is launched

- The path was rehearsed with stand-in servers on the laptop (`--stand-in local`), no Modal and
  no money; the record is listed under "Rehearsal" below once it has run.
- Nothing else of this plan runs on Modal at the same time, so the bill is this run's.

## Rehearsal

Run B's exact command with `--stand-in local --budget-usd 1` instead of the driver and the cap,
on 2026-11-25 from 02:18 to 02:38 CET, control plane built at `24942cd8`. No Modal, nothing
spent. Record: `docs/testing/evidence/poc-modal-stand-in-cell-campaign-2026-11-25.json`.

- The cell's own requests ran at 128 at once; the planner was asked 47 s into them.
- The gateway recorded the shape of 1,000 of them, and the campaign measured on it at 128 with
  text from the corpus (the result names the corpus by its sha256).
- The plain-command arm and the copy were added and measured against the current setup in turns,
  answer-checked and ranked. The copy did not win. The broker stopped every stand-in.
- The planner proposed nothing runnable on the stand-in: a stand-in reports no cache layout, so
  speculation is not chosen there, and larger batches and FP8 weights do not apply. The campaign
  therefore had three arms. The path of the planner's setup into the campaign is covered by the
  offline test `test_the_plain_arm_measures_both_documented_setups_next_to_the_planners_first`.
- The harness recorded `uncommitted_changes: true`: files of another package were being written
  in the same worktree while the run started. The control plane it ran was built at `24942cd8`.

## Attempt 2

Written 2026-11-25 03:35 CET, after run A of attempt 1 and before any server of attempt 2.

### What attempt 1 showed, and why

Run A of attempt 1 (launched 03:01:30 CET from `24942cd8`, billed $0.72, 1,007 cell requests, none
failed) was a **miss by these rules**, and run B was not launched. The planner's diagnosis was the
expected one: a full cache with requests waiting, and "Speculative decoding with one draft token"
chosen first. But the product built no setup: every chosen technique was refused with "The
starting setup is a Dynamo serving graph". At the overload profile the starting setup was picked by
content hash, and `24942cd8` did not have the fix merged to main as #279. The stand-in rehearsal
had shown the same refusals; they were taken for a stand-in limit and their reasons were not
checked. That was an error in reading the rehearsal, not the harness's.

### What changed for attempt 2

- The #279 fix is in the branch (cherry-pick of `076303b2` as `c425fcf6`).
- An offline test replays attempt 1's own recorded diagnosis with the harness's exact request
  (provider kubernetes, H100, vLLM, throughput, the overload profile and three others) and requires
  a runnable one-draft setup first (`TestStep1RunADiagnosisYieldsARunnableOneDraftSetup`). It fails
  on the code of attempt 1 and passes now.
- The harness stops any run when the product refuses what its planner chose because of its own
  starting setup (a serving graph or a pinned image), and logs every chosen technique it did not
  build, with the reason.
- The branch now also carries packages 1 to 8 of the plan (rule set `diagnosis-technique-rules-v8`).
  For this cell they keep "one draft token" first for a throughput goal: the run asks for
  throughput, and the harness deploys its current setup as `custom-oci`, so the plan starts from
  the planner's own vLLM setup as before (package 8 reads the endpoint's setup only when the engine
  matches). The campaign records useful output only when a limit is set; this run sets none.

### Commands

As in "Commands" above, from a clean worktree at the commit that holds this section, with run B's
cap lowered to what the track has left after attempt 1 and a second run A:

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --current-setup publisher-speculation --engine vllm-0.34.0 --budget-usd 1.5
```

```bash
python3 tools/poc-modal/poc_modal.py run --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --corpus "$CORPUS/fast-track-v1-corpus.json" --current-setup publisher-speculation --plain-arm --planner-setups 1 --engine vllm-0.34.0 --driver 620.70.06 --budget-usd 29
```

Run B starts only if run A's first runnable proposal is one draft token, as before. Its estimate
stays $22 to $25; the cap of $29 leaves no room for a rerun of B.

### Rehearsal of attempt 2

Run B's command with `--stand-in local --budget-usd 1`, on local stand-in servers, no Modal,
nothing spent. Record: `docs/testing/evidence/poc-modal-stand-in-cell-campaign-attempt-2-2026-11-25.json`.

- The first try (03:37, control plane at `cddbfc86`) stopped at the planner: the harness's CLI had
  been built at `24942cd8` and kept, and an older CLI refuses every proposal that carries a newer
  field ("proposal input digest does not match normalized input"). Fixed in `b27c05c3`: the
  harness builds its CLI again whenever the checkout changes. A fresh worktree was never
  affected; the attempt-1 worktree `console-step1-run` would have been, with its CLI from
  `24942cd8`.
- The second try (03:47 to 04:07, control plane `cddbfc86`, harness `b27c05c3`) passed: the
  planner was asked 45 s into the cell's requests at 128; no technique was refused for the
  product's own starting setup; the two it chose and did not build were refused for the
  stand-in's own reasons (the model name marks quantized weights; the stand-in reports no cache
  layout, so the draft length rule could not run); the plain command and the copy were measured
  against the current setup in turns and ranked; the copy did not win.
- What a stand-in cannot show is whether the real server's diagnosis builds one draft token. That
  is `TestStep1RunADiagnosisYieldsARunnableOneDraftSetup`, on attempt 1's own recorded diagnosis.

## Attempt 3

Written 2026-11-25 07:41 CET, after run A of attempt 2 and before any server of attempt 3.

### What attempt 2 showed, and why

Run A of attempt 2 (launched by the coordinator from `76d37838` at 04:13, ended 04:23:29 CET,
billed $0.70) stopped before the planner was asked: "cell load ended after 80 s: 5492 requests,
4550 failed". Run B was not launched. The server was ready after 399 s on driver 620.70.06.

The failures were the product's, not the server's. The deployment's own events and request
records show it:

- 03:21:42 UTC the cell load started; the gateway recorded requests streaming normally.
- 03:21:48 `replica_unhealthy`: one health probe of the server did not pass. The control plane
  probes `/health` and `/v1/models` with 5 s for both; the runtime proxy answers `/health` by
  asking the server over the Modal tunnel with 4 s. The server was then admitting the cell's
  first 128 requests at once.
- On that one failed probe the reconciler withdrew the deployment's route. For about ten
  seconds, until its next pass, the gateway refused every new request at once for want of a
  route, before anything is recorded, and the harness's 128 workers sent the next request 0.2 s
  after each refusal: about 4,500 refusals, none of them in the gateway's request records, which
  hold 950 requests (942 answered; 8 failed as upstream errors, 7 of them as the router was
  replaced).
- In those ten seconds the server kept streaming every request already running: 84 completed
  between 10 and 15 s after the start, while none started.
- 03:21:58 `replica_healthy`: the route came back with a new router (generation 2), and the
  load ran on with no further failure until the planner's hold ended.

Attempt 1's records (same load, same code path) show one router generation and no unhealthy
event: its probes happened to pass. Nothing in this branch between `24942cd8` and `76d37838`
touches the gateway, the router or the reconciler, and main has the same rule (since
`e0b36ff1`, 9 August). The rehearsals did not catch it because the stand-in answered `/health` at
once and each request within about 3 s.

### What changed for attempt 3

- **The product (`e9d4c3bf`):** a target that was healthy keeps its route through one probe that
  fails or times out, recorded as a `replica_probe_missed` event, and loses it when a second probe
  in a row fails. A target that answers without its model, a new or unhealthy target, and one
  whose probe cannot run get no grace. Race tests; documented in
  `docs/architecture/data-flows.md` and `docs/features/gateway.md`. This is a product behavior
  change: a server that dies keeps its route up to one more pass (10 s), and its requests fail as
  upstream errors instead of being refused without a route. Its commit message says the route
  was withdrawn 11 times in the rehearsal without it; the events show 10. The same fix, which
  also drops the count of a server no longer probed, is on `fix/reconcile-probe-grace`, cut from
  main; this run does not use that branch.
- **The harness (`8c2853f9`, `e918eed0`, `dbb1be90`):** a local stand-in for a cell at 64 requests
  at once or more acts like the overloaded server: while 64 or more run, each new request waits
  5 s for its first token, and `/health` takes 6 s in the first 8 s of every 25 s. A lone request
  answers at once, since the router's readiness check is a one-token completion that must answer
  within 500 ms (`e918eed0`; the first version held every request and no router could start). The
  stand-in listens with uvicorn's queue of 2048 (`dbb1be90`): with the default of 5 the runtime
  proxy's 128 dials at a cell's start were partly refused, which is where the 23 and 52 failed
  requests of the earlier rehearsals came from. A stand-in cell load fails on any failed request
  and passes only if the deployment's events show a missed probe with the route kept. Every cell
  load records why each failed request failed.

### Rehearsals

Run A's command with `--stand-in local --budget-usd 1`, on local stand-in servers, no Modal,
nothing spent, each on a fresh cluster:

| | Control plane | Cell load | Failed | Health checks | Route | Planner |
|---|---|---|---|---|---|---|
| Without the fix (06:47 to 06:54) | `dbb1be90` (main's rule) | 231 s, 40,128 requests | 36,489, all 404 | 10 missed | withdrawn 10 times, 9 router generations | never asked |
| With the fix (06:58 to 07:02) | `e9d4c3bf` | 57 s, 1,087 requests | 0 | 1 missed | kept | asked 50 s into the hold |

Without the fix the 3,639 requests the gateway recorded all answered 200; every refusal was a 404
before admission, as in the paid run. With the fix the planner answered as on every stand-in
(no cache layout is reported, so the draft length rule cannot run and nothing is built); what the
real server's readings build is `TestStep1RunADiagnosisYieldsARunnableOneDraftSetup`, which
passes at `e9d4c3bf`.

Run B's command with `--stand-in local --budget-usd 1` (07:03 to 07:40, control plane
`e9d4c3bf`, fresh cluster) passed: the cell's own 1,087 requests at 128 at once with 0 failed,
2 probes missed with the route kept and none withdrawn; the planner was asked 50 s into the
hold; the plain command and the copy were measured against the current setup in turns, every
setup with 0 failed requests, and ranked; the copy did not win; the broker stopped every
stand-in.

Records: `docs/testing/evidence/poc-modal-stand-in-route-withdrawal-without-grace-2026-11-25.json`
(run A without the fix: events and router generations),
`docs/testing/evidence/poc-modal-stand-in-planner-only-attempt-3-2026-11-25.json` (run A with
it) and `docs/testing/evidence/poc-modal-stand-in-cell-campaign-attempt-3-2026-11-25.json` (run
B).

### Commands

As in "Commands" above, from a clean worktree at the commit that holds this section; run B's cap
lowered to what the track has left after attempts 1 and 2 and a third run A ($31.30 - $1.42 -
$1.50 = $28.38):

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --current-setup publisher-speculation --engine vllm-0.34.0 --budget-usd 1.5
```

```bash
python3 tools/poc-modal/poc_modal.py run --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --corpus "$CORPUS/fast-track-v1-corpus.json" --current-setup publisher-speculation --plain-arm --planner-setups 1 --engine vllm-0.34.0 --driver 620.70.06 --budget-usd 28
```

Run B starts only if run A's first runnable proposal is one draft token and its cell load had no
failed request. Its estimate stays $22 to $25; the cap of $28 leaves no room for a rerun of B.
On the real server a missed probe now shows as `replica_probe_missed` in the deployment's events
(the harness checks the events only on stand-ins) and the route stays. Two in a row still withdraw
it; the cell load then fails, and its record counts the 404s by reason.

## Attempt 4

Written 2026-11-26 00:02 CET, after run A of attempt 3 and before any server of attempt 4.

### What attempt 3 showed

Run A of attempt 3 (launched by the coordinator from `85f6b890` at 23:34 CET on 25 November,
ended 23:46:33 with exit 0, billed $0.62, server ready after 298 s on driver 620.70.06) asked the
planner 45 s into the hold. Its first runnable proposal was "Speculative decoding with one draft
token from the model's own prediction head" (66 of the 125 sent at once waited). That is run A's
answer, not a planner hit: rule 1 reads run B's own record. The cell load ran 54 s: 749 requests,
1 failed (503). The gate said "no failed request", so run B was not launched, as the rules say.

### Where the one failure came from

- The gateway recorded it (`request_records`): 22:45:01.668 UTC, status 503, error type
  `upstream_unavailable`, 94 ms, no retry, streamed. The gateway's log prints a line only for an
  answered request, which is why the log shows 748 requests, all 200.
- `upstream_unavailable` in 94 ms means the router answered the gateway with 502, 503 or 504: the
  gateway's forward loop treats those as no answer, and a streamed request has no retry budget
  (`internal/gateway/gateway.go`). A failed connection to the router is logged by the gateway's
  upstream transport (`internal/gateway/upstream.go`); there is no such line.
- The router logged only its start (22:43:32 UTC) and its stop when the deployment was deleted. It
  started once. The deployment's events have no `replica_unhealthy`, no `replica_probe_missed` and
  no route change, so the route fix was not exercised. No other request failed, so the router
  never stopped sending to its server.
- So the 5xx came from behind the router: the runtime proxy pod (`modal-gpu-proxy`, this harness's
  stand-in for a model server pod, never part of the product), the Modal tunnel, or vLLM. Neither
  the pod's log nor vLLM's was kept: the pod went with the cluster, and vLLM writes its log inside
  its container, which publishes the tail only after a failed start. Which of the three it was is
  **unknown**.
- One cause fits, and a test reproduces it. vLLM 0.34.0 closes a connection that has been idle for
  5 s (`VLLM_HTTP_TIMEOUT_KEEP_ALIVE`). The proxy's pool kept idle connections for 120 s, and Go
  does not send a POST again on a reused connection that the server closed while the request was
  on its way: the proxy answers 502. Through the tunnel the server's close arrives a round trip
  late, which widens that window. `TestAPoolThatOutlivesTheServersKeepAliveAnswers502` gets the
  502 through a relay that passes the close on late;
  `TestAPoolThatDropsIdleConnectionsFirstAnswers` gets 200 when the pool drops idle connections
  first. That attempt 3's failure was this, and not a tunnel or server error, is not shown.
- Nothing recorded points at the product. The gateway, the router and the reconciler did what they
  are built to do: a streamed request that got no answer upstream fails and is not retried.

### What changed for attempt 4

The harness only (`internal/testtools/modal-gpu-proxy`, `tools/poc-modal`); the product is
attempt 3's.

- The runtime proxy drops a connection that has been idle for 4 s, before vLLM's 5 s.
- The runtime proxy no longer caches a health check that its caller cut short. The kubelet's
  readiness probe allows 2 s, the control plane's 5 s. While the remote server's `/health` was
  slow, the proxy kept the kubelet's cut-short check as "unhealthy" for 2 s, and a control-plane
  probe in those 2 s failed at once. The first stand-in rehearsal of attempt 4 (00:04 to 00:10
  CET, before this change) showed it: 11 probes failed, each at once (at its pass's own tick,
  where a passing probe took about 60 ms), two in a row four times, so the route was withdrawn
  four times and 14,376 of the cell's 16,804 requests were refused with 404. Whether attempt 2's
  one failed probe on the real server (03:21:48) was this is unknown: that proxy's log was not
  kept. `TestACheckCutShortByItsCallerIsNotCachedForOthers` fails without the change and passes
  with it.
- The runtime proxy counts, in `/proxy/stats`, every `/v1` request it answers 502 for want of an
  answer from its remote server (time, path, error), every 5xx the remote side answered (by
  status), and every request whose caller left first.
- The harness reads every runtime proxy's counts just before and just after the cell load, and
  writes `traffic.failures_by_hop`: how many failed on the network to the remote GPU, how many the
  remote side answered with a server error, how many failed before the runtime proxy (in the
  gateway or the router), and whether the cell load is within this attempt's gate, with why. A
  failed request is recorded with the gateway's message, such as "503 Inference upstream is
  unavailable" or "404 Unknown model alias: gpu-baseline".
- The GPU server publishes its own answers to `/v1` when it stops, counted from its access log by
  method, path and status (`engine_answers` on the lease in the broker ledger).

### Planner rule set (note of 2026-11-26 00:26 CET)

Added after attempt 4 was launched from `17c9940c` (00:27 CET), before run A's result. Attempt 4
runs planner rule set v9 (`diagnosis-technique-rules-v9`, `32004faa`); attempts 1 to 3 ran v8.
v9 adds the model's memory (weight bytes, KV cache bytes per token) to a diagnosis when the model
check reads them, predicts the cache room of FP8 weights, an FP8 KV cache and both, and plans the
pair first when both are chosen. For this run's model it should not change the answer: precision
rooms are not predicted for a model with recurrent-state layers, FP8 weights are not proposed for
a model whose name says its weights are FP8, and the replay of attempt 1's recorded diagnosis
(`TestStep1RunADiagnosisYieldsARunnableOneDraftSetup`) still yields one draft token first at
`17c9940c` (run by the coordinator before the launch). The result page names the rule set the run's own record reports.

### When run B starts

Run B is launched only if both hold in run A's record:

1. Run A's first runnable proposal is speculation with one draft token (unchanged).
2. Its cell load is within this gate (`traffic.failures_by_hop.within_attempt_4_gate`): no request
   failed, or every failed request is a 503 for want of an answer upstream that a runtime proxy
   counted as a chat request it answered 502 for want of an answer from the remote server, and
   there are at most 4.

Why those may fail and run B still follow. Such a request got no answer from the engine, so the
engine's readings that the planner reads (requests running and waiting, cache use, cache blocks)
are those of the cell's load. Its worker sends its next request 0.2 s later, so at most 4 of 128
slots are empty for 0.2 s each. Readiness refuses a diagnosis only above 2% failed, and 4 of the
about 750 requests of a run A is 0.5%. The hop they fail on, a tunnel over the internet from a
laptop's cluster to Modal, is this harness's, not a deployment's.

Any of these fails the gate, and run B is not launched:

- a refusal, whatever the count: 404 (no route), 429 (admission);
- a failure no runtime proxy counted, which failed in the gateway or the router: it is explained
  first, as the route fault was;
- a 5xx the remote side answered (vLLM, or the tunnel's edge); `engine_answers` then says whether
  it was vLLM;
- a stream that ended early, or a failed connection between the harness and the gateway;
- more than 4 failures on the network to the remote GPU;
- counts that could not be read.

A `replica_probe_missed` event in the deployment's events is reported and is not a failure: the
route stays.

In run B, the product's benchmark records failed requests per setup as before, and these rules add
no gate there. The record's `network` section now holds each setup's server pod counts beside
them, read every 30 s while the campaign runs, so a setup's last seconds may be missing.

### Rehearsals

Run A's command with `--stand-in local --budget-usd 1`, on local stand-in servers, no Modal,
nothing spent, each on a fresh cluster, control plane `32004faa` (the route fix included):

| | Harness | Cell load | Failed | Placed | Health checks | Route |
|---|---|---|---|---|---|---|
| Before the health-check change (00:04 to 00:10 CET) | this attempt's, without that change | 144 s, 16,804 requests | 14,376, all 404 | all before the runtime proxy | 11 failed, 2 in a row 4 times | withdrawn 4 times |
| Clean (00:13 to 00:18) | this attempt's | 53 s, 969 requests | 0 | none to place | 1 missed | kept |
| One request dropped (00:18 to 00:23) | this attempt's, `STAND_IN_DROP_CHAT_REQUESTS=300` | 54 s, 966 requests | 1, "503 Inference upstream is unavailable" | on the network to the remote GPU (the proxy's EOF at 23:21:37 UTC) | 2 missed | kept |

The dropped request reached the harness exactly as attempt 3's failure did: a 503 for want of an
answer upstream. This time the runtime proxy's own count placed it, and the cell load was within
the gate. In both later rehearsals the planner was asked 45 s into the hold and answered as on
every stand-in (no cache layout reported, nothing built).

Records: `docs/testing/evidence/poc-modal-stand-in-health-cache-before-fix-2026-11-26.json`,
`docs/testing/evidence/poc-modal-stand-in-planner-only-attempt-4-2026-11-26.json` and
`docs/testing/evidence/poc-modal-stand-in-planner-only-attempt-4-dropped-request-2026-11-26.json`.
Not rehearsed: `engine_answers`, which only a server on Modal writes (a unit test reads an access
log the way the server writes it).

### Commands

As in "Commands" of attempt 3, from a clean worktree at the commit that holds this section. Run B's
cap is what the track has left after attempts 1 to 3 and a fourth run A ($31.30 - $2.04 - $1.50
= $27.76):

```bash
python3 tools/poc-modal/poc_modal.py ask-planner --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --current-setup publisher-speculation --engine vllm-0.34.0 --budget-usd 1.5
```

```bash
python3 tools/poc-modal/poc_modal.py run --cell short/128 --cells "$CORPUS/fast-track-map-cells-h100.json" --cells-sha256 2b90a36ae706d516ca1674a999366bb5dc0e134acdf95a1915fa76a909918af4 --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --corpus "$CORPUS/fast-track-v1-corpus.json" --current-setup publisher-speculation --plain-arm --planner-setups 1 --engine vllm-0.34.0 --driver 620.70.06 --budget-usd 27
```

Run B's estimate stays $22 to $25; its cap of $27 leaves no room for a rerun of B.
