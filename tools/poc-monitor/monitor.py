#!/usr/bin/env python3
"""Live engine view for the local optimization PoC.

Lists the serving pods InferCrane manages in one namespace, reads each pod's
Prometheus /metrics through the Kubernetes API server proxy, and redraws a
compact terminal view every interval: output and prompt tokens per second,
running and waiting requests, KV cache use, and speculative acceptance when
the engine reports them. llama.cpp, vLLM, and SGLang metric names are
understood. A missing metric is shown as "-", never as zero.

It reads only; it never changes the cluster. On a laptop the node is a CPU
machine, and the view says so instead of presenting GPU numbers.
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import time
from dataclasses import dataclass, field

SPARK = "▁▂▃▄▅▆▇█"
LINE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([-+0-9.eEinfNa]+)")

# Each signal lists the metric names engines use for it. Counters become rates.
COUNTERS = {
    "output_tokens": ("llamacpp:tokens_predicted_total", "vllm:generation_tokens_total", "sglang:generation_tokens_total"),
    "prompt_tokens": ("llamacpp:prompt_tokens_total", "vllm:prompt_tokens_total", "sglang:prompt_tokens_total"),
    "spec_draft": ("llamacpp:spec_decode_num_draft_tokens_total", "vllm:spec_decode_num_draft_tokens_total"),
    "spec_accepted": ("llamacpp:spec_decode_num_accepted_tokens_total", "vllm:spec_decode_num_accepted_tokens_total"),
    "preemptions": ("vllm:num_preemptions_total", "sglang:num_retracted_reqs_total"),
}
GAUGES = {
    "running": ("llamacpp:requests_processing", "vllm:num_requests_running", "sglang:num_running_reqs"),
    "waiting": ("llamacpp:requests_deferred", "vllm:num_requests_waiting", "sglang:num_queue_reqs"),
    "kv_usage": ("llamacpp:kv_cache_usage_ratio", "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc", "sglang:token_usage"),
}


@dataclass
class PodState:
    last: dict[str, float] = field(default_factory=dict)
    last_time: float = 0.0
    history: list[float] = field(default_factory=list)


def kubectl(context: str | None, *args: str, timeout: float = 10) -> str:
    command = ["kubectl"] + (["--context", context] if context else []) + list(args)
    completed = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    if completed.returncode != 0:
        raise RuntimeError(completed.stderr.strip() or "kubectl failed")
    return completed.stdout


def parse_metrics(text: str) -> dict[str, float]:
    totals: dict[str, float] = {}
    for raw in text.splitlines():
        if not raw or raw.startswith("#"):
            continue
        match = LINE.match(raw)
        if not match:
            continue
        try:
            value = float(match.group(3))
        except ValueError:
            continue
        totals[match.group(1)] = totals.get(match.group(1), 0.0) + value
    return totals


def pick(values: dict[str, float], names: tuple[str, ...]) -> float | None:
    for name in names:
        if name in values:
            return values[name]
    return None


def engine_name(values: dict[str, float]) -> str:
    for prefix, label in (("llamacpp:", "llama.cpp"), ("vllm:", "vLLM"), ("sglang:", "SGLang")):
        if any(key.startswith(prefix) for key in values):
            return label
    return "unknown"


def sparkline(history: list[float], width: int = 16) -> str:
    recent = history[-width:]
    if not recent:
        return ""
    peak = max(recent) or 1.0
    return "".join(SPARK[min(len(SPARK) - 1, int(value / peak * (len(SPARK) - 1)))] for value in recent)


def fmt(value: float | None, unit: str = "", digits: int = 0) -> str:
    if value is None:
        return "-"
    return f"{value:,.{digits}f}{unit}"


def node_banner(context: str | None, resource: str | None) -> str:
    try:
        nodes = json.loads(kubectl(context, "get", "nodes", "-o", "json"))["items"]
    except Exception as error:  # noqa: BLE001 - shown to the operator
        return f"nodes unavailable: {error}"
    gpus = 0
    for node in nodes:
        capacity = node.get("status", {}).get("capacity", {})
        if "nvidia.com/gpu" in capacity or "amd.com/gpu" in capacity:
            gpus += int(capacity.get("nvidia.com/gpu", capacity.get("amd.com/gpu", "0")))
    if gpus:
        return f"{len(nodes)} node(s), {gpus} GPU(s)"
    simulated = f"; scheduling uses a simulated accelerator ({resource})" if resource else ""
    return f"{len(nodes)} CPU node(s), no GPU{simulated}. Numbers here are CPU numbers, not GPU numbers."


def render(namespace: str, banner: str, rows: list[list[str]], note: str) -> None:
    width = shutil.get_terminal_size((100, 30)).columns
    out = ["\x1b[H\x1b[2J", f"\x1b[1mInferCrane engine monitor\x1b[0m  namespace {namespace}  {time.strftime('%H:%M:%S')}", banner, ""]
    header = ["pod", "engine", "out tok/s", "trend", "prompt tok/s", "running", "waiting", "KV cache", "spec accept"]
    widths = [max(len(header[i]), *(len(row[i]) for row in rows)) if rows else len(header[i]) for i in range(len(header))]
    out.append("  ".join(header[i].ljust(widths[i]) for i in range(len(header)))[:width])
    for row in rows:
        out.append("  ".join(row[i].ljust(widths[i]) for i in range(len(row)))[:width])
    if not rows:
        out.append("no serving pods yet")
    if note:
        out += ["", note[:width]]
    sys.stdout.write("\n".join(out) + "\n")
    sys.stdout.flush()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--context", default=None, help="kubectl context (default: current)")
    parser.add_argument("--namespace", default="infercrane-system")
    parser.add_argument("--selector", default="app.kubernetes.io/managed-by=infercrane")
    parser.add_argument("--port", type=int, default=8080, help="serving container port")
    parser.add_argument("--interval", type=float, default=2.0)
    parser.add_argument("--simulated-resource", default=None, help="name of the simulated accelerator resource, if any")
    parser.add_argument("--once", action="store_true", help="print one frame and exit")
    args = parser.parse_args()

    states: dict[str, PodState] = {}
    banner = node_banner(args.context, args.simulated_resource)
    while True:
        rows: list[list[str]] = []
        note = ""
        try:
            pods = json.loads(kubectl(args.context, "get", "pods", "-n", args.namespace, "-l", args.selector, "-o", "json"))["items"]
        except Exception as error:  # noqa: BLE001 - shown to the operator
            pods, note = [], f"cannot list pods: {error}"
        now = time.monotonic()
        for pod in pods:
            name = pod["metadata"]["name"]
            phase = pod.get("status", {}).get("phase", "")
            state = states.setdefault(name, PodState())
            if phase != "Running":
                rows.append([name, phase.lower() or "pending", "-", "", "-", "-", "-", "-", "-"])
                continue
            try:
                text = kubectl(args.context, "get", "--raw", f"/api/v1/namespaces/{args.namespace}/pods/{name}:{args.port}/proxy/metrics", timeout=5)
                values = parse_metrics(text)
            except Exception:  # noqa: BLE001 - a starting server has no metrics yet
                rows.append([name, "starting", "-", "", "-", "-", "-", "-", "-"])
                continue
            rates: dict[str, float | None] = {}
            elapsed = now - state.last_time if state.last_time else 0.0
            for key, names in COUNTERS.items():
                current = pick(values, names)
                previous = state.last.get(key)
                rates[key] = (current - previous) / elapsed if current is not None and previous is not None and elapsed > 0 and current >= previous else None
                if current is not None:
                    state.last[key] = current
            state.last_time = now
            output = rates["output_tokens"]
            state.history.append(output or 0.0)
            kv = pick(values, GAUGES["kv_usage"])
            kv_pct = None if kv is None else (kv * 100 if kv <= 1.0 else kv)
            drafted, accepted = rates["spec_draft"], rates["spec_accepted"]
            acceptance = accepted / drafted if drafted and accepted is not None and drafted > 0 else None
            rows.append([
                name, engine_name(values), fmt(output, digits=1), sparkline(state.history), fmt(rates["prompt_tokens"], digits=1),
                fmt(pick(values, GAUGES["running"])), fmt(pick(values, GAUGES["waiting"])), fmt(kv_pct, "%"), fmt(acceptance, digits=2),
            ])
        render(args.namespace, banner, rows, note)
        if args.once:
            return 0
        time.sleep(args.interval)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(0)
