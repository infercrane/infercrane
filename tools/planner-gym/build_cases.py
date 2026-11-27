#!/usr/bin/env python3
"""Builds the planner gym's cases from measured runs.

    build_cases.py --map EVIDENCE_DIR --product PRODUCT_EVIDENCE_DIR --out CASES.json

EVIDENCE_DIR is the map on one H100 of 2026-11-23 (`summary.json`, `crossover-summary.json`,
and the servers' result files in `raw/` and `raw-crossover/`). PRODUCT_EVIDENCE_DIR holds the
product path's records of the same day: eight planner checks on the map's cells and two
campaigns. Both folders live outside this repository; the cases file this writes is the test
data of `internal/optimizer` (planner_gym_test.go), and it records the hash of every file it
read.

Every number in the cases file is measured or read from a record. A value a run did not record
is left out, never filled in. Two kinds of case come out:

- **map window**: one per map cell and current setup. The planner input is rebuilt from what
  the map read inside its 90-second measured window (requests running and waiting, cache use,
  the engine's cache layout, prompt and answer lengths, first-token and request times).
- **product reading**: the planner input the product itself recorded when it was asked on a
  map cell (or before a campaign). The engine's cache layout, which the agent did not relay on
  23 November, is added from the map's servers of the same setup; the case says which values.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import statistics
from pathlib import Path

LIMIT_SECONDS = 1.0  # the first-token limit the gym scores "useful output" against

SETUP_IDS = {"h100-vllm-publisher": "plain", "h100-vllm-publisher-mtp3": "three-drafts", "h100-vllm-publisher-mtp1": "one-draft",
             "h100-planner-mtp3-kvfp8": "three-drafts-fp8-kv"}
# What each setup is, as the planner's setups describe themselves: draft tokens and KV cache
# format, and the cache layout its engine reported (cache_config_info) on every server.
SETUPS = {
    "plain": {"label": "The publisher's command (with --max-num-seqs 512)", "drafts": 0, "kv_cache_dtype": "auto"},
    "three-drafts": {"label": "Plus the documented speculation option, 3 draft tokens", "drafts": 3, "kv_cache_dtype": "auto"},
    "one-draft": {"label": "The same option with 1 draft token", "drafts": 1, "kv_cache_dtype": "auto"},
    "three-drafts-fp8-kv": {"label": "3 draft tokens plus an FP8 KV cache (arguments written by hand)", "drafts": 3, "kv_cache_dtype": "fp8"},
    # Measured only in campaign 2, by the product's judge; the map has no server of it.
    "plain-fp8-kv": {"label": "The publisher's command plus an FP8 KV cache", "drafts": 0, "kv_cache_dtype": "fp8"},
}
# The cells the planner's rules were written from, and by which change. The gym reports its
# score on these and on the rest separately.
BUILT_FROM = {
    "short/32": "capacity test (103ded87)", "short/48": "capacity test (103ded87)", "short/64": "capacity test (103ded87)",
    "short/96": "capacity test (103ded87)", "short/128": "capacity test (103ded87)",
    "long/128": "FP8 KV cache on a full state cache (103ded87)", "output/128": "FP8 KV cache on a full state cache (103ded87)",
}

# The product's eight planner checks on map cells (product-path-planner-cells.md). "waiting"
# is what the harness logged when the planner was asked; the recorded input has no waiting
# signal because the agent did not relay it then.
CHECKS = [
    ("1-short-128-speculation", "short/128", "three-drafts", 66),
    ("2-output-128-speculation", "output/128", "three-drafts", 74),
    ("3-short-128-publisher", "short/128", "plain", 0),
    ("4-output-128-publisher", "output/128", "plain", 0),
    ("5-short-32-publisher", "short/32", "plain", 0),
    ("6-output-32-publisher", "output/32", "plain", 0),
    ("7-prefix-32-publisher", "prefix/32", "plain", 0),
    ("8-long-32-publisher", "long/32", "plain", 0),
]
CAMPAIGNS = [
    ("campaign-1-short-chat-2026-11-23", "campaign 1: short chat trace replay at 32 from the plain command",
     {"Your current setup (the publisher's vLLM command)": "plain", "Speculative decoding with the model's own prediction head": "three-drafts"}),
    ("campaign-2-long-prefix-2026-11-23", "campaign 2: long shared prefix trace replay at 32 from the plain command (the burst case)",
     {"Your current setup (the publisher's vLLM command)": "plain", "FP8 KV cache": "plain-fp8-kv", "The publisher's documented speculation option": "three-drafts"}),
]


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def percentile(values: list[float], quantile: float) -> float | None:
    ordered = sorted(values)
    if not ordered:
        return None
    position = (len(ordered) - 1) * quantile
    lower, upper = int(position), min(int(position) + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def tokens_in_window(row: dict, start: float, end: float) -> float:
    """As the map's load generator counts output: a row's tokens spread evenly between its
    first and last token, the part inside the window (tools/fast-track-map, loadgen)."""
    if not row["ok"] or row["first"] is None:
        return 0.0
    first, last, tokens = row["first"], row["last"], row["completion_tokens"]
    if last <= first:
        return float(tokens) if start <= first < end else 0.0
    overlap = min(last, end) - max(first, start)
    return tokens * overlap / (last - first) if overlap > 0 else 0.0


def spread(values: list[float]) -> dict:
    return {"median": round(statistics.median(values), 2), "min": round(min(values), 2), "max": round(max(values), 2), "servers": len(values)}


def server_cells(path: Path) -> dict:
    """Per cell of one server: all output, output of requests whose first token came within
    the limit, and the request times the gateway would see, all inside the measured window."""
    return cells_of(json.loads(path.read_text())["cells"])


def cells_of(cells: list[dict]) -> dict:
    out = {}
    for cell in cells:
        columns = cell["rows"]["columns"]
        rows = [dict(zip(columns, values)) for values in cell["rows"]["rows"]]
        summary = cell["summary"]
        start = summary["warmup_seconds"]
        end = start + summary["window_seconds"]
        counted = [row for row in rows if not row["aborted"]]
        within = [row for row in counted if row["ok"] and row["first"] is not None and row["first"] - row["sent"] <= LIMIT_SECONDS]
        sent_in = [row for row in counted if start <= row["sent"] < end and row["ok"] and row["first"] is not None]
        delta = cell.get("metric_delta") or {}
        hits = sum(value for name, value in delta.items() if name.startswith("vllm:prefix_cache_hits_total"))
        queries = sum(value for name, value in delta.items() if name.startswith("vllm:prefix_cache_queries_total"))
        out[cell["key"]] = {
            "completed": sum(1 for row in counted if row["ok"]),
            "output": sum(tokens_in_window(row, start, end) for row in counted) / summary["window_seconds"],
            "useful_output": sum(tokens_in_window(row, start, end) for row in within) / summary["window_seconds"],
            "generation_p95_ms": percentile([(row["end"] - row["first"]) * 1000 for row in sent_in], 0.95),
            "latency_p95_ms": summary["latency_p95_ms"], "requests": summary["requests_sent_in_window"],
            "prefix_cache_hit_rate": hits / queries if queries else None,
        }
    return out


def cell_layout(cell: dict) -> dict:
    """The cache layout an engine reported in a cell (vLLM's cache_config_info). The state
    page is reported only by a model with recurrent-state layers."""
    for name in cell.get("metric_delta") or {}:
        if name.startswith("vllm:cache_config_info"):
            labels = dict(part.split("=", 1) for part in name[name.index("{") + 1:-1].split('",') if "=" in part)
            labels = {key: value.strip('"') for key, value in labels.items()}
            entry = {"blocks": int(labels["num_gpu_blocks"]), "block_tokens": int(labels["block_size"])}
            if labels.get("mamba_page_size_padded", "None") not in ("None", ""):
                entry["state_page_bytes"] = int(labels["mamba_page_size_padded"])
            return entry
    return {}


def window_of(cell: dict, values: dict) -> dict:
    """The readings of one cell of one server, in the gym's window form."""
    summary = cell["summary"]
    delta = cell.get("metric_delta") or {}
    gauge = lambda prefix: next((value for name, value in delta.items() if name.startswith(prefix)), None)  # noqa: E731
    drafted = gauge("vllm:spec_decode_num_drafts_total")
    accepted = gauge("vllm:spec_decode_num_accepted_tokens_total")
    window = {"requests": summary["requests_sent_in_window"], "prompt_tokens_mean": summary.get("prompt_tokens_mean"),
              "completion_tokens_mean": summary.get("completion_tokens_mean"), "latency_p95_ms": summary.get("latency_p95_ms"),
              "generation_p95_ms": values.get("generation_p95_ms"), "prefix_cache_hit_rate": values.get("prefix_cache_hit_rate"),
              # The mean of the five-second readings inside the window, where the runner kept them; else the last reading.
              "running": summary.get("requests_running_mean", gauge("vllm:num_requests_running")),
              "waiting": summary.get("requests_waiting_mean", gauge("vllm:num_requests_waiting")),
              "cache_use_max": summary.get("kv_cache_use_max", gauge("vllm:kv_cache_usage_perc")),
              "accepted_per_draft": accepted / drafted if drafted else None}
    return {key: round(value, 4) if isinstance(value, float) else value for key, value in window.items() if value is not None}


def single(value: float) -> dict:
    return {"median": round(value, 2), "min": round(value, 2), "max": round(value, 2), "servers": 1}


def server_cases(path: Path) -> dict:
    """A cases file from one measured server: its setup is the current setup of every cell,
    and nothing else was measured, so it serves predictions only.

    The product reads an endpoint over its last hour, so the planner input counts every request
    the cell completed (warm-up, window and the wait after it); the p95 times and the engine's
    readings are the measured window's. A 90-second window of long prompts on a small GPU holds
    fewer requests than the readiness rule's minimum of 20."""
    document = json.loads(path.read_text())
    setup = document["setup"]
    values = cells_of(document["cells"])
    cells, layout = [], {}
    for cell in document["cells"]:
        layout = layout or cell_layout(cell)
        window = window_of(cell, values[cell["key"]])
        window["requests_in_window"], window["requests"] = window["requests"], max(window["requests"], values[cell["key"]]["completed"])
        measured = {"output": single(values[cell["key"]]["output"]), "useful_output": single(values[cell["key"]]["useful_output"]),
                    "ttft_p50_ms": cell["summary"].get("ttft_p50_ms"), "ttft_p95_ms": cell["summary"].get("ttft_p95_ms"), "window": window}
        cells.append({"key": cell["key"], "kind": cell["shape"], "requests_at_once": cell["level"], "reasoning": "on" if cell.get("thinking") else "off",
                      "run": document.get("run_id", ""), "rules_built_from": "", "measured": {setup["id"]: measured}})
    # The plan's other setups are named too, so a proposal can be read as one of them.
    setups = {name: dict(entry) for name, entry in L4_SETUPS.items()} if setup["id"] in L4_SETUPS else {}
    setups[setup["id"]] = dict(setups.get(setup["id"]) or {"label": setup["label"], "drafts": 0, "kv_cache_dtype": "auto"}, **layout)
    return {"setups": setups, "cells": cells, "current": setup["id"], "model": document.get("model") or setup.get("model"), "gpu": (document.get("facts") or {}).get("name", "")}


def screen_spread(measured: dict, current: str) -> None:
    """On one machine the spread is what the current setup's two runs differ by (screen.py's
    rule): a setup is ahead of another only by more than that. Each setup's range becomes its
    value (the mean of the current setup's two runs) up to that value plus the difference, so
    the gym's "inside the spread of the best" reads "the best is not ahead of it". Without two
    runs of the current setup the difference is unknown and a range is its value alone."""
    for column in ("output", "useful_output"):
        reference = (measured.get(current) or {}).get(column) or {}
        two = reference.get("servers") == 2 and reference.get("median", 0) > 0
        noise = (reference["max"] - reference["min"]) / reference["median"] if two else 0.0
        for entry in measured.values():
            value = entry.get(column)
            if not value:
                continue
            entry[column] = {"median": value["median"], "min": value["median"], "max": round(value["median"] * (1 + noise), 2), "servers": value["servers"],
                             "runs": [value["min"], value["max"]] if value["servers"] == 2 else [value["median"]],
                             "noise": round(noise, 4) if two else None}


def screen_cases(path: Path, setup_table: dict) -> dict:
    """A cases file from a screen: every setup measured in turn on one machine. The current
    setup ran first and last; every other setup once. Ranges follow screen_spread."""
    document = json.loads(path.read_text())
    runs = document["runs"]
    current = runs[0]["setup"]["id"]
    by_setup: dict = {}
    for run in runs:
        if run.get("error"):
            continue
        by_setup.setdefault(run["setup"]["id"], []).append(run)
    cells = []
    for cell in runs[0].get("cells") or []:
        key, measured = cell["key"], {}
        for setup_id, setup_runs in by_setup.items():
            found = [(run, next((item for item in run.get("cells") or [] if item["key"] == key), None)) for run in setup_runs]
            found = [(run, item) for run, item in found if item]
            if not found:
                continue
            values = [cells_of([item])[key] for _, item in found]
            measured[setup_id] = {"output": spread([value["output"] for value in values]), "useful_output": spread([value["useful_output"] for value in values]),
                                  "ttft_p50_ms": found[0][1]["summary"].get("ttft_p50_ms"), "ttft_p95_ms": found[0][1]["summary"].get("ttft_p95_ms"),
                                  "window": window_of(found[0][1], values[0])}
        screen_spread(measured, current)
        cells.append({"key": key, "kind": cell["shape"], "requests_at_once": cell["level"], "reasoning": "on" if cell.get("thinking") else "off",
                      "run": document.get("run_id", ""), "rules_built_from": SCREEN_BUILT_FROM.get(document.get("run_id", ""), ""), "measured": measured})
    setups = {}
    for setup_id, setup_runs in by_setup.items():
        layout = next((cell_layout(item) for item in setup_runs[0].get("cells") or [] if cell_layout(item)), {})
        setups[setup_id] = dict(setup_table.get(setup_id) or {"label": setup_runs[0]["setup"]["label"], "drafts": 0, "kv_cache_dtype": "auto"}, **layout)
    return {"setups": setups, "cells": cells, "current": current, "model": document.get("model"), "gpu": (document.get("facts") or {}).get("name", "")}


def setup_of(path: Path) -> str:
    return SETUP_IDS[json.loads(path.read_text())["setup"]["id"]]


def layout(evidence: Path) -> dict:
    """The cache layout each setup's engine reported, read from its servers' results."""
    found: dict = {}
    for path in sorted((evidence / "raw").glob("h100-*.json")):
        document = json.loads(path.read_text())
        setup = SETUP_IDS[document["setup"]["id"]]
        for cell in document["cells"]:
            for name in cell.get("metric_delta") or {}:
                if not name.startswith("vllm:cache_config_info"):
                    continue
                labels = dict(part.split("=", 1) for part in name[name.index("{") + 1:-1].split('",') if "=" in part)
                labels = {key: value.strip('"') for key, value in labels.items()}
                entry = {"blocks": int(labels["num_gpu_blocks"]), "block_tokens": int(labels["block_size"]), "state_page_bytes": int(labels["mamba_page_size_padded"])}
                if found.setdefault(setup, entry) != entry:
                    raise SystemExit(f"{setup}: servers reported different cache layouts: {found[setup]} and {entry}")
    return found


def window_cases(evidence: Path, files: list) -> list[dict]:
    summaries = [json.loads((evidence / "summary.json").read_text()), json.loads((evidence / "crossover-summary.json").read_text())]
    per_server: dict = {}
    for folder in ("raw", "raw-crossover"):
        for path in sorted((evidence / folder).glob("h100-*.json")):
            files.append(path)
            setup = setup_of(path)
            for key, values in server_cells(path).items():
                per_server.setdefault(key, {}).setdefault(setup, []).append(values)
    cells = []
    for summary in summaries:
        for key, by_setup in summary["cells"].items():
            if key in {cell["key"] for cell in cells}:
                continue
            measured = {}
            for setup_id, row in by_setup.items():
                setup = SETUP_IDS[setup_id]
                servers = per_server[key][setup]
                median = lambda name: row[name]["median"] if row.get(name) else None  # noqa: E731
                generation = [server["generation_p95_ms"] for server in servers if server["generation_p95_ms"] is not None]
                hits = [server["prefix_cache_hit_rate"] for server in servers if server["prefix_cache_hit_rate"] is not None]
                measured[setup] = {
                    "output": spread([server["output"] for server in servers]),
                    "useful_output": spread([server["useful_output"] for server in servers]),
                    "ttft_p50_ms": median("ttft_p50_ms"), "ttft_p95_ms": median("ttft_p95_ms"),
                    "window": {key2: value for key2, value in {
                        "requests": int(statistics.median([server["requests"] for server in servers])),
                        "running": median("requests_running_mean"), "waiting": median("requests_waiting_mean"),
                        "cache_use_max": median("kv_cache_use_max"),
                        "prompt_tokens_mean": median("prompt_tokens_mean"), "completion_tokens_mean": median("completion_tokens_mean"),
                        "accepted_per_draft": median("accepted_per_draft"),
                        "latency_p95_ms": round(statistics.median([server["latency_p95_ms"] for server in servers]), 2),
                        "generation_p95_ms": round(statistics.median(generation), 2) if generation else None,
                        "prefix_cache_hit_rate": round(statistics.median(hits), 4) if hits else None,
                        "evicted": median("evicted"),
                    }.items() if value is not None},
                }
            kind, load = key.split("/")[0], int(key.split("/")[1])
            cells.append({"key": key, "kind": kind, "requests_at_once": load, "reasoning": "on" if key.endswith("/reasoning") else "off",
                          "run": "crossover" if summary is summaries[1] else "map part A and part B", "rules_built_from": BUILT_FROM.get(key, ""),
                          "measured": measured})
    order = {"short": 0, "prefix": 1, "long": 2, "output": 3}
    cells.sort(key=lambda cell: (order[cell["kind"]], cell["key"].endswith("/reasoning"), cell["requests_at_once"]))
    return cells


def product_cases(product: Path, cells: list[dict], layouts: dict, files: list) -> list[dict]:
    by_key = {cell["key"]: cell for cell in cells}
    out = []
    for name, cell_key, current, waiting in CHECKS:
        path = product / "planner-cells-2026-11-23" / f"{name}-result.json"
        files.append(path)
        record = json.loads(path.read_text())
        diagnosis = record["planner"]["input_diagnosis"]
        cell = by_key[cell_key]
        added = {"runtime_requests_waiting_per_replica": waiting, "runtime_kv_cache_blocks_per_replica": layouts[current]["blocks"],
                 "runtime_kv_cache_block_tokens": layouts[current]["block_tokens"], "runtime_state_cache_page_bytes": layouts[current]["state_page_bytes"],
                 "mean_output_tokens": cell["measured"][current]["window"]["completion_tokens_mean"]}
        if SETUPS[current]["drafts"]:
            added["runtime_spec_decode_draft_length"] = SETUPS[current]["drafts"]
        out.append({"name": f"check {name.split('-')[0]}: {cell_key}", "source": f"planner-cells-2026-11-23/{path.name}",
                    "cell": cell_key, "current": current, "requests_at_once": cell["requests_at_once"], "rules_built_from": cell["rules_built_from"],
                    "diagnosis": diagnosis, "added_signals": added,
                    "added_because": "The agent on 23 November did not relay these. Waiting is what the harness logged when the planner was asked; the cache layout is what this setup's engine reported on every map server; the answer length is the map's for this cell and setup."})
    for folder, title, arms in CAMPAIGNS:
        path = next((product / folder).glob("*-result.json"))
        files.append(path)
        record = json.loads(path.read_text())
        measured = {}
        for row in record["setups"]:
            setup = arms.get(row["label"])
            if setup is None:
                continue
            comparison = (row.get("comparison") or {}).get("output_token_throughput") or {}
            if not comparison:
                measured[setup] = {"output": {"median": 1.0, "min": 1.0, "max": 1.0, "servers": 1}, "judge": "the current setup"}
                continue
            delta, floor = comparison["delta"], comparison["noise_floor"]
            measured[setup] = {"output": {"median": round(1 + delta, 4), "min": round(1 + delta - floor, 4), "max": round(1 + delta + floor, 4), "servers": 1},
                               "judge": comparison["decision"]}
        out.append({"name": title, "source": f"{folder}/{path.name}", "cell": "", "current": "plain", "requests_at_once": 32, "rules_built_from": "",
                    "diagnosis": record["planner"]["input_diagnosis"],
                    "added_signals": {"runtime_kv_cache_blocks_per_replica": layouts["plain"]["blocks"], "runtime_kv_cache_block_tokens": layouts["plain"]["block_tokens"],
                                      "runtime_state_cache_page_bytes": layouts["plain"]["state_page_bytes"]},
                    "added_because": "The agent on 23 November did not relay the cache layout; it is what the plain command's engine reported on every map server. Waiting and answer length were not recorded and stay unknown.",
                    "measured": measured,
                    "measured_as": "relative output tokens per second against the current setup, from the product's judge in the campaign's first window; the range is the judge's noise floor"})
    return out


# The memory facts the product's model check reads for a model it checks: served weight bytes
# from Hugging Face's safetensors summary and KV cache bytes per token from config.json, at the
# commit the run served. Read 2026-11-25 from the public API.
MODEL_MEMORY = {("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"): {
    "weights_bytes": 8190735360 * 2, "kv_cache_bytes_per_token": 2 * 36 * 8 * 128 * 2,
    "source": "safetensors summary: 8,190,735,360 BF16 parameters; config.json: 36 layers, 8 KV heads, head size 128, bfloat16"}}

# Screens whose cells a rule set was written from, after the screen was seen.
SCREEN_BUILT_FROM = {"l4-8b-screen-1": "rules v9: written after this screen (cache room per precision change, the step-bytes share, FP8 weights and an FP8 KV cache together)"}


def small_model_file(document: dict, files: list[Path], gpu: str, title: str) -> dict:
    """The cases file of a run on another model: its own model and GPU, no product cases."""
    model = document["model"]
    return {"schema": "infercrane.planner-gym/v1", "title": title,
            "model": {"id": model["id"], "revision": model["revision"], "features": []},
            "model_memory": MODEL_MEMORY.get((model["id"], model["revision"])),
            "gpu": gpu, "engine": "vllm", "engine_version": "0.34.0", "max_num_seqs": 0,
            "first_token_limit_ms": LIMIT_SECONDS * 1000,
            "useful_output": "output tokens per second, counted inside the measured window, of requests whose first token came within the limit",
            "setups": document["setups"], "cells": document["cells"], "product_cases": [],
            # The setups the gym asks the planner from: the screen's or server's current setup.
            "current_setups": [document["current"]],
            "built_from": [{"file": path.name, "sha256": sha256(path)} for path in files]}


# What each setup of the L4 plan is, as the planner's setups describe themselves.
L4_SETUPS = {"l4-8b-defaults": {"label": "vLLM 0.34.0 defaults", "drafts": 0, "kv_cache_dtype": "auto"},
             "l4-8b-fp8-weights": {"label": "FP8 weights", "drafts": 0, "kv_cache_dtype": "auto", "quantization": "fp8"},
             "l4-8b-fp8-kv": {"label": "FP8 KV cache", "drafts": 0, "kv_cache_dtype": "fp8"},
             "l4-8b-fp8-both": {"label": "FP8 weights and an FP8 KV cache", "drafts": 0, "kv_cache_dtype": "fp8", "quantization": "fp8"},
             "l4-8b-ngram": {"label": "Prompt-lookup speculation", "drafts": 0, "kv_cache_dtype": "auto", "ngram": True}}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--map", type=Path)
    parser.add_argument("--product", type=Path)
    parser.add_argument("--server", type=Path, help="one measured server of another model: cases for predictions only")
    parser.add_argument("--screen", type=Path, help="a screen (several setups on one machine) of another model: cases the gym scores")
    parser.add_argument("--gpu", default="L4", help="with --server or --screen: the GPU name the planner is asked about")
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if args.server or args.screen:
        source = args.server or args.screen
        document = server_cases(source) if args.server else screen_cases(source, L4_SETUPS)
        title = f"{document['model']['id']} on one {args.gpu}: " + ("the current setup's measured server, for predictions" if args.server else "the screen of every setup on one machine")
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(json.dumps(small_model_file(document, [source], args.gpu, title), indent=1) + "\n")
        print(f"{len(document['cells'])} cells -> {args.out}")
        return 0
    if not (args.map and args.product):
        parser.error("--map and --product, or --server, or --screen")
    files: list = [args.map / "summary.json", args.map / "crossover-summary.json"]
    layouts = layout(args.map)
    for setup, entry in layouts.items():
        SETUPS[setup].update(entry)
    cells = window_cases(args.map, files)
    cases = product_cases(args.product, cells, layouts, files)
    document = {
        "schema": "infercrane.planner-gym/v1",
        "title": "Qwen/Qwen3.8-27B-FP8 on one H100, vLLM 0.34.0: the map, the crossover run and the product's planner checks of 2026-11-23",
        "model": {"id": "Qwen/Qwen3.8-27B-FP8", "revision": "c3a462cc3d38cf351f3b26cbd763469eeb74ef81", "features": ["native_mtp"]},
        "gpu": "H100", "engine": "vllm", "engine_version": "0.34.0",
        # vLLM 0.34.0 refuses more requests at once than this model's state cache holds on one H100.
        "max_num_seqs": 904,
        "first_token_limit_ms": LIMIT_SECONDS * 1000,
        "useful_output": "output tokens per second, counted inside the measured window, of requests whose first token came within the limit",
        "setups": SETUPS, "cells": cells, "product_cases": cases,
        "built_from": [{"file": str(path.relative_to(args.map.parent)), "sha256": sha256(path)} for path in files],
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(document, indent=1, sort_keys=False) + "\n")
    print(f"{len(cells)} cells, {len(cases)} product cases -> {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
