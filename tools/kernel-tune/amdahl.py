"""Rung 1 of the kernel-tuning ladder: how much of a decode step the fused-MoE kernel takes,
and what a faster kernel could give end to end (Amdahl), before any money is spent.

Two modes:

- `estimate`: bytes a decode step must read, by part, from the model's config and the load.
  Decode at these loads is memory-bound, so each part's share of the bytes stands in for its
  share of the time. It is an estimate, labelled as one: kernels differ in how close they get
  to peak bandwidth, and prefill steps are left out.
- `measured`: the fused-MoE kernel's own time at the step size the load runs (the tuner's
  timing of the engine's default) times the MoE layers, over the step time the window measured
  (requests at once over output tokens per second). That share is measured, with one
  approximation said beside it: every step is taken to be a decode step of that size.

The gate is the kernel planner's (internal/kernelplanner): a hotspot of at least 5% of device
time and a ceiling of at least 1.05x end to end.

Standard library only.
"""

from __future__ import annotations

import argparse
import json
import math

HOTSPOT_GATE = 0.05   # kernelplanner MinHotspotFraction
CEILING_GATE = 1.05   # kernelplanner MinMaxEndToEndSpeedup
SPEEDUPS = (1.05, 1.10, 1.20)


def amdahl(share: float, speedup: float) -> float:
    """End-to-end speedup when a part taking `share` of the time gets `speedup` faster."""
    return 1 / ((1 - share) + share / speedup)


def expected_distinct(experts: int, draws: int) -> float:
    return experts * (1 - (1 - 1 / experts) ** draws)


def text_config(config: dict) -> dict:
    return config.get("text_config") or config


def estimate(config: dict, parameters: dict, running: int, mean_context: float, weight_bytes: float = 1.0) -> dict:
    """Bytes one decode step reads at `running` requests at once, by part. `parameters` is the
    Hugging Face safetensors count by dtype; expert weights are taken to be FP8 (one byte)."""
    t = text_config(config)
    layers, hidden = t["num_hidden_layers"], t["hidden_size"]
    experts, topk, inter = t["num_experts"], t["num_experts_per_tok"], t["moe_intermediate_size"]
    per_expert = 3 * hidden * inter * weight_bytes
    all_experts = layers * experts * 3 * hidden * inter
    distinct = expected_distinct(experts, running * topk)
    moe = layers * distinct * per_expert
    fp8 = parameters.get("F8_E4M3", 0) - all_experts
    bf16 = parameters.get("BF16", 0)
    vocab = t["vocab_size"]
    embedding = vocab * hidden  # read a few rows a step, not the table
    lm_head = vocab * hidden * 2  # BF16, read whole every step
    # The rest of BF16 (norms, routers, any vision tower) is left out: small, or not read when
    # serving text only.
    dense = max(0, fp8) + lm_head
    interval = t.get("full_attention_interval")
    attention_layers = layers // interval if interval else layers
    kv_per_token = attention_layers * t["num_key_value_heads"] * t["head_dim"] * 2 * 2  # K and V, BF16
    kv = running * mean_context * kv_per_token
    state = 0.0
    if interval:
        linear = layers - attention_layers
        state_bytes = 4 if t.get("mamba_ssm_dtype", "float32") == "float32" else 2
        per_layer = t["linear_num_value_heads"] * t["linear_key_head_dim"] * t["linear_value_head_dim"] * state_bytes
        state = running * linear * per_layer * 2  # read and written every step
    total = moe + dense + kv + state
    share = moe / total
    return {"running": running, "mean_context_tokens": mean_context, "experts_touched": round(distinct, 1),
            "bytes": {"moe_experts": round(moe), "dense_weights": round(dense), "kv_cache": round(kv), "recurrent_state": round(state)},
            "moe_share": round(share, 3), "bf16_params_left_out": max(0, bf16 - 2 * embedding),
            "end_to_end": {f"{speedup:.2f}x kernel": round(amdahl(share, speedup), 3) for speedup in SPEEDUPS},
            "gate": {"hotspot_at_least_5_percent": share >= HOTSPOT_GATE, "ceiling_at_least_1_05_needs_kernel_speedup": round(needed_speedup(share, CEILING_GATE), 3)}}


def needed_speedup(share: float, target: float) -> float:
    """The kernel speedup at which a part of `share` gives `target` end to end (inf if none)."""
    denominator = 1 / target - (1 - share)
    return math.inf if denominator <= 0 else share / denominator


def measured(report: dict, detail: dict, moe_layers: int) -> list[dict]:
    """Measured share per cell of a window: the default MoE kernel's time at the nearest tuned
    step size, times the MoE layers, over the step time (at once / output tokens per second),
    taken from each control run and averaged."""
    keys = {int(key): row for key, row in report.get("keys", {}).items() if row.get("engine_us")}
    out = []
    cells = {}
    for run in detail.get("runs", []):
        if run.get("arm") != "control":
            continue
        for cell, summary in run.get("cells", {}).items():
            if summary.get("output_token_throughput"):
                cells.setdefault(cell, []).append((summary["concurrency"], summary["output_token_throughput"]))
    for cell, rows in sorted(cells.items()):
        at_once = rows[0][0]
        throughput = sum(value for _, value in rows) / len(rows)
        step_us = at_once / throughput * 1e6
        key = min(keys, key=lambda k: abs(k - at_once)) if keys else None
        if key is None:
            out.append({"cell": cell, "moe_share": None, "reason": "the tuning report has no engine timing"})
            continue
        moe_us = moe_layers * keys[key]["engine_us"]
        share = moe_us / step_us
        out.append({"cell": cell, "requests_at_once": at_once, "output_tokens_per_second": round(throughput, 1), "step_us": round(step_us, 1),
                    "kernel_step_size": key, "default_kernel_us": keys[key]["engine_us"], "moe_us_per_step": round(moe_us, 1), "moe_share": round(share, 3),
                    "approximation": "every step taken as a decode step of the load's size; prefill steps make the true share lower",
                    "end_to_end_at_tuned_kernel": round(amdahl(min(share, 1.0), keys[key]["engine_us"] / keys[key]["best_us"]), 3) if keys[key].get("best_us") else None})
    return out


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="mode", required=True)
    est = sub.add_parser("estimate")
    est.add_argument("--config", required=True)
    est.add_argument("--parameters", required=True, help='safetensors counts, e.g. {"BF16": 1, "F8_E4M3": 2}')
    est.add_argument("--running", type=int, nargs="+", default=[32, 128])
    est.add_argument("--mean-context", type=float, default=210.0)
    meas = sub.add_parser("measured")
    meas.add_argument("--report", required=True)
    meas.add_argument("--detail", required=True)
    meas.add_argument("--moe-layers", type=int, required=True)
    args = parser.parse_args()
    if args.mode == "estimate":
        config = json.load(open(args.config))
        parameters = json.loads(args.parameters)
        print(json.dumps([estimate(config, parameters, running, args.mean_context) for running in args.running], indent=1))
    else:
        print(json.dumps(measured(json.load(open(args.report)), json.load(open(args.detail)), args.moe_layers), indent=1))


if __name__ == "__main__":
    main()
