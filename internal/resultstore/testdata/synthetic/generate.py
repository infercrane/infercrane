#!/usr/bin/env python3
"""Synthetic measured results for the results store's and the prior's tests.

Every number here is made up. The files have the shape of real runs (planner-gym
cases files, a benchmark runner run directory, rules' answers) so that the tests
exercise every path: a winner that moves with load, near ties, setups that serve
nothing within the first-token limit, a cell whose servers disagree wildly,
setups measured in some cells only, two engines, rules' answers that cannot
start, change nothing or make a setup no run measured, and cells to hold out.
No real run's result, ID or time is in them.

    python3 internal/resultstore/testdata/synthetic/generate.py          # write
    python3 internal/resultstore/testdata/synthetic/generate.py --check  # compare

The output is a function of SEED and this file only.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import random
import sys
from pathlib import Path

SEED = 20010102
HERE = Path(__file__).resolve().parent
RECORDED = {"map": "2001-01-02T03:00:00Z", "screen": "2001-01-03T03:00:00Z", "confirm": "2001-01-04T03:00:00Z",
            "two-engines": "2001-01-05T03:00:00Z", "bench": "2001-01-06T03:00:00Z"}


def digest(text: str) -> str:
    return "sha256:" + hashlib.sha256(text.encode()).hexdigest()


VLLM_IMAGE = digest("synthetic vllm image")
SGLANG_IMAGE = digest("synthetic sglang image")
COMMIT = "0123456789abcdef0123456789abcdef01234567"
HYBRID = {"id": "example/hybrid-27b-fp8", "revision": "1111111111111111111111111111111111111111", "features": ["native_mtp"]}
HYBRID_FACTS = {"id": HYBRID["id"], "architecture": "hybrid", "params_b": 27, "checkpoint": "fp8", "features": ["native_mtp"]}
DENSE = {"id": "example/dense-8b", "revision": "2222222222222222222222222222222222222222", "features": []}
DENSE_FACTS = {"id": DENSE["id"], "architecture": "dense", "params_b": 8.2, "checkpoint": "bf16"}


class Numbers:
    def __init__(self, seed: int) -> None:
        self.random = random.Random(seed)

    def spread(self, value: float, low: float = 0.001, high: float = 0.015) -> dict:
        if value <= 0:
            return {"median": 0.0, "min": 0.0, "max": 0.0, "servers": 3}
        below, above = self.random.uniform(low, high), self.random.uniform(low, high)
        return {"median": round(value, 2), "min": round(value * (1 - below), 2), "max": round(value * (1 + above), 2), "servers": 3}

    def jitter(self, value: float, size: float = 0.01) -> float:
        return value * (1 + self.random.uniform(-size, size))


# ---------------------------------------------------------------- a map: one GPU, four kinds of traffic

KINDS = {  # scale, saturation load, prompt tokens, answer tokens, gain factor for speculation
    "short": (80.0, 120.0, 40, 270, 1.0),
    "prefix": (70.0, 40.0, 3860, 240, 0.4),
    "long": (28.0, 30.0, 3900, 295, 0.4),
    "output": (75.0, 110.0, 830, 1380, 1.0),
}
MAP_CELLS = [("short", 4, "all"), ("short", 32, "all"), ("short", 48, "three"), ("short", 64, "three"), ("short", 96, "three"), ("short", 128, "all"),
             ("short", 32, "reasoning"), ("prefix", 4, "all"), ("prefix", 32, "all"), ("prefix", 128, "all"), ("long", 32, "all"), ("long", 128, "all"),
             ("output", 4, "all"), ("output", 32, "all"), ("output", 128, "all")]
MAP_SETUPS = {
    "plain": {"label": "The model's plain command", "drafts": 0, "kv_cache_dtype": "auto", "blocks": 860, "block_tokens": 832, "state_page_bytes": 3407872},
    "three-drafts": {"label": "Plus speculation, 3 draft tokens", "drafts": 3, "kv_cache_dtype": "auto", "blocks": 780, "block_tokens": 832, "state_page_bytes": 3407872},
    "one-draft": {"label": "The same speculation with 1 draft token", "drafts": 1, "kv_cache_dtype": "auto", "blocks": 790, "block_tokens": 832, "state_page_bytes": 3407872},
    "three-drafts-fp8-kv": {"label": "3 draft tokens plus an FP8 KV cache", "drafts": 3, "kv_cache_dtype": "fp8", "blocks": 810, "block_tokens": 1600, "state_page_bytes": 3276800},
    "plain-fp8-kv": {"label": "The plain command plus an FP8 KV cache (never measured)", "drafts": 0, "kv_cache_dtype": "fp8"},
}


def map_gain(setup: str, kind: str, load: int) -> float:
    """Speculation gains shrink as load grows: three drafts lead at low load, one
    draft from about 48, and the plain command beats three drafts at 128."""
    level = math.log2(load) - 2
    factor = KINDS[kind][4]
    three, one = 0.95 - 0.21 * level, 0.55 - 0.08 * level
    if kind == "prefix" and load >= 128:
        return {"plain": 0.0, "three-drafts": 0.012, "one-draft": 0.003, "three-drafts-fp8-kv": 0.033}[setup]
    return {"plain": 0.0, "three-drafts": factor * three, "one-draft": factor * one,
            "three-drafts-fp8-kv": factor * three + (0.01 if load < 128 else 0.06)}[setup]


def map_useful(setup: str, kind: str, load: int, output: float) -> float:
    """Output within a 1 s first token: three drafts queue past it from 96 on short
    traffic and at 128 on long answers; prompt-heavy traffic at 128 serves almost
    nothing in time."""
    if kind == "long" and load >= 128:
        return 0.0
    if kind == "prefix" and load >= 128:
        return {"plain": 0.86, "three-drafts": 0.0, "one-draft": 0.21, "three-drafts-fp8-kv": 1.09}[setup]
    if setup in ("three-drafts", "three-drafts-fp8-kv") and ((kind == "short" and load >= 96) or (kind == "output" and load >= 128)):
        return 0.0
    if setup == "one-draft" and kind == "short" and load >= 128:
        return output * 0.5
    if setup == "one-draft" and kind == "output" and load >= 128:
        return 6.67
    if setup != "plain" and kind == "long":
        return output * 0.975
    return output


def map_cases(numbers: Numbers) -> dict:
    cells = []
    for kind, load, which in MAP_CELLS:
        scale, saturation, prompt, answer, _ = KINDS[kind]
        plain = scale * load / (1 + load / saturation)
        setups = {"all": ["plain", "three-drafts", "one-draft", "three-drafts-fp8-kv"], "three": ["plain", "three-drafts", "one-draft"],
                  "reasoning": ["plain", "three-drafts"]}[which]
        measured = {}
        for setup in setups:
            gain = map_gain(setup, kind, load) * (0.7 if which == "reasoning" else 1.0)
            output = numbers.jitter(plain * (1 + gain), 0.004)
            useful = map_useful(setup, kind, load, output)
            drafts = MAP_SETUPS[setup]["drafts"]
            room = MAP_SETUPS[setup]["blocks"] / (4 + 3 * drafts)
            running = round(min(load, room) * 0.98, 4)
            waiting = round(max(0.0, load - room), 4)
            ttft95 = 80.0 + 40 * load / 32 + (5000.0 if waiting > 0 else 0.0)
            measured[setup] = {
                "output": numbers.spread(output), "useful_output": numbers.spread(useful),
                "ttft_p50_ms": round(ttft95 * 0.6, 2), "ttft_p95_ms": round(ttft95, 2),
                "window": {"requests": float(load * 3 + numbers.random.randint(0, 9)), "running": running, "waiting": waiting,
                           "cache_use_max": round(min(0.99, 0.02 + load / 140), 4), "prompt_tokens_mean": float(prompt), "completion_tokens_mean": float(answer),
                           "latency_p95_ms": round(answer * 13.5, 2), "generation_p95_ms": round(answer * 13.2, 2),
                           "prefix_cache_hit_rate": 0.75 if kind == "prefix" and load < 128 else 0.0},
            }
        key = f"{kind}/{load}" + ("/reasoning" if which == "reasoning" else "")
        built = "synthetic rule written from this cell" if (kind, load) in (("short", 32), ("short", 128), ("long", 128)) and which != "reasoning" else ""
        cells.append({"key": key, "kind": kind, "requests_at_once": load, "reasoning": "on" if which == "reasoning" else "off",
                      "run": "synthetic map", "rules_built_from": built, "measured": measured})
    return {"schema": "infercrane.planner-gym/v1", "title": "Synthetic: a hybrid model with its own draft head on one H100, every number made up",
            "model": HYBRID, "gpu": "H100", "engine": "vllm", "engine_version": "0.34.0", "max_num_seqs": 904, "first_token_limit_ms": 1000.0,
            "useful_output": "output tokens per second of requests whose first token came within the limit", "setups": MAP_SETUPS,
            "cells": cells, "product_cases": [], "built_from": [{"file": "generate.py", "sha256": "synthetic"}]}


# ---------------------------------------------------------------- a screen and its confirmation on a small GPU

SCREEN_SETUPS = {
    "defaults": {"label": "Engine defaults", "drafts": 0, "kv_cache_dtype": "auto", "blocks": 400, "block_tokens": 64},
    "fp8-weights": {"label": "FP8 weights", "drafts": 0, "kv_cache_dtype": "auto", "quantization": "fp8", "blocks": 1100, "block_tokens": 64},
    "fp8-kv": {"label": "FP8 KV cache", "drafts": 0, "kv_cache_dtype": "fp8", "blocks": 640, "block_tokens": 64},
    "fp8-both": {"label": "FP8 weights and an FP8 KV cache", "drafts": 0, "kv_cache_dtype": "fp8", "quantization": "fp8", "blocks": 2030, "block_tokens": 64},
    "ngram": {"label": "Prompt-lookup speculation", "drafts": 0, "kv_cache_dtype": "auto", "ngram": True, "blocks": 390, "block_tokens": 64},
}
# per cell: the defaults' output, each setup's multiple, and which setups serve within the limit (a share of output)
SCREEN_CELLS = {
    "short/16": (240.0, {"fp8-weights": 1.63, "fp8-kv": 1.013, "fp8-both": 1.67, "ngram": 0.983}, {}),
    "short/64": (790.0, {"fp8-weights": 1.47, "fp8-kv": 1.07, "fp8-both": 1.65, "ngram": 0.65}, {}),
    "long/4": (45.0, {"fp8-weights": 1.5, "fp8-kv": 1.06, "fp8-both": 1.65, "ngram": 1.1}, {"defaults": 0.0, "fp8-kv": 0.0, "ngram": 0.0}),
    "long/16": (62.0, {"fp8-weights": 2.04, "fp8-kv": 1.4, "fp8-both": 2.54, "ngram": 1.0},
                {"defaults": 0.0, "fp8-weights": 0.0, "fp8-kv": 0.0, "ngram": 0.0, "fp8-both": 0.61}),
    "output/16": (200.0, {"fp8-weights": 1.48, "fp8-kv": 1.08, "fp8-both": 1.67, "ngram": 0.99},
                  {"defaults": 0.69, "fp8-weights": 0.88, "fp8-kv": 0.73, "fp8-both": 0.94, "ngram": 0.69}),
}
SCREEN_SHAPE = {"short": (42, 268), "long": (3900, 295), "output": (760, 1390)}


def screen_cases(numbers: Numbers) -> dict:
    cells = []
    for key, (base, multiples, useful_share) in SCREEN_CELLS.items():
        kind, load = key.split("/")
        # One machine: a setup is ahead only by more than the current setup's two runs differ.
        spread = numbers.random.uniform(0.0002, 0.09)
        measured = {}
        for setup in SCREEN_SETUPS:
            output = round(numbers.jitter(base * multiples.get(setup, 1.0), 0.003), 2)
            useful = round(output * useful_share.get(setup, 1.0), 2)
            prompt, answer = SCREEN_SHAPE[kind]
            measured[setup] = {
                "output": {"median": output, "min": output, "max": round(output * (1 + spread), 2)},
                "useful_output": {"median": useful, "min": useful, "max": round(useful * (1 + spread), 2)},
                "ttft_p95_ms": round(200 + 900 * (1 - useful_share.get(setup, 1.0)) + int(load) * 10, 2),
                "window": {"requests": float(int(load) * 5 + numbers.random.randint(0, 9)), "running": float(load) * 0.97, "waiting": 0.0,
                           "cache_use_max": round(min(0.99, 0.05 + int(load) / 90), 4), "prompt_tokens_mean": float(prompt), "completion_tokens_mean": float(answer),
                           "latency_p95_ms": round(answer * 30.0, 2), "generation_p95_ms": round(answer * 29.0, 2), "prefix_cache_hit_rate": 0.0},
            }
        cells.append({"key": key, "kind": kind, "requests_at_once": int(load), "reasoning": "off", "run": "synthetic screen",
                      "rules_built_from": "synthetic rules written after this screen", "measured": measured})
    return {"schema": "infercrane.planner-gym/v1", "title": "Synthetic: a dense model on one L4, every setup in turn on one machine, every number made up",
            "model": DENSE, "model_memory": {"weights_bytes": 16.4e9, "kv_cache_bytes_per_token": 147456.0}, "gpu": "L4", "engine": "vllm",
            "engine_version": "0.34.0", "max_num_seqs": 256, "first_token_limit_ms": 1000.0, "useful_output": "output tokens per second within the limit",
            "setups": SCREEN_SETUPS, "cells": cells, "product_cases": [], "current_setups": ["defaults"], "built_from": [{"file": "generate.py", "sha256": "synthetic"}]}


def confirm_cases(numbers: Numbers, screen: dict) -> dict:
    setups = {name: dict(SCREEN_SETUPS[name], image_digest=VLLM_IMAGE) for name in ("defaults", "fp8-weights", "fp8-both")}
    for setup in setups.values():
        setup.pop("blocks", None)
        setup.pop("block_tokens", None)
    cells = []
    for cell in screen["cells"]:
        measured = {}
        for name in setups:
            output = cell["measured"][name]["output"]["median"]
            useful = cell["measured"][name]["useful_output"]["median"]
            measured[name] = {"output": numbers.spread(numbers.jitter(output, 0.01)), "useful_output": numbers.spread(numbers.jitter(useful, 0.01))}
        cells.append({"key": cell["key"], "kind": cell["kind"], "requests_at_once": cell["requests_at_once"], "reasoning": "off", "measured": measured})
    # One cell whose servers disagree wildly: one served almost everything in time, two almost nothing.
    for cell in cells:
        if cell["key"] == "long/16":
            cell["measured"]["fp8-both"]["useful_output"] = {"median": 0.6, "min": 0.0, "max": 112.8, "servers": 3}
        if cell["key"] == "long/4":
            cell["measured"]["fp8-weights"]["useful_output"] = {"median": 68.3, "min": 8.2, "max": 68.4, "servers": 3}
    return {"schema": "infercrane.planner-gym/v1", "title": "Synthetic: the screen's dense model confirmed on three fresh L4 servers each, every number made up",
            "set": "synthetic-confirm", "recorded_at": RECORDED["confirm"], "basis": "fresh-servers", "model": DENSE, "model_facts": DENSE_FACTS,
            "gpu": "L4", "engine": "vllm", "engine_version": "0.34.0", "max_num_seqs": 0, "first_token_limit_ms": 1000.0, "baseline": "defaults",
            "current_setups": ["defaults"], "setups": setups, "cells": cells, "built_from": [{"file": "generate.py", "sha256": "synthetic"}]}


# ---------------------------------------------------------------- two engines on a large GPU

TWO_ENGINE_SETUPS = {
    "vllm-plain": {"label": "The model's plain command on vLLM", "engine": "vllm", "engine_version": "0.34.0", "drafts": 0, "kv_cache_dtype": "auto", "image_digest": VLLM_IMAGE},
    "vllm-mtp": {"label": "Plus speculation, 3 draft tokens", "engine": "vllm", "engine_version": "0.34.0", "drafts": 3, "speculation": "mtp", "kv_cache_dtype": "auto",
                 "image_digest": VLLM_IMAGE},
    "sglang-fp8": {"label": "SGLang with an FP8 KV cache", "engine": "sglang", "engine_version": "0.5.20", "drafts": 0, "kv_cache_dtype": "fp8_e4m3",
                   "flags": ["--context-length=65536", "--mem-fraction-static=0.90"], "image_digest": SGLANG_IMAGE},
    "sglang-mtp": {"label": "SGLang with speculation and bounded graphs", "engine": "sglang", "engine_version": "0.5.20", "drafts": 3, "speculation": "mtp",
                   "kv_cache_dtype": "fp8_e4m3", "flags": ["--context-length=65536", "--cuda-graph-bs-decode=1 2 4 8 16 32", "--mem-fraction-static=0.90"],
                   "image_digest": SGLANG_IMAGE},
}


def two_engine_cases(numbers: Numbers) -> dict:
    cells = []
    for kind in ("long", "short"):
        for load in (4, 12, 32):
            base = (75.0 if kind == "long" else 95.0) * load / (1 + load / (40 if kind == "long" else 90))
            level = math.log2(load) - 2
            multiples = {"vllm-plain": 1.0, "vllm-mtp": 1.75 - 0.18 * level * (1.4 if kind == "long" else 1.0), "sglang-fp8": 0.95,
                         "sglang-mtp": 1.45 - 0.25 * level}
            cells.append({"key": f"{kind}/{load}", "kind": kind, "requests_at_once": load, "reasoning": "off",
                          "measured": {name: {"output": numbers.spread(base * value)} for name, value in multiples.items()}})
    return {"schema": "infercrane.planner-gym/v1", "title": "Synthetic: the hybrid model on one H200 on two engines, every number made up",
            "set": "synthetic-two-engines", "recorded_at": RECORDED["two-engines"], "basis": "separate-servers", "model": HYBRID, "model_facts": HYBRID_FACTS,
            "gpu": "H200", "engine": "vllm", "engine_version": "0.34.0", "max_num_seqs": 0, "baseline": "vllm-plain", "current_setups": ["vllm-plain"],
            "setups": TWO_ENGINE_SETUPS, "cells": cells, "built_from": [{"file": "generate.py", "sha256": "synthetic"}]}


# ---------------------------------------------------------------- the rules' answers, as TestPlannerPredict writes them (trimmed)

def proposal(label: str, setup: str = "", measured: str = "") -> dict:
    out = {"label": label}
    if setup:
        out["setup"] = setup
    if measured:
        out["measured"] = measured
    return out


def map_rules(cases: dict) -> dict:
    cells = []
    for cell in cases["cells"]:
        load = cell["requests_at_once"]
        for current in ("plain", "three-drafts", "one-draft"):
            if current not in cell["measured"]:
                continue
            answers = []
            for objective in ("throughput", "useful-output"):
                if cell["kind"] == "long":
                    proposals = [proposal("Speculation, 3 draft tokens", "three-drafts")] if current == "plain" else []
                elif load <= 32:
                    proposals = {"plain": [proposal("Speculation, 3 draft tokens", "three-drafts")], "three-drafts": [],
                                 "one-draft": [proposal("Speculation, 3 draft tokens", "three-drafts")]}[current]
                elif load < 128:
                    proposals = {"plain": [proposal("Speculation, 3 draft tokens", "three-drafts"), proposal("Speculation, 1 draft token", "one-draft")],
                                 "three-drafts": [proposal("Speculation, 1 draft token", "one-draft")],
                                 "one-draft": [proposal("Fewer requests at once", "", "unmeasured: --max-num-seqs 48")]}[current]
                else:
                    proposals = {"plain": [proposal("FP8 KV cache", "plain-fp8-kv"), proposal("Larger batches", "", "cannot start: --max-num-seqs 2048, the engine holds at most 904")],
                                 "three-drafts": [proposal("Speculation off", "plain") if objective == "useful-output" else proposal("Speculation, 1 draft token", "one-draft"),
                                                  proposal("Fewer requests at once", "", "unmeasured: --max-num-seqs 32")],
                                 "one-draft": [proposal("Speculation, 1 draft token", "one-draft", "no change"),
                                               proposal("FP8 KV cache", "", "unmeasured: 1 draft tokens with a fp8 KV cache and the model's own weights")]}[current]
                answers.append({"objective": objective, "proposals": proposals})
            cells.append({"key": cell["key"], "current": current, "answers": answers})
    cells.sort(key=lambda item: (item["key"], item["current"]))
    return {"schema": "infercrane.planner-predictions/v1", "rules_version": "synthetic-rules", "model": HYBRID, "gpu": "H100",
            "note": "Synthetic rules' answers in TestPlannerPredict's trimmed format; made up to exercise every path, not asked of the planner.", "cells": cells}


def screen_rules(cases: dict) -> dict:
    cells = []
    for cell in cases["cells"]:
        proposals = {"short/16": [proposal("FP8 weights", "fp8-weights"), proposal("FP8 KV cache", "fp8-kv")],
                     "short/64": [proposal("FP8 weights and an FP8 KV cache", "fp8-both")]}.get(cell["key"], [])
        cells.append({"key": cell["key"], "current": "defaults", "answers": [{"objective": objective, "proposals": proposals} for objective in ("throughput", "useful-output")]})
    return {"schema": "infercrane.planner-predictions/v1", "rules_version": "synthetic-rules", "model": DENSE, "gpu": "L4",
            "note": "Synthetic rules' answers; made up to exercise every path, not asked of the planner.", "cells": cells}


# ---------------------------------------------------------------- a benchmark runner run directory

BENCH_ARMS = [
    {"key": "engine-defaults", "kind": "baseline", "setup_id": "example-8b-engine-defaults", "technique": "baseline-engine-defaults", "label": "Engine defaults (the current setup)", "args": []},
    {"key": "publisher", "kind": "baseline", "setup_id": "example-8b-publisher", "technique": "baseline-publisher", "label": "The publisher's documented command",
     "args": ["--reasoning-parser", "example_parser"]},
    {"key": "copy", "kind": "baseline", "setup_id": "example-8b-copy", "technique": "baseline-copy", "label": "A copy of the current setup", "args": ["--seed", "1"]},
    {"key": "fp8-weights", "kind": "candidate", "setup_id": "example-8b-fp8-weights", "technique": "fp8-weights", "label": "FP8 weights", "args": ["--quantization", "fp8"]},
    {"key": "fp8-kv-cache", "kind": "candidate", "setup_id": "example-8b-fp8-kv-cache", "technique": "fp8-kv-cache", "label": "FP8 KV cache", "args": ["--kv-cache-dtype", "fp8"]},
]
# per cell: the current setup's two runs, each other arm's useful output, and the cell's bars
BENCH_CELLS = {
    "example-8b/long/16": ([800.0, 804.0], {"example-8b-publisher": 900.0, "example-8b-copy": 795.0, "example-8b-fp8-weights": 1250.0, "example-8b-fp8-kv-cache": 1500.0},
                           {"one-machine": "pass", "repeats-steady": "pass", "copy-inside-noise": "pass"}),
    "example-8b/short/16": ([780.0, 782.0], {"example-8b-publisher": 880.0, "example-8b-copy": 805.0, "example-8b-fp8-weights": 1220.0, "example-8b-fp8-kv-cache": 1480.0},
                            {"one-machine": "pass", "repeats-steady": "pass", "copy-inside-noise": "fail"}),
}


def bench_run() -> dict[str, object]:
    files: dict[str, object] = {}
    summaries, cells, records = [], {}, []
    for cell_id, (runs, arms, bars) in BENCH_CELLS.items():
        _, kind, load = cell_id.split("/")
        mean = round(sum(runs) / len(runs), 2)
        spread = round(abs(runs[0] - runs[1]) / mean, 4)
        copy_change = abs(arms["example-8b-copy"] / mean - 1)
        noise = round(max(spread, copy_change, 0.02), 4)
        summaries.append({"cell": cell_id, "first_token_limit_ms": 3000 if kind == "long" else 1000, "machine_error": None,
                          "order": [arm["setup_id"] for arm in BENCH_ARMS] + [BENCH_ARMS[0]["setup_id"]],
                          "arms": {setup: {"key": next(a["key"] for a in BENCH_ARMS if a["setup_id"] == setup), "kind": next(a["kind"] for a in BENCH_ARMS if a["setup_id"] == setup),
                                           "useful_output": value, "output": value, "change": round(value / mean - 1, 4), "error": None, "requests": 50} for setup, value in arms.items()},
                          "current": BENCH_ARMS[0]["setup_id"], "current_useful_output": {"runs": runs, "mean": mean, "spread": spread},
                          "current_output": {"runs": runs, "mean": mean, "spread": spread}, "copy_change": round(copy_change, 4), "noise_floor": noise})
        cells[cell_id] = {"state": "done", "reason": "", "container_seconds": 50.0, "bars": bars, "found": "example-8b-fp8-kv-cache", "found_value": arms["example-8b-fp8-kv-cache"],
                          "best_baseline": "example-8b-publisher", "best_baseline_value": arms["example-8b-publisher"], "noise_floor": noise,
                          "verdicts": {setup: "passed" for setup in arms}}
        command = f"python3 tools/benchmark/bench_local.py tools/benchmark/specs/example-rung.json --out out --cells {cell_id}"
        for arm in BENCH_ARMS[1:]:
            change = round(arms[arm["setup_id"]] / mean - 1, 4)
            decision = "improved" if change > noise else ("regressed" if change < -noise else "no_measurable_difference")
            kv = "fp8" if arm["key"] == "fp8-kv-cache" else "bf16 (engine auto)"
            weights = "fp8 (quantized on load from bf16)" if arm["key"] == "fp8-weights" else "bf16"
            records.append({
                "baseline": {"basis": "same-machine", "setup": BENCH_ARMS[0]["setup_id"]}, "candidate": arm["setup_id"],
                "correctness": {"method": "task-score", "verdict": "passed"},
                "evidence": [{"digest": digest("synthetic cell record " + cell_id), "name": "cell-record", "summary": f"the cell's screen record ({cell_id})"}],
                "hardware": {"accelerator": "none (dry run, stand-in server)", "count": 0},
                "measured": {"change": change, "decision": decision, "metric": "useful_output_tokens_per_second", "noise_floor": noise},
                "precision": {"kv_cache": kv, "weights": weights},
                "reproduction": {"command": command, "commit": COMMIT, "image_digest": VLLM_IMAGE, "traffic_shape": f"{cell_id}: synthetic prompt file"},
                "schema_version": "infercrane.dev/technique-result/v1",
                "software": {"engine": "vllm", "engine_version": "0.34.0", "image_digest": VLLM_IMAGE},
                "technique": arm["technique"], "technique_version": "1.0.0",
                "workload": {"kind": kind, "load": int(load), "requests": 50, "shape_digest": digest("synthetic shape " + cell_id)},
            })
        files[f"cells/{cell_id.replace('/', '-')}/screen.json"] = {"benchmark": {
            "cell": {"id": cell_id, "kind": kind, "load": int(load), "first_token_limit_ms": 3000 if kind == "long" else 1000},
            "model": {"id": DENSE["id"], "revision": DENSE["revision"], "name": "example-8b", "precision": {"weights": "bf16"}},
            "engine": {"name": "vllm", "version": "0.34.0", "image": "vllm/vllm-openai@" + VLLM_IMAGE},
            "arms": [dict(arm, technique_version="1.0.0") for arm in BENCH_ARMS]}}
    files["rung.json"] = {"schema": "infercrane.dev/benchmark-run/v1", "run_id": "synthetic-rung", "dry": True, "backend": "local",
                          "spec": {"id": "example-rung", "sha256": digest("synthetic spec")[7:]}, "commit": COMMIT, "stopped": "",
                          "note": "synthetic run: every number is made up", "cells": cells, "summaries": summaries}
    files["results.jsonl"] = records
    files["plan.json"] = {"schema": "infercrane.dev/benchmark-run/v1", "run_id": "synthetic-rung", "spec": {"id": "example-rung", "models": [
        {"name": "example-8b", "id": DENSE["id"], "revision": DENSE["revision"], "kind": "dense, small", "precision": {"weights": "bf16", "kv_cache": "bf16 (engine auto)"},
         "weights": {"bytes": 16.4e9}}]}}
    files["run.json"] = {"run_id": "synthetic-rung", "stopped": "", "ended_at": RECORDED["bench"], "estimated_usd": 0.0,
                         "jobs": [{"cell": cell_id, "state": "done", "reason": ""} for cell_id in BENCH_CELLS]}
    return files


# ---------------------------------------------------------------- the prior gym's list of runs, and a ranking query

DATASETS = {"note": "The prior gym's runs (internal/resultprior TestPriorGym). INFERCRANE_PRIOR_GYM_DATA names a directory with its own datasets.json to score other runs.",
            "datasets": [
                {"name": "H100, hybrid model (synthetic map)", "file": "map-hybrid.json", "currents": ["plain", "three-drafts", "one-draft"], "rules": "rules-map-hybrid.json",
                 "rules_built_from": "synthetic: some cells are marked as the rules' source",
                 "options": {"architecture": "hybrid", "params_b": 27, "checkpoint": "fp8", "recorded_at": RECORDED["map"], "image_digest": VLLM_IMAGE,
                             "basis": "separate-servers", "commit": COMMIT[:8], "set": "synthetic-map"}},
                {"name": "L4, dense model (synthetic screen)", "file": "screen-dense.json", "currents": ["defaults"], "rules": "rules-screen-dense.json",
                 "rules_built_from": "synthetic: every cell is marked as the rules' source",
                 "options": {"architecture": "dense", "params_b": 8.2, "checkpoint": "bf16", "recorded_at": RECORDED["screen"], "image_digest": VLLM_IMAGE,
                             "basis": "same-machine", "set": "synthetic-screen"}},
                {"name": "L4, dense model (synthetic confirmation)", "file": "confirm-dense.json", "currents": ["defaults"], "rules": "rules-screen-dense.json",
                 "rules_built_from": "synthetic: the screen's cells"},
                {"name": "H200, hybrid model on two engines (synthetic)", "file": "two-engines-hybrid.json", "currents": ["vllm-plain"],
                 "rules_built_from": "not asked: the gym cannot express a change of engine"},
            ]}
QUERY = {"model": {"id": "example/dense-8b", "architecture": "dense", "params_b": 8.2, "checkpoint": "bf16"}, "accelerator": "L4", "engine_version": "0.34.0",
         "kind": "short", "load": 32, "metric": "output_tokens_per_second", "current": {"engine": "vllm"},
         "candidates": [{"name": "fp8-weights", "knobs": {"engine": "vllm", "quantization": "fp8"}},
                        {"name": "fp8-kv-cache", "knobs": {"engine": "vllm", "kv_cache": "fp8"}},
                        {"name": "fp8-weights-and-kv-cache", "knobs": {"engine": "vllm", "quantization": "fp8", "kv_cache": "fp8"}},
                        {"name": "prompt-lookup", "knobs": {"engine": "vllm", "speculation": "ngram"}},
                        {"name": "mtp-3", "knobs": {"engine": "vllm", "speculation": "mtp", "drafts": 3}}]}


def render() -> dict[str, str]:
    numbers = Numbers(SEED)
    screen = screen_cases(numbers)
    cases = map_cases(numbers)
    out: dict[str, object] = {"map-hybrid.json": cases, "screen-dense.json": screen, "confirm-dense.json": confirm_cases(numbers, screen),
                              "two-engines-hybrid.json": two_engine_cases(numbers), "rules-map-hybrid.json": map_rules(cases),
                              "rules-screen-dense.json": screen_rules(screen), "datasets.json": DATASETS, "rank-query.json": QUERY}
    for name, value in bench_run().items():
        out["bench-run/" + name] = value
    text: dict[str, str] = {}
    for name, value in out.items():
        if name.endswith(".jsonl"):
            text[name] = "".join(json.dumps(row, sort_keys=True) + "\n" for row in value)
        else:
            text[name] = json.dumps(value, indent=1) + "\n"
    return text


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--check", action="store_true", help="compare with the files on disk instead of writing them")
    args = parser.parse_args()
    stale = []
    for name, body in render().items():
        path = HERE / name
        if args.check:
            if not path.exists() or path.read_text() != body:
                stale.append(name)
            continue
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body)
    if stale:
        print("stale: " + ", ".join(stale) + " (run generate.py)", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
