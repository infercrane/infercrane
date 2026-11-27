# vLLM 0.34.0 with the state-by-draft patch

The engine image of the technique `adaptive-speculation-one-engine`
([docs](../../docs/features/adaptive-speculation.mdx)). It is vLLM 0.34.0
(`vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484`) with
InferCrane's patch `infercrane-state-by-draft-v4` applied at build.

vLLM's per-batch draft schedule (`num_speculative_tokens_per_batch_size`) lowers the draft length as
the batch grows. But on a model with recurrent-state layers, every request still reserves the state
of the longest draft length. The patch does five things:

1. Each request reserves the state of the draft length the schedule gives the batch it joins.
2. A step drafts no more than the smallest reservation among the requests it schedules.
3. Every 50 admissions, the scheduler logs the running requests by reserved draft tokens.
4. (v3) vLLM's Prometheus logger exports the running requests by reserved draft tokens as gauges on
   `/metrics`, so the product records the mix with the engine's other metrics:
   - `vllm:infercrane_running_reserving_0_drafts`, `_1_drafts` and `_3_drafts`;
   - `vllm:infercrane_running_mixed_reservations`, the smaller of those reserving three and those
     reserving fewer. It is above zero only when both ran together.

   This goes through vLLM's Python front end, the product's default. With `VLLM_USE_RUST_FRONTEND=1`
   the gauges are not served, and the technique proposes no setup with engine environment.
5. (v4) When that logger starts, the engine runs the image's verifier itself: the receipt and the
   sha256 of all four patched files. It exports the answer as
   `vllm:infercrane_state_by_draft_receipt_verified`, 1 or 0. The product reads this gauge, so the
   evidence is the engine's own on any deploy path, including a proxy in front of a remote engine.

It covers recurrent-state cache mode `none`, so the engine runs with prefix caching off.

v3 and v4 keep the reservation edits of v2, the patch the H100 proofs ran, byte for byte: v3 adds
the gauges and v4 the engine's own receipt check. `test_state_by_draft.py` checks that against a
digest of v2's edits.

## Files

| File | What it does |
|---|---|
| `patch_state_by_draft.py` | Applies the patch. It refuses any vLLM source whose sha256 it does not know, applies each edit exactly once, and writes a receipt. Its own sha256 (`53e1b1ed…`) is pinned in `internal/optimizer`, so an edit fails a test until the pin is reviewed. |
| `check_state_by_draft.py` | CPU check with vLLM's own block manager: each request holds 1 + its reserved draft tokens' state blocks, and the scheduler carries every edit. |
| `verify_state_by_draft.py` | Checks an image at build and before the engine starts. The receipt must name the patch, and all four vLLM files must have the built sha256 pinned here. |
| `serve-state-by-draft` | The image's entrypoint and the technique's server command. It refuses a Rust front end (`VLLM_USE_RUST_FRONTEND` other than `0`), runs the verifier with the engine's own Python (`/opt/infercrane/engine-python`, written at build), then `exec vllm serve "$@"`. On a stock vLLM image the file does not exist, so the technique's setup cannot start there. |
| `Dockerfile` | Applies the patch, runs the check and the verifier, leaves the receipt read-only at `/opt/infercrane/state-by-draft-receipt.json`, and sets the entrypoint to `serve-state-by-draft`. |
| `modal_image_check.py` | Builds the image in Modal's image builder and runs the checks on a CPU container: the verifier, vLLM's import, the CPU check, the entrypoint handing its arguments to `vllm serve` (a stand-in `vllm`, since vLLM's CLI needs a GPU), its refusal of a Rust front end and of a tampered receipt, and the entrypoint's absence on stock vLLM. Nothing is pushed. |
| `test_state_by_draft.py` | Tests of the patch's edits and the verifier, without vLLM. |

## Pins

| What | sha256 |
|---|---|
| Base image | `cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484` |
| `vllm/v1/core/single_type_kv_cache_manager.py`, before / after | `a1266bb3…cfa53` / `c89ac08c…f28494b` |
| `vllm/v1/core/sched/scheduler.py`, before / after | `8f5bd5d0…9d514b41` / `f0b66f79…2cecb662` |
| `vllm/v1/metrics/stats.py`, before / after | `0357ab7a…4b849506` / `55eb30e3…f8dd310773` |
| `vllm/v1/metrics/loggers.py`, before / after | `a1b927fc…089225b6` / `dca681c5…115a6867` |

A vLLM upgrade changes the before-sha256, so the patch refuses and the build fails. The technique
is then unavailable until the patch is ported and proven again on an H100.

## Build and check

```bash
uv run --no-project --with modal==1.6.4 modal run deploy/vllm-state-by-draft/modal_image_check.py
```

```bash
python3 -m unittest deploy/vllm-state-by-draft/test_state_by_draft.py
```

Publishing the image to a registry, and the digest the product then names, are the user's call. Do
not push it from a session.

## What the lab showed

In lab runs on two H100s, on one model and a mixed-load trace, the engine gave more output within
one second of first token than the best fixed setup. Neither run showed more output overall. At 128
requests at once it served less output within one second than the plain vLLM command; the cause is
not read. Those lab runs used patch v2 and vLLM's Rust front end (`VLLM_USE_RUST_FRONTEND=1`) for
every setup; the product runs patch v4 and the Python front end. Its KL at a mixed batch sat within
the fixed setup's floor, and every greedy answer at load 1 was identical. These are in the
maintainer's run notes, not in this repository. On the
second machine, the copy of the no-speculation setup did not finish (the launching laptop slept);
the rules' missing-copy allowance of 0.75% applied.

The technique is lab-proven. The product makes no claim for it: its verdict measures one load level
per run, so it cannot yet show this gain on a customer's traffic.
