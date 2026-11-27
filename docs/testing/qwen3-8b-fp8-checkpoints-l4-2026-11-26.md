# Two published FP8 checkpoints of Qwen3-8B on one L4: answers checked on fresh servers

On 26 November 2026 two published FP8 checkpoints of `Qwen/Qwen3-8B` answered a 1,000-item task set
(GSM8K and MMLU) on three fresh NVIDIA L4 servers each, against the original BF16 model on three
of its own. An FP8 checkpoint that also quantizes the KV cache scored 1.33 points lower: 84.43%
against 85.77%, 95% interval −2.57 to −0.10 points, so by the run's rules it does not hold answers.
The model publisher's own FP8 checkpoint scored the same as the original: 85.77% against 85.77%,
interval −1.03 to +1.03 points, "no measurable difference" by the run's rules. Under InferCrane's
stricter rule for setups that change answers, which needs the lower end above −1 point, it reads
"not shown to hold within 1 point".

This page is the receipt. The rules were committed before any server started (commit `a52ee962`),
and each of the two relaunches added a section before its servers started (the last at commit
`c0af74e7`). The harness that ran it (`tools/fast-track-map`) is on an unreleased branch, so this
page records what ran; it is not a reproduction script in this release. The run applied its rules
in that harness, not through the product's campaign.

