# Rules: the product's band study on one L4 (Qwen3-8B at 16 and 64 requests at once)

Written 2026-11-26 06:23 CET, before any server of this run starts. Internal; nothing here is
published. The coordinator decided on 26 Nov: the product's band study runs on L4 with the small
model first, two campaigns at 16 and 64, and an H100 band study only if this one passes and money
is left.

## What it checks

The product path, not a gain:

1. **The planner at each load.** It is asked while the cell's own requests run.
2. **A campaign at a load no benchmark profile holds.** It measures at the proposal's
   `--target-concurrency` (core change on `feat/optimizer-useful-output`, `measuredConcurrency`).
3. **The judge on L4.** It runs its paired turns, the copy of the current setup, and the
   fresh-server confirmation.
4. **The band study over the two finished campaigns.** It uses `infercrane optimize bands`, the
   same read as `GET /api/v1/optimization/band-study`.

It is also the product path for Friday evening's system test.

## What runs

- **Model and engine:** Qwen3-8B (`b968826d`, BF16) on vLLM 0.34.0. The current setup is vLLM's
  defaults plus `--max-model-len 8192`, as in the fast-track map's L4 runs.
- **Hardware:** one L4 per server, on NVIDIA driver 610.57.04 (`--driver`), the one the map's L4
  runs had.
- **Cells:** from `fast-track-map-cells-l4-rehearsal.json` (sha256 `a52c1fab...62b2d`), the same
  two cells as the harness's L4 rehearsal:
  - `short/16`: prompt set v1;
  - `short/64`: prompt set v2, the first 2,500 prompts.
- **Text:** the campaign's requests take their text from `fast-track-v1-corpus-qwen3-8b.json`
  (sha256 `62b3ad15...03dbf`). That is the product path's corpus, counted with Qwen3-8B's
  tokenizer.

Per load, one `poc_modal.py run --cell`:

1. **The planner phase.** One server runs the current setup. The cell's own requests run at the
   cell's load, and the planner is asked after the warm-up.
2. **The campaign.** It measures three setups:
   - the current setup;
   - the planner's first setup an L4 can run (`--planner-setups 1`);
   - the copy (`--seed 1`).

   Every measured run replays the recorded shape of the cell's requests: 64 of them at 16
   (`--cell-requests 64`, four rounds) and 192 at 64 (`--cell-requests 192`, three rounds). A
   setup that gains is measured again on fresh servers.

   As in step 1, the recorded shape does not keep the cells' reasoning switch. The campaign's
   requests leave reasoning on, each capped at its recorded length.
3. **The band study.** After both campaigns, `poc_modal.py bands --campaigns ID16,ID64`.

## The pass bar

| Bar | Passes when |
|---|---|
| Planner asked at both loads | In each run the planner answers during its cell's load, and no cell request fails. |
| Both campaigns complete | Each campaign ends `completed`, and every measured server ran on 610.57.04. |
| The copy does not win | In neither campaign. |
| Band study read | `optimize bands` returns a study over both runs, with bands `short/16` and `short/64`. Each band is decided `winner` or `keep_current`, and has a reason. |

A band that is `unknown` or `no_decision` fails the last bar, and its reason is recorded. Whether
the product and the harness rehearsal (`l4-8b-rehearsal-2`, same cells) name the same setup per
band is recorded as a finding, not a bar.

## Money

**Rate:** $0.000222 a second for the L4, plus 4 cores and 24 GiB, which is $0.00032768 a second
($1.18 an hour) per server at list price.

**Estimate per campaign.** From the map's L4 numbers, a short request takes about 20 s. So a
measured run takes about 95 s at 16 and 87 s at 64, warm-ups about 60 s, and one setup's turns
about 15 to 16 minutes.

- **Server time:** about 7 server-minutes for the planner phase, then the three servers until the
  ranking (about 40 minutes). A gain adds two fresh servers for about 20 minutes, while the first
  three are kept.
- **At 16:** about $2.50 without a gain, about $4.50 with one confirmed gain.
- **At 64:** about $2.40 and about $4.30.
- **Both:** $4.90 to $8.80. The coordinator expected $3 to $6. The difference is the judge's work:
  about 33 measured runs and a fresh-server confirmation per campaign, with the servers held until
  the ranking. That is the product as it ships.

**Hard cap: $6.50 per campaign** (`--budget-usd 6.50`), **$13 for both.** The broker refuses a new
server the cap cannot cover, and stops every server when its estimate reaches the cap. Modal's
billing report is the authority. A host on another driver is refused before the model loads, with
four tries a start.

**Free steps before:**
- `download` stages the weights in a CPU container. It does nothing when they are on the volume
  already, which the map's L4 runs left them.
- A stand-in rehearsal of the whole path on the laptop costs nothing, but needs the kind cluster.
  It runs only when the coordinator's queue gives the laptop to it.

## Commands

From a clean worktree at the commit named in the launch request, with
`CORPUS=<corpus directory>` (the local path was anonymised; the rules are otherwise unchanged) and
`L4="--model Qwen/Qwen3-8B --gpu L4 --engine vllm-0.34.0"`:

```bash
python3 tools/poc-modal/poc_modal.py up --corpus "$CORPUS/fast-track-v1-corpus-qwen3-8b.json" $L4
python3 tools/poc-modal/poc_modal.py download $L4
python3 tools/poc-modal/poc_modal.py run --cell short/16 --cells "$CORPUS/fast-track-map-cells-l4-rehearsal.json" --cells-sha256 a52c1fab3a1c3cebc295e5f89507a5e37d5e7f0dc68f10c23f28cf20b2962b2d --cell-prompts "$CORPUS/fast-track-prompts-v1.json" --corpus "$CORPUS/fast-track-v1-corpus-qwen3-8b.json" --cell-requests 64 --planner-setups 1 --driver 610.57.04 $L4 --budget-usd 6.50
python3 tools/poc-modal/poc_modal.py run --cell short/64 --cells "$CORPUS/fast-track-map-cells-l4-rehearsal.json" --cells-sha256 a52c1fab3a1c3cebc295e5f89507a5e37d5e7f0dc68f10c23f28cf20b2962b2d --cell-prompts "$CORPUS/fast-track-prompts-v2.json" --corpus "$CORPUS/fast-track-v1-corpus-qwen3-8b.json" --cell-requests 192 --planner-setups 1 --driver 610.57.04 $L4 --budget-usd 6.50
python3 tools/poc-modal/poc_modal.py bands --campaigns CAMPAIGN_AT_16,CAMPAIGN_AT_64
```

The stand-in rehearsal is the same two `run` commands with `--stand-in local` and no `--driver`,
then `bands`.

## What it cannot show

- **A gain on production traffic, or on another model, GPU or engine.**
- **The H100.** The judge's machine allowance floor was measured there; here the copy measures
  this run's own.
- **Answer quality beyond the product's built-in check.** No task set is run here.
- **Loads between and beyond 16 and 64.** The band table gives the higher band's setup to a load
  between them.