- [Packed evidence](https://github.com/infercrane/infercrane/blob/main/docs/testing/evidence/qwen3-8b-fp8-checkpoints-l4-2026-11-26.json): setups, machines, the
  engine log lines about FP8 and the KV cache, and every server's task-set result as right or
  wrong per item. No prompts or answer texts.
- [Summary](https://github.com/infercrane/infercrane/blob/main/docs/testing/evidence/qwen3-8b-fp8-checkpoints-l4-2026-11-26-summary.json): the comparison below, as
  the run's report script computed it from the nine servers' answers.

Local paths are anonymised as `<evidence>` and `<corpus>`. The third-party checkpoint is named by
its setting, not by its publisher: its repository id, served name and label are withheld, and its
revision and weight-file hashes are kept. Each JSON file names what was removed and the sha256 of
the files it was made from. The packed evidence recomputes the comparison from the servers'
answers with the run's own scoring code; it matches the summary exactly.

## What ran

| Field | Value |
|---|---|
| Date | 2026-11-26, 09:06 to 14:41 CET, in three launches (see below) |
| Original model | `Qwen/Qwen3-8B` at revision `b968826d9c46dd6066d109eabc6255188de91218`, BF16 weights. Apache-2.0 by its model card (read 2026-11-27; the run's files do not record licences). |
| The publisher's FP8 checkpoint | `Qwen/Qwen3-8B-FP8` at revision `220b46e3b2180893580a4454f21f22d3ebb187d3` (block FP8), Apache-2.0 by its model card (read 2026-11-27). On this GPU vLLM logged that it ran the checkpoint as weight-only FP8 through its Marlin kernel. |
| The FP8 checkpoint that also quantizes the KV cache | A third-party FP8 checkpoint of Qwen3-8B at revision `2cebc4c89e25abc17668c81b01dceaf3d8b914d5`, 14 files, 9,451,776,318 bytes, `config.json` sha256 `d2c9588779d49ab7805dc1054a471107a53df713a50583a7fc1ae6a708905fad`. No KV cache flag was passed; with this checkpoint vLLM stored the KV cache in FP8 (`fp8_e4m3`) and logged a KV cache scaling factor of 1.0. Its licence is not recorded here. |
| Hardware | One NVIDIA L4 (23,034 MiB) per server on Modal. Nine servers, three per setup, each in its own container, in three batches that each hold one server of every setup; they ran on eight GPUs (one GPU served the publisher's checkpoint's second server and, three hours later, the third-party checkpoint's third). The rules ask for driver 610.57.04 and take any host after two refused ones: one server ran on 610.57.04, eight on 620.70.06. Every server's GPU identifier, driver and region are in the evidence. |
| vLLM | 0.34.0, `vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484`, every setup with `--max-model-len 8192` |
| Task set | 1,000 items: 500 from `openai/gsm8k` (test split, revision `740312ad`, MIT) and 500 from `cais/mmlu` (test split, revision `c30699e8`, MIT), sha256 `83c3df7b82f8d8a3b9fa892f1fdcd81679f93646337c8b78d456a828101d72e4`. Every server answered all 1,000 with no failed request, 32 at once, temperature 0, reasoning off, answers up to 1,024 tokens. An answer is right when its last `Answer:` line gives the gold number or letter. |
| Baseline | The original model on vLLM 0.34.0's defaults. |
| Rules | `rules-l4-qwen3-8b-fp8-ckpt-confirm-2026-11-26.md`, last sha256 `a5d9ad334902be7537381c90f3e65f2e34dc4e28e69b64a1b4f07a2d7526fb1b` at commit `c0af74e7048382c93e066021f330016d45484e28`. The full text is in the appendix. |

The rules file has four committed versions; the decision rule is the same in all of them:

| Commit | sha256 | What it added |
|---|---|---|
| `a52ee962` | `9916e776bf0c3f9aee68bb251a27fbec97068037221d5037d4d7a7429a6c80b2` | The rules, written 09:04 CET and committed 09:06, before any server |
| `b9238bc5` | `9ac07be3f429d95f82f0bca3fcfcb19727b69ef880c56a0c3d33fed42a046584` | Written 10:17 CET, before the first relaunch: keep batch 1, run servers 2 and 3 again |
| `57463480` | `564496b1f7ea98f20fb73247dc93640247cd7fbb9bbfa6934ef75dca86bf1ff4` | 10:22 CET, after the first relaunch started: its spend cap and budget flag only |
| `c0af74e7` | `a5d9ad334902be7537381c90f3e65f2e34dc4e28e69b64a1b4f07a2d7526fb1b` | Written 13:11 CET, before the second relaunch of the three servers still missing |

**Three launches.** The first launch kept batch 1 (one server of each setup) and lost batch 2
when the laptop that ran it went to sleep and its app stopped. The first relaunch, from a harness
that runs its loop in the cloud, measured the current setup's servers 2 and 3 and the publisher's
checkpoint's server 2. Its other three servers got no L4 in time or hung to their cancel with no
record. The second relaunch measured those three. Each setup has three servers that answered all
1,000 items, and only those are counted.

## Results

The task set, scored item by item: each item's score on a side is the share of that side's three
servers that got it right, and the interval is 1.96 standard errors of the per-item differences.

| Setup | Servers right | Score | Difference against the original (95% interval) | Items the original's servers got right more often / the setup's did | Verdict by the rules (3-point margin) | InferCrane's 1-point rule |
|---|---|---:|---|---|---|---|
| Original model, BF16 | 86.1%, 85.2%, 86.0% | 85.77% | | | | |
| FP8 checkpoint that also quantizes the KV cache | 84.3%, 84.2%, 84.8% | 84.43% | −1.33 points (−2.57 to −0.10) | 71 / 48 | worse: does not hold answers | worse |
| The publisher's FP8 checkpoint | 85.8%, 85.6%, 85.9% | 85.77% | 0.00 points (−1.03 to +1.03) | 34 / 45 | no measurable difference: holds answers | not shown to hold within 1 point |

By the run's rules, a checkpoint whose interval lies inside ±3 points holds answers and is confirmed
on answers; one whose interval lies below zero is "worse". InferCrane's rule for a setup that
changes answers (another checkpoint or precision) counts answers as held only when the lower end of
the interval lies above −1 point ([semantic quality](/features/semantic-quality)). The publisher's
checkpoint misses that by 0.03 points, so the product would not promote it on this evidence. That
is "no measurable difference", not "identical".

Exact text is not a usable test here either. Two servers of the original gave word-for-word the same
answer on 324 to 333 of the 1,000 items; a server of the original and one of the publisher's
checkpoint, on 108 to 120; a server of the original and one of the FP8 checkpoint that also
quantizes the KV cache, on 20 to 32.

What the engine logged at startup, from the kept log lines (in the evidence for every server):

| Setup | KV cache | Logged |
|---|---:|---|
| Original model | 26,112 tokens | |
| The publisher's FP8 checkpoint | 71,680 tokens | "Your GPU does not have native support for FP8 computation", then weight-only FP8 through the Marlin kernel |
| FP8 checkpoint that also quantizes the KV cache | 134,144 tokens | the KV cache stored as `fp8_e4m3` with a scaling factor of 1.0; no q scaling factor in the checkpoint, so k's is used |

## What this does and doesn't show

It shows that on one NVIDIA L4 with vLLM 0.34.0, on this task set, the FP8 checkpoint that also
quantizes the KV cache lost about 1.3 points against the original on three servers per setup, and
the publisher's FP8 checkpoint showed no measurable difference, inside an interval just wider than
±1 point.

It does not show:

- Speed. This run measured answers only. The checkpoints' speed was measured in a separate screen on
  one machine, which is not published and is not paired with these answers here.
- Which part of the third-party checkpoint costs the points: its FP8 weights, its FP8 KV cache, or
  how its scales were set. Nothing here separates them.
- That the publisher's checkpoint holds within 1 point. Its interval reaches −1.03.
- Answer quality on other tasks, on answers longer than 1,024 tokens, or on another model, GPU or
  engine version.
- That the product refused or promoted either checkpoint: the product's campaign did not run these
  servers.

FP8 quantized by vLLM at load, measured with speed on three fresh servers per setup, is in
[the FP8-at-load receipt](/testing/qwen3-8b-fp8-l4-confirmation-2026-11-26).

## Commands

From `<worktree>` at the commit named for each launch, with the spend-limit flags left out. The
first launch, at `a52ee962`:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1
```

The first relaunch, at `b9238bc5`:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run --detach tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1-s23 --only l4-8b-defaults:2,l4-8b-fp8-ckpt-fp8-kv:2,l4-8b-fp8-ckpt-qwen:2,l4-8b-defaults:3,l4-8b-fp8-ckpt-fp8-kv:3,l4-8b-fp8-ckpt-qwen:3
```

The second relaunch, at `c0af74e7`:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run --detach tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1-s3b --only l4-8b-fp8-ckpt-qwen:3,l4-8b-fp8-ckpt-fp8-kv:2,l4-8b-fp8-ckpt-fp8-kv:3
```

The third-party checkpoint's setup id is published as `l4-8b-fp8-ckpt-fp8-kv`; the harness's own
id names its publisher. The nine counted records were put in one folder, and the summary came from:

```bash
python3 tools/fast-track-map/confirm_report.py <evidence> --plan l4-qwen3-8b-fp8-ckpt-confirm --tasks <corpus>/fast-track-quality-v1.json
```

## Appendix: the rules as committed

The rules below are the last committed version of the file, with headings moved down two levels to
fit this page, the spend amounts, rates and budget flags removed, and the third-party checkpoint's
publisher replaced by "[the third-party checkpoint]" and its setup id by `l4-8b-fp8-ckpt-fp8-kv`.
The files they name (`setups.py`, `confirm_report.py`, `quality.py`, the screen's rules) are on the
same unreleased branch.

### Rules: the task-set confirmation of two published FP8 checkpoints (Qwen3-8B, L4)

Written 2026-11-26 09:04 CET, before any server of this run starts. Internal; nothing here is
published. Plan `l4-qwen3-8b-fp8-ckpt-confirm` in `setups.py`. The coordinator asked for it on
26 Nov, after the checkpoint screen `l4-8b-fp8ckpt-1`.

#### Why

Step 2 refused FP8 quantized at load on answers. On three fresh servers each it scored −1.37 points
against the current setup on the 1,000-item task set, interval −2.32 to −0.42, so it was worse. The
question here is whether a checkpoint its publisher calibrated holds answers where FP8 at load did
not.

The screen `l4-8b-fp8ckpt-1` (rules `rules-l4-qwen3-8b-fp8-checkpoints-2026-11-26.md`) put both
checkpoints ahead of the current setup in both cells. On one run each, every setup read "no
measurable difference" on the task set. One run each is too few to settle answers: FP8 at load
read +1.20 points there, against −1.37 on step 2's fresh servers, and the copy read +0.70. So the
screen's decision rule sends both checkpoints here.

Speed is not the question; the screen showed it. This run measures answers only.

#### What runs

**Three setups, each on three fresh L4s.** Qwen3-8B family on vLLM 0.34.0, every setup with
`--max-model-len 8192` and the engine's default KV cache, as on the screen:

1. **The current setup:** `Qwen/Qwen3-8B` at `b968826d`, vLLM defaults.
2. **[The third-party checkpoint]:** [a third-party FP8 checkpoint of Qwen3-8B, repository id withheld for publication] at `2cebc4c8`.
3. **Qwen's checkpoint:** `Qwen/Qwen3-8B-FP8` at `220b46e3` (block FP8).

Servers start in three batches. Each batch holds one server of each setup, so no setup has all its
servers at one time of day.

**Where a server runs:** step 2's rule. Each server must be on NVIDIA driver 610.57.04, and a server
on another host is started again up to twice. After that it is taken on whatever host it gets, and
the driver is named.

**Task set:** the 1,000 items (GSM8K and MMLU), 32 at once, reasoning off, on every server. No
cells.

#### What it decides

Each checkpoint's answers are compared with the current setup's over three servers each, item by
item, by `quality.py`. This is the rule that refused FP8 at load in step 2: the share right, the
paired difference, its 95% interval, and the 3-point margin. A setup with fewer than three servers
that answered the task set is "unknown".

A checkpoint **holds answers** when its verdict is "no measurable difference" or "better". It is
then confirmed on answers. Its speed was shown on one machine (the screen), and it is not confirmed
on speed by this run.

A checkpoint whose verdict is "worse" does not hold answers, as FP8 at load did not.

**KL is not read here.** The task score decides. For later evidence: the tokenizer guard allows a
divergence reading for Qwen's checkpoint against the base model, which lists the same tokenizer
files with the same content. For [the third-party checkpoint] it does not: its `tokenizer_config.json`
differs and its chat template is a separate file, so KL would be "unknown".

#### Money and limits

[Removed for publication: the rate, the estimate and the spend cap. The limits that are not money stay:]

**Hard limits:**
- each server stops itself at 22 minutes;
- the caller cancels it 2 minutes later;
- the function timeout is 24 minutes;
- the measure action refuses a budget below the cap.

Waiting for a GPU is not billed, and the
caller gives up after 30 minutes without one. Before the GPUs start, one CPU container checks that
every function exists inside a container. The weights of all three models are
already on the volume from the screen.

#### Commands

From a clean `feat/fast-track-fp8-checkpoints` worktree at the commit named in the launch request:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1
python3 tools/fast-track-map/confirm_report.py .infercrane/fast-track-map/l4-8b-fp8ckpt-confirm-1 --plan l4-qwen3-8b-fp8-ckpt-confirm --tasks <corpus directory>/fast-track-quality-v1.json
```

#### What it cannot show

- **Speed on fresh servers.** No cells run here.
- **FP8 at load, again.** Step 2 measured it; it is not in this run.
- **An FP8 KV cache with [the third-party checkpoint's] calibrated scales.** It is not asked for.
- **Other models, GPUs or engines.**
- **Long answers.** The task set's answers are short worked solutions and choices.

#### Relaunch after the laptop slept

Written 2026-11-26 10:17 CET, before any server of the relaunch starts.

The first launch, `l4-8b-fp8ckpt-confirm-1`, started at 09:06:40. The laptop went to sleep at
09:52:24. Modal stops an app launched without `--detach` when its client disconnects, so it
stopped the run between 09:55 and 09:56.

**Kept: batch 1.** The first server of each setup answered all 1,000 items, with no failed
request:

| Server | Driver | Task set |
|---|---|---|
| current setup, s1 | 620.70.06, taken after two restarts | 697 s |
| [the third-party checkpoint], s1 | 610.57.04 | 394 s |
| Qwen's checkpoint, s1 | 620.70.06, taken after two restarts | 422 s |

The driver rule allows both: up to two restarts, then any host, with the driver named.

**Lost: batch 2.** No server of batch 2 answered the task set. The current setup's and [the third-party
checkpoint's] last records are driver refusals. Qwen's had started on driver 610.57.04 and had not reached the
task set.

**The relaunch** runs servers 2 and 3 of each setup as batches 2 and 3, under the run ID
`l4-8b-fp8ckpt-confirm-1-s23`. A new ID keeps old status entries from cancelling reused tags,
as in step 2's s3 rerun. Everything else is as above: the same setups, rule, task set and
limits.

The report reads s1 from the first launch and s2 and s3 from the relaunch, in one folder. A
setup still needs three servers that answered the task set, or it is "unknown". The first
launch's batch 2 records are not read.

The harness now runs its loop in the cloud and is launched with `modal run --detach`, so a
laptop that sleeps no longer stops a run. Each server's result is on the volume, and
`--action fetch` brings the folder back.

**Money.** [Removed for publication.]

**Commands.** From a clean worktree of `feat/fast-track-fp8-checkpoints` at the commit named in
the launch request:

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run --detach tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1-s23 --only l4-8b-defaults:2,l4-8b-fp8-ckpt-fp8-kv:2,l4-8b-fp8-ckpt-qwen:2,l4-8b-defaults:3,l4-8b-fp8-ckpt-fp8-kv:3,l4-8b-fp8-ckpt-qwen:3
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run tools/fast-track-map/fast_track.py --action fetch --run-id l4-8b-fp8ckpt-confirm-1-s23
cp <first launch folder>/l4-8b-*-s1.json .infercrane/fast-track-map/l4-8b-fp8ckpt-confirm-1-s23/
python3 tools/fast-track-map/confirm_report.py .infercrane/fast-track-map/l4-8b-fp8ckpt-confirm-1-s23 --plan l4-qwen3-8b-fp8-ckpt-confirm --tasks <corpus directory>/fast-track-quality-v1.json
```

The first launch's folder is the launching worktree's
`.infercrane/fast-track-map/l4-8b-fp8ckpt-confirm-1/`. `--action cancel --run-id
l4-8b-fp8ckpt-confirm-1-s23` stops the relaunch.

#### Second relaunch: the three servers still missing

Written 2026-11-26 13:11 CET, before any server of it starts.

`l4-8b-fp8ckpt-confirm-1-s23` ended at 13:11 with no loop error. Modal was short of L4s:

- **The current setup:** s2 and s3 answered all 1,000 items. It now has three servers with s1.
- **Qwen's checkpoint:** s2 answered. Its s3 got no L4 within the 30-minute queue limit.
- **[The third-party checkpoint]:** s2 started and ran to its 24-minute cancel with no record. Its s3 got
  no L4 within 30 minutes.

So Qwen's checkpoint has two servers and [the third-party checkpoint] one, and both read "unknown" under the rule
above. That was checked with `confirm_report.py` at 13:11.

**This relaunch** runs only Qwen's s3 and [the third-party checkpoint's] s2 and s3, under the run ID
`l4-8b-fp8ckpt-confirm-1-s3b`, from the harness at the commit named in the launch request. That
harness keeps a hung server's record, refuses a preempted restart, and rounds its cap up to the
cent. Everything else is as above.

The report reads s1 from the first launch, the rest of `-s23`, and these servers, in one folder.

**Money.** [Removed for publication.]

```bash
FAST_TRACK_PLAN=l4-qwen3-8b-fp8-ckpt-confirm uv run --no-project --with modal==1.6.4 modal run --detach tools/fast-track-map/fast_track.py --action measure --run-id l4-8b-fp8ckpt-confirm-1-s3b --only l4-8b-fp8-ckpt-qwen:3,l4-8b-fp8-ckpt-fp8-kv:2,l4-8b-fp8-ckpt-fp8-kv:3
```
